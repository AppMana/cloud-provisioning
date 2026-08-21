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

// Configure addresses every interface, sets every route, and applies
// each edge's policy.
func Configure(ctx context.Context, t lab.Topology, r rig.Rig, h Host) error {
	for _, n := range t.Nodes {
		node := r.Node(n.Name)
		for _, i := range n.Interfaces {
			if i.Address != "" {
				if _, err := node.Exec(ctx, "ip", "addr", "replace", i.Address, "dev", i.Name); err != nil {
					return fmt.Errorf("addressing %s %s: %w", n.Name, i.Name, err)
				}
			}
			if _, err := node.Exec(ctx, "ip", "link", "set", i.Name, "up"); err != nil {
				return fmt.Errorf("raising %s %s: %w", n.Name, i.Name, err)
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
	gateway := map[string]string{
		lab.LANSegment:    lab.SitePrefix + ".1",
		lab.CloudASegment: lab.CloudAPrefix + ".1",
		lab.CloudBSegment: lab.CloudBPrefix + ".1",
	}

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
			via, ok := gateway[seg]
			if !ok {
				return fmt.Errorf("%s is on %s, which has no edge", n.Name, seg)
			}
			if _, err := node.Exec(ctx, "ip", "route", "replace", "default",
				"via", via, "dev", n.Interfaces[0].Name); err != nil {
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
func Prove(ctx context.Context, t lab.Topology, h Host) error {
	site := t.NodesInRole(lab.ControlPlane, lab.Worker, lab.Bastion)
	remotes := t.NodesInRole(lab.Remote)

	// The site can get out.
	for _, n := range site {
		for _, r := range remotes {
			if !reaches(ctx, h, n.Name, r.Address(r.Interfaces[0].Segment), 3) {
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
			if !reaches(ctx, h, a.Name, b.Address(b.Interfaces[0].Segment), 3) {
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
			addrs, err := addresses(ctx, h, n.Name)
			if err != nil {
				return fmt.Errorf("reading %s's addresses: %w", n.Name, err)
			}
			// An empty list would test nothing and report success,
			// which is the failure mode this exists to prevent.
			if len(addrs) == 0 {
				return fmt.Errorf("%s reported no addresses, so this check would pass having tested nothing", n.Name)
			}
			for _, a := range addrs {
				if reaches(ctx, h, r.Name, a, 2) {
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
			if dials(ctx, h, r.Name, cp.Address(lab.LANSegment), 6443) {
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
		if !reaches(ctx, h, n.Name, "1.1.1.1", 3) {
			return fmt.Errorf("%s has no path off the lab", n.Name)
		}
	}
	return nil
}

func reaches(ctx context.Context, h Host, node, addr string, waitSeconds int) bool {
	_, err := h.InNamespace(ctx, node, "ping", "-c1", fmt.Sprintf("-W%d", waitSeconds), addr)
	return err == nil
}

func dials(ctx context.Context, h Host, node, addr string, port int) bool {
	_, err := h.InNamespace(ctx, node, "timeout", "3", "bash", "-c",
		fmt.Sprintf("</dev/tcp/%s/%d", addr, port))
	return err == nil
}

// addresses reads every global IPv4 address a node holds.
func addresses(ctx context.Context, h Host, node string) ([]string, error) {
	out, err := h.InNamespace(ctx, node, "ip", "-4", "-o", "addr", "show", "scope", "global")
	if err != nil {
		return nil, err
	}
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
	return addrs, nil
}
