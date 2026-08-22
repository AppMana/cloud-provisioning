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
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/bringup"
	"github.com/appmana/cloud-provisioning/harness/e2e/check"
	"github.com/appmana/cloud-provisioning/harness/e2e/claim"
	"github.com/appmana/cloud-provisioning/harness/e2e/cluster"
	_ "github.com/appmana/cloud-provisioning/harness/e2e/cluster/kubeadm"
	"github.com/appmana/cloud-provisioning/harness/e2e/install"
	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/network"
	_ "github.com/appmana/cloud-provisioning/harness/e2e/network/calico"
	"github.com/appmana/cloud-provisioning/harness/e2e/outage"
	"github.com/appmana/cloud-provisioning/harness/e2e/provider"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig/container"
)

func main() {
	var (
		distro  = flag.String("distro", "", "also build the site cluster with this distribution")
		cni     = flag.String("cni", "", "also install this container network")
		product = flag.Bool("product", false, "also install the product's chart")
		remotes = flag.String("remotes", "", "comma-separated remotes to claim and bootstrap (e.g. remote1)")
		checks  = flag.Bool("check", false, "also run the reachability matrix")
		outages = flag.String("outage", "", "also run an outage row: <victim>:<cut|reboot>")
		places  = flag.String("placements", "", "also walk these placements, comma separated (e.g. control-plane,two-workers,all-nodes)")
		repoDir = flag.String("repo-dir", "../..", "the repository root")
		workDir = flag.String("work-dir", "_work", "where the generated topology is written")
		down    = flag.Bool("down", false, "destroy the lab instead of building it")
		timeout = flag.Duration("timeout", 2*time.Hour, "deadline for the whole run")
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

		var claimed []string
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
			step("the kinds the product watches")
			if err := prod.ApplyCRDs(ctx); err != nil {
				fail("%v", err)
			}
			// cert-manager and Cluster API: the product's README names
			// both as dependencies, and the join path reads a field
			// only Cluster API's Machine controller writes.
			step("cert-manager and Cluster API")
			if err := prod.InstallCAPI(ctx); err != nil {
				fail("%v", err)
			}
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

			for _, name := range strings.Split(*remotes, ",") {
				if name = strings.TrimSpace(name); name == "" {
					continue
				}
				c := &claim.Claimer{
					Kube: d.Kube, Rig: r, Topology: topo, LabName: topo.Name,
					Provider: &provider.Controller{Kube: d.Kube, Topology: topo, LabName: topo.Name},
				}
				step("claiming " + name)
				if err := c.Claim(ctx, name, name); err != nil {
					fail("%v", err)
				}
				fmt.Println("  the mesh published a peer")

				step("bootstrapping " + name)
				if err := c.Bootstrap(ctx, name, name); err != nil {
					fail("%v", err)
				}

				// The node joins after the machine is reported, and
				// then the cloud tells Kubernetes which machine it is.
				// That is the cloud's job — a cloud controller manager
				// does it, or kubelet started with a provider — and
				// here the lab is the cloud.
				adoptCtx, cancelAdopt := context.WithTimeout(ctx, 10*time.Minute)
				err := c.Provider.AdoptNodes(adoptCtx, claim.Namespace, 5*time.Second)
				cancelAdopt()
				if err != nil {
					fail("%v", err)
				}

				claimed = append(claimed, name)
				fmt.Println("  the node ran the userdata the product rendered")
				if cn, ok := r.Node(name).(*container.Node); ok {
					for _, why := range cn.Accommodations() {
						fmt.Printf("  NOTE this rig %s\n", why)
					}
				}
			}

			if *checks {
				step("the reachability matrix")
				nodes, err := d.Kube.Nodes(ctx)
				if err != nil {
					fail("reading the cluster's nodes: %v", err)
				}
				if err := waitReady(ctx, d.Kube, nodes); err != nil {
					fail("%v", err)
				}
				pods := &check.Pods{
					Kube: d.Kube, Rig: r,
					Namespace:   check.UniqueNamespace(time.Now().Unix()),
					CRIEndpoint: b.CRIEndpoint(),
				}
				targets, err := pods.Start(ctx, nodes, 5*time.Minute)
				if err != nil {
					fail("%v", err)
				}
				defer pods.Stop(context.Background())

				// Now, and not before: a node has no pod block until
				// something that needs one runs on it, and the probes
				// are the first such thing. Asserting earlier would
				// wait for a block nothing had asked to be allocated.
				//
				// This is the product's own evidence that it resolved
				// the machine to a node — not Machine.status.nodeRef,
				// which Cluster API only writes when it holds a
				// connection to the workload cluster, and the setup
				// this product documents creates nothing that would
				// give it one.
				for _, name := range claimed {
					blocks, err := claim.WaitForPodBlocks(ctx, d.Kube, name, 5*time.Minute)
					if err != nil {
						fail("%v", err)
					}
					fmt.Printf("  the mesh published %s's pod blocks: %s\n", name, blocks)
				}

				// The placement axis: which site nodes hold tunnels.
				//
				// The claim is that changing it is invisible from the
				// pod network — a node with no tunnel reaches a remote
				// by transiting one that has one — so every ordered
				// pair must pass under every placement. The move goes
				// through helm, the way an operator makes it.
				for _, name := range strings.Split(*places, ",") {
					if name = strings.TrimSpace(name); name == "" {
						continue
					}
					placement, err := install.PlacementNamed(name)
					if err != nil {
						fail("%v", err)
					}
					step("placement: " + placement.Name + " (" + placement.Endpoints + ")")
					if err := prod.Move(ctx, placement, install.Options{
						JoinProvider: *distro,
					}, sha); err != nil {
						fail("%v", err)
					}
					pm := check.Converge(ctx, pods, targets,
						check.Options{Port: check.Port, ExternalURL: "http://1.1.1.1"},
						10*time.Minute, 15*time.Second)
					fmt.Println(pm.Report())
					if !pm.OK() {
						fail("the %s placement is not green", placement.Name)
					}
				}

				if *outages != "" {
					victim, mode, _ := strings.Cut(*outages, ":")
					step("outage: " + victim + " " + mode)
					res := outage.Run(ctx, outage.Row{
						Name: victim + "-" + mode, Victim: victim, Mode: outage.Mode(mode),
					}, outage.Deps{
						Rig: r, Prober: pods, Targets: targets,
						Options:  check.Options{Port: check.Port, ExternalURL: "http://1.1.1.1"},
						Converge: 15 * time.Minute, Down: 90 * time.Second,
						// What a platform gives a machine back: its NIC,
						// its address, its gateway. Everything else the
						// node must rebuild from what it runs at boot.
						// A pod that died with its node comes back at a
						// different address; measuring the old one reads
						// as a routing fault and is not.
						Refresh: func(ctx context.Context, want []string) ([]check.Target, error) {
							return pods.Start(ctx, want, 8*time.Minute)
						},
						Restart: func(ctx context.Context, v string) error {
							return bringup.Replumb(ctx, topo, r, host, v)
						},
					})
					fmt.Println(res)
					for _, m := range []*check.Matrix{res.Baseline, res.Survivors, res.Returned} {
						if m != nil {
							fmt.Println(m.Report())
						}
					}
					if !res.OK() {
						fail("the outage row failed")
					}
					return
				}

				m := check.Converge(ctx, pods, targets,
					check.Options{Port: check.Port, ExternalURL: "http://1.1.1.1"},
					5*time.Minute, 15*time.Second)
				fmt.Println(m.Report())
				if !m.OK() {
					fail("the matrix is not green")
				}
			}
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
