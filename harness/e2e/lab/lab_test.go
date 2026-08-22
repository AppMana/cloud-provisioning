package lab

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The topology's whole reason for existing is that the four segments
// are separate. A model that can silently put two of them on one
// bridge would reintroduce the failure the containerlab topology was
// built to end: the kind harness simulated the distance with host
// routes, the simulation could be bypassed, and it was.
func TestEverySegmentIsItsOwnBroadcastDomain(t *testing.T) {
	topo := Default()

	seen := map[string]string{}
	for _, n := range topo.Nodes {
		for _, iface := range n.Interfaces {
			if prior, ok := seen[iface.Address]; ok {
				t.Errorf("%s and %s both claim %s", prior, n.Name, iface.Address)
			}
			seen[iface.Address] = n.Name
		}
	}

	// The two remotes must not share a segment. Two remotes on one
	// bridge are neighbours on a broadcast domain, which is not two
	// clouds, and it would presuppose the answer to whether the mesh
	// forms directly between them.
	r1 := topo.MustNode("remote1").Segments()
	r2 := topo.MustNode("remote2").Segments()
	for _, a := range r1 {
		for _, b := range r2 {
			if a == b {
				t.Fatalf("remote1 and remote2 share segment %s, so they are not in two clouds", a)
			}
		}
	}
}

// A node reaches the segments its interfaces are on and no others.
// The site must not be able to address a cloud directly, because a
// site that can is not firewalled and every result taken on it means
// something weaker than it appears to.
func TestTheSiteAndTheCloudsShareNoSegment(t *testing.T) {
	topo := Default()

	site := map[string]bool{}
	for _, n := range topo.NodesInRole(ControlPlane, Worker) {
		for _, s := range n.Segments() {
			site[s] = true
		}
	}
	for _, n := range topo.NodesInRole(Remote) {
		for _, s := range n.Segments() {
			if site[s] {
				t.Errorf("%s is on %s, which a site node is also on", n.Name, s)
			}
		}
	}
}

// Three control planes, not two: stacked etcd needs a majority, and a
// majority of two is two, so with two either death halts the cluster.
// The outage rows exist to prove a single death costs nothing, which
// requires a third.
func TestQuorumSurvivesOneDeath(t *testing.T) {
	topo := Default()
	cps := topo.NodesInRole(ControlPlane)
	if len(cps) < 3 {
		t.Fatalf("%d control planes: a majority of them must survive one death", len(cps))
	}
	if len(cps)%2 == 0 {
		t.Errorf("%d control planes: an even count wastes a member without buying a failure", len(cps))
	}
}

// The generated topology has to be something containerlab accepts,
// and the node kind is the one thing that differs between the two
// rigs. Everything else — segments, links, addresses — is shared, so
// that a VM row and a container row differ in what a node *is* and
// nothing else.
func TestTheRigDecidesOnlyWhatANodeIsMadeOf(t *testing.T) {
	asContainers, err := Default().ContainerlabYAML(Container)
	if err != nil {
		t.Fatal(err)
	}
	asVMs, err := Default().ContainerlabYAML(VM)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(asContainers, NodeImage) {
		t.Error("the container rig does not name the node image")
	}
	// A machine is a wrapper carrying one, named by its image rather
	// than by a kind: containerlab runs it as an ordinary node.
	vmNodes := strings.Count(asVMs, "image: "+VMImage)
	if want := len(Default().NodesInRole(ControlPlane, Worker, Remote)); vmNodes != want {
		t.Errorf("the VM rig made %d nodes into machines, want %d (the cluster, and nothing else)", vmNodes, want)
	}
	for _, appliance := range []string{"router", "edge-a", "edge-b", "bastion"} {
		node, _, _ := strings.Cut(afterNode(t, asVMs, appliance), "\n    ")
		if strings.Contains(node, VMImage) {
			t.Errorf("%s is a machine: an appliance with no kubelet costs RAM and a boot for nothing", appliance)
		}
	}

	// The data plane is the same lab either way. Only the management
	// channel differs, because a machine needs one and a container
	// reached by docker exec does not.
	if dataLinks(t, asContainers) != dataLinks(t, asVMs) {
		t.Errorf("the two rigs wire the lab differently, so their rows are not comparable:\n container %s\n vm       %s",
			dataLinks(t, asContainers), dataLinks(t, asVMs))
	}
}

// Every machine is reached on a bridge of its own.
//
// containerlab's management network is one L2 for the whole lab, and
// a topology modelling a private site and two separate clouds cannot
// have one: it would give every remote a path to the site the routers
// do not explain, which is exactly how it went unnoticed the first
// time. So each machine gets a bridge with one node on it.
func TestEveryMachineIsReachedOnABridgeOfItsOwn(t *testing.T) {
	asVMs, err := Default().ContainerlabYAML(VM)
	if err != nil {
		t.Fatal(err)
	}

	on := map[string][]string{}
	for _, line := range strings.Split(asVMs, "\n") {
		m := regexp.MustCompile(`- endpoints: \["([^:]+):[^"]+", "([^:]+):`).FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if strings.HasPrefix(m[2], "cldt-mgmt-") {
			on[m[2]] = append(on[m[2]], m[1])
		}
	}

	machines := Default().NodesInRole(ControlPlane, Worker, Remote)
	if len(on) != len(machines) {
		t.Fatalf("%d management bridges for %d machines", len(on), len(machines))
	}
	for bridge, nodes := range on {
		if len(nodes) != 1 {
			t.Errorf("%s carries %v: a bridge with two machines on it is a path the segments do not explain",
				bridge, nodes)
		}
	}
	// And no appliance is on one, because nothing reaches an appliance
	// that way.
	for _, appliance := range []string{"router", "edge-a", "edge-b", "bastion"} {
		if _, ok := on[ManagementBridge(appliance)]; ok {
			t.Errorf("%s has a management bridge it does not need", appliance)
		}
	}
}

// dataLinks is every cable that is not a management one.
func dataLinks(t *testing.T, yaml string) string {
	t.Helper()
	_, links, ok := strings.Cut(yaml, "links:")
	if !ok {
		t.Fatal("the generated topology has no links")
	}
	var kept []string
	for _, line := range strings.Split(links, "\n") {
		if strings.TrimSpace(line) == "" || strings.Contains(line, "cldt-mgmt-") {
			continue
		}
		kept = append(kept, strings.TrimSpace(line))
	}
	sort.Strings(kept)
	return strings.Join(kept, "\n")
}

// afterNode returns the generated stanza for one node, so an
// assertion about that node cannot accidentally read another's.
func afterNode(t *testing.T, yaml, name string) string {
	t.Helper()
	_, rest, ok := strings.Cut(yaml, "\n    "+name+":\n")
	if !ok {
		t.Fatalf("the generated topology has no node %q", name)
	}
	return rest
}

func linksOf(t *testing.T, yaml string) string {
	t.Helper()
	_, links, ok := strings.Cut(yaml, "links:")
	if !ok {
		t.Fatal("the generated topology has no links")
	}
	return links
}
