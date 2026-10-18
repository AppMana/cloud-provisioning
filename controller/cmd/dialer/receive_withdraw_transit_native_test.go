package main

import (
	"context"
	"net"
	"os"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// Isolated guest namespaces only. Install through the normal follower path,
// then remove only admitted destinations with shared withdrawal checks. No local
// WireGuard interface exists. No API/receipt authority, packet continuity or
// underlay-loss recovery is established; unreachable relay evidence must reject.
func TestNativeFollowerWithdrawalRoutes(t *testing.T) {
	if os.Getenv("CLDT_NATIVE_FOLLOWER_WITHDRAWAL") != "1" {
		t.Skip("requires isolated guest namespaces")
	}
	for _, kind := range []string{"net", "mnt"} {
		self, e := os.Readlink("/proc/self/ns/" + kind)
		host, h := os.Readlink("/proc/1/ns/" + kind)
		if e != nil || h != nil || self == host {
			t.Fatal("refusing guest host namespace")
		}
	}
	for _, family := range []string{"v4", "v6"} {
		for _, fault := range []string{"none", "protocol", "source", "gateway", "shadow", "cancelled", "underlay-down"} {
			t.Run(family+"/"+fault, func(t *testing.T) {
				must := func(err error) {
					t.Helper()
					if err != nil {
						t.Fatal(err)
					}
				}
				cfg := config{iface: "absent-tunnel", routeTable: 518, fwmark: 518}
				link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "underlay"}}
				must(netlink.LinkAdd(link))
				defer netlink.LinkDel(link)
				must(netlink.LinkSetUp(link))
				source, via, target, survivor, shadow := "10.90.0.4", "10.90.0.2", "172.29.0.0/24", "172.30.0.0/24", "172.29.0.7/32"
				address := "10.90.0.4/24"
				if family == "v6" {
					source, via, target, survivor, shadow = "fd90::4", "fd90::2", "fd29::/64", "fd30::/64", "fd29::7/128"
					address = "fd90::4/64"
				}
				addr, err := netlink.ParseAddr(address)
				must(err)
				addr.Flags = unix.IFA_F_NODAD
				must(netlink.AddrAdd(link, addr))
				own := []string{source}
				must(installSiteTransit(cfg, &tunnel.TransitSpec{Via: via, Blocks: []string{target, survivor}}, own))
				defer removeRouteRule(cfg.routeTable, cfg.fwmark)
				defer func() {
					routes, _ := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: cfg.routeTable}, netlink.RT_FILTER_TABLE)
					for i := range routes {
						_ = netlink.RouteDel(&routes[i])
					}
				}()
				routes, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: cfg.routeTable}, netlink.RT_FILTER_TABLE)
				must(err)
				var targetRoute netlink.Route
				for _, r := range routes {
					if r.Dst != nil && r.Dst.String() == target {
						targetRoute = r
					}
				}
				if targetRoute.Dst == nil {
					t.Fatal("ordinary follower route not installed")
				}
				switch fault {
				case "protocol":
					targetRoute.Protocol = unix.RTPROT_STATIC
					must(netlink.RouteReplace(&targetRoute))
				case "source":
					targetRoute.Src = nil
					must(netlink.RouteReplace(&targetRoute))
				case "gateway":
					if family == "v4" {
						targetRoute.Gw = net.ParseIP("10.90.0.3")
					} else {
						targetRoute.Gw = net.ParseIP("fd90::3")
					}
					must(netlink.RouteReplace(&targetRoute))
				case "shadow":
					_, dst, e := net.ParseCIDR(shadow)
					must(e)
					must(netlink.RouteAdd(&netlink.Route{Dst: dst, LinkIndex: link.Attrs().Index, Table: cfg.routeTable, Protocol: unix.RTPROT_STATIC}))
				case "underlay-down":
					must(netlink.LinkSetDown(link))
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if fault == "cancelled" {
					cancel()
				}
				err = withdrawReceiveDeviceRoutes(ctx, cfg, []map[string]receiveRouteSelection{{}}, own, map[string]string{target: via})
				if fault != "none" {
					if err == nil {
						t.Fatal("foreign or unavailable route authorized cleanup")
					}
					remaining, e := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: cfg.routeTable}, netlink.RT_FILTER_TABLE)
					must(e)
					found := false
					for _, r := range remaining {
						found = found || (r.Dst != nil && r.Dst.String() == target)
					}
					if fault != "underlay-down" && !found {
						t.Fatal("rejected cleanup removed target")
					}
					return
				}
				must(err)
				must(withdrawReceiveDeviceRoutes(ctx, cfg, []map[string]receiveRouteSelection{{}}, own, map[string]string{target: via}))
				remaining, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: cfg.routeTable}, netlink.RT_FILTER_TABLE)
				must(err)
				if len(remaining) != 1 || remaining[0].Dst == nil || remaining[0].Dst.String() != survivor || !remaining[0].Gw.Equal(net.ParseIP(via)) || !remaining[0].Src.Equal(net.ParseIP(source)) || remaining[0].Protocol != unix.RTPROT_BOOT || remaining[0].LinkIndex != link.Attrs().Index {
					t.Fatal("survivor forwarding changed", remaining)
				}
				must(verifyReceivePolicyRules(cfg))
			})
		}
	}
}
