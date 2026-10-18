package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// Run only inside an isolated guest mount and network namespace. No handshake
// exists: the retiring worker is unavailable. Verify both route families,
// unchanged protected routes, no mutation on dependency rejection and retries.
// This measures kernel readback, not workload continuity, controller authority,
// source-exception retirement, reboot recovery or retirement acknowledgements.
func TestNativeReceiveWithdrawalRoutes(t *testing.T) {
	if os.Getenv("CLDT_NATIVE_RECEIVE_WITHDRAWAL") != "1" {
		t.Skip("requires isolated guest net and mount namespaces")
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
	devices, err := wgctrl.New()
	if err != nil {
		t.Fatal(err)
	}
	defer devices.Close()
	for _, family := range []string{"ipv4", "ipv6"} {
		for _, fault := range []string{"none", "alias", "permissions", "foreign-table", "foreign-destination", "address", "protocol", "cancelled"} {
			t.Run(family+"/"+fault, func(t *testing.T) {
				must := func(err error) {
					t.Helper()
					if err != nil {
						t.Fatal(err)
					}
				}
				create := func(name, alias string) (netlink.Link, wgtypes.Key) {
					t.Helper()
					key, err := wgtypes.GeneratePrivateKey()
					must(err)
					link := &netlink.Wireguard{LinkAttrs: netlink.LinkAttrs{Name: name}}
					must(netlink.LinkAdd(link))
					t.Cleanup(func() { netlink.LinkDel(link) })
					must(netlink.LinkSetAlias(link, alias))
					mark := 518
					must(devices.ConfigureDevice(name, wgtypes.Config{PrivateKey: &key, FirewallMark: &mark}))
					must(netlink.LinkSetUp(link))
					return link, key
				}
				root, _ := create("withdrawroot", "")
				cfg := config{iface: root.Attrs().Name, fwmark: 518, routeTable: 518}
				must(ensureRouteRule(cfg.routeTable, cfg.fwmark))
				t.Cleanup(func() { removeRouteRule(cfg.routeTable, cfg.fwmark) })
				destination, protected := "198.19.0.1/32", "198.19.0.2/32"
				if family == "ipv6" {
					destination, protected = "2001:db8:19::1/128", "2001:db8:19::2/128"
				}
				_, dst, err := net.ParseCIDR(destination)
				must(err)
				_, other, err := net.ParseCIDR(protected)
				must(err)
				remote, err := wgtypes.GeneratePrivateKey()
				must(err)
				var generations []map[string]receiveRouteSelection
				var links []netlink.Link
				for i := 0; i < 3; i++ {
					name := fmt.Sprintf("withdrawrx%d", i)
					alias := fmt.Sprintf("cloud-provisioning:receive:v1:withdrawal-%d", i)
					link, key := create(name, alias)
					links = append(links, link)
					must(devices.ConfigureDevice(name, wgtypes.Config{Peers: []wgtypes.PeerConfig{{PublicKey: remote.PublicKey(), AllowedIPs: []net.IPNet{*dst}}}}))
					selection := receiveRouteSelection{Interface: name, Alias: alias, PublicKey: key.PublicKey().String(), PeerPublicKey: remote.PublicKey().String(), AllowedSources: []string{destination}}
					generations = append(generations, map[string]receiveRouteSelection{destination: selection})
				}
				route := netlink.Route{Dst: dst, LinkIndex: links[0].Attrs().Index, Table: cfg.routeTable, Scope: netlink.SCOPE_LINK, Protocol: unix.RTPROT_BOOT, Priority: receiveRoutePriority(dst)}
				must(netlink.RouteAdd(&route))
				protectedRoute := netlink.Route{Dst: other, LinkIndex: root.Attrs().Index, Table: cfg.routeTable, Scope: netlink.SCOPE_LINK, Protocol: unix.RTPROT_BOOT, Priority: receiveRoutePriority(other)}
				must(netlink.RouteAdd(&protectedRoute))
				strict := cfg
				strict.receiveRouteScope = map[string]bool{destination: true}
				if err := preflightReceiveRouteGenerations(strict, generations[:2], nil, nil); err == nil {
					t.Fatal("forwarding accepted an unauthenticated receiver")
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				switch fault {
				case "alias":
					must(netlink.LinkSetAlias(links[0], "foreign"))
				case "permissions":
					must(devices.ConfigureDevice(links[0].Attrs().Name, wgtypes.Config{Peers: []wgtypes.PeerConfig{{PublicKey: remote.PublicKey(), ReplaceAllowedIPs: true, AllowedIPs: []net.IPNet{*other}}}}))
				case "foreign-table", "foreign-destination":
					foreign := route
					foreign.LinkIndex = links[1].Attrs().Index
					if fault == "foreign-table" {
						foreign.Table = 519
					} else {
						foreign.Dst = other
						foreign.Priority = 999
					}
					must(netlink.RouteAdd(&foreign))
				case "address":
					must(netlink.AddrAdd(links[1], &netlink.Addr{IPNet: other, Flags: unix.IFA_F_NODAD}))
				case "protocol":
					route.Protocol = unix.RTPROT_STATIC
					must(netlink.RouteReplace(&route))
				case "cancelled":
					cancel()
				}
				err = withdrawReceiveDeviceRoutes(ctx, cfg, generations, nil)
				if fault == "none" {
					must(err)
					must(withdrawReceiveDeviceRoutes(ctx, cfg, generations, nil))
				} else if err == nil {
					t.Fatal("unsafe withdrawal accepted")
				}
				routes, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: cfg.routeTable}, netlink.RT_FILTER_TABLE)
				must(err)
				found, protectedFound := false, false
				for _, current := range routes {
					if current.Dst != nil && current.Dst.String() == destination && current.LinkIndex == links[0].Attrs().Index {
						found = true
					}
					if current.Dst != nil && current.Dst.String() == protected && current.LinkIndex == root.Attrs().Index && current.Protocol == protectedRoute.Protocol && current.Priority == protectedRoute.Priority {
						protectedFound = true
					}
				}
				if found != (fault != "none") || !protectedFound {
					t.Fatal("withdrawal changed unexpected routes")
				}
				if fault == "none" {
					for _, generation := range generations {
						must(retireRecordedSiteReceiver(devices, cfg, []receiveRouteSelection{generation[destination]}))
						must(retireRecordedSiteReceiver(devices, cfg, []receiveRouteSelection{generation[destination]}))
					}
				}
			})
		}
	}
}
