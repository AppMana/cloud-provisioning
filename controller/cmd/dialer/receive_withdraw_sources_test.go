package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"reflect"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// Isolated kernel ownership and absence checks, without traffic or live API
// admission. No handshake is created. A 55-second process deadline is required
// by the guest runner; this does not qualify recovery deadlines or cloud deletion.
func TestNativeReceiveSourceWithdrawal(t *testing.T) {
	if os.Getenv("CLDT_SOURCE_WITHDRAWAL_NETNS") != "1" {
		t.Skip("requires disposable guest mount and network namespaces")
	}
	for _, kind := range []string{"net", "mnt"} {
		self, err := os.Readlink("/proc/self/ns/" + kind)
		host, e := os.Readlink("/proc/1/ns/" + kind)
		if err != nil || e != nil || self == host {
			t.Fatal("refusing guest host namespace")
		}
	}
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		for _, fault := range []string{"none", "foreign-route", "alias", "key", "selector", "uncommitted", "cancelled", "last-route"} {
			t.Run(fmt.Sprintf("%d/%s", family, fault), func(t *testing.T) {
				must := func(err error) {
					t.Helper()
					if err != nil {
						t.Fatal(err)
					}
				}
				devices, err := wgctrl.New()
				must(err)
				defer devices.Close()
				legacy := &netlink.Wireguard{LinkAttrs: netlink.LinkAttrs{Name: "withdraw-root"}}
				must(netlink.LinkAdd(legacy))
				defer netlink.LinkDel(legacy)
				must(netlink.LinkSetUp(legacy))
				cfg := config{iface: "withdraw-root", routeTable: 518, fwmark: 518, receiveRouteScope: map[string]bool{}, receiveSourceCandidates: map[string][]receiveRouteSelection{}}
				source, target, survivor := "10.88.0.1/24", "10.88.0.128/32", "10.88.0.200/32"
				if family == netlink.FAMILY_V6 {
					source, target, survivor = "fd20::1/64", "fd20::128/128", "fd20::200/128"
				}
				_, host, _ := net.ParseCIDR(target)
				_, kept, _ := net.ParseCIDR(survivor)
				cfg.receiveRouteScope[target] = true
				for generation := 0; generation < 3; generation++ {
					local, _ := wgtypes.GeneratePrivateKey()
					remote, _ := wgtypes.GeneratePrivateKey()
					name := fmt.Sprintf("withdraw-src%d", generation)
					alias := "cloud-provisioning:receive-client:v1:" + name
					link := &netlink.Wireguard{LinkAttrs: netlink.LinkAttrs{Name: name, Alias: alias}}
					must(netlink.LinkAdd(link))
					defer netlink.LinkDel(link)
					must(netlink.LinkSetAlias(link, alias))
					must(netlink.LinkSetUp(link))
					mark := cfg.fwmark
					must(devices.ConfigureDevice(name, wgtypes.Config{PrivateKey: &local, FirewallMark: &mark, Peers: []wgtypes.PeerConfig{{PublicKey: remote.PublicKey(), AllowedIPs: []net.IPNet{*host}}}}))
					cfg.receiveSourceCandidates[target] = append(cfg.receiveSourceCandidates[target], receiveRouteSelection{Interface: name, Alias: alias, PublicKey: local.PublicKey().String(), PeerPublicKey: remote.PublicKey().String(), AllowedSources: []string{target}})
				}
				selected := cfg.receiveSourceCandidates[target][2]
				receiver, err := netlink.LinkByName(selected.Interface)
				must(err)
				table, err := tunnelSourceTable(cfg.routeTable)
				must(err)
				allocation, err := tunnelSourceAllocations([]string{source})
				must(err)
				rule := netlink.NewRule()
				rule.Table = table
				rule.Priority = cfg.routeTable - 1
				rule.Family = family
				rule.Src = allocation[family]
				must(netlink.RuleAdd(rule))
				defer netlink.RuleDel(rule)
				route := netlink.Route{Dst: host, LinkIndex: receiver.Attrs().Index, Table: table, Scope: netlink.SCOPE_LINK, Protocol: unix.RTPROT_BOOT}
				protected := netlink.Route{Dst: kept, LinkIndex: legacy.Attrs().Index, Table: table, Scope: netlink.SCOPE_LINK, Protocol: unix.RTPROT_BOOT}
				must(netlink.RouteAdd(&route))
				defer netlink.RouteDel(&route)
				if fault != "last-route" {
					must(netlink.RouteAdd(&protected))
					defer netlink.RouteDel(&protected)
				}
				// Forwarding remains handshake-strict even for the otherwise valid selected
				// third historical receiver. Use a one-candidate projection to test liveness.
				strict := cfg
				strict.receiveSourceCandidates = map[string][]receiveRouteSelection{target: {selected}}
				if _, _, err := inspectConfiguredTunnelSources(strict, table, legacy.Attrs().Index); err == nil {
					t.Fatal("forwarding accepted absent handshake")
				}
				admitted := []net.IPNet{*host}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				switch fault {
				case "foreign-route":
					route.Protocol = unix.RTPROT_STATIC
					must(netlink.RouteReplace(&route))
				case "alias":
					must(netlink.LinkSetAlias(receiver, "foreign"))
				case "key":
					replacement, _ := wgtypes.GeneratePrivateKey()
					must(devices.ConfigureDevice(selected.Interface, wgtypes.Config{PrivateKey: &replacement}))
				case "selector":
					must(netlink.RuleDel(rule))
					_, rule.Src, _ = net.ParseCIDR(survivor)
					must(netlink.RuleAdd(rule))
				case "uncommitted":
					admitted = nil
				case "cancelled":
					cancel()
				}
				snapshot := func() []netlink.Route {
					routes, err := netlink.RouteListFiltered(family, &netlink.Route{Table: table}, netlink.RT_FILTER_TABLE)
					must(err)
					return routes
				}
				before := snapshot()
				err = withdrawReceiveSourceRoutes(ctx, cfg, []string{source}, admitted)
				if fault != "none" && fault != "last-route" {
					if err == nil {
						t.Fatal("unsafe withdrawal accepted")
					}
					if !reflect.DeepEqual(before, snapshot()) {
						t.Fatal("rejected withdrawal changed routes")
					}
					return
				}
				must(err)
				must(withdrawReceiveSourceRoutes(ctx, cfg, []string{source}, admitted))
				after := snapshot()
				if fault == "last-route" {
					if len(after) != 0 {
						t.Fatal("target route remains")
					}
				} else {
					if len(after) != 1 || after[0].Dst.String() != survivor || after[0].LinkIndex != legacy.Attrs().Index {
						t.Fatal("surviving source route lost")
					}
				}
				rules, err := netlink.RuleList(family)
				must(err)
				count := 0
				for _, r := range rules {
					if r.Table == table {
						count++
						if r.Src.String() != allocation[family].String() {
							t.Fatal("surviving selector changed")
						}
					}
				}
				want := 1
				if fault == "last-route" {
					want = 0
				}
				if count != want {
					t.Fatal("selector cleanup differs", count, want)
				}
			})
		}
	}
}
