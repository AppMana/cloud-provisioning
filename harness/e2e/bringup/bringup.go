// Package bringup gives the topology its addresses and routes, and
// then proves it behaves like four separate segments.
//
// The proofs are the point. A topology that does not isolate makes
// every result taken on it mean something weaker than it appears to,
// and the failure is silent: the predecessor to this lab simulated
// the distance between site and cloud with host routes, the
// simulation could be bypassed, and it was. So the assertions run
// before anything is installed, and a lab that fails them is not used.
package bringup

import (
	"context"
	"fmt"
	"strings"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

// Host runs commands on the machine hosting the lab, for the parts
// that are not inside any node: the bridges, this host's own address
// on the wan, and the masquerade that gives the lab a way out.
type Host interface {
	Run(ctx context.Context, argv ...string) ([]byte, error)
	// InNamespace runs a command inside a node's network namespace,
	// using this host's tools.
	//
	// The node image ships no ping, so a reachability test run as a
	// command on the node fails because the tool is absent and reads
	// exactly like the network being broken. Same packets, same
	// interfaces, a result that means what it says.
	InNamespace(ctx context.Context, node string, argv ...string) ([]byte, error)
}

// Prober answers questions about a node from inside that node.
//
// Which is not the same place for every rig, and getting it wrong is
// silent. A container's interfaces are in its own namespace, so this
// host can enter it and use its own tools — the node image ships no
// ping, and a probe that fails because the tool is absent reads
// exactly like the network being broken. A machine's interfaces are
// inside the guest; its wrapper's namespace holds the taps qemu was
// handed and none of the addresses, so entering that would answer
// confidently about the wrong thing.
type Prober interface {
	// Reaches reports whether a node can reach an address.
	Reaches(ctx context.Context, node, addr string, waitSeconds int) bool
	// Dials reports whether a node can open a connection to a port.
	Dials(ctx context.Context, node, addr string, port int) bool
	// Addresses is every global IPv4 address a node holds.
	Addresses(ctx context.Context, node string) ([]string, error)
}

// HostProber answers from a container's own namespace, using this
// host's tools.
type HostProber struct{ Host Host }

func (p HostProber) Reaches(ctx context.Context, node, addr string, waitSeconds int) bool {
	_, err := p.Host.InNamespace(ctx, node, "ping", "-c1", fmt.Sprintf("-W%d", waitSeconds), addr)
	return err == nil
}

func (p HostProber) Dials(ctx context.Context, node, addr string, port int) bool {
	_, err := p.Host.InNamespace(ctx, node, "timeout", "3", "bash", "-c",
		fmt.Sprintf("</dev/tcp/%s/%d", addr, port))
	return err == nil
}

func (p HostProber) Addresses(ctx context.Context, node string) ([]string, error) {
	out, err := p.Host.InNamespace(ctx, node, "ip", "-4", "-o", "addr", "show", "scope", "global")
	if err != nil {
		return nil, err
	}
	return parseAddresses(out), nil
}

// GuestProber answers from inside a machine, which is where a
// machine's interfaces are.
type GuestProber struct{ Rig rig.Rig }

func (p GuestProber) Reaches(ctx context.Context, node, addr string, waitSeconds int) bool {
	_, err := p.Rig.Node(node).Exec(ctx, "ping", "-c1", fmt.Sprintf("-W%d", waitSeconds), addr)
	return err == nil
}

func (p GuestProber) Dials(ctx context.Context, node, addr string, port int) bool {
	_, err := p.Rig.Node(node).Exec(ctx, "timeout", "3", "bash", "-c",
		fmt.Sprintf("</dev/tcp/%s/%d", addr, port))
	return err == nil
}

func (p GuestProber) Addresses(ctx context.Context, node string) ([]string, error) {
	out, err := p.Rig.Node(node).Exec(ctx, "ip", "-4", "-o", "addr", "show", "scope", "global")
	if err != nil {
		return nil, err
	}
	return p.parse(out)
}

// parse retains every address: a hidden management address must fail the proof.
func (p GuestProber) parse(out []byte) ([]string, error) {
	return parseAddresses(out), nil
}

// MixedProber asks each node in whichever way that node can answer.
//
// A lab on machines still has containers in it — the routers, the
// cloud edges, the bastion — and the two cannot be asked the same
// way. A container is entered from this host because the node image
// ships no ping, and a probe that fails for a missing tool reads
// exactly like a broken network; a machine is asked from inside
// itself, because that is where its interfaces are.
//
// Getting this wrong is not an error, it is a wrong answer: the first
// VM bring-up reported that the bastion could not reach a remote,
// when what had happened is that ping does not exist in a container.
type MixedProber struct {
	Topology lab.Topology
	Rig      rig.Rig
	Host     Host
}

func (p MixedProber) prober(node string) Prober {
	if p.Topology.MustNode(node).IsClusterNode() {
		return GuestProber{Rig: p.Rig}
	}
	return HostProber{Host: p.Host}
}

func (p MixedProber) Reaches(ctx context.Context, node, addr string, waitSeconds int) bool {
	return p.prober(node).Reaches(ctx, node, addr, waitSeconds)
}

func (p MixedProber) Dials(ctx context.Context, node, addr string, port int) bool {
	return p.prober(node).Dials(ctx, node, addr, port)
}

func (p MixedProber) Addresses(ctx context.Context, node string) ([]string, error) {
	return p.prober(node).Addresses(ctx, node)
}

// Configure addresses every interface, sets every route, and applies
// each edge's policy.
func Configure(ctx context.Context, t lab.Topology, r rig.Rig, h Host) error {
	for _, n := range t.Nodes {
		node := r.Node(n.Name)
		for idx, i := range n.Interfaces {
			// The node's own name for the link, not the topology's: a
			// machine's kernel names interfaces for the bus it finds
			// them on, and addressing a name the guest does not have
			// succeeds at nothing while reporting nothing.
			dev := node.Interface(idx)
			if i.Address != "" {
				if _, err := node.Exec(ctx, "ip", "addr", "replace", i.Address, "dev", dev); err != nil {
					return fmt.Errorf("addressing %s %s: %w", n.Name, dev, err)
				}
			}
			if _, err := node.Exec(ctx, "ip", "link", "set", dev, "up"); err != nil {
				return fmt.Errorf("raising %s %s: %w", n.Name, dev, err)
			}
		}
	}

	if err := routes(ctx, t, r); err != nil {
		return err
	}
	return policy(ctx, t, r)
}

// routes points each node at its own segment's edge, and each edge at
// the others across the wan.
func routes(ctx context.Context, t lab.Topology, r rig.Rig) error {
	for _, n := range t.Nodes {
		node := r.Node(n.Name)
		switch n.Role {
		case lab.Router, lab.Edge:
			// An edge's own way out is this host, which is the wan's
			// last hop.
			if _, err := node.Exec(ctx, "ip", "route", "replace", "default",
				"via", lab.WANPrefix+".254", "dev", "eth2"); err != nil {
				return fmt.Errorf("%s default route: %w", n.Name, err)
			}
		default:
			seg := n.Interfaces[0].Segment
			via, ok := lab.Gateway(seg)
			if !ok {
				return fmt.Errorf("%s is on %s, which has no edge", n.Name, seg)
			}
			if _, err := node.Exec(ctx, "ip", "route", "replace", "default",
				"via", via, "dev", node.Interface(0)); err != nil {
				return fmt.Errorf("%s default route: %w", n.Name, err)
			}
		}
	}

	// Each edge knows how to reach the other clouds across the wan.
	// The site's router needs no route back into the site from
	// outside, because nothing outside addresses anything inside.
	cloudVia := map[string]string{
		lab.CloudAPrefix + ".0/24": lab.WANPrefix + ".2",
		lab.CloudBPrefix + ".0/24": lab.WANPrefix + ".3",
	}
	for _, n := range t.NodesInRole(lab.Router, lab.Edge) {
		node := r.Node(n.Name)
		for cidr, via := range cloudVia {
			if n.Address(lab.WANSegment) == via {
				continue // its own cloud
			}
			if _, err := node.Exec(ctx, "ip", "route", "replace", cidr, "via", via, "dev", "eth2"); err != nil {
				return fmt.Errorf("%s route to %s: %w", n.Name, cidr, err)
			}
		}
	}
	return nil
}

// policy is what each edge does, and it is the whole difference
// between a site and a cloud.
func policy(ctx context.Context, t lab.Topology, r rig.Rig) error {
	for _, n := range t.NodesInRole(lab.Router, lab.Edge) {
		node := r.Node(n.Name)
		if _, err := node.Exec(ctx, "sysctl", "-qw", "net.ipv4.ip_forward=1"); err != nil {
			return err
		}
		if _, err := node.Exec(ctx, "iptables", "-F", "FORWARD"); err != nil {
			return err
		}

		if n.Role == lab.Edge {
			// A public address is reachable, which is the reason a
			// remote is put in a cloud at all. No translation, no
			// filtering, both ways.
			if _, err := node.Exec(ctx, "iptables", "-P", "FORWARD", "ACCEPT"); err != nil {
				return err
			}
			continue
		}

		// The site: anything may leave wearing the router's address,
		// and only the answer to something that left may come back.
		//
		// Masquerading alone is not a site. A router that forwards
		// forwards inward too, and that is what the assertions below
		// caught the first time this was written.
		for _, argv := range [][]string{
			{"iptables", "-t", "nat", "-F", "POSTROUTING"},
			{"iptables", "-t", "nat", "-A", "POSTROUTING", "-s", lab.SitePrefix + ".0/24", "-o", "eth2", "-j", "MASQUERADE"},
			{"iptables", "-P", "FORWARD", "DROP"},
			{"iptables", "-A", "FORWARD", "-i", "eth1", "-o", "eth2", "-j", "ACCEPT"},
			{"iptables", "-A", "FORWARD", "-i", "eth2", "-o", "eth1", "-m", "conntrack",
				"--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"},
		} {
			if _, err := node.Exec(ctx, argv...); err != nil {
				return fmt.Errorf("%s policy: %w", n.Name, err)
			}
		}
	}
	return nil
}

// Prove asserts the topology behaves like four separate segments.
//
// Every failure here stops the lab, because each one means a
// different thing was measured than the one intended.
func Prove(ctx context.Context, t lab.Topology, p Prober) error {
	site := t.NodesInRole(lab.ControlPlane, lab.Worker, lab.Bastion)
	remotes := t.NodesInRole(lab.Remote)

	// The site can get out.
	for _, n := range site {
		for _, r := range remotes {
			if !reachesVia(ctx, p, n.Name, r.Address(r.Interfaces[0].Segment), 3) {
				return fmt.Errorf("%s cannot reach %s, so the site has no way out", n.Name, r.Name)
			}
		}
	}

	// Different clouds, meeting only across the wan.
	for _, a := range remotes {
		for _, b := range remotes {
			if a.Name == b.Name {
				continue
			}
			if !reachesVia(ctx, p, a.Name, b.Address(b.Interfaces[0].Segment), 3) {
				return fmt.Errorf("%s cannot reach %s, so the clouds do not meet", a.Name, b.Name)
			}
		}
	}

	// The property everything else rests on, checked against every
	// address a site node actually holds rather than the ones this
	// package happens to know.
	//
	// A back channel is by definition an address nobody thought to
	// check, which is exactly how containerlab's management network
	// went unnoticed: it joined the site and both clouds on one L2,
	// and every isolation result taken before it was found meant
	// nothing.
	for _, r := range remotes {
		for _, n := range site {
			addrs, err := addressesVia(ctx, p, n.Name)
			if err != nil {
				return fmt.Errorf("reading %s's addresses: %w", n.Name, err)
			}
			// An empty list would test nothing and report success,
			// which is the failure mode this exists to prevent.
			if len(addrs) == 0 {
				return fmt.Errorf("%s reported no addresses, so this check would pass having tested nothing", n.Name)
			}
			for _, a := range addrs {
				if reachesVia(ctx, p, r.Name, a, 2) {
					return fmt.Errorf("%s reached %s at %s: a path exists that the segments do not explain, "+
						"and every result taken on this lab would be meaningless", r.Name, n.Name, a)
				}
			}
		}
	}

	// A tunnel must be the only way in, or joining over one stays
	// untested.
	for _, r := range remotes {
		for _, cp := range t.NodesInRole(lab.ControlPlane) {
			if dialsVia(ctx, p, r.Name, cp.Address(lab.LANSegment), 6443) {
				return fmt.Errorf("%s opened a connection to %s's API server directly, "+
					"so a tunnel is not the only way in", r.Name, cp.Name)
			}
		}
	}

	// And everything has a path off the lab: the site through its own
	// router, a cloud node straight out from a public address.
	for _, n := range append(site, remotes...) {
		if n.Role == lab.Bastion {
			continue
		}
		if !reachesVia(ctx, p, n.Name, "1.1.1.1", 3) {
			return fmt.Errorf("%s has no path off the lab", n.Name)
		}
	}
	return nil
}

func reachesVia(ctx context.Context, p Prober, node, addr string, waitSeconds int) bool {
	return p.Reaches(ctx, node, addr, waitSeconds)
}

func dialsVia(ctx context.Context, p Prober, node, addr string, port int) bool {
	return p.Dials(ctx, node, addr, port)
}

func addressesVia(ctx context.Context, p Prober, node string) ([]string, error) {
	return p.Addresses(ctx, node)
}

// parseAddresses reads every global IPv4 address out of ip's output.
func parseAddresses(out []byte) []string {
	var addrs []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		for i, f := range fields {
			if f == "inet" && i+1 < len(fields) {
				addr, _, _ := strings.Cut(fields[i+1], "/")
				addrs = append(addrs, addr)
			}
		}
	}
	return addrs
}
