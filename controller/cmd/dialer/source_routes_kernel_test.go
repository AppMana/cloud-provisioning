package main

import (
	"net"
	"os"
	"testing"

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
			t.Cleanup(func() { removeRouteRule(cfg.routeTable) })
			_, host, _ := net.ParseCIDR(tc.remote)
			physical, _, _ := net.ParseCIDR(tc.physical)
			tunnel, _, _ := net.ParseCIDR(tc.tunnel)
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
			if err := installRoutes(cfg, []net.IPNet{*host}, nil, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			check(physical, 0, wg.Attrs().Index)
			// Retain direct access only for the source accepted by the bare peer.
			for i := 0; i < 2; i++ { // reconciliation must not accumulate rules.
				if err := reconcileTunnelSourceRoutes(cfg, tc.tunnel, []net.IPNet{*host}); err != nil {
					t.Fatal(err)
				}
				if err := installRoutes(cfg, nil, nil, nil, net.ParseIP(tc.relay), []net.IPNet{*host}); err != nil {
					t.Fatal(err)
				}
			}
			check(physical, 0, lan.Attrs().Index)
			check(tunnel, 0, wg.Attrs().Index)
			// The tunnel's own marked outer packets still bypass both tables.
			check(tunnel, uint32(cfg.fwmark), wg.Attrs().Index)
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
			if err := installRoutes(cfg, []net.IPNet{*host}, nil, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			if err := reconcileTunnelSourceRoutes(cfg, tc.tunnel, nil); err != nil {
				t.Fatal(err)
			}
			check(physical, 0, wg.Attrs().Index)
			check(tunnel, 0, wg.Attrs().Index)
			rules, err = netlink.RuleListFiltered(family, &netlink.Rule{Table: table}, netlink.RT_FILTER_TABLE)
			if err != nil || len(rules) != 0 {
				t.Fatalf("stale source rules: %+v, %v", rules, err)
			}
		})
	}
}
