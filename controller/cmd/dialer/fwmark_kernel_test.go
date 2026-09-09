package main

import (
	"net"
	"os"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// The tunnel's own encrypted packets carry a mark so that the dialer's
// table may hold routes to the very addresses the tunnel dials. That
// only works if the rule exempting the mark is consulted before any
// other rule that could match it, and a CNI's classifier matches by
// MASK: cilium installs "from all fwmark 0x200/0xf00 lookup 2004" at
// priority 9, our 0x205 & 0xf00 == 0x200, and its table 2004 is
// "local default dev lo". Every encrypted packet the dialer sent on a
// cilium node was therefore delivered to loopback: no egress, no
// handshake, and a join that waited for a tunnel that could never
// come up.
//
// This reproduces that collision against the real kernel and asserts
// the outcome that matters: a lookup carrying the tunnel's mark
// resolves to a real egress, not to loopback.
//
// Exercises the real kernel: run under a network namespace.
func TestTheTunnelsOwnPacketsEscapeAForeignMarkClassifier(t *testing.T) {
	if os.Getenv("CLDT_NETNS") != "1" {
		t.Skip("set CLDT_NETNS=1 inside a namespace")
	}

	const (
		fwmark       = 0x205
		table        = 517
		foreignTable = 2004
		foreignPrio  = 9
	)

	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "cldttest1"}}
	if err := netlink.LinkAdd(link); err != nil {
		t.Fatalf("adding the test link: %v", err)
	}
	defer netlink.LinkDel(link)
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatalf("bringing the test link up: %v", err)
	}
	addr, err := netlink.ParseAddr("10.10.0.11/24")
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.AddrAdd(link, addr); err != nil {
		t.Fatalf("addressing the test link: %v", err)
	}

	lo, err := netlink.LinkByName("lo")
	if err != nil {
		t.Fatalf("looking up loopback: %v", err)
	}
	if err := netlink.LinkSetUp(lo); err != nil {
		t.Fatalf("bringing loopback up: %v", err)
	}

	// The foreign datapath's own table, and the classifier that steers
	// into it: cilium's shape exactly, a local default via loopback.
	if err := netlink.RouteAdd(&netlink.Route{
		Table:     foreignTable,
		LinkIndex: lo.Attrs().Index,
		Dst:       &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)},
		Type:      unix.RTN_LOCAL,
		Scope:     netlink.SCOPE_HOST,
	}); err != nil {
		t.Fatalf("installing the foreign table's local default: %v", err)
	}
	mask := uint32(0xf00)
	foreign := netlink.NewRule()
	foreign.Family = netlink.FAMILY_V4
	foreign.Table = foreignTable
	foreign.Priority = foreignPrio
	foreign.Mark = 0x200
	foreign.Mask = &mask
	if err := netlink.RuleAdd(foreign); err != nil {
		t.Fatalf("installing the foreign classifier: %v", err)
	}
	defer netlink.RuleDel(foreign)

	if err := ensureRouteRule(table, fwmark); err != nil {
		t.Fatalf("ensureRouteRule: %v", err)
	}
	defer removeRouteRule(table)

	// The question the kernel answers, not the rule list this process
	// believes it wrote: where does a packet carrying the tunnel's
	// mark actually go?
	routes, err := netlink.RouteGetWithOptions(net.ParseIP("10.10.0.14"), &netlink.RouteGetOptions{Mark: fwmark})
	if err != nil {
		t.Fatalf("looking up a marked route: %v", err)
	}
	if len(routes) == 0 {
		t.Fatal("a marked lookup resolved to nothing")
	}
	if routes[0].LinkIndex == lo.Attrs().Index {
		t.Errorf("the tunnel's own packets were routed to loopback: the foreign classifier at priority %d captured mark %#x", foreignPrio, fwmark)
	}
	if routes[0].LinkIndex != link.Attrs().Index {
		t.Errorf("a marked lookup left by index %d, want the test link %d", routes[0].LinkIndex, link.Attrs().Index)
	}

	// And an unmarked lookup is none of our business: it must still
	// follow whatever the rest of the system decided, which here is
	// the foreign classifier's business only when the mark matches.
	plain, err := netlink.RouteGetWithOptions(net.ParseIP("10.10.0.14"), &netlink.RouteGetOptions{})
	if err != nil {
		t.Fatalf("looking up an unmarked route: %v", err)
	}
	if len(plain) == 0 || plain[0].LinkIndex != link.Attrs().Index {
		t.Error("an unmarked lookup no longer follows the ordinary path")
	}
}
