package main

import (
	"fmt"
	"net"
	"os"
	"reflect"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestTunnelSourceRuleOwnership(t *testing.T) {
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		for _, fault := range []string{"none", "source", "broad", "destination", "mark", "invert", "iif", "table", "priority", "action", "tos", "suppress", "protocol", "mask"} {
			t.Run(fmt.Sprintf("%d/%s", family, fault), func(t *testing.T) {
				rule := netlink.NewRule()
				rule.Family = family
				rule.Table = 66054
				rule.Priority = 517
				raw := "10.100.0.1/32"
				if family == netlink.FAMILY_V6 {
					raw = "fd20::1/128"
				}
				_, rule.Src, _ = net.ParseCIDR(raw)
				switch fault {
				case "source":
					rule.Src = nil
				case "broad":
					_, rule.Src, _ = net.ParseCIDR("10.100.0.0/24")
				case "destination":
					_, rule.Dst, _ = net.ParseCIDR(raw)
				case "mark":
					rule.Mark = 1
				case "invert":
					rule.Invert = true
				case "iif":
					rule.IifName = "eth0"
				case "table":
					rule.Table++
				case "priority":
					rule.Priority++
				case "action":
					rule.Type = unix.RTN_BLACKHOLE
				case "tos":
					rule.Tos = 1
				case "suppress":
					rule.SuppressPrefixlen = 0
				case "protocol":
					rule.IPProto = 17
				case "mask":
					mask := uint32(1)
					rule.Mask = &mask
				}
				if got := matchesTunnelSourceRule(*rule, family, 66054, 517); got != (fault == "none") {
					t.Fatalf("ownership=%v", got)
				}
			})
		}
	}
}

func TestNativeTunnelSourceOwnership(t *testing.T) {
	if os.Getenv("CLDT_SOURCE_OWNERSHIP_NETNS") != "1" {
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
	wg := &netlink.Wireguard{LinkAttrs: netlink.LinkAttrs{Name: "source-owned"}}
	foreign := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "source-foreign"}}
	for _, link := range []netlink.Link{wg, foreign} {
		if err := netlink.LinkAdd(link); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { netlink.LinkDel(link) })
		if err := netlink.LinkSetUp(link); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config{iface: wg.Attrs().Name, routeTable: 518, fwmark: 518}
	table, _ := tunnelSourceTable(cfg.routeTable)
	allocations := []string{"10.100.0.1/24", "fd20::1/64"}
	_, v4, _ := net.ParseCIDR("10.100.0.160/32")
	_, v6, _ := net.ParseCIDR("fd20::160/128")
	hosts := []net.IPNet{*v4, *v6}
	if err := reconcileTunnelSourceRoutes(cfg, allocations, hosts); err != nil {
		t.Fatal(err)
	}
	type state struct {
		Rules  []netlink.Rule
		Routes []netlink.Route
	}
	snapshot := func() state {
		t.Helper()
		var s state
		for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
			rules, err := netlink.RuleList(family)
			if err != nil {
				t.Fatal(err)
			}
			s.Rules = append(s.Rules, rules...)
			routes, err := netlink.RouteListFiltered(family, &netlink.Route{Table: table}, netlink.RT_FILTER_TABLE)
			if err != nil {
				t.Fatal(err)
			}
			s.Routes = append(s.Routes, routes...)
		}
		return s
	}
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		for _, fault := range []string{"rule-mark", "rule-other-table", "rule-other-priority", "rule-blackhole", "rule-destination", "route-device", "route-metric", "route-block"} {
			t.Run(fmt.Sprintf("%d/%s", family, fault), func(t *testing.T) {
				raw := "10.100.0.1/32"
				dst := v4
				if family == netlink.FAMILY_V6 {
					raw = "fd20::1/128"
					dst = v6
				}
				rule := netlink.NewRule()
				rule.Family = family
				rule.Table = table
				rule.Priority = cfg.routeTable - 1
				_, rule.Src, _ = net.ParseCIDR(raw)
				route := netlink.Route{LinkIndex: wg.Attrs().Index, Dst: dst, Table: table, Scope: netlink.SCOPE_LINK, Protocol: unix.RTPROT_BOOT, Priority: receiveRoutePriority(dst)}
				isRule := true
				switch fault {
				case "rule-mark":
					rule.Mark = 7
				case "rule-other-table":
					rule.Table = 333
				case "rule-other-priority":
					rule.Priority = cfg.routeTable - 2
				case "rule-blackhole":
					rule.Type = unix.RTN_BLACKHOLE
				case "rule-destination":
					rule.Dst = dst
				case "route-device":
					isRule = false
					route.LinkIndex = foreign.Attrs().Index
					other := "10.100.0.161/32"
					if family == netlink.FAMILY_V6 {
						other = "fd20::161/128"
					}
					_, route.Dst, _ = net.ParseCIDR(other)
				case "route-metric":
					isRule = false
					route.Priority++
				case "route-block":
					isRule = false
					block := "10.101.0.0/24"
					if family == netlink.FAMILY_V6 {
						block = "fd21::/64"
					}
					_, route.Dst, _ = net.ParseCIDR(block)
				}
				if isRule {
					if err := netlink.RuleAdd(rule); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := netlink.RouteAdd(&route); err != nil {
						t.Fatal(err)
					}
				}
				before := snapshot()
				if err := reconcileTunnelSourceRoutes(cfg, allocations, hosts); err == nil {
					t.Fatal("reconcile accepted foreign state")
				}
				if err := clearTunnelSourceRoutes(cfg.routeTable, cfg.iface); err == nil {
					t.Fatal("cleanup accepted foreign state")
				}
				if after := snapshot(); !reflect.DeepEqual(before, after) {
					t.Fatal("collision checks changed either family")
				}
				if isRule {
					if err := netlink.RuleDel(rule); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := netlink.RouteDel(&route); err != nil {
						t.Fatal(err)
					}
				}
				if err := reconcileTunnelSourceRoutes(cfg, allocations, hosts); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
	// Interrupted source replacement can leave distinct old and new owned
	// selectors. Retry must remove the stale host selector without rejecting it
	// as a duplicate or withdrawing the other family's current path.
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		rule := netlink.NewRule()
		rule.Family, rule.Table, rule.Priority = family, table, cfg.routeTable-1
		raw := "10.100.0.2/32"
		if family == netlink.FAMILY_V6 {
			raw = "fd20::2/128"
		}
		_, rule.Src, _ = net.ParseCIDR(raw)
		if err := netlink.RuleAdd(rule); err != nil {
			t.Fatal(err)
		}
	}
	if err := reconcileTunnelSourceRoutes(cfg, allocations, hosts); err != nil {
		t.Fatal(err)
	}
	rules, _, err := inspectTunnelSourceState(cfg.routeTable, table, wg.Attrs().Index)
	if err != nil || len(rules) != 2 {
		t.Fatalf("source replacement retry: %+v %v", rules, err)
	}
	if err := clearTunnelSourceRoutes(cfg.routeTable); err == nil {
		t.Fatal("cleanup without device authority removed live routes")
	}
	if err := clearTunnelSourceRoutes(cfg.routeTable, cfg.iface); err != nil {
		t.Fatal(err)
	}
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		rules, err := netlink.RuleListFiltered(family, &netlink.Rule{Table: table}, netlink.RT_FILTER_TABLE)
		if err != nil || len(rules) != 0 {
			t.Fatal("owned rules remained after cleanup")
		}
		routes, err := netlink.RouteListFiltered(family, &netlink.Route{Table: table}, netlink.RT_FILTER_TABLE)
		if err != nil || len(routes) != 0 {
			t.Fatal("owned routes remained after cleanup")
		}
	}
}

func TestTunnelSourceRuleSetRejectsDuplicates(t *testing.T) {
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		rule := netlink.NewRule()
		rule.Family, rule.Table, rule.Priority = family, 66054, 517
		raw := "10.100.0.1/32"
		if family == netlink.FAMILY_V6 {
			raw = "fd20::1/128"
		}
		_, rule.Src, _ = net.ParseCIDR(raw)
		if selected, err := selectTunnelSourceRules([]netlink.Rule{*rule, *rule}, family, 66054, 517); err == nil || selected != nil {
			t.Fatal("duplicate selectors accepted")
		}
	}
}
