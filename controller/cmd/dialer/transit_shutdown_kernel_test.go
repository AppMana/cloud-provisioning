package main

import (
	"net"
	"os"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/vishvananda/netlink"
)

func TestUnpublishedEndpointShutdownPreservesTransit(t *testing.T) {
	if os.Getenv("CLDT_NETNS") != "1" {
		t.Skip("set CLDT_NETNS=1 inside an isolated network namespace")
	}
	for _, tc := range []struct{ name, address, relay, remote string }{
		{"ipv4", "10.10.0.11/24", "10.10.0.10", "172.29.0.31"},
		{"ipv6", "fd10::11/64", "fd10::10", "fd20::31"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lan := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "shutdown-lan"}}
			if err := netlink.LinkAdd(lan); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { netlink.LinkDel(lan) })
			if err := netlink.LinkSetUp(lan); err != nil {
				t.Fatal(err)
			}
			addr, err := netlink.ParseAddr(tc.address)
			if err != nil {
				t.Fatal(err)
			}
			addr.Flags = 0x02 // IFA_F_NODAD
			if err := netlink.AddrAdd(lan, addr); err != nil {
				t.Fatal(err)
			}
			cfg := config{iface: "retired-wg", routeTable: 517, fwmark: 517}
			t.Cleanup(func() { removeRouteRule(cfg.routeTable) })
			if err := installSiteTransit(cfg, &tunnel.TransitSpec{Via: tc.relay, Hosts: []string{tc.remote}}, []string{addr.IP.String()}); err != nil {
				t.Fatal(err)
			}
			check := func() {
				t.Helper()
				routes, err := netlink.RouteGet(net.ParseIP(tc.remote))
				if err != nil || len(routes) != 1 || routes[0].LinkIndex != lan.Attrs().Index || !routes[0].Gw.Equal(net.ParseIP(tc.relay)) {
					t.Fatalf("transit lookup: %+v, %v", routes, err)
				}
			}
			check()
			// Both a retired device and an already absent device occur when
			// a transit-only dialer stops during a rolling update.
			wg := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: cfg.iface}}
			if err := netlink.LinkAdd(wg); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { netlink.LinkDel(wg) })
			retireUnpublishedEndpoint(cfg)
			if _, err := netlink.LinkByName(cfg.iface); err == nil {
				t.Fatal("retired device remains")
			}
			check()
			retireUnpublishedEndpoint(cfg)
			check()
		})
	}
}
