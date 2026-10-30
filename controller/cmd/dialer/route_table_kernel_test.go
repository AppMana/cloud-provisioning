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
	if err := installRoutes(cfg, []net.IPNet{host}, nil, []net.IPNet{block}, nil, nil, nil); err != nil {
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

// Identity-site nodes carry multiple addresses on vip0 (cluster_vip 10.101.0.1/32,
// fd8f:cf26:522a::1/128 plus lan_vip 10.19.3.240/32, fd8f:cf26:522a::240/128),
// multi-metric on-link routes for 10.101.0.0/24 (metrics 100, 300, 640 with
// src 10.101.0.N), and jumbo /30 RoCE point-to-point links (10.255.0.1/30,
// 10.255.1.1/30 at MTU 9978). Verify in a real kernel namespace that:
//  1. installRoutes binds route Src strictly to the node's cluster identity
//     (10.101.0.1 / fd8f:cf26:522a::1), never to lan_vip (10.19.3.240 / ::240)
//     or RoCE /30 addresses (10.255.0.1).
//  2. underlayMTU() caps the automatic WireGuard MTU to the default-route LAN
//     link (1500 -> 1420) and is never inflated by the 9978-byte RoCE links.
//  3. Multi-metric on-link routes for 10.101.0.0/24 and RoCE /30 kernel routes
//     in the main table are preserved untouched across installRoutes.
func TestVIP0ServiceAddressesMultiMetricRoutesAndRoCELinksArePreserved(t *testing.T) {
	if os.Getenv("CLDT_NETNS") != "1" {
		t.Skip("set CLDT_NETNS=1 inside a namespace")
	}

	vip0 := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "vip0", MTU: 1500}}
	lan := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "enP7s7", MTU: 1500}}
	wifi := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "wlP9p1s0", MTU: 1500}}
	roce0 := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "enp1s0f1np1", MTU: 9978}}
	roce1 := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "enP2p1s0f1np1", MTU: 9978}}
	wg := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "cldt0", MTU: 1420}}
	for _, link := range []*netlink.Dummy{vip0, lan, wifi, roce0, roce1, wg} {
		if err := netlink.LinkAdd(link); err != nil {
			t.Fatal(err)
		}
		defer netlink.LinkDel(link)
		if err := netlink.LinkSetUp(link); err != nil {
			t.Fatal(err)
		}
	}

	// Put lan_vip BEFORE cluster_vip on vip0 to prove source selection uses
	// the node's Kubernetes InternalIPs (own) rather than interface address order.
	for _, item := range []struct {
		link netlink.Link
		cidr string
	}{
		{vip0, "10.19.3.240/32"},
		{vip0, "fd8f:cf26:522a::240/128"},
		{vip0, "10.101.0.1/32"},
		{vip0, "fd8f:cf26:522a::1/128"},
		{lan, "10.19.3.14/24"},
		{lan, "fd8f:cf26:522a::14/64"},
		{wifi, "10.19.3.114/24"},
		{roce0, "10.255.0.1/30"},
		{roce1, "10.255.1.1/30"},
	} {
		addr, err := netlink.ParseAddr(item.cidr)
		if err != nil {
			t.Fatal(err)
		}
		if addr.IP.To4() == nil {
			addr.Flags = unix.IFA_F_NODAD
		}
		if err := netlink.AddrAdd(item.link, addr); err != nil {
			t.Fatal(err)
		}
	}

	// Default routes: wired metric 100 (MTU 1500), Wi-Fi metric 640 (MTU 1500).
	defaultDst := &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}
	for _, r := range []*netlink.Route{
		{LinkIndex: lan.Attrs().Index, Dst: defaultDst, Gw: net.ParseIP("10.19.3.1"), Priority: 100},
		{LinkIndex: wifi.Attrs().Index, Dst: defaultDst, Gw: net.ParseIP("10.19.3.1"), Priority: 640},
	} {
		if err := netlink.RouteAdd(r); err != nil {
			t.Fatal(err)
		}
	}

	// Multi-metric on-link routes for 10.101.0.0/24 with src 10.101.0.1.
	_, vipCIDR, _ := net.ParseCIDR("10.101.0.0/24")
	for _, item := range []struct {
		link   netlink.Link
		metric int
	}{
		{lan, 100},
		{lan, 300},
		{wifi, 640},
	} {
		r := &netlink.Route{
			LinkIndex: item.link.Attrs().Index,
			Dst:       vipCIDR,
			Src:       net.ParseIP("10.101.0.1"),
			Scope:     netlink.SCOPE_LINK,
			Protocol:  unix.RTPROT_STATIC,
			Priority:  item.metric,
		}
		if err := netlink.RouteAdd(r); err != nil {
			t.Fatal(err)
		}
	}

	// 1. Jumbo RoCE links (9978 MTU) must not inflate underlayMTU() above 1420.
	if got := underlayMTU(); got != 1420 {
		t.Fatalf("underlayMTU() = %d with 9978-byte RoCE links, want 1420", got)
	}

	// 2. installRoutes must bind Src to cluster_vip (10.101.0.1 / fd8f:cf26:522a::1),
	// never lan_vip (10.19.3.240 / ::240) or RoCE (10.255.0.1).
	cfg := config{iface: wg.Attrs().Name, routeTable: 517, fwmark: 517}
	defer removeRouteRule(cfg.routeTable, cfg.fwmark)
	_, block4, _ := net.ParseCIDR("10.244.64.0/26")
	_, block6, _ := net.ParseCIDR("fd00:10:244::4000/122")
	own := []string{"10.101.0.1", "fd8f:cf26:522a::1"}
	if err := installRoutes(cfg, nil, nil, []net.IPNet{*block4, *block6}, nil, nil, own); err != nil {
		t.Fatalf("installRoutes: %v", err)
	}

	routes4, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: cfg.routeTable}, netlink.RT_FILTER_TABLE)
	if err != nil || len(routes4) != 1 || !routes4[0].Src.Equal(net.ParseIP("10.101.0.1")) {
		t.Fatalf("IPv4 tunnel route Src = %+v, want 10.101.0.1", routes4)
	}
	routes6, err := netlink.RouteListFiltered(netlink.FAMILY_V6, &netlink.Route{Table: cfg.routeTable}, netlink.RT_FILTER_TABLE)
	if err != nil || len(routes6) != 1 || !routes6[0].Src.Equal(net.ParseIP("fd8f:cf26:522a::1")) {
		t.Fatalf("IPv6 tunnel route Src = %+v, want fd8f:cf26:522a::1", routes6)
	}

	// 3. Main-table multi-metric 10.101.0.0/24 routes and RoCE /30 routes must remain intact.
	mainRoutes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: unix.RT_TABLE_MAIN}, netlink.RT_FILTER_TABLE)
	if err != nil {
		t.Fatal(err)
	}
	metricsFound := map[int]bool{}
	roceFound := map[string]bool{}
	for _, r := range mainRoutes {
		if r.Dst != nil && r.Dst.String() == "10.101.0.0/24" {
			metricsFound[r.Priority] = true
		}
		if r.Dst != nil && (r.Dst.String() == "10.255.0.0/30" || r.Dst.String() == "10.255.1.0/30") {
			roceFound[r.Dst.String()] = true
		}
	}
	for _, m := range []int{100, 300, 640} {
		if !metricsFound[m] {
			t.Errorf("multi-metric 10.101.0.0/24 route with metric %d was lost", m)
		}
	}
	for _, cidr := range []string{"10.255.0.0/30", "10.255.1.0/30"} {
		if !roceFound[cidr] {
			t.Errorf("RoCE route %s was lost", cidr)
		}
	}
}
