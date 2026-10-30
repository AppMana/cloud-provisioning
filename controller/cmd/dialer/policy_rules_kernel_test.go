package main

import (
	"net"
	"os"
	"reflect"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestNativeTunnelPolicyOwnership(t *testing.T) {
	if os.Getenv("CLDT_NETNS") != "1" {
		t.Skip("requires isolated network namespace")
	}
	self, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	host, err := os.Readlink("/proc/1/ns/net")
	if err != nil || self == host {
		t.Fatal("refusing host network namespace")
	}
	const table, mark = 518, 518
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		for _, fault := range []string{"blackhole", "wrong-table", "masked-mark", "inverted-mark", "source-mark", "conditional-table", "wrong-priority", "occupied-priority"} {
			t.Run(netlinkFamilyName(family)+"/"+fault, func(t *testing.T) {
				r := netlink.NewRule()
				r.Family = family
				r.Priority = exemptRulePriority
				r.Table = unix.RT_TABLE_MAIN
				r.Mark = mark
				switch fault {
				case "blackhole":
					r.Type = unix.FR_ACT_BLACKHOLE
				case "wrong-table":
					r.Table = 900
				case "masked-mark":
					mask := uint32(0xff)
					r.Mask = &mask
				case "inverted-mark":
					r.Invert = true
				case "source-mark":
					raw := "192.0.2.0/24"
					if family == netlink.FAMILY_V6 {
						raw = "2001:db8::/64"
					}
					_, r.Src, _ = net.ParseCIDR(raw)
				case "conditional-table":
					r.Priority = table
					r.Table = table
					r.Mark = 42
				case "wrong-priority":
					r.Priority = table + 1
					r.Table = table
					r.Mark = 0
				case "occupied-priority":
					r.Priority = table
					r.Table = 900
					r.Mark = 0
				}
				if err := netlink.RuleAdd(r); err != nil {
					t.Fatal(err)
				}
				defer netlink.RuleDel(r)
				before, err := netlink.RuleList(netlink.FAMILY_ALL)
				if err != nil {
					t.Fatal(err)
				}
				if err := ensureRouteRule(table, mark); err == nil {
					t.Fatal("foreign rule accepted")
				}
				after, err := netlink.RuleList(netlink.FAMILY_ALL)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(before, after) {
					t.Fatal("rejected collision changed policy rules")
				}
				removeRouteRule(table, mark)
				after, err = netlink.RuleList(netlink.FAMILY_ALL)
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatalf("teardown changed foreign policy: %v", err)
				}

			})
		}
	}
	// A disjoint exact exemption at the shared priority must survive teardown.
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		r := netlink.NewRule()
		r.Family = family
		r.Priority = exemptRulePriority
		r.Table = unix.RT_TABLE_MAIN
		r.Mark = 999
		if err := netlink.RuleAdd(r); err != nil {
			t.Fatal(err)
		}
		defer netlink.RuleDel(r)
	}
	before, err := netlink.RuleList(netlink.FAMILY_ALL)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureRouteRule(table, mark); err != nil {
		t.Fatal(err)
	}
	installed, err := netlink.RuleList(netlink.FAMILY_ALL)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureRouteRule(table, mark); err != nil {
		t.Fatal(err)
	}
	again, err := netlink.RuleList(netlink.FAMILY_ALL)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(installed, again) {
		t.Fatal("idempotent setup changed rules")
	}
	removeRouteRule(table, mark)
	after, err := netlink.RuleList(netlink.FAMILY_ALL)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("teardown removed foreign policy")
	}
}

// Site nodes running tailscaled (routing table 52, rule 5210: fwmark 0x80000/0xff0000
// lookup main) alongside tailscale-calico-routefix.service (rule 5200: to
// 100.64.0.0/10 lookup 52 for IPv4 and to fd7a:115c:a1e0::/48 lookup 52 for IPv6).
// Verify inside a real kernel namespace that wg-dialer's policy rules (exempt at
// priority 1, source table at 516, tunnel table at 517) coexist with Tailscale's
// rules and table 52 without conflict or accidental removal, and that route lookups
// steer tunnel destinations to wg, Tailscale destinations to tailscale0 (table 52),
// and Calico-marked (0x80000) pod/LAN traffic correctly.
func TestTailscaleAndCalicoFwmarkPolicyRulesCoexistWithTunnelRules(t *testing.T) {
	if os.Getenv("CLDT_NETNS") != "1" {
		t.Skip("requires isolated network namespace")
	}

	lan := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "site-lan0", MTU: 1500}}
	ts := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "tailscale0", MTU: 1280}}
	wg := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "cldt-wg0", MTU: 1420}}
	for _, link := range []*netlink.Dummy{lan, ts, wg} {
		if err := netlink.LinkAdd(link); err != nil {
			t.Fatal(err)
		}
		defer netlink.LinkDel(link)
		if err := netlink.LinkSetUp(link); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []struct {
		link netlink.Link
		cidr string
	}{
		{lan, "10.19.3.14/24"},
		{lan, "fd8f:cf26:522a::14/64"},
		{ts, "100.72.78.124/32"},
		{ts, "fd7a:115c:a1e0::1/128"},
		{wg, "10.100.0.1/24"},
		{wg, "fd00:10:100::a64:1/96"},
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

	// Install Tailscale table 52 routes and priority 5200/5210 policy rules.
	const tailscaleTable = 52
	_, tsNet4, _ := net.ParseCIDR("100.64.0.0/10")
	_, tsNet6, _ := net.ParseCIDR("fd7a:115c:a1e0::/48")
	for _, dst := range []*net.IPNet{tsNet4, tsNet6} {
		r := &netlink.Route{LinkIndex: ts.Attrs().Index, Dst: dst, Scope: netlink.SCOPE_LINK, Table: tailscaleTable}
		if err := netlink.RouteReplace(r); err != nil {
			t.Fatal(err)
		}
	}
	calicoMask := uint32(0xff0000)
	tsRules := []*netlink.Rule{
		{Family: netlink.FAMILY_V4, Priority: 5200, Table: tailscaleTable, Dst: tsNet4},
		{Family: netlink.FAMILY_V6, Priority: 5200, Table: tailscaleTable, Dst: tsNet6},
		{Family: netlink.FAMILY_V4, Priority: 5210, Table: unix.RT_TABLE_MAIN, Mark: 0x80000, Mask: &calicoMask},
		{Family: netlink.FAMILY_V6, Priority: 5210, Table: unix.RT_TABLE_MAIN, Mark: 0x80000, Mask: &calicoMask},
	}
	for _, r := range tsRules {
		rule := netlink.NewRule()
		rule.Family = r.Family
		rule.Priority = r.Priority
		rule.Table = r.Table
		rule.Dst = r.Dst
		rule.Mark = r.Mark
		rule.Mask = r.Mask
		if err := netlink.RuleAdd(rule); err != nil {
			t.Fatal(err)
		}
		defer netlink.RuleDel(rule)
	}

	beforeRules, err := netlink.RuleList(netlink.FAMILY_ALL)
	if err != nil {
		t.Fatal(err)
	}

	cfg := config{iface: wg.Attrs().Name, routeTable: 517, fwmark: 517}
	_, remoteHost4, _ := net.ParseCIDR("10.100.0.128/32")
	_, remotePod4, _ := net.ParseCIDR("10.244.64.0/26")
	_, remotePod6, _ := net.ParseCIDR("fd00:10:244::4000/122")
	if err := installRoutes(cfg, []net.IPNet{*remoteHost4}, nil, []net.IPNet{*remotePod4, *remotePod6}, nil, nil, []string{"10.101.0.1", "fd8f:cf26:522a::1"}); err != nil {
		t.Fatalf("installRoutes with Tailscale rules present: %v", err)
	}
	if err := reconcileTunnelSourceRoutes(cfg, []string{"10.100.0.1/24", "fd00:10:100::a64:1/96"}, []net.IPNet{*remoteHost4}); err != nil {
		t.Fatalf("reconcileTunnelSourceRoutes with Tailscale rules present: %v", err)
	}

	// Verify kernel routing lookups across tunnel, Tailscale (table 52), and Calico fwmark (0x80000).
	checkLookup := func(dst string, mark uint32, wantLink int) {
		t.Helper()
		routes, err := netlink.RouteGetWithOptions(net.ParseIP(dst), &netlink.RouteGetOptions{Mark: mark})
		if err != nil || len(routes) == 0 {
			t.Fatalf("RouteGet(%s, mark=%#x): %v", dst, mark, err)
		}
		if routes[0].LinkIndex != wantLink {
			t.Fatalf("RouteGet(%s, mark=%#x) resolved via link %d, want %d", dst, mark, routes[0].LinkIndex, wantLink)
		}
	}
	// Cloud pod CIDRs (unmarked and Calico-marked 0x80000) resolve via WireGuard (table 517 at pref 517 < 5210).
	checkLookup("10.244.64.10", 0, wg.Attrs().Index)
	checkLookup("10.244.64.10", 0x80000, wg.Attrs().Index)
	checkLookup("fd00:10:244::4001", 0, wg.Attrs().Index)
	checkLookup("fd00:10:244::4001", 0x80000, wg.Attrs().Index)
	// Tailscale CGNAT / ULA destinations miss table 517 and hit pref 5200 -> table 52 -> tailscale0.
	checkLookup("100.90.1.2", 0, ts.Attrs().Index)
	checkLookup("fd7a:115c:a1e0::2", 0, ts.Attrs().Index)
	// LAN destinations with Calico mark 0x80000 hit main via pref 5210 -> site-lan0.
	checkLookup("10.19.3.11", 0x80000, lan.Attrs().Index)

	// Teardown of tunnel source and route rules must leave Tailscale's 5200/5210 rules intact.
	if err := clearTunnelSourceRoutes(cfg.routeTable, cfg.iface); err != nil {
		t.Fatal(err)
	}
	removeRouteRule(cfg.routeTable, cfg.fwmark)
	afterRules, err := netlink.RuleList(netlink.FAMILY_ALL)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeRules, afterRules) {
		t.Fatalf("tunnel teardown altered Tailscale/Calico policy rules:\nbefore=%+v\nafter=%+v", beforeRules, afterRules)
	}
}
func netlinkFamilyName(family int) string {
	if family == netlink.FAMILY_V4 {
		return "ipv4"
	}
	return "ipv6"
}
