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
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/adoption"
	"github.com/appmana/cloud-provisioning/harness/e2e/bringup"
	"github.com/appmana/cloud-provisioning/harness/e2e/check"
	"github.com/appmana/cloud-provisioning/harness/e2e/claim"
	"github.com/appmana/cloud-provisioning/harness/e2e/cluster"
	"github.com/appmana/cloud-provisioning/harness/e2e/cluster/k0s"
	_ "github.com/appmana/cloud-provisioning/harness/e2e/cluster/k3s"
	_ "github.com/appmana/cloud-provisioning/harness/e2e/cluster/kubeadm"
	_ "github.com/appmana/cloud-provisioning/harness/e2e/cluster/microk8s"
	_ "github.com/appmana/cloud-provisioning/harness/e2e/cluster/rke2"
	"github.com/appmana/cloud-provisioning/harness/e2e/install"
	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/network"
	_ "github.com/appmana/cloud-provisioning/harness/e2e/network/calico"
	"github.com/appmana/cloud-provisioning/harness/e2e/observe"
	"github.com/appmana/cloud-provisioning/harness/e2e/outage"
	"github.com/appmana/cloud-provisioning/harness/e2e/provider"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig/container"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig/vm"
	"github.com/appmana/cloud-provisioning/harness/e2e/wait"
)

// NodeImageName is what cmd/nodeimage writes into the work directory.
const NodeImageName = "kubeadm-node.qcow2"

func main() { os.Exit(exitCode(runLab)) }

type runFailure struct{}

// Unwind lab cleanup before returning a failing process status. An immediate
// os.Exit in fail used to leave probe namespaces behind after failed runs.
func exitCode(run func()) (code int) {
	defer func() {
		if failure := recover(); failure != nil {
			if _, expected := failure.(runFailure); !expected {
				panic(failure)
			}
			code = 1
		}
	}()
	run()
	return 0
}

func runLab() {
	var (
		distro                 = flag.String("distro", "", "also build the site cluster with this distribution")
		cni                    = flag.String("cni", "", "distribution-supported network profile (default: bundled default)")
		calicoMTU              = flag.Int("k0s-calico-mtu", 0, "bundled Calico MTU for a fresh k0s site; zero preserves the distro default")
		calicoManagedAddresses = flag.Bool("k0s-calico-managed-addresses", false, "use stored per-node Calico addresses on a fresh k0s site instead of repeated IP autodetection")
		linuxEgress            = flag.Bool("k0s-linux-egress", false, "add Linux default-route Konnectivity agents on a fresh k0s VM site for mixed-OS API egress")
		product                = flag.Bool("product", false, "also install the product's chart")
		remoteSlots            = flag.Int("remote-slots", 2, "fixed single-NIC remote VM capacity (2..32), including spare group slots")
		remotes                = flag.String("remotes", "", "comma-separated remotes to claim and bootstrap (e.g. remote1)")
		lifecycle              = flag.Bool("lifecycle", true, "remove and re-add each claimed remote under every requested placement (initial two-workers if none)")
		capiMode               = flag.String("capi-mode", "imported", "imported (CAPI associates Nodes) or unconnected (product providerID fallback)")
		checks                 = flag.Bool("check", false, "also run the reachability matrix")
		outages                = flag.String("outage", "", "outages under each placement: all or comma-separated victim:cut/victim:reboot")
		places                 = flag.String("placements", "", "also walk these placements, comma separated (e.g. control-plane,two-workers,all-nodes)")
		repoDir                = flag.String("repo-dir", "../..", "the repository root")
		rigKind                = flag.String("rig", "vm", "cluster node platform: vm (single-NIC KVM guest) or legacy container")
		workDir                = flag.String("work-dir", "_work", "where the generated topology is written")
		reuseSite              = flag.Bool("reuse-site", false, "resume tests on a verified existing VM site; does not establish a fresh bringup")
		recoverHost            = flag.Bool("recover-host", false, "with -reuse-site after a host reboot: rebuild this host's segments and every wrapper's links, booting each machine from its existing disk")
		down                   = flag.Bool("down", false, "destroy the lab instead of building it")
		timeout                = flag.Duration("timeout", 2*time.Hour, "deadline for the whole run")
	)
	flag.Parse()
	if *capiMode != "imported" && *capiMode != "unconnected" {
		fail("unsupported CAPI mode %q", *capiMode)
	}
	var selected network.Profile
	if *distro != "" {
		var err error
		selected, err = network.Select(*distro, *cni)
		if err != nil {
			fail("%v", err)
		}
		*cni = selected.Network
	}
	if *calicoMTU != 0 && (*distro != "k0s" || selected.Network != "calico" || *reuseSite || *calicoMTU < 1280 || *calicoMTU > 65535) {
		fail("--k0s-calico-mtu requires a fresh k0s Calico site and an MTU between 1280 and 65535")
	}
	if *calicoManagedAddresses && (*distro != "k0s" || selected.Network != "calico" || *reuseSite) {
		fail("--k0s-calico-managed-addresses requires a fresh k0s Calico site")
	}
	if *linuxEgress && (*distro != "k0s" || *rigKind != "vm" || *reuseSite) {
		fail("--k0s-linux-egress requires a fresh k0s VM site")
	}
	if *recoverHost && (!*reuseSite || *rigKind != "vm") {
		fail("--recover-host rebuilds the host side of an existing VM site and requires -reuse-site")
	}

	if err := preconditions(*distro, *cni, *product, *remotes, *outages, *checks); err != nil {
		fail("%v", err)
	}

	if err := validatePlacements(*places, *checks); err != nil {
		fail("%v", err)
	}

	lock, err := os.OpenFile(filepath.Join(os.TempDir(), "cloud-provisioning-cldt.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		fail("%v", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fail("another process owns the cldt lab: %v", err)
	}

	if err := os.MkdirAll(*workDir, 0700); err != nil {
		fail("%v", err)
	}
	reportPath = filepath.Join(*workDir, "events.jsonl")
	if _, err := os.Stat(reportPath); err == nil {
		if err := os.Rename(reportPath, filepath.Join(*workDir, fmt.Sprintf("events-%d.jsonl", time.Now().UnixNano()))); err != nil {
			fail("%v", err)
		}
	}
	if err := os.WriteFile(reportPath, nil, 0600); err != nil {
		fail("%v", err)
	}
	recordEvent("run-start", map[string]any{"args": os.Args[1:], "profile": selected, "capiMode": *capiMode})

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, *timeout)
	defer cancelTimeout()

	topo, err := lab.WithRemoteSlots(*remoteSlots)
	if err != nil {
		fail("%v", err)
	}

	// Which disk the machines boot from, decided before the rig is
	// made: the rig renders the topology it was given.
	//
	// A distribution that configures a node rather than making one
	// needs one that is already a node. Its site could be built in
	// place, but a remote is launched as a new instance so that its
	// own cloud-init reads the rendered document, and that document
	// joins a cluster on a disk seconds old. Refused rather than
	// worked around: a run without the image would fail later, on the
	// remote, as something that looks like the product.
	if *rigKind == "vm" && *distro != "" {
		b, err := cluster.For(*distro)
		if err != nil {
			fail("%v", err)
		}
		if b.NeedsNodeImage() {
			if _, err := os.Stat(filepath.Join(*workDir, NodeImageName)); err != nil {
				fail("%s on machines boots a node image, and %s is not there: "+
					"build one with `go run ./cmd/nodeimage -work-dir %s`",
					*distro, filepath.Join(*workDir, NodeImageName), *workDir)
			}
			topo.VMBaseImage = NodeImageName
		}
	}

	if *rigKind == "vm" && topo.VMBaseImage == "" {
		if _, err := os.Stat(filepath.Join(*workDir, "platform-node.qcow2")); err != nil {
			fail("single-NIC guests require a platform image: go run ./cmd/nodeimage -platform-only -work-dir %s", *workDir)
		}
		topo.VMBaseImage = "platform-node.qcow2"
	}

	host := bringup.LocalHost{Lab: topo.Name}

	// What a node is made of, and nothing else: every stage below
	// reaches nodes through the same interface either way.
	var r rig.Rig
	var images cluster.Images
	var prober bringup.Prober
	switch *rigKind {
	case "container":
		cr := container.New(topo, *workDir)
		r, images, prober = cr, cr, bringup.HostProber{Host: host}
	case "vm":
		vr := vm.New(topo, *workDir)
		r, images, prober = vr, vr, bringup.MixedProber{Topology: topo, Rig: vr, Host: host}
	default:
		fail("no rig called %q: container or vm", *rigKind)
	}

	if *down {
		if err := r.Down(ctx); err != nil {
			fail("tearing down: %v", err)
		}
		fmt.Println("the lab is gone")
		return
	}

	if !*reuseSite {
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

	}

	if *recoverHost {
		step("recovering this host's side of the lab")
		if err := recoverHostSide(ctx, topo, r, host); err != nil {
			fail("recovering: %v", err)
		}
		recordEvent("host-recovered", topo.Name)
	}

	// Before anything is installed, because a topology that does not
	// isolate makes every result taken on it meaningless.
	if vr, ok := r.(*vm.Rig); ok {
		proof := vr.ProveHardware
		if *reuseSite {
			proof = vr.ProveExistingHardware
		}
		if err := proof(ctx); err != nil {
			fail("%v", err)
		}
	}
	if !*reuseSite {
		step("proving it")
		if err := bringup.Prove(ctx, topo, prober); err != nil {
			fail("%v", err)
		}

	}

	if *distro != "" {
		b, err := cluster.For(*distro)
		if err != nil {
			fail("%v", err)
		}
		step("building the " + *distro + " site")
		d := cluster.Deps{
			Topology: topo, Rig: r, WorkDir: *workDir, Network: selected.BuilderNetwork,
			K0sCalicoMTU:              *calicoMTU,
			K0sCalicoManagedAddresses: *calicoManagedAddresses,
			Images:                    cluster.Importer{Images: images, Args: b.ImportArgs()},
			PodCIDR:                   "10.244.0.0/16", SvcCIDR: "10.96.0.0/12",
			Kube: &kube.Client{Bastion: r.Node("bastion"), ControlPlanes: cluster.ControlPlaneAddresses(topo)},
		}
		if *reuseSite {
			reuser, ok := b.(cluster.SiteReuser)
			if !ok || *rigKind != "vm" {
				fail("%s does not support verified VM site reuse", b.Name())
			}
			if err := reuser.Reuse(ctx, d); err != nil {
				fail("verifying existing site: %v", err)
			}
			recordEvent("site-reused", selected)
		} else if err := b.Build(ctx, d); err != nil {
			fail("building the site: %v", err)
		}
		step("no kubelet depends on another node")
		if err := b.KubeletInvariant(ctx, d); err != nil {
			fail("%v", err)
		}
		// Every node the topology puts at the site, not whichever
		// happened to have registered by now: a short list here is
		// measured by everything downstream.
		nodes, err := cluster.WaitRegistered(ctx, d.Kube, topo)
		if err != nil {
			fail("%v", err)
		}
		fmt.Printf("  registered: %v\n", nodes)

		if *cni != "" {
			inst, err := network.ForProfile(*distro, *cni)
			if err != nil {
				fail("%v", err)
			}
			step("observing network " + *cni)
			nd := network.Deps{
				Topology: topo, Rig: r, Kube: d.Kube, Images: d.Images, WorkDir: *workDir,
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
			if err := observe.Capture(ctx, *workDir, "site-ready", d.Kube, r, rigNodes(topo, nodes)); err != nil {
				fail("%v", err)
			}
		}

		if *linuxEgress {
			step("installing Linux default-route Konnectivity agents")
			if err := k0s.InstallDefaultRouteAgents(ctx, d); err != nil {
				fail("%v", err)
			}
			recordEvent("k0s-linux-egress-installed", selected)
		}

		var claimed []string
		if *product {
			step("installing the product")
			prod := &install.Product{
				Kube: d.Kube, Rig: r, Images: d.Images, Topology: topo,
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
			// Where a node fetches the first-boot binary. It has to
			// answer before any remote is launched, because a remote
			// fetches it during its own first boot.
			origin := &install.Origin{Path: prod.BinaryPath()}
			if err := origin.Start(ctx); err != nil {
				fail("%v", err)
			}
			defer func() { _ = origin.Stop(context.Background()) }()

			if err := prod.Install(ctx, install.Options{
				TunnelEndpoints: "kubernetes.io/hostname in (w1,w2)",
				JoinProvider:    *distro,
				DialerURL:       origin.URL(),
			}, sha); err != nil {
				fail("%v", err)
			}
			fmt.Println("  the chart is installed")

			awaitAdoption := func(names []string) {
				for _, name := range names {
					observed, err := adoption.Wait(ctx, d.Kube, name, name, 5*time.Minute)
					if err != nil {
						fail("%v", err)
					}
					recordEvent("remote-adopted", observed)
				}
			}

			joinRemote := func(name string) {
				c := &claim.Claimer{
					ImportedControlPlane: *capiMode == "imported",
					Kube:                 d.Kube, Rig: r, Topology: topo, LabName: topo.Name,
					Provider: &provider.Controller{Kube: d.Kube, Rig: r, Topology: topo, LabName: topo.Name},
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

				if _, ok := r.Node(name).(rig.BootstrapCompletion); ok {
					if err := rig.WaitBootstrap(ctx, r.Node(name), 15*time.Minute); err != nil {
						fail("%v", err)
					}
					recordEvent("remote-bootstrap-completed", map[string]string{"node": name})
				}

				if c.ImportedControlPlane {
					if err := c.Provider.WaitAssociation(ctx, claim.Namespace, name, name); err != nil {
						fail("%v", err)
					}
					recordEvent("capi-node-associated", map[string]string{"machine": name, "node": name})
				}

				if !slices.Contains(claimed, name) {
					claimed = append(claimed, name)
				}
				fmt.Println("  the node ran the userdata the product rendered")

				// The dialer's image, now that this node has a runtime
				// of its own: the DaemonSet copy is what keeps its
				// peer list current once it has joined.
				if err := prod.LoadOnto(ctx, []string{name}); err != nil {
					fail("%v", err)
				}

				// And the network's images, for the same reason. Before the join it had none, and
				// on a distribution that brings its own containerd the
				// one on the node's PATH is not the one its kubelet
				// talks to.
				if *cni != "" {
					inst, err := network.ForProfile(*distro, *cni)
					if err != nil {
						fail("%v", err)
					}
					if err := inst.LoadImages(ctx, network.Deps{
						Topology: topo, Rig: r, Kube: d.Kube, Images: d.Images,
						WorkDir: *workDir, PodCIDR: d.PodCIDR,
					}, []string{name}); err != nil {
						fail("%v", err)
					}
				}
				awaitAdoption([]string{name})
				if cn, ok := r.Node(name).(*container.Node); ok {
					for _, why := range cn.Accommodations() {
						fmt.Printf("  NOTE this rig %s\n", why)
					}
				}
			}

			for _, name := range strings.Split(*remotes, ",") {
				if name = strings.TrimSpace(name); name != "" {
					joinRemote(name)
				}
			}

			if *checks {
				if vr, ok := r.(*vm.Rig); ok {
					if err := vr.ProveHardware(ctx); err != nil {
						fail("%v", err)
					}
				}
				step("the reachability matrix")
				// Every site node and every remote that was claimed: a
				// matrix taken before a remote joined measures a lab
				// that does not include the thing under test, and
				// passes.
				nodes, err := cluster.WaitRegistered(ctx, d.Kube, topo, claimed...)
				if err != nil {
					fail("%v", err)
				}
				if err := waitReady(ctx, d.Kube, nodes); err != nil {
					fail("%v", err)
				}
				// The probes run inside the pod being measured from
				// and reach it through the node's own CRI, so every
				// node needs the tool for that. A Kubernetes node
				// image has one; a cloud image does not.
				if err := cluster.EnsureCRICTL(ctx, r, *workDir, nodes); err != nil {
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
				defer func() {
					cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					pods.Stop(cleanup)
				}()

				// Now, and not before: a node has no pod block until
				// something that needs one runs on it, and the probes
				// are the first such thing. Asserting earlier would
				// wait for a block nothing had asked to be allocated.
				//
				// This checks the product's published pod reachability.
				// Imported mode separately checks the real CAPI
				// Machine controller's nodeRef association above;
				// association alone does not prove tunnel routes.
				for _, name := range claimed {
					inst, err := network.ForProfile(*distro, *cni)
					if err != nil {
						fail("%v", err)
					}
					blocks, err := claim.WaitForRemoteReach(ctx, d.Kube, name,
						inst.Encapsulation(), 5*time.Minute)
					if err != nil {
						fail("%v", err)
					}
					fmt.Printf("  the mesh carries %s's %s\n", name, blocks)
				}

				requireManagement := func(stage string, live []string) {
					if err := qualifyManagement(ctx, d.Kube, pods.Namespace, live, stage); err != nil {
						fail("%v", err)
					}
				}

				runLifecycle := func(placement install.Placement) {
					if !*lifecycle {
						return
					}
					recordEvent("lifecycle-start", placement.Name)
					for _, name := range slices.Clone(claimed) {
						if err := prod.WaitPlacement(ctx, placement); err != nil {
							fail("before removing %s: %v", name, err)
						}
						step("remove and re-add: " + name)
						oldUID, err := d.Kube.Get(ctx, "", "node", name, "{.metadata.uid}")
						if err != nil || oldUID == "" {
							fail("reading %s's original Node UID: %v", name, err)
						}
						c := &claim.Claimer{Kube: d.Kube, Rig: r, Topology: topo, LabName: topo.Name, Provider: &provider.Controller{Kube: d.Kube, Rig: r, Topology: topo, LabName: topo.Name}}
						if err := c.Delete(ctx, name, name); err != nil {
							fail("%v", err)
						}
						recordEvent("remote-removed", map[string]string{"node": name, "oldNodeUID": oldUID, "placement": placement.Name})
						surviving := slices.DeleteFunc(slices.Clone(nodes), func(n string) bool { return n == name })
						awaitAdoption(slices.DeleteFunc(slices.Clone(claimed), func(n string) bool { return n == name }))
						remaining, err := pods.Start(ctx, surviving, 5*time.Minute)
						if err != nil {
							fail("%v", err)
						}
						matrix := check.Converge(ctx, pods, remaining, matrixOptions(), 5*time.Minute, 15*time.Second)
						reportMatrix(matrix)
						if !matrix.OK() {
							fail("removing %s disrupted surviving nodes", name)
						}
						requireManagement("removed "+name+" at "+placement.Name, surviving)
						joinRemote(name)
						awaitAdoption(claimed)
						if err := prod.WaitPlacement(ctx, placement); err != nil {
							fail("after re-adding %s: %v", name, err)
						}
						newUID, err := d.Kube.Get(ctx, "", "node", name, "{.metadata.uid}")
						if err != nil || newUID == "" || oldUID == newUID {
							fail("%s did not register a new Node: %v", name, err)
						}
						if vr, ok := r.(*vm.Rig); ok {
							if err := vr.ProveHardware(ctx); err != nil {
								fail("hardware after replacing %s: %v", name, err)
							}
							recordEvent("replacement-hardware-proved", map[string]string{"node": name, "newNodeUID": newUID, "placement": placement.Name})
						}
						recordEvent("remote-recreated", map[string]string{"node": name, "oldNodeUID": oldUID, "newNodeUID": newUID, "placement": placement.Name})
						if err := cluster.EnsureCRICTL(ctx, r, *workDir, []string{name}); err != nil {
							fail("%v", err)
						}
						targets, err = pods.Start(ctx, nodes, 8*time.Minute)
						if err != nil {
							fail("%v", err)
						}
						matrix = check.Converge(ctx, pods, targets, matrixOptions(), 5*time.Minute, 15*time.Second)
						reportMatrix(matrix)
						if !matrix.OK() {
							fail("re-adding %s did not restore the full matrix", name)
						}
						requireManagement("recreated "+name+" at "+placement.Name, nodes)
					}
					recordEvent("lifecycle-passed", placement.Name)
				}
				if *lifecycle && strings.TrimSpace(*places) == "" {
					baseline := check.Converge(ctx, pods, targets, matrixOptions(), 5*time.Minute, 15*time.Second)
					reportMatrix(baseline)
					if !baseline.OK() {
						fail("baseline failed before lifecycle tests")
					}
					requireManagement("lifecycle baseline", nodes)
					runLifecycle(install.OnTwoWorkers)
				}

				if err := observe.Capture(ctx, *workDir, "remotes-ready", d.Kube, r, rigNodes(topo, nodes)); err != nil {
					fail("%v", err)
				}

				runOutages := func(placement install.Placement) {
					if *outages == "" {
						return
					}
					requests := strings.Split(*outages, ",")
					if *outages == "all" {
						requests = nil
						for _, node := range nodes {
							requests = append(requests, node+":cut", node+":reboot")
						}
					}
					for _, request := range requests {
						victim, mode, _ := strings.Cut(request, ":")
						step("outage: " + victim + " " + mode)
						var isolated []string
						if (placement.Name == "control-plane" && victim == "cp") || (placement.Name == "one-worker" && victim == "w1") {
							isolated = slices.Clone(claimed)
						}
						res := outage.Run(ctx, outage.Row{
							Name: victim + "-" + mode, Victim: victim, Mode: outage.Mode(mode), SiteIsolated: isolated,
						}, outage.Deps{
							Rig: r, Prober: pods, Targets: targets,
							Observe: func(ctx context.Context, stage string, survivors []string) error {
								return observe.Capture(ctx, *workDir, placement.Name+"-"+victim+"-"+mode+"-"+stage, d.Kube, r, survivors)
							},
							Options:  matrixOptions(),
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
						recordEvent("outage", res.String())
						for _, m := range []*check.Matrix{res.Baseline, res.Survivors, res.Returned} {
							if m != nil {
								reportMatrix(m)
							}
						}
						if !res.OK() {
							fail("the outage row failed")
						}
						targets, err = pods.Start(ctx, nodes, 8*time.Minute)
						if err != nil {
							fail("refreshing targets after outage: %v", err)
						}
					}
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
						DialerURL:    origin.URL(),
					}, sha); err != nil {
						fail("%v", err)
					}
					awaitAdoption(claimed)
					pm := check.Converge(ctx, pods, targets,
						matrixOptions(),
						10*time.Minute, 15*time.Second)
					reportMatrix(pm)
					if !pm.OK() {
						fail("the %s placement is not green", placement.Name)
					}
					requireManagement("placement "+placement.Name, nodes)
					runLifecycle(placement)
					runOutages(placement)
				}

				if strings.TrimSpace(*places) == "" {
					runOutages(install.OnTwoWorkers)
				}

				m := check.Converge(ctx, pods, targets,
					matrixOptions(),
					5*time.Minute, 15*time.Second)
				reportMatrix(m)
				if !m.OK() {
					fail("the matrix is not green")
				}
				requireManagement("final matrix", nodes)
			}
		}
	}

	recordEvent("run-passed", nil)
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
	if len(nodes) == 0 {
		return fmt.Errorf("no node was waited for, so this proved nothing")
	}
	return wait.Until(ctx, 10*time.Minute, "not every node became Ready", func(ctx context.Context) error {
		for _, n := range nodes {
			ready, err := k.Ready(ctx, n)
			if err != nil {
				return fmt.Errorf("%s: %w", n, err)
			}
			if !ready {
				return fmt.Errorf("%s is not Ready", n)
			}
		}
		return nil
	})
}

func step(name string) { recordEvent("stage", name); fmt.Printf("--- %s ---\n", name) }

func fail(format string, args ...any) {
	recordEvent("run-failed", fmt.Sprintf(format, args...))
	fmt.Fprintf(os.Stderr, "FAIL: "+format+"\n", args...)
	panic(runFailure{})
}
