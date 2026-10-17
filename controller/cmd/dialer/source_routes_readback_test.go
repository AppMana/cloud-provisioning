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

func TestNativeTunnelSourceReadback(t *testing.T) {
	if os.Getenv("CLDT_SOURCE_READBACK_NETNS") != "1" {
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
	wg := &netlink.Wireguard{LinkAttrs: netlink.LinkAttrs{Name: "source-readback"}}
	if err := netlink.LinkAdd(wg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { netlink.LinkDel(wg) })
	if err := netlink.LinkSetUp(wg); err != nil {
		t.Fatal(err)
	}
	cfg := config{iface: wg.Attrs().Name, routeTable: 518, fwmark: 518}
	table, _ := tunnelSourceTable(cfg.routeTable)
	allocations := []string{"10.100.0.1/24", "fd20::1/64"}
	_, v4, _ := net.ParseCIDR("10.100.0.160/32")
	_, v6, _ := net.ParseCIDR("fd20::160/128")
	hosts := []net.IPNet{*v4, *v6}
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
		for _, fault := range []string{"missing-rule", "wrong-source", "extra-source", "missing-route", "wrong-destination", "extra-destination", "empty-intent"} {
			t.Run(fmt.Sprintf("%d/%s", family, fault), func(t *testing.T) {
				if err := reconcileTunnelSourceRoutes(cfg, allocations, hosts); err != nil {
					t.Fatal(err)
				}
				if err := verifyTunnelSourceRoutes(cfg, allocations, hosts); err != nil {
					t.Fatal(err)
				}
				index := 0
				otherSource := "10.100.0.2/32"
				otherDestination := "10.100.0.161/32"
				if family == netlink.FAMILY_V6 {
					index = 1
					otherSource = "fd20::2/128"
					otherDestination = "fd20::161/128"
				}
				source, _, _ := net.ParseCIDR(allocations[index])
				bits := 32
				if index == 1 {
					bits = 128
				}
				rule := netlink.NewRule()
				rule.Family, rule.Table, rule.Priority = family, table, cfg.routeTable-1
				rule.Src = &net.IPNet{IP: source, Mask: net.CIDRMask(bits, bits)}
				route := netlink.Route{LinkIndex: wg.Attrs().Index, Dst: &hosts[index], Table: table, Scope: netlink.SCOPE_LINK, Protocol: unix.RTPROT_BOOT, Priority: receiveRoutePriority(&hosts[index])}
				switch fault {
				case "missing-rule":
					if err := netlink.RuleDel(rule); err != nil {
						t.Fatal(err)
					}
				case "wrong-source", "extra-source":
					if fault == "wrong-source" {
						if err := netlink.RuleDel(rule); err != nil {
							t.Fatal(err)
						}
					}
					_, rule.Src, _ = net.ParseCIDR(otherSource)
					if err := netlink.RuleAdd(rule); err != nil {
						t.Fatal(err)
					}
				case "missing-route":
					if err := netlink.RouteDel(&route); err != nil {
						t.Fatal(err)
					}
				case "wrong-destination", "extra-destination":
					if fault == "wrong-destination" {
						if err := netlink.RouteDel(&route); err != nil {
							t.Fatal(err)
						}
					}
					_, route.Dst, _ = net.ParseCIDR(otherDestination)
					if err := netlink.RouteAdd(&route); err != nil {
						t.Fatal(err)
					}
				}
				before := snapshot()
				expectedHosts := hosts
				if fault == "empty-intent" {
					expectedHosts = nil
				}
				if err := verifyTunnelSourceRoutes(cfg, allocations, expectedHosts); err == nil {
					t.Fatal("readback accepted drift")
				}
				if after := snapshot(); !reflect.DeepEqual(before, after) {
					t.Fatal("readback repaired or deleted state")
				}
				// Reconciliation is a separate authorized operation, never part of readback.
				if err := reconcileTunnelSourceRoutes(cfg, allocations, hosts); err != nil {
					t.Fatal(err)
				}
				if err := verifyTunnelSourceRoutes(cfg, allocations, hosts); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
	if err := reconcileTunnelSourceRoutes(cfg, allocations, nil); err != nil {
		t.Fatal(err)
	}
	if err := verifyTunnelSourceRoutes(cfg, allocations, nil); err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkDel(wg); err != nil {
		t.Fatal(err)
	}
	if err := verifyTunnelSourceRoutes(cfg, nil, nil); err != nil {
		t.Fatal(err)
	}
}
