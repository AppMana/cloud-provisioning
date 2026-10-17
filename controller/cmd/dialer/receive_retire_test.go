package main

import (
	"net"
	"net/netip"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func TestNativeReceivePeerRetirement(t *testing.T) {
	if os.Getenv("CLDT_RECEIVE_RETIRE") != "1" {
		t.Skip("requires isolated VM network and mount namespaces")
	}
	for _, kind := range []string{"net", "mnt"} {
		self, err := os.Readlink("/proc/self/ns/" + kind)
		if err != nil {
			t.Fatal(err)
		}
		host, err := os.Readlink("/proc/1/ns/" + kind)
		if err != nil {
			t.Fatal(err)
		}
		if self == host {
			t.Fatal("refusing guest host namespace")
		}
	}
	devices, err := wgctrl.New()
	if err != nil {
		t.Fatal(err)
	}
	defer devices.Close()
	root, _ := wgtypes.GeneratePrivateKey()
	first, _ := wgtypes.GeneratePrivateKey()
	second, _ := wgtypes.GeneratePrivateKey()
	psk, _ := wgtypes.GenerateKey()
	keepalive := 23 * time.Second
	mark := 518
	parse := func(raw string) net.IPNet {
		_, prefix, err := net.ParseCIDR(raw)
		if err != nil {
			t.Fatal(err)
		}
		return *prefix
	}
	for _, family := range []string{"ipv4", "ipv6"} {
		t.Run(family, func(t *testing.T) {
			source, other, own, broad := "10.90.0.10/32", "10.91.0.0/24", "10.90.0.3/24", "10.90.0.0/24"
			if family == "ipv6" {
				source, other, own, broad = "fd90::10/128", "fd91::/64", "fd90::3/64", "fd90::/64"
			}
			link := &netlink.GenericLink{LinkAttrs: netlink.LinkAttrs{Name: "retire0"}, LinkType: "wireguard"}
			if err := netlink.LinkAdd(link); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = netlink.LinkDel(link) })
			if err := netlink.LinkSetUp(link); err != nil {
				t.Fatal(err)
			}
			address, _ := netlink.ParseAddr(own)
			if err := netlink.AddrAdd(link, address); err != nil {
				t.Fatal(err)
			}
			if err := devices.ConfigureDevice("retire0", wgtypes.Config{PrivateKey: &root, FirewallMark: &mark, Peers: []wgtypes.PeerConfig{
				{PublicKey: first.PublicKey(), AllowedIPs: []net.IPNet{parse(source)}},
				{PublicKey: second.PublicKey(), AllowedIPs: []net.IPNet{parse(other)}, PresharedKey: &psk, PersistentKeepaliveInterval: &keepalive},
			}}); err != nil {
				t.Fatal(err)
			}
			retire := func(key string, sources []string) error {
				return withdrawLegacyReceivePeer(devices, "retire0", key, first.PublicKey().String(), sources, own)
			}
			assertBoth := func() {
				t.Helper()
				current, err := devices.Device("retire0")
				if err != nil || len(current.Peers) != 2 {
					t.Fatalf("rejection changed membership: %v", err)
				}
			}
			if err := withdrawLegacyReceivePeer(devices, "retire0", root.PublicKey().String(), first.PublicKey().String(), []string{source}); err == nil {
				t.Fatal("connected route accepted without committed allocation")
			}
			assertBoth()
			if err := retire(second.PublicKey().String(), []string{source}); err == nil {
				t.Fatal("accepted wrong local key")
			}
			assertBoth()
			if err := retire(root.PublicKey().String(), []string{broad}); err == nil {
				t.Fatal("accepted changed permissions")
			}
			assertBoth()
			if err := devices.ConfigureDevice("retire0", wgtypes.Config{Peers: []wgtypes.PeerConfig{{PublicKey: second.PublicKey(), AllowedIPs: []net.IPNet{parse(broad)}}}}); err != nil {
				t.Fatal(err)
			}
			if err := retire(root.PublicKey().String(), []string{source}); err == nil {
				t.Fatal("accepted overlapping source permissions on another legacy peer")
			}
			assertBoth()
			if err := devices.ConfigureDevice("retire0", wgtypes.Config{Peers: []wgtypes.PeerConfig{{PublicKey: second.PublicKey(), ReplaceAllowedIPs: true, AllowedIPs: []net.IPNet{parse(other)}}}}); err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				name, destination string
				table             int
				multipath         bool
			}{
				{"owned", source, 518, false},
				{"foreign-broad", broad, 777, false},
				{"foreign-multipath", source, 778, true},
				{"default", "", 779, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					route := netlink.Route{Table: tc.table, LinkIndex: link.Attrs().Index}
					if tc.destination != "" {
						dst := parse(tc.destination)
						route.Dst = &dst
					} else if family == "ipv6" {
						dst := parse("::/0")
						route.Dst = &dst
					} else {
						dst := parse("0.0.0.0/0")
						route.Dst = &dst
					}
					if tc.multipath {
						route.MultiPath = []*netlink.NexthopInfo{{LinkIndex: link.Attrs().Index}}
						if family == "ipv6" {
							route.MultiPath[0].Gw = net.ParseIP("fd90::1")
							route.MultiPath[0].Flags = unix.RTNH_F_ONLINK
						}
						route.LinkIndex = 0
					}
					if err := netlink.RouteAdd(&route); err != nil {
						t.Fatal(err)
					}
					defer netlink.RouteDel(&route)
					if err := retire(root.PublicKey().String(), []string{source}); err == nil {
						t.Fatal("retired a peer with a live legacy route")
					}
					assertBoth()
				})
			}
			// Another worker continues to own its routes and receive permissions.
			otherDst := parse(other)
			if err := netlink.RouteAdd(&netlink.Route{Table: 518, Dst: &otherDst, LinkIndex: link.Attrs().Index}); err != nil {
				t.Fatal(err)
			}
			addresses, err := netlink.AddrList(link, netlink.FAMILY_ALL)
			if err != nil {
				t.Fatal(err)
			}
			routes, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{}, netlink.RT_FILTER_TABLE)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := retire(root.PublicKey().String(), []string{source}); err != nil {
					t.Fatal(err)
				}
				current, err := devices.Device("retire0")
				if err != nil {
					t.Fatal(err)
				}
				if len(current.Peers) != 1 || current.Peers[0].PublicKey != second.PublicKey() || current.Peers[0].PresharedKey != psk || current.Peers[0].PersistentKeepaliveInterval != keepalive || current.FirewallMark != mark || current.PublicKey != root.PublicKey() {
					t.Fatal("retirement changed surviving peer or device")
				}
				remainingAddresses, err := netlink.AddrList(link, netlink.FAMILY_ALL)
				if err != nil {
					t.Fatal(err)
				}
				remainingRoutes, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{}, netlink.RT_FILTER_TABLE)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(addresses, remainingAddresses) || !reflect.DeepEqual(routes, remainingRoutes) {
					t.Fatal("retirement changed addresses or routes")
				}
			}
		})
	}
}

func TestSharedConnectedRouteRequiresExactAllocationAndKernelShape(t *testing.T) {
	prefix := netip.MustParsePrefix("10.90.0.3/24")
	address, _ := netlink.ParseAddr(prefix.String())
	_, dst, _ := net.ParseCIDR(prefix.Masked().String())
	valid := netlink.Route{LinkIndex: 7, Table: unix.RT_TABLE_MAIN, Protocol: unix.RTPROT_KERNEL, Type: unix.RTN_UNICAST, Family: netlink.FAMILY_V4, Scope: netlink.SCOPE_LINK, Dst: dst, Src: net.ParseIP("10.90.0.3")}
	for _, fault := range []string{"none", "table", "protocol", "type", "device", "source", "gateway", "metric", "scope", "multipath", "no-address", "wrong-allocation", "wrong-width", "noprefixroute"} {
		t.Run(fault, func(t *testing.T) {
			route := valid
			allocation := []netip.Prefix{prefix}
			addresses := []netlink.Addr{*address}
			switch fault {
			case "table":
				route.Table = 777
			case "protocol":
				route.Protocol = unix.RTPROT_BOOT
			case "type":
				route.Type = unix.RTN_LOCAL
			case "device":
				route.LinkIndex++
			case "source":
				route.Src = net.ParseIP("10.90.0.4")
			case "gateway":
				route.Gw = net.ParseIP("10.90.0.1")
			case "metric":
				route.Priority = 42
			case "scope":
				route.Scope = netlink.SCOPE_HOST
			case "multipath":
				route.MultiPath = []*netlink.NexthopInfo{{LinkIndex: 7}}
			case "no-address":
				addresses = nil
			case "wrong-allocation":
				allocation[0] = netip.MustParsePrefix("10.90.0.4/24")
			case "wrong-width":
				allocation[0] = netip.MustParsePrefix("10.90.0.3/16")
			case "noprefixroute":
				addresses[0].Flags |= unix.IFA_F_NOPREFIXROUTE
			}
			if got := receiveSharedConnectedRoute(route, 7, allocation, addresses); got != (fault == "none") {
				t.Fatalf("exception=%v", got)
			}
		})
	}
}
