package main

import (
	"context"
	"net"
	"os"
	"reflect"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestReceiveRelayRoutesRejectUnboundSelections(t *testing.T) {
	for _, fault := range []string{"scope", "destination", "receiver", "family", "default", "multicast", "recursive", "mark"} {
		t.Run(fault, func(t *testing.T) {
			raw, gateway := "172.29.0.1/32", "10.10.0.1"
			cfg := config{fwmark: 518, receiveRouteScope: map[string]bool{raw: true}, receiveRelayRoutes: map[string]string{raw: gateway}}
			_, dst, _ := net.ParseCIDR(raw)
			hosts := []net.IPNet{*dst}
			switch fault {
			case "scope":
				cfg.receiveRouteScope = nil
			case "destination":
				hosts = nil
			case "receiver":
				cfg.receiveRoutes = map[string]receiveRouteSelection{raw: {}}
			case "family":
				cfg.receiveRelayRoutes[raw] = "2001:db8::1"
			case "default":
				cfg.receiveRelayRoutes = map[string]string{"0.0.0.0/0": gateway}
			case "multicast":
				cfg.receiveRelayRoutes[raw] = "224.0.0.1"
			case "recursive":
				cfg.receiveRelayRoutes[raw] = "172.29.0.1"
			case "mark":
				cfg.fwmark = 0
			}
			if _, err := validateReceiveRelayRoutes(cfg, hosts, nil); err == nil {
				t.Fatal("unbound relay accepted")
			}
		})
	}
}

func TestNativeReceiveRelayRouteSwitch(t *testing.T) {
	if os.Getenv("CLDT_NATIVE_RECEIVE_RELAY") != "1" {
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
	for _, tc := range []struct{ name, address, gateway, other, destination, second, unrelated string }{
		{"ipv4", "10.10.0.2/24", "10.10.0.1", "10.10.0.3", "172.29.0.1/32", "172.29.0.2/32", "203.0.113.77/32"},
		{"ipv6", "2001:db8:1::2/64", "2001:db8:1::1", "2001:db8:1::3", "2001:db8:2::1/128", "2001:db8:2::2/128", "2001:db8:3::77/128"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lan := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "relay-lan"}}
			wg := &netlink.Wireguard{LinkAttrs: netlink.LinkAttrs{Name: "relay-wg"}}
			for _, link := range []netlink.Link{lan, wg} {
				if err := netlink.LinkAdd(link); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { netlink.LinkDel(link) })
				if err := netlink.LinkSetUp(link); err != nil {
					t.Fatal(err)
				}
			}
			addr, _ := netlink.ParseAddr(tc.address)
			addr.Flags = unix.IFA_F_NODAD
			if err := netlink.AddrAdd(lan, addr); err != nil {
				t.Fatal(err)
			}
			own := []string{addr.IP.String()}
			_, dst, _ := net.ParseCIDR(tc.destination)
			_, second, _ := net.ParseCIDR(tc.second)
			_, unrelated, _ := net.ParseCIDR(tc.unrelated)
			cfg := config{iface: wg.Attrs().Name, fwmark: 518, routeTable: 518, receiveRouteScope: map[string]bool{tc.destination: true, tc.second: true}}
			t.Cleanup(func() { removeRouteRule(cfg.routeTable, cfg.fwmark) })
			for _, table := range []int{518, unix.RT_TABLE_MAIN} {
				route := netlink.Route{Dst: unrelated, LinkIndex: wg.Attrs().Index, Table: table, Scope: netlink.SCOPE_LINK, Protocol: unix.RTPROT_BOOT}
				if err := netlink.RouteAdd(&route); err != nil {
					t.Fatal(err)
				}
			}
			hosts := []net.IPNet{*dst, *second}
			if err := installRoutes(cfg, hosts, nil, nil, nil, nil, own); err != nil {
				t.Fatal(err)
			}
			// Repeated forward/reverse switches preserve every unrelated route.
			for i := 0; i < 2; i++ {
				cfg.receiveRelayRoutes = map[string]string{tc.destination: tc.gateway, tc.second: tc.other}
				if err := preflightReceiveRoutePhase(cfg, nil, own); err != nil {
					t.Fatal(err)
				}
				if err := installRoutes(cfg, hosts, nil, nil, nil, nil, own); err != nil {
					t.Fatal(err)
				}
				paths, err := netlink.RouteGetWithOptions(dst.IP, &netlink.RouteGetOptions{SrcAddr: addr.IP})
				if err != nil || len(paths) != 1 || paths[0].LinkIndex != lan.Attrs().Index || !paths[0].Gw.Equal(net.ParseIP(tc.gateway)) {
					t.Fatalf("relay lookup: %+v %v", paths, err)
				}
				secondPaths, err := netlink.RouteGetWithOptions(second.IP, &netlink.RouteGetOptions{SrcAddr: addr.IP})
				if err != nil || len(secondPaths) != 1 || !secondPaths[0].Gw.Equal(net.ParseIP(tc.other)) {
					t.Fatalf("second worker lost its distinct relay: %+v %v", secondPaths, err)
				}
				good := netlink.Route{Dst: dst, LinkIndex: lan.Attrs().Index, Gw: net.ParseIP(tc.gateway), Src: addr.IP, Table: 518, Protocol: unix.RTPROT_BOOT}
				bad := good
				bad.Gw = net.ParseIP(tc.other)
				if err := netlink.RouteReplace(&bad); err != nil {
					t.Fatal(err)
				}
				family := netlink.FAMILY_V6
				if addr.IP.To4() != nil {
					family = netlink.FAMILY_V4
				}
				before, err := netlink.RouteListFiltered(family, &netlink.Route{Table: 518}, netlink.RT_FILTER_TABLE)
				if err != nil {
					t.Fatal(err)
				}
				forward := map[string]string{tc.destination: tc.other, tc.second: tc.other}
				if err := preflightReceiveRoutePhase(cfg, nil, own, cfg.receiveRelayRoutes, forward); err != nil {
					t.Fatal("committed second relay generation rejected", err)
				}
				if err := verifyReceiveRouteInstallation(cfg, hosts, nil, own); err == nil {
					t.Fatal("readback accepted foreign gateway")
				}
				if err := preflightReceiveRoutePhase(cfg, nil, own); err == nil {
					t.Fatal("preflight accepted foreign gateway")
				}
				after, err := netlink.RouteListFiltered(family, &netlink.Route{Table: 518}, netlink.RT_FILTER_TABLE)
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatal("verification changed routes")
				}
				if err := netlink.RouteReplace(&good); err != nil {
					t.Fatal(err)
				}
				admitted := cfg.receiveRelayRoutes
				cfg.receiveRelayRoutes = nil
				if err := preflightReceiveRoutePhase(cfg, nil, own, admitted); err != nil {
					t.Fatal(err)
				}
				if err := installRoutes(cfg, hosts, nil, nil, nil, nil, own); err != nil {
					t.Fatal(err)
				}
				paths, err = netlink.RouteGetWithOptions(dst.IP, &netlink.RouteGetOptions{SrcAddr: addr.IP})
				if err != nil || len(paths) != 1 || paths[0].LinkIndex != wg.Attrs().Index || len(paths[0].Gw) != 0 {
					t.Fatalf("direct lookup: %+v %v", paths, err)
				}
				for _, table := range []int{518, unix.RT_TABLE_MAIN} {
					routes, err := netlink.RouteListFiltered(family, &netlink.Route{Table: table, Dst: unrelated}, netlink.RT_FILTER_TABLE|netlink.RT_FILTER_DST)
					if err != nil || len(routes) != 1 || routes[0].LinkIndex != wg.Attrs().Index {
						t.Fatalf("unrelated route changed: %+v %v", routes, err)
					}
				}
			}
			// Withdrawal is destination-scoped: a surviving worker can use the
			// same underlay and gateway without losing its route. This observes
			// kernel state only, not packets, API admission or cloud deletion.
			for _, fault := range []string{"none", "gateway", "protocol", "metric", "cancelled", "older-gateway"} {
				t.Run("withdraw/"+fault, func(t *testing.T) {
					must := func(err error) {
						t.Helper()
						if err != nil {
							t.Fatal(err)
						}
					}
					good := netlink.Route{Dst: dst, LinkIndex: lan.Attrs().Index, Gw: net.ParseIP(tc.gateway), Src: addr.IP, Table: 518, Protocol: unix.RTPROT_BOOT}
					must(netlink.RouteReplace(&good))
					cfg.receiveRelayRoutes = map[string]string{tc.destination: tc.gateway, tc.second: tc.other}
					must(installRoutes(cfg, hosts, nil, nil, nil, nil, own))
					relays := []map[string]string{{tc.destination: tc.gateway}}
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					bad := good
					switch fault {
					case "gateway":
						bad.Gw = net.ParseIP(tc.other)
					case "protocol":
						bad.Protocol = unix.RTPROT_STATIC
					case "metric":
						bad.Priority = 123
					case "cancelled":
						cancel()
					case "older-gateway":
						bad.Gw = net.ParseIP(tc.other)
						relays = append(relays, map[string]string{tc.destination: tc.other}, map[string]string{tc.destination: tc.gateway})
					}
					// RouteReplace with another metric may add a second route;
					// remove the original first so the rejection tests one owner.
					must(netlink.RouteDel(&good))
					must(netlink.RouteAdd(&bad))
					family := netlink.FAMILY_V6
					if addr.IP.To4() != nil {
						family = netlink.FAMILY_V4
					}
					snapshot := func() []netlink.Route {
						r, e := netlink.RouteListFiltered(family, &netlink.Route{Table: 518}, netlink.RT_FILTER_TABLE)
						must(e)
						return r
					}
					before := snapshot()
					err := withdrawReceiveDeviceRoutes(ctx, cfg, []map[string]receiveRouteSelection{{}}, own, relays...)
					if fault != "none" && fault != "older-gateway" {
						if err == nil || !reflect.DeepEqual(before, snapshot()) {
							t.Fatal("unsafe relay withdrawal changed routes", err)
						}
						must(netlink.RouteDel(&bad))
						must(netlink.RouteAdd(&good))
						return
					}
					must(err)
					must(withdrawReceiveDeviceRoutes(ctx, cfg, []map[string]receiveRouteSelection{{}}, own, relays...))
					var expected []netlink.Route
					for _, route := range before {
						if route.Dst == nil || route.Dst.String() != tc.destination {
							expected = append(expected, route)
						}
					}
					if !reflect.DeepEqual(expected, snapshot()) {
						t.Fatal("relay withdrawal changed another worker")
					}
				})
			}

		})
	}
}
