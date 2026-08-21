// Command lab brings the topology up and leaves it standing.
//
// For working on the lab by hand: the rows themselves run under go
// test, where each is a subtest and the results are values rather
// than something to grep. This is the door into the same machinery
// when what is wanted is a lab to look at.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/bringup"
	"github.com/appmana/cloud-provisioning/harness/e2e/cluster"
	_ "github.com/appmana/cloud-provisioning/harness/e2e/cluster/kubeadm"
	"github.com/appmana/cloud-provisioning/harness/e2e/install"
	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/network"
	_ "github.com/appmana/cloud-provisioning/harness/e2e/network/calico"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig/container"
)

func main() {
	var (
		distro  = flag.String("distro", "", "also build the site cluster with this distribution")
		cni     = flag.String("cni", "", "also install this container network")
		product = flag.Bool("product", false, "also install the product's chart")
		repoDir = flag.String("repo-dir", "../..", "the repository root")
		workDir = flag.String("work-dir", "_work", "where the generated topology is written")
		down    = flag.Bool("down", false, "destroy the lab instead of building it")
		timeout = flag.Duration("timeout", 20*time.Minute, "deadline for the whole bring-up")
	)
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, *timeout)
	defer cancelTimeout()

	topo := lab.Default()
	r := container.New(topo, *workDir)
	host := bringup.LocalHost{Lab: topo.Name}

	if *down {
		if err := r.Down(ctx); err != nil {
			fail("tearing down: %v", err)
		}
		fmt.Println("the lab is gone")
		return
	}

	step("the segments")
	if err := bringup.PrepareHost(ctx, topo, host); err != nil {
		fail("preparing this host: %v", err)
	}

	step("deploying")
	if err := r.Up(ctx); err != nil {
		fail("deploying: %v", err)
	}

	step("addressing and routing")
	if err := bringup.Configure(ctx, topo, r, host); err != nil {
		fail("configuring: %v", err)
	}

	// Before anything is installed, because a topology that does not
	// isolate makes every result taken on it meaningless.
	step("proving it")
	if err := bringup.Prove(ctx, topo, host); err != nil {
		fail("%v", err)
	}

	if *distro != "" {
		b, err := cluster.For(*distro)
		if err != nil {
			fail("%v", err)
		}
		step("building the " + *distro + " site")
		d := cluster.Deps{
			Topology: topo, Rig: r, WorkDir: *workDir, Images: r,
			PodCIDR: "10.244.0.0/16", SvcCIDR: "10.96.0.0/12",
			Kube: &kube.Client{Bastion: r.Node("bastion"), ControlPlanes: cluster.ControlPlaneAddresses(topo)},
		}
		if err := b.Build(ctx, d); err != nil {
			fail("building the site: %v", err)
		}
		step("no kubelet depends on another node")
		if err := b.KubeletInvariant(ctx, d); err != nil {
			fail("%v", err)
		}
		nodes, err := d.Kube.Nodes(ctx)
		if err != nil {
			fail("reading the cluster's nodes: %v", err)
		}
		fmt.Printf("  registered: %v\n", nodes)

		if *cni != "" {
			inst, err := network.For(*cni)
			if err != nil {
				fail("%v", err)
			}
			step("installing " + *cni)
			nd := network.Deps{
				Topology: topo, Rig: r, Kube: d.Kube, Images: r, WorkDir: *workDir,
				PodCIDR: d.PodCIDR, APIServer: cluster.ControlPlaneAddresses(topo)[0],
			}
			if err := inst.Install(ctx, nd); err != nil {
				fail("installing %s: %v", *cni, err)
			}
			step("waiting for every node to be Ready")
			if err := waitReady(ctx, d.Kube, nodes); err != nil {
				fail("%v", err)
			}
			fmt.Println("  every node Ready")
		}

		if *product {
			step("installing the product")
			prod := &install.Product{
				Kube: d.Kube, Rig: r, Images: r, Topology: topo,
				RepoDir: *repoDir, WorkDir: *workDir,
			}
			sha, err := prod.Build(ctx)
			if err != nil {
				fail("building: %v", err)
			}
			fmt.Printf("  dialer %s\n", sha)
			if err := prod.Distribute(ctx, sha); err != nil {
				fail("distributing: %v", err)
			}
			if err := prod.Install(ctx, install.Options{
				TunnelEndpoints: "kubernetes.io/hostname in (w1,w2)",
				JoinProvider:    *distro,
			}, sha); err != nil {
				fail("%v", err)
			}
			fmt.Println("  the chart is installed")
		}
	}

	fmt.Println("\n  the site reaches both clouds")
	fmt.Println("  the two clouds reach each other across the wan")
	fmt.Println("  neither cloud reaches any address any site node holds")
	fmt.Println("  neither cloud reaches the API server, so a tunnel is the only way in")
	fmt.Println("  the site and both clouds reach the internet")
	fmt.Printf("\nsite     %s.0/24      bastion .2  cp .10  cp2 .13  cp3 .14  w1 .11  w2 .12  router .1\n", lab.SitePrefix)
	fmt.Printf("wan      %s.0/24  router .1  edge-a .2  edge-b .3  this host .254\n", lab.WANPrefix)
	fmt.Printf("cloud A  %s.0/24  remote1 .10\n", lab.CloudAPrefix)
	fmt.Printf("cloud B  %s.0/24    remote2 .10\n", lab.CloudBPrefix)
}

// waitReady blocks until every node reports Ready. A node with no
// network installed is legitimately NotReady, so this is only ever
// called after one is.
func waitReady(ctx context.Context, k *kube.Client, nodes []string) error {
	deadline := time.Now().Add(10 * time.Minute)
	for {
		all := len(nodes) > 0
		for _, n := range nodes {
			ready, err := k.Ready(ctx, n)
			if err != nil || !ready {
				all = false
				break
			}
		}
		if all {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("not every node became Ready")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

func step(name string) { fmt.Printf("--- %s ---\n", name) }

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "FAIL: "+format+"\n", args...)
	os.Exit(1)
}
