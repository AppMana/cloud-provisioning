// Command okdsite prepares and observes an official compact OKD installation.
// It deliberately does not advertise a remote-join matrix result yet.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/bringup"
	"github.com/appmana/cloud-provisioning/harness/e2e/cluster"
	"github.com/appmana/cloud-provisioning/harness/e2e/cluster/okd"
	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/observe"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig/coreos"
)

func main() {
	work := flag.String("work-dir", ".state/okd-site", "fresh site directory")
	tools := flag.String("tools", ".state/okd-tools", "verified release installer and oc binaries")
	disk := flag.String("disk", ".state/okd-tools/scos.qcow2", "verified SCOS stream disk for remote slots")
	prepareOnly := flag.Bool("prepare-only", false, "generate artifacts without changing the cldt lab")
	observeOnly := flag.Bool("observe-only", false, "observe an existing installed site without deploying or bootstrapping")
	resume := flag.Bool("resume", false, "restart preserved installed disks and configure only external appliances")
	timeout := flag.Duration("timeout", 3*time.Hour, "installation deadline")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	ctx, stop := context.WithTimeout(ctx, *timeout)
	defer stop()
	if (*prepareOnly && *observeOnly) || (*resume && (*prepareOnly || *observeOnly)) {
		fmt.Fprintln(os.Stderr, "prepare-only, observe-only and resume are mutually exclusive")
		os.Exit(1)
	}
	if err := run(ctx, *work, *tools, *disk, *prepareOnly, *observeOnly, *resume); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, work, tools, disk string, prepareOnly, observeOnly, resume bool) error {
	assets := okd.Assets{WorkDir: filepath.Join(work, "assets"), ToolsDir: tools}
	if err := os.MkdirAll(work, 0700); err != nil {
		return err
	}
	iso := filepath.Join(assets.WorkDir, "agent-serial.iso")
	if _, err := os.Stat(iso); os.IsNotExist(err) && !observeOnly {
		fmt.Println("Preparing the pinned OKD agent ISO and serial management")
		if err := assets.Prepare(ctx); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if prepareOnly {
		fmt.Println("Prepared: " + iso)
		return nil
	}
	lock, err := os.OpenFile(filepath.Join(os.TempDir(), "cloud-provisioning-cldt.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("another process owns the cldt lab: %w", err)
	}
	topo := okd.Topology()
	if resume {
		for _, n := range topo.NodesInRole(lab.ControlPlane) {
			if _, err := os.Stat(filepath.Join(work, "nodes", n.Name, "installed")); err != nil {
				return fmt.Errorf("resume requires an installed disk for %s: %w", n.Name, err)
			}
		}
	}
	r := coreos.New(topo, work, iso, disk)
	k := &kube.Client{Bastion: r.Node("bastion"), ControlPlanes: []string{"api." + okd.Domain}}
	if observeOnly {
		return observeSite(ctx, work, r, k)
	}
	host := bringup.LocalHost{Lab: topo.Name}
	fmt.Println("Deploying the compact SCOS site and two remote platform slots")
	if err := bringup.PrepareHost(ctx, topo, host); err != nil {
		return err
	}
	if err := r.Up(ctx); err != nil {
		return err
	}
	if resume {
		// OVN owns the installed guests' br-ex addresses and routes. Only
		// the newly recreated outer appliances need host-side configuration.
		appliances := topo
		appliances.Nodes = slices.DeleteFunc(slices.Clone(topo.Nodes), func(n lab.Node) bool { return n.IsClusterNode() })
		if err := bringup.Configure(ctx, appliances, r, host); err != nil {
			return err
		}
		if err := assets.StartDNS(ctx, r); err != nil {
			return err
		}
		config, err := os.ReadFile(filepath.Join(assets.WorkDir, "auth", "kubeconfig"))
		if err != nil {
			return err
		}
		if err := k.Install(ctx, config); err != nil {
			return err
		}
		return observeSite(ctx, work, r, k)
	}
	if err := bringup.Configure(ctx, topo, r, host); err != nil {
		return err
	}
	if err := r.ProveHardware(ctx); err != nil {
		return err
	}
	if err := bringup.Prove(ctx, topo, bringup.MixedProber{Topology: topo, Rig: r, Host: host}); err != nil {
		return err
	}
	if err := assets.StartDNS(ctx, r); err != nil {
		return err
	}
	fmt.Println("Waiting for the official installer; logs are in the assets directory")
	if err := assets.WaitInstall(ctx, r, k); err != nil {
		return err
	}
	return observeSite(ctx, work, r, k)
}

func observeSite(ctx context.Context, work string, r *coreos.Rig, k *kube.Client) error {
	nodes, err := cluster.WaitRegistered(ctx, k, r.Topology)
	if err != nil {
		return err
	}
	if _, err := k.Run(ctx, "wait", "node", "--all", "--for=condition=Ready", "--timeout=10m"); err != nil {
		return err
	}
	if err := r.ProveHardware(ctx); err != nil {
		return err
	}
	if err := observe.Capture(ctx, work, "okd-site-ready", k, r, nodes); err != nil {
		return err
	}
	for _, resource := range []string{"clusterversion", "clusteroperators", "network.operator.openshift.io"} {
		raw, err := k.Run(ctx, "get", resource, "-o", "json")
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(work, resource+".json"), raw, 0600); err != nil {
			return err
		}
	}
	if err := okd.ObserveMCS(ctx, work, k); err != nil {
		return err
	}
	fmt.Println("OKD site installed; remote Ignition joining remains a separate validation step")
	return nil
}
