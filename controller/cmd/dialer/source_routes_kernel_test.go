package main

import (
	"net"
	"os"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/vishvananda/netlink"
)

// Reproduce the MicroK8s Calico VXLAN addresses observed during endpoint
// withdrawal. Both destinations are identical; the permitted peer differs
// according to the packet's source. This must exercise kernel lookups, not
// infer the selected route from the routes we intended to install.
func TestRetainedTunnelRoutesRespectPacketSource(t *testing.T) {
	if os.Getenv("CLDT_NETNS") != "1" {
		t.Skip("set CLDT_NETNS=1 inside an isolated network namespace")
	}
	for _, tc := range []struct{ name, physical, tunnel, relay, remote string }{
		{"ipv4", "10.10.0.11/24", "10.100.0.1/24", "10.10.0.10", "10.100.0.160/32"},
		{"ipv6", "fd10::11/64", "fd20::1/64", "fd10::10", "fd20::160/128"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lan := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "source-lan"}}
			wg := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "source-wg"}}
			for _, link := range []*netlink.Dummy{lan, wg} {
				if err := netlink.LinkAdd(link); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { netlink.LinkDel(link) })
				if err := netlink.LinkSetUp(link); err != nil {
					t.Fatal(err)
				}
			}
			for i, cidr := range []string{tc.physical, tc.tunnel} {
				addr, err := netlink.ParseAddr(cidr)
				if err != nil {
					t.Fatal(err)
				}
				addr.Flags = 0x02 // IFA_F_NODAD: deterministic IPv6 lookup in the isolated test.
				if err := netlink.AddrAdd([]*netlink.Dummy{lan, wg}[i], addr); err != nil {
					t.Fatal(err)
				}
			}
			cfg := config{iface: wg.Attrs().Name, routeTable: 517, fwmark: 517}
			t.Cleanup(func() { removeRouteRule(cfg.routeTable, cfg.fwmark) })
			_, host, _ := net.ParseCIDR(tc.remote)
			physical, _, _ := net.ParseCIDR(tc.physical)
			tunnelAddr, _, _ := net.ParseCIDR(tc.tunnel)
			check := func(source net.IP, mark uint32, want int) {
				t.Helper()
				routes, err := netlink.RouteGetWithOptions(host.IP, &netlink.RouteGetOptions{SrcAddr: source, Mark: mark})
				if err != nil {
					t.Fatal(err)
				}
				if len(routes) != 1 || routes[0].LinkIndex != want {
					t.Fatalf("source %s mark %d: routes %+v, want interface %d", source, mark, routes, want)
				}
			}
			// Start with the direct routes of an owning endpoint.
			if err := installRoutes(cfg, []net.IPNet{*host}, nil, nil, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			check(physical, 0, wg.Attrs().Index)
			// Retain direct access only for the source accepted by the bare peer.
			for i := 0; i < 2; i++ { // reconciliation must not accumulate rules.
				if err := reconcileTunnelSourceRoutes(cfg, []string{tc.tunnel}, []net.IPNet{*host}); err != nil {
					t.Fatal(err)
				}
				if err := installRoutes(cfg, nil, nil, nil, &tunnel.TransitSpec{Via: tc.relay}, []net.IPNet{*host}, nil); err != nil {
					t.Fatal(err)
				}
			}
			check(physical, 0, lan.Attrs().Index)
			check(tunnelAddr, 0, wg.Attrs().Index)
			// The tunnel's own marked outer packets still bypass both tables.
			check(tunnelAddr, uint32(cfg.fwmark), wg.Attrs().Index)
			table, _ := tunnelSourceTable(cfg.routeTable)
			family := netlink.FAMILY_V6
			if physical.To4() != nil {
				family = netlink.FAMILY_V4
			}
			rules, err := netlink.RuleListFiltered(family, &netlink.Rule{Table: table}, netlink.RT_FILTER_TABLE)
			if err != nil || len(rules) != 1 {
				t.Fatalf("source rules: %+v, %v", rules, err)
			}
			// Promotion restores the ordinary routes, then removes the exception.
			if err := installRoutes(cfg, []net.IPNet{*host}, nil, nil, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			if err := reconcileTunnelSourceRoutes(cfg, []string{tc.tunnel}, nil); err != nil {
				t.Fatal(err)
			}
			check(physical, 0, wg.Attrs().Index)
			check(tunnelAddr, 0, wg.Attrs().Index)
			rules, err = netlink.RuleListFiltered(family, &netlink.Rule{Table: table}, netlink.RT_FILTER_TABLE)
			if err != nil || len(rules) != 0 {
				t.Fatalf("stale source rules: %+v, %v", rules, err)
			}
		})
	}
}

func TestTunnelSourceRoutesKeepBothFamilies(t *testing.T) {
	if os.Getenv("CLDT_DUAL_SOURCE_NETNS") != "1" {
		t.Skip("requires isolated guest network and mount namespaces")
	}
	for _, kind := range []string{"net", "mnt"} {
		self, err := os.Readlink("/proc/self/ns/" + kind)
		if err != nil {
			t.Fatal(err)
		}
		host, err := os.Readlink("/proc/1/ns/" + kind)
		if err != nil || self == host {
			t.Fatal("refusing guest host namespace")
		}
	}
	lan := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dual-source-lan"}}
	wg := &netlink.Wireguard{LinkAttrs: netlink.LinkAttrs{Name: "dual-source-wg"}}
	for _, link := range []netlink.Link{lan, wg} {
		if err := netlink.LinkAdd(link); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { netlink.LinkDel(link) })
		if err := netlink.LinkSetUp(link); err != nil {
			t.Fatal(err)
		}
	}
	allocations := []string{"10.100.0.1/24", "fd20::1/64"}
	physical := []string{"10.10.0.11/24", "fd10::11/64"}
	remote := []string{"10.100.0.160/32", "fd20::160/128"}
	gateways := []string{"10.10.0.10", "fd10::10"}
	var hosts []net.IPNet
	var own []string
	for i := range allocations {
		for j, raw := range []string{physical[i], allocations[i]} {
			addr, err := netlink.ParseAddr(raw)
			if err != nil {
				t.Fatal(err)
			}
			addr.Flags = 2 // IFA_F_NODAD
			if err := netlink.AddrAdd([]netlink.Link{lan, wg}[j], addr); err != nil {
				t.Fatal(err)
			}
			if j == 0 {
				own = append(own, addr.IP.String())
			}
		}
		_, dst, _ := net.ParseCIDR(remote[i])
		hosts = append(hosts, *dst)
	}
	cfg := config{iface: wg.Attrs().Name, routeTable: 518, fwmark: 518, receiveRouteScope: map[string]bool{remote[0]: true, remote[1]: true}}
	t.Cleanup(func() { clearTunnelSourceRoutes(cfg.routeTable); removeRouteRule(cfg.routeTable, cfg.fwmark) })
	if err := installRoutes(cfg, hosts, nil, nil, nil, nil, own); err != nil {
		t.Fatal(err)
	}
	check := func(relayed6 bool) {
		t.Helper()
		table, _ := tunnelSourceTable(cfg.routeTable)
		for i := range hosts {
			family := netlink.FAMILY_V4
			if i == 1 {
				family = netlink.FAMILY_V6
			}
			for j, raw := range []string{physical[i], allocations[i]} {
				source, _, _ := net.ParseCIDR(raw)
				paths, err := netlink.RouteGetWithOptions(hosts[i].IP, &netlink.RouteGetOptions{SrcAddr: source})
				want := wg.Attrs().Index
				if j == 0 && (i == 0 || relayed6) {
					want = lan.Attrs().Index
				}
				if err != nil || len(paths) != 1 || paths[0].LinkIndex != want {
					t.Fatalf("family %d source %s: %+v %v, want link %d", family, source, paths, err, want)
				}
			}
			rules, err := netlink.RuleListFiltered(family, &netlink.Rule{Table: table}, netlink.RT_FILTER_TABLE)
			count := 1
			if i == 1 && !relayed6 {
				count = 0
			}
			if err != nil || len(rules) != count {
				t.Fatalf("family %d rules: %+v %v", family, rules, err)
			}
			routes, err := netlink.RouteListFiltered(family, &netlink.Route{Table: table}, netlink.RT_FILTER_TABLE)
			if err != nil || len(routes) != count {
				t.Fatalf("family %d routes: %+v %v", family, routes, err)
			}
		}
	}
	for repeat := 0; repeat < 2; repeat++ {
		if err := reconcileTunnelSourceRoutes(cfg, allocations, hosts); err != nil {
			t.Fatal(err)
		}
		cfg.receiveRelayRoutes = map[string]string{remote[0]: gateways[0], remote[1]: gateways[1]}
		if err := installRoutes(cfg, hosts, nil, nil, nil, nil, own); err != nil {
			t.Fatal(err)
		}
		check(true)
	}
	// Restore the ordinary IPv6 path before dropping only its exception.
	cfg.receiveRelayRoutes = map[string]string{remote[0]: gateways[0]}
	if err := installRoutes(cfg, hosts, nil, nil, nil, nil, own); err != nil {
		t.Fatal(err)
	}
	if err := reconcileTunnelSourceRoutes(cfg, allocations, hosts[:1]); err != nil {
		t.Fatal(err)
	}
	check(false)
	// A retry may restore both families, without losing the still-active IPv4 path.
	if err := reconcileTunnelSourceRoutes(cfg, allocations, hosts); err != nil {
		t.Fatal(err)
	}
	cfg.receiveRelayRoutes = map[string]string{remote[0]: gateways[0], remote[1]: gateways[1]}
	if err := installRoutes(cfg, hosts, nil, nil, nil, nil, own); err != nil {
		t.Fatal(err)
	}
	check(true)
	// The complete input must validate before either family's rules are touched.
	if err := reconcileTunnelSourceRoutes(cfg, []string{allocations[0], "invalid"}, hosts); err == nil {
		t.Fatal("accepted invalid second family")
	}
	check(true)
	cfg.receiveRelayRoutes = nil
	if err := installRoutes(cfg, hosts, nil, nil, nil, nil, own); err != nil {
		t.Fatal(err)
	}
	if err := reconcileTunnelSourceRoutes(cfg, allocations, nil); err != nil {
		t.Fatal(err)
	}
	table, _ := tunnelSourceTable(cfg.routeTable)
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		rules, err := netlink.RuleListFiltered(family, &netlink.Rule{Table: table}, netlink.RT_FILTER_TABLE)
		if err != nil || len(rules) != 0 {
			t.Fatalf("source rules survived promotion: %+v %v", rules, err)
		}
		routes, err := netlink.RouteListFiltered(family, &netlink.Route{Table: table}, netlink.RT_FILTER_TABLE)
		if err != nil || len(routes) != 0 {
			t.Fatalf("source routes survived promotion: %+v %v", routes, err)
		}
	}
}
