// Package lab is the topology: the site, two clouds, and the wan
// between them, as a value rather than a YAML file kept in step by
// hand.
//
// Four separate L2 segments, not one bridge with routes pretending:
//
//	cldt-lan      the site: control planes and workers, private
//	cldt-wan      the transit segment, standing in for the internet
//	cldt-cloud-a  one cloud, one remote with a public address
//	cldt-cloud-b  another cloud, another remote
//
// The three routers meet on the wan. The site's router masquerades
// what leaves and forwards nothing in, so the site can open a
// connection outward and nothing can open one to it. The cloud edges
// forward both ways, because a node with a public address is
// reachable, which is the whole reason a remote is put where it is.
//
// The predecessor put every node on one bridge and simulated the
// distance with host routes and masquerading. Simulating it meant it
// could be bypassed, and it was: the remote had to reach the API
// server directly while it booted, so the control plane was excluded
// from the masquerade, and that exception produced most of the
// failures worth chasing. Here a node has the interfaces its segments
// give it and nothing else.
//
// One model, two rigs. Which rig a row runs on decides what a node
// *is* — a container or a virtual machine — and nothing else: the
// segments, the links and the addresses are identical, so a VM row
// and a container row are comparable by construction.
package lab

import (
	"fmt"
	"sort"
	"strings"
)

// Rig is what a cluster node is made of.
type Rig int

const (
	// Container runs cluster nodes as kindest/node containers. Fast
	// enough to iterate on, and the kernel is real, but it shares this
	// host's kernel, has no bootloader, and never runs first-boot
	// userdata.
	Container Rig = iota
	// VM runs cluster nodes as virtual machines under QEMU/KVM. Slower
	// per row, and the only way to reach a real init path: per-node
	// kernels, a bootloader, and userdata a platform processes rather
	// than the harness interpreting it.
	VM
)

func (r Rig) String() string {
	if r == VM {
		return "vm"
	}
	return "container"
}

// Role is what a node is for. It decides which nodes become virtual
// machines under the VM rig (the cluster) and which stay containers
// (the appliances): a router with no kubelet gains nothing from a
// kernel of its own and costs RAM and a boot to give it one.
type Role int

const (
	// Router is the site's edge: anything may leave, nothing may
	// arrive.
	Router Role = iota
	// Edge is a cloud's edge, forwarding both ways.
	Edge
	// Bastion is how the site is reached and provisioned, and the only
	// thing that reaches it. An operator's jump host: it holds the
	// kubeconfig and runs kubectl, and under the VM rig it is also
	// what carries a session to a machine whose only NIC is on a
	// network nothing outside can address.
	Bastion
	// ControlPlane is a site node running the API server.
	ControlPlane
	// Worker is a site node that is not a control plane.
	Worker
	// Remote is a node in a cloud, joined over a tunnel.
	Remote
)

// Interface is one NIC on one segment.
type Interface struct {
	Name    string // inside the node: eth1, eth2
	Segment string // the bridge it lands on
	Address string // CIDR, or empty when the segment assigns none
}

// Node is one machine in the topology.
type Node struct {
	Name       string
	Role       Role
	Interfaces []Interface
	// Binds are host paths a container node needs so that a container
	// runtime's state does not land on an overlay filesystem, which
	// fails in ways that look like the product misbehaving. A VM has a
	// disk and needs none of it.
	Binds []string
}

// Segments lists the segments this node has an interface on.
func (n Node) Segments() []string {
	var out []string
	for _, i := range n.Interfaces {
		out = append(out, i.Segment)
	}
	return out
}

// Topology is the whole lab.
type Topology struct {
	Name     string
	Segments []string
	Nodes    []Node
}

// Site addressing. A control plane is .10, .13, .14 and a worker .11,
// .12 so that the numbering does not imply the roles are contiguous:
// which node holds a tunnel is a chart value, not a fact about where
// it sits in a list.
const (
	SitePrefix   = "10.10.0"
	WANPrefix    = "198.51.100"
	CloudAPrefix = "203.0.113"
	CloudBPrefix = "192.0.2"

	LANSegment    = "cldt-lan"
	WANSegment    = "cldt-wan"
	CloudASegment = "cldt-cloud-a"
	CloudBSegment = "cldt-cloud-b"
)

// Default is the topology every row runs on.
func Default() Topology {
	site := func(name string, role Role, host int) Node {
		return Node{
			Name: name, Role: role,
			Interfaces: []Interface{{Name: "eth1", Segment: LANSegment,
				Address: fmt.Sprintf("%s.%d/24", SitePrefix, host)}},
			Binds: clusterBinds(name),
		}
	}
	remote := func(name, prefix, segment string) Node {
		return Node{
			Name: name, Role: Remote,
			Interfaces: []Interface{{Name: "eth1", Segment: segment,
				Address: fmt.Sprintf("%s.10/24", prefix)}},
			Binds: clusterBinds(name),
		}
	}

	return Topology{
		Name:     "cldt",
		Segments: []string{LANSegment, WANSegment, CloudASegment, CloudBSegment},
		Nodes: []Node{
			{Name: "router", Role: Router, Interfaces: []Interface{
				{Name: "eth1", Segment: LANSegment, Address: SitePrefix + ".1/24"},
				{Name: "eth2", Segment: WANSegment, Address: WANPrefix + ".1/24"},
			}},
			{Name: "edge-a", Role: Edge, Interfaces: []Interface{
				{Name: "eth1", Segment: CloudASegment, Address: CloudAPrefix + ".1/24"},
				{Name: "eth2", Segment: WANSegment, Address: WANPrefix + ".2/24"},
			}},
			{Name: "edge-b", Role: Edge, Interfaces: []Interface{
				{Name: "eth1", Segment: CloudBSegment, Address: CloudBPrefix + ".1/24"},
				{Name: "eth2", Segment: WANSegment, Address: WANPrefix + ".3/24"},
			}},
			{Name: "bastion", Role: Bastion, Interfaces: []Interface{
				{Name: "eth1", Segment: LANSegment, Address: SitePrefix + ".2/24"},
			}},

			// Three control planes, because two is worse than one:
			// stacked etcd needs a majority, and a majority of two is
			// two, so either death halts the cluster.
			site("cp", ControlPlane, 10),
			site("cp2", ControlPlane, 13),
			site("cp3", ControlPlane, 14),
			site("w1", Worker, 11),
			site("w2", Worker, 12),

			// One node in each cloud, so that they are in different
			// clouds.
			remote("remote1", CloudAPrefix, CloudASegment),
			remote("remote2", CloudBPrefix, CloudBSegment),
		},
	}
}

// clusterBinds keeps each runtime's state off an overlay filesystem.
// A container runtime unpacking images onto the upper layer of the
// overlay it is itself running on fails in ways that read as the
// product misbehaving, so every distribution's data root is a real
// directory on the host.
func clusterBinds(name string) []string {
	return []string{
		"var/" + name + ":/var/lib/containerd",
		"var-k0s/" + name + ":/var/lib/k0s",
		"var-rancher/" + name + ":/var/lib/rancher",
	}
}

// MustNode returns the named node, panicking if the topology has no
// such node. Callers name nodes as constants, so a miss is a bug in
// the harness rather than a condition to handle.
func (t Topology) MustNode(name string) Node {
	for _, n := range t.Nodes {
		if n.Name == name {
			return n
		}
	}
	panic("lab: no node named " + name)
}

// NodesInRole returns every node in any of the given roles, in
// topology order.
func (t Topology) NodesInRole(roles ...Role) []Node {
	want := map[Role]bool{}
	for _, r := range roles {
		want[r] = true
	}
	var out []Node
	for _, n := range t.Nodes {
		if want[n.Role] {
			out = append(out, n)
		}
	}
	return out
}

// IsClusterNode reports whether this node runs a kubelet, which is
// what decides whether the VM rig gives it a machine of its own.
func (n Node) IsClusterNode() bool {
	return n.Role == ControlPlane || n.Role == Worker || n.Role == Remote
}

// Address returns the node's address on the given segment, without
// its prefix length.
func (n Node) Address(segment string) string {
	for _, i := range n.Interfaces {
		if i.Segment == segment {
			addr, _, _ := strings.Cut(i.Address, "/")
			return addr
		}
	}
	return ""
}

// NodeImage is the container image a cluster node runs under the
// container rig. It carries a container runtime and the kubelet's
// dependencies, which is what makes a container able to stand in for
// a machine at all.
const NodeImage = "kindest/node:v1.34.0"

// ApplianceImage is what the routers and the bastion run. They need
// no kubelet under either rig.
const ApplianceImage = NodeImage

// ContainerlabYAML renders the topology in the form containerlab
// consumes.
//
// The rig decides one thing: whether a cluster node is a container or
// a virtual machine. Segments, links and addresses are identical
// either way, so a row's results are comparable across rigs, and a
// divergence between them is a finding rather than a difference in
// what was measured.
func (t Topology) ContainerlabYAML(rig Rig) (string, error) {
	var b strings.Builder

	fmt.Fprintf(&b, "# Generated by harness/lab. Do not edit: the topology is a value\n")
	fmt.Fprintf(&b, "# in lab.Default(), so that both rigs derive from one model.\n")
	fmt.Fprintf(&b, "name: %s\n\ntopology:\n  kinds:\n", t.Name)

	// No management network under either rig. containerlab attaches
	// every node to one by default, which joins the site and both
	// clouds on a single L2: a remote then reaches the control plane
	// whatever the routers say, and the isolation this topology exists
	// to model is not real. It also hands each node a second address
	// to register, which a real instance does not have.
	fmt.Fprintf(&b, "    linux:\n      image: %s\n      network-mode: none\n", ApplianceImage)
	if rig == VM {
		// The guest is reached through its wrapper rather than over a
		// management network, for the same reason: a control channel
		// that is also an L2 shortcut between segments would answer
		// the questions this topology asks.
		fmt.Fprintf(&b, "    generic_vm:\n      image: %s\n      network-mode: none\n", VMImage)
	}

	fmt.Fprintf(&b, "\n  nodes:\n")
	for _, s := range t.Segments {
		fmt.Fprintf(&b, "    %s:\n      kind: bridge\n", s)
	}
	for _, n := range t.Nodes {
		kind := "linux"
		if rig == VM && n.IsClusterNode() {
			kind = "generic_vm"
		}
		fmt.Fprintf(&b, "    %s:\n      kind: %s\n", n.Name, kind)
		// Binds exist to keep a container runtime's state off an
		// overlay. A VM has a disk of its own and needs none of them.
		if rig == Container && len(n.Binds) > 0 {
			fmt.Fprintf(&b, "      binds:\n")
			for _, bind := range n.Binds {
				fmt.Fprintf(&b, "        - %s\n", bind)
			}
		}
		if rig == VM && n.IsClusterNode() {
			fmt.Fprintf(&b, "      env:\n        RAM: %d\n", VMMemoryMB)
		}
	}

	fmt.Fprintf(&b, "\n  links:\n")
	for _, l := range t.Links() {
		fmt.Fprintf(&b, "    - endpoints: [%q, %q]\n", l.From, l.To)
	}
	return b.String(), nil
}

// VMImage is the vrnetlab-wrapped guest image cluster nodes run under
// the VM rig.
const VMImage = "vrnetlab/cldt_site:latest"

// VMMemoryMB is what a cluster node gets under the VM rig. The
// upstream vrnetlab default is 512, which is not enough to run a
// kubelet and a control plane.
const VMMemoryMB = 4096

// Link is one cable.
type Link struct{ From, To string }

// Links derives every cable from the nodes' own interfaces, so that a
// node and its links cannot disagree.
func (t Topology) Links() []Link {
	var out []Link
	for _, n := range t.Nodes {
		for _, i := range n.Interfaces {
			out = append(out, Link{
				From: n.Name + ":" + i.Name,
				To:   i.Segment + ":" + endpointName(i.Segment, n.Name),
			})
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].To < out[b].To })
	return out
}

// endpointName names the bridge side of a cable. containerlab needs
// each to be unique on its bridge, and a name that says which node it
// leads to makes a stale interface on the host identifiable when a
// deploy refuses because one is still there.
func endpointName(segment, node string) string {
	short := strings.TrimPrefix(segment, "cldt-")
	switch short {
	case "cloud-a":
		short = "a"
	case "cloud-b":
		short = "b"
	}
	return short + "-" + node
}
