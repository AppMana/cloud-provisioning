package main

import (
	"net"
	"os"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func TestNativeDedicatedReceiverRetirement(t *testing.T) {
	if os.Getenv("CLDT_NETNS") != "1" {
		t.Skip("requires isolated network namespace")
	}
	self, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	host, err := os.Readlink("/proc/1/ns/net")
	if err != nil || self == host {
		t.Fatal("refusing guest host namespace")
	}
	devices, err := wgctrl.New()
	if err != nil {
		t.Fatal(err)
	}
	defer devices.Close()
	for _, family := range []string{"ipv4", "ipv6"} {
		for _, fault := range []string{"none", "empty-peer", "alias", "key", "peer", "permissions", "address", "route", "foreign-route", "foreign-multicast", "extra-peer", "foreign-device"} {
			t.Run(family+"/"+fault, func(t *testing.T) {
				local, err := wgtypes.GeneratePrivateKey()
				if err != nil {
					t.Fatal(err)
				}
				peer, err := wgtypes.GeneratePrivateKey()
				if err != nil {
					t.Fatal(err)
				}
				source := "192.0.2.7/32"
				foreignSource := "198.51.100.9/32"
				address := "192.0.2.1/32"
				if family == "ipv6" {
					source = "2001:db8:7::7/128"
					foreignSource = "2001:db8:9::9/128"
					address = "2001:db8:7::1/128"
				}
				selection := receiveRouteSelection{Interface: "cldt-retire", Alias: receiverAlias("mesh", peer.PublicKey().String()), PublicKey: local.PublicKey().String(), PeerPublicKey: peer.PublicKey().String(), AllowedSources: []string{source}}
				var link netlink.Link = &netlink.GenericLink{LinkAttrs: netlink.LinkAttrs{Name: selection.Interface}, LinkType: "wireguard"}
				if fault == "foreign-device" {
					link = &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: selection.Interface}}
				}
				if err := netlink.LinkAdd(link); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { netlink.LinkDel(link) })
				if err := netlink.LinkSetAlias(link, selection.Alias); err != nil {
					t.Fatal(err)
				}
				if err := netlink.LinkSetUp(link); err != nil {
					t.Fatal(err)
				}
				_, prefix, err := net.ParseCIDR(source)
				if err != nil {
					t.Fatal(err)
				}
				public := peer.PublicKey()
				mark := 517
				if fault != "foreign-device" {
					if err := devices.ConfigureDevice(selection.Interface, wgtypes.Config{PrivateKey: &local, FirewallMark: &mark, Peers: []wgtypes.PeerConfig{{PublicKey: public, AllowedIPs: []net.IPNet{*prefix}}}}); err != nil {
						t.Fatal(err)
					}
				}
				switch fault {
				case "foreign-multicast":
					_, dst, _ := net.ParseCIDR("ff00::/8")
					if err := netlink.RouteAdd(&netlink.Route{LinkIndex: link.Attrs().Index, Dst: dst, Table: unix.RT_TABLE_LOCAL, Protocol: unix.RTPROT_STATIC, Type: unix.RTN_MULTICAST, Priority: 257}); err != nil {
						t.Fatal(err)
					}
				case "empty-peer":
					if err := devices.ConfigureDevice(selection.Interface, wgtypes.Config{Peers: []wgtypes.PeerConfig{{PublicKey: public, Remove: true}}}); err != nil {
						t.Fatal(err)
					}
				case "alias":
					selection.Alias += "wrong"
				case "key":
					other, _ := wgtypes.GeneratePrivateKey()
					selection.PublicKey = other.PublicKey().String()
				case "peer":
					other, _ := wgtypes.GeneratePrivateKey()
					selection.PeerPublicKey = other.PublicKey().String()
				case "permissions":
					selection.AllowedSources = []string{foreignSource}
				case "address":
					addr, err := netlink.ParseAddr(address)
					if err != nil {
						t.Fatal(err)
					}
					addr.Flags = 2
					if err := netlink.AddrAdd(link, addr); err != nil {
						t.Fatal(err)
					}
				case "route", "foreign-route":
					dst := prefix
					if fault == "foreign-route" {
						_, dst, err = net.ParseCIDR(foreignSource)
						if err != nil {
							t.Fatal(err)
						}
					}
					if err := netlink.RouteAdd(&netlink.Route{LinkIndex: link.Attrs().Index, Dst: dst, Table: 2001, Scope: netlink.SCOPE_LINK}); err != nil {
						t.Fatal(err)
					}
				case "extra-peer":
					other, _ := wgtypes.GeneratePrivateKey()
					_, otherPrefix, err := net.ParseCIDR(foreignSource)
					if err != nil {
						t.Fatal(err)
					}
					if err := devices.ConfigureDevice(selection.Interface, wgtypes.Config{Peers: []wgtypes.PeerConfig{{PublicKey: other.PublicKey(), AllowedIPs: []net.IPNet{*otherPrefix}}}}); err != nil {
						t.Fatal(err)
					}
				}
				retireErr := retireRecordedSiteReceiver(devices, config{fwmark: 517}, []receiveRouteSelection{selection})
				if fault == "none" || fault == "empty-peer" {
					if retireErr != nil {
						t.Fatal(retireErr)
					}
					if _, err := netlink.LinkByName(selection.Interface); !isLinkNotFound(err) {
						t.Fatal("receiver remains")
					}
					if err := retireRecordedSiteReceiver(devices, config{fwmark: 517}, []receiveRouteSelection{selection}); err != nil {
						t.Fatal("idempotent retirement", err)
					}
					foreign := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: selection.Interface}}
					if err := netlink.LinkAdd(foreign); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { netlink.LinkDel(foreign) })
					if err := retireRecordedSiteReceiver(devices, config{fwmark: 517}, []receiveRouteSelection{selection}); err == nil {
						t.Fatal("retry deleted replacement interface")
					}
					if current, err := netlink.LinkByName(selection.Interface); err != nil || current.Type() != "dummy" {
						t.Fatal("replacement interface was lost")
					}

				} else {
					if retireErr == nil {
						t.Fatal("unsafe receiver deletion accepted")
					}
					current, err := netlink.LinkByName(selection.Interface)
					if err != nil || current.Attrs().Index != link.Attrs().Index {
						t.Fatal("rejected retirement removed device")
					}
					if fault != "foreign-device" {
						live, err := devices.Device(selection.Interface)
						if err != nil {
							t.Fatal(err)
						}
						found := false
						for _, p := range live.Peers {
							found = found || p.PublicKey == public
						}
						if live.PublicKey != local.PublicKey() || !found {
							t.Fatal("rejected retirement changed peer identity")
						}
					}
				}
			})
		}
	}
}
