package main

import (
	"net"
	"os"
	"reflect"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestTunnelSourceScopeIncludesHostsWithinOwnedBlocks(t *testing.T) {
	cfg := config{receiveRouteScope: map[string]bool{"10.100.0.128/25": true, "fd20::100/120": true}}
	for _, tc := range []struct {
		raw  string
		want bool
	}{{"10.100.0.160/32", true}, {"fd20::160/128", true}, {"10.100.0.16/32", false}, {"fd20::16/128", false}} {
		_, host, _ := net.ParseCIDR(tc.raw)
		if err := validateTunnelSourceScope(cfg, []net.IPNet{*host}); (err == nil) != tc.want {
			t.Fatalf("%s: %v", tc.raw, err)
		}
	}
	for _, scope := range []map[string]bool{{}, {"0.0.0.0/0": true}, {"10.100.0.1/24": true}, {"10.100.0.0/24": false}} {
		if err := validateTunnelSourceScope(config{receiveRouteScope: scope}, nil); err == nil {
			t.Fatal("invalid scope accepted")
		}
	}
}

func TestNativeScopedTunnelSourcesPreserveOtherWorkers(t *testing.T) {
	if os.Getenv("CLDT_SOURCE_SCOPE_NETNS") != "1" {
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
	wg := &netlink.Wireguard{LinkAttrs: netlink.LinkAttrs{Name: "source-scoped"}}
	if err := netlink.LinkAdd(wg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { netlink.LinkDel(wg) })
	if err := netlink.LinkSetUp(wg); err != nil {
		t.Fatal(err)
	}
	cfg := config{iface: wg.Attrs().Name, routeTable: 518, fwmark: 518}
	table, _ := tunnelSourceTable(cfg.routeTable)
	addresses := []string{"10.100.0.1/24", "fd20::1/64"}
	var owned, unowned []net.IPNet
	for i, raw := range []string{"10.100.0.160/32", "fd20::160/128", "10.100.0.16/32", "fd20::16/128"} {
		_, host, _ := net.ParseCIDR(raw)
		if i < 2 {
			owned = append(owned, *host)
		} else {
			unowned = append(unowned, *host)
		}
	}
	all := append(append([]net.IPNet{}, owned...), unowned...)
	if err := reconcileTunnelSourceRoutes(cfg, addresses, all); err != nil {
		t.Fatal(err)
	}
	type state struct {
		Rules  []netlink.Rule
		Routes []netlink.Route
	}
	snapshot := func(onlyUnowned bool) state {
		t.Helper()
		var s state
		for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
			rules, err := netlink.RuleListFiltered(family, &netlink.Rule{Table: table}, netlink.RT_FILTER_TABLE)
			if err != nil {
				t.Fatal(err)
			}
			s.Rules = append(s.Rules, rules...)
			routes, err := netlink.RouteListFiltered(family, &netlink.Route{Table: table}, netlink.RT_FILTER_TABLE)
			if err != nil {
				t.Fatal(err)
			}
			for _, route := range routes {
				if !onlyUnowned || route.Dst.String() == unowned[0].String() || route.Dst.String() == unowned[1].String() {
					s.Routes = append(s.Routes, route)
				}
			}
		}
		return s
	}
	preserved := snapshot(true)
	cfg.receiveRouteScope = map[string]bool{"10.100.0.128/25": true, "fd20::100/120": true}
	checkPreserved := func() {
		t.Helper()
		if got := snapshot(true); !reflect.DeepEqual(got, preserved) {
			t.Fatal("other worker's source policy changed")
		}
	}
	// Empty scoped intent must preserve outside-scope routes and their selectors.
	for _, hosts := range [][]net.IPNet{owned, nil, owned[:1], owned, nil, owned} {
		before := snapshot(false)
		staged, err := preflightReceiveSourceSwitch(cfg, addresses, hosts, owned)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before, snapshot(false)) {
			t.Fatal("switch preflight mutated policy")
		}
		if err := reconcileTunnelSourceRoutes(cfg, addresses, staged); err != nil {
			t.Fatal(err)
		}
		// An interrupted switch can retry without losing the staged union.
		retry, err := preflightReceiveSourceSwitch(cfg, addresses, hosts, owned)
		if err != nil || !reflect.DeepEqual(staged, retry) {
			t.Fatal("staged switch is not retryable", err)
		}
		if err := verifyTunnelSourceRoutes(cfg, addresses, staged); err != nil {
			t.Fatal(err)
		}
		checkPreserved()
		if err := reconcileTunnelSourceRoutes(cfg, addresses, hosts); err != nil {
			t.Fatal(err)
		}
		if err := verifyTunnelSourceRoutes(cfg, addresses, hosts); err != nil {
			t.Fatal(err)
		}
		checkPreserved()
	}
	// A more-specific host inside an owned block is still in scope.
	_, extra, _ := net.ParseCIDR("10.100.0.161/32")
	route := netlink.Route{LinkIndex: wg.Attrs().Index, Dst: extra, Table: table, Scope: netlink.SCOPE_LINK, Protocol: unix.RTPROT_BOOT}
	if err := netlink.RouteAdd(&route); err != nil {
		t.Fatal(err)
	}
	before := snapshot(false)
	if _, err := preflightReceiveSourceSwitch(cfg, addresses, nil, owned); err == nil {
		t.Fatal("switch accepted uncommitted destination")
	}
	if err := verifyTunnelSourceRoutes(cfg, addresses, owned); err == nil {
		t.Fatal("unexpected scoped host was hidden as unrelated")
	}
	if after := snapshot(false); !reflect.DeepEqual(before, after) {
		t.Fatal("readback removed unexpected host")
	}
	if err := reconcileTunnelSourceRoutes(cfg, addresses, owned); err != nil {
		t.Fatal(err)
	}
	checkPreserved()
	for _, fault := range []string{"changed-allocation", "outside-scope", "missing-shared-rule", "wrong-shared-rule"} {
		t.Run(fault, func(t *testing.T) {
			input := addresses
			hosts := owned
			rule := netlink.NewRule()
			rule.Table, rule.Priority, rule.Family = table, cfg.routeTable-1, netlink.FAMILY_V6
			_, rule.Src, _ = net.ParseCIDR("fd20::1/128")
			switch fault {
			case "changed-allocation":
				input = []string{"10.100.0.2/24", "fd20::1/64"}
			case "outside-scope":
				hosts = all
			case "missing-shared-rule", "wrong-shared-rule":
				if err := netlink.RuleDel(rule); err != nil {
					t.Fatal(err)
				}
				if fault == "wrong-shared-rule" {
					_, rule.Src, _ = net.ParseCIDR("fd20::2/128")
					if err := netlink.RuleAdd(rule); err != nil {
						t.Fatal(err)
					}
				}
			}
			before := snapshot(false)
			if _, err := preflightReceiveSourceSwitch(cfg, input, hosts, owned); err == nil {
				t.Fatal("switch accepted unadmitted state")
			}
			if err := reconcileTunnelSourceRoutes(cfg, input, hosts); err == nil {
				t.Fatal("scoped reconcile changed unadmitted state")
			}
			if err := verifyTunnelSourceRoutes(cfg, input, hosts); err == nil {
				t.Fatal("scoped readback accepted unadmitted state")
			}
			if after := snapshot(false); !reflect.DeepEqual(before, after) {
				t.Fatal("failed scoped operation changed state")
			}
			if fault == "wrong-shared-rule" {
				if err := netlink.RuleDel(rule); err != nil {
					t.Fatal(err)
				}
			}
			if fault == "missing-shared-rule" || fault == "wrong-shared-rule" {
				_, rule.Src, _ = net.ParseCIDR("fd20::1/128")
				if err := netlink.RuleAdd(rule); err != nil {
					t.Fatal(err)
				}
			}
			checkPreserved()
		})
	}
	if err := reconcileTunnelSourceRoutes(cfg, addresses, nil); err != nil {
		t.Fatal(err)
	}
	checkPreserved()
	if err := clearTunnelSourceRoutes(cfg.routeTable, cfg.iface); err != nil {
		t.Fatal(err)
	}
}
