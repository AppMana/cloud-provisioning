package main

import (
	"fmt"
	"net"
	"os"
	"testing"

	"github.com/vishvananda/netlink"
)

// Run only inside an isolated VM mount and network namespace. Verify the
// actual device budget at creation; this is not a workload/CNI or PMTU test.
func TestNativeDefaultTunnelMTU(t *testing.T) {
	if os.Getenv("CLDT_MTU_CEILING") != "1" {
		t.Skip("requires isolated VM namespaces")
	}
	requireStableAddressNamespace(t)
	for _, tc := range []struct{ underlay, want int }{{9001, 1420}, {1500, 1420}, {1450, 1370}, {1360, 1280}} {
		t.Run(fmt.Sprint(tc.underlay), func(t *testing.T) {
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			attrs := netlink.NewLinkAttrs()
			attrs.Name = "mtuunderlay"
			attrs.MTU = tc.underlay
			link := &netlink.Dummy{LinkAttrs: attrs}
			must(netlink.LinkAdd(link))
			t.Cleanup(func() { _ = netlink.LinkDel(link) })
			must(netlink.LinkSetUp(link))
			must(netlink.RouteAdd(&netlink.Route{LinkIndex: link.Attrs().Index, Scope: netlink.SCOPE_LINK, Dst: &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}}))
			cfg := config{iface: "mtutunnel"}
			must(ensureLink(cfg, nil))
			tunnel, err := netlink.LinkByName(cfg.iface)
			must(err)
			t.Cleanup(func() { _ = netlink.LinkDel(tunnel) })
			if tunnel.Attrs().MTU != tc.want {
				t.Fatalf("initial tunnel MTU %d, want %d", tunnel.Attrs().MTU, tc.want)
			}
			must(ensureLink(cfg, nil))
			underlay, err := netlink.LinkByName(attrs.Name)
			must(err)
			if underlay.Attrs().MTU != tc.underlay {
				t.Fatal("changed underlying interface")
			}
			cfg.mtu = 1300
			must(ensureLink(cfg, nil))
			tunnel, err = netlink.LinkByName(cfg.iface)
			must(err)
			if tunnel.Attrs().MTU != 1300 {
				t.Fatal("explicit tunnel MTU was ignored")
			}
		})
	}
}
