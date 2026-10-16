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
func netlinkFamilyName(family int) string {
	if family == netlink.FAMILY_V4 {
		return "ipv4"
	}
	return "ipv6"
}
