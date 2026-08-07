package main

import (
	"net"
	"os"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// Routes this dialer installs are this node's private knowledge of the
// mesh, not claims to announce. A CNI whose router learns alien routes
// from the main table re-announces everything it finds there that sits
// inside the cluster's pools, with this node as the owner. Every node
// with a tunnel holds routes for the whole mesh, so leaving them in
// main makes every such node claim every prefix, and a node choosing
// between claims steers traffic to a peer whose accept list will drop
// it. So the dialer's routes go in their own table, consulted by an ip
// rule for this node's own lookups and invisible to a main-table scan.
//
// Exercises the real kernel: run under a network namespace.
func TestInstalledRoutesAreInvisibleToAMainTableScan(t *testing.T) {
	if os.Getenv("CLDT_NETNS") != "1" {
		t.Skip("set CLDT_NETNS=1 inside a namespace")
	}

	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "cldttest0"}}
	if err := netlink.LinkAdd(link); err != nil {
		t.Fatalf("adding the test link: %v", err)
	}
	defer netlink.LinkDel(link)
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatalf("bringing the test link up: %v", err)
	}

	cfg := config{iface: "cldttest0", routeTable: 517}
	host := net.IPNet{IP: net.ParseIP("10.10.0.10").To4(), Mask: net.CIDRMask(32, 32)}
	block := net.IPNet{IP: net.ParseIP("10.244.242.64").To4(), Mask: net.CIDRMask(26, 32)}
	if err := installRoutes(cfg, []net.IPNet{host}, nil, []net.IPNet{block}, nil, nil); err != nil {
		t.Fatalf("installRoutes: %v", err)
	}

	inTable := func(table int, dst string) bool {
		routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4,
			&netlink.Route{Table: table}, netlink.RT_FILTER_TABLE)
		if err != nil {
			t.Fatalf("listing table %d: %v", table, err)
		}
		for _, r := range routes {
			if r.Dst != nil && r.Dst.String() == dst {
				return true
			}
		}
		return false
	}

	for _, dst := range []string{host.String(), block.String()} {
		if inTable(unix.RT_TABLE_MAIN, dst) {
			t.Errorf("%s is in the main table, where the CNI's router will learn it and claim it for this node", dst)
		}
		if !inTable(cfg.routeTable, dst) {
			t.Errorf("%s is not in table %d, so this node's own lookups cannot use it", dst, cfg.routeTable)
		}
	}

	// The table is consulted: a lookup for the host resolves through
	// the tunnel device, which is the ip rule doing its work.
	routes, err := netlink.RouteGet(net.ParseIP("10.10.0.10"))
	if err != nil || len(routes) == 0 {
		t.Fatalf("route lookup for the host: %v", err)
	}
	if routes[0].LinkIndex != link.Attrs().Index {
		t.Errorf("a lookup for the host resolves via link %d, not the tunnel device %d: the rule is not in place",
			routes[0].LinkIndex, link.Attrs().Index)
	}
}
