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
	"strconv"
	"strings"

	clab "github.com/appmana/labcontainers/pkg/containerlab"
	"github.com/srl-labs/containerlab/core"
	"github.com/srl-labs/containerlab/links"
	"github.com/srl-labs/containerlab/types"
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
	// VMBaseImage is a disk the machines boot from instead of the one
	// their wrapper ships.
	//
	// For distributions that configure a node rather than making one.
	// A site node can be built in place, because the harness reaches
	// it before anything asks it to be a node; a remote cannot, since
	// it is launched as a new instance precisely so its own cloud-init
	// reads the rendered document, and that document joins a cluster
	// on a disk seconds old. Which is what a prepared image is for,
	// and what such a deployment assumes it has.
	VMBaseImage string

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
	config, err := t.ContainerlabConfig(rig)
	if err != nil {
		return "", err
	}
	source, err := clab.Source(config)
	if err != nil {
		return "", err
	}
	return string(source.GetYaml()), nil
}

// ContainerlabConfig returns upstream objects, not a YAML template or a second
// generic topology schema. This package's product-specific site/cloud model
// stays in cloud-provisioning; Labcontainers owns serialization and deployment.
func (t Topology) ContainerlabConfig(rig Rig) (*core.Config, error) {
	if rig != VM && rig != Container {
		return nil, fmt.Errorf("unknown rig %d", rig)
	}
	config := &core.Config{Name: t.Name, Topology: &types.Topology{
		Kinds: map[string]*types.NodeDefinition{
			"linux": {Image: ApplianceImage, NetworkMode: "none"},
		},
		Nodes: map[string]*types.NodeDefinition{},
	}}
	for _, segment := range t.Segments {
		config.Topology.Nodes[segment] = &types.NodeDefinition{Kind: "bridge"}
	}
	for _, node := range t.Nodes {
		definition := &types.NodeDefinition{Kind: "linux"}
		if rig == VM && node.IsClusterNode() {
			definition.Image = VMImage
			// etcd and kubelets must see the actual machine's name, not the
			// wrapper launcher's default hostname shared by every guest.
			definition.Cmd = "--hostname " + node.Name
			definition.Binds = []string{
				"seed/" + node.Name + "/extra-setup.sh:/extra-setup.sh:ro",
				"seed/" + node.Name + "/extra-userdata.yaml:/extra-userdata.yaml:ro",
				"seed/" + node.Name + "/extra-network.yaml:/extra-network.yaml:ro",
				"seed/" + node.Name + "/extra-authorized-keys:/extra-authorized-keys:ro",
			}
			if t.VMBaseImage != "" {
				definition.Binds = append(definition.Binds, t.VMBaseImage+":"+VMBaseImagePath+":ro")
			}
			definition.Env = map[string]string{
				"QEMU_MEMORY": strconv.Itoa(VMMemoryMB), "QEMU_SMP": strconv.Itoa(VMCPUs),
				"CLAB_MGMT_INTF": "eth1", "VR_MGMT_IS_A_LINK": "1",
			}
		}
		if rig == Container {
			definition.Binds = append([]string(nil), node.Binds...)
		}
		config.Topology.Nodes[node.Name] = definition
	}
	for _, link := range t.Links() {
		config.Topology.Links = append(config.Topology.Links, &links.LinkDefinition{
			Link: &links.LinkBriefRaw{Endpoints: []string{link.From, link.To}},
		})
	}
	return config, nil
}

// VMImage is the vrnetlab-wrapped guest image cluster nodes run under
// the VM rig.
//
// A stock Ubuntu cloud image in a vrnetlab wrapper. Nothing about the
// cluster is baked in: a machine gets its distribution the way a real
// one does, from what its userdata tells it to install, which is part
// of what this tier is for.
const VMImage = "cloud-provisioning/vm:single-nic"

// VMBaseImagePath is where the wrapper's launcher looks for the disk
// its guest boots from.
const VMBaseImagePath = "/jammy-ubuntu-cloud.qcow2"

// VMMemoryMB is what a cluster node gets under the VM rig. The
// upstream vrnetlab default is 512, which is a network device's worth
// and not enough to run a kubelet and a control plane.
const VMMemoryMB = 4096

// VMCPUs per cluster node.
const VMCPUs = 2

// Gateway is the edge a segment leaves by.
//
// One statement of the fact, because two would drift: the harness
// configures a running node by it, and a machine is handed it at boot
// in its platform's network configuration. A node addressed by one
// and routed by the other would be reachable and unable to answer.
func Gateway(segment string) (string, bool) {
	switch segment {
	case LANSegment:
		return SitePrefix + ".1", true
	case CloudASegment:
		return CloudAPrefix + ".1", true
	case CloudBSegment:
		return CloudBPrefix + ".1", true
	}
	return "", false
}

// MaxInterfaceName is what the kernel accepts, IFNAMSIZ less its
// terminator.
const MaxInterfaceName = 15

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
				To:   i.Segment + ":" + endpointName(i.Segment, n),
			})
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].To < out[b].To })
	return out
}

// endpointName names the bridge side of a cable.
//
// These become interface names on this host, so each must be unique
// on its bridge and short enough for the kernel to take. A name that
// says which node it leads to is what makes a stale one identifiable
// when a deploy refuses because the interface is still there, which
// happens after an ungraceful teardown.
//
// A cloud's own edge is named just "edge": within one cloud there is
// only one, so the suffix its node name carries to tell the two
// clouds apart repeats what the bridge already says.
// EndpointName is exported so that a rig re-plumbing a rebooted
// machine names its host side exactly as the deploy did.
func EndpointName(segment string, n Node) string { return endpointName(segment, n) }

func endpointName(segment string, n Node) string {
	short := strings.TrimPrefix(segment, "cldt-")
	switch short {
	case "cloud-a":
		short = "a"
	case "cloud-b":
		short = "b"
	}
	name := n.Name
	if n.Role == Edge && strings.HasPrefix(segment, "cldt-cloud-") {
		name = "edge"
	}
	return short + "-" + name
}
