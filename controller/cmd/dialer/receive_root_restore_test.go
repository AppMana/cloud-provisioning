package main

import (
	"context"
	"net"
	"os"
	"reflect"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func TestNativeRestoreReceiveRoot(t *testing.T) {
	if os.Getenv("CLDT_RESTORE_RECEIVE_ROOT") != "1" {
		t.Skip("requires isolated VM mount and network namespaces")
	}
	requireStableAddressNamespace(t)
	for _, fault := range []string{"none", "foreign-type", "foreign-key", "unbound", "foreign-alias", "mark", "port", "cancelled"} {
		t.Run(fault, func(t *testing.T) {
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			devices, err := wgctrl.New()
			must(err)
			defer devices.Close()
			key, err := wgtypes.GeneratePrivateKey()
			must(err)
			cfg := config{iface: "restore-root", mtu: 1400, fwmark: 518, listenPort: 51820}
			t.Cleanup(func() {
				if link, err := netlink.LinkByName(cfg.iface); err == nil {
					_ = netlink.LinkDel(link)
				}
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if fault == "cancelled" {
				cancel()
			} else if fault == "foreign-type" {
				must(netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: cfg.iface}}))
			} else if fault != "none" {
				link, err := createWireGuardLink(cfg, cfg.mtu)
				must(err)
				if fault != "unbound" {
					other, err := wgtypes.GeneratePrivateKey()
					must(err)
					private, mark, port := key, cfg.fwmark, cfg.listenPort
					if fault == "foreign-key" {
						private = other
					}
					if fault == "mark" {
						mark++
					}
					if fault == "port" {
						port++
					}
					must(devices.ConfigureDevice(cfg.iface, wgtypes.Config{PrivateKey: &private, FirewallMark: &mark, ListenPort: &port}))
				}
				if fault == "foreign-alias" {
					must(netlink.LinkSetAlias(link, "foreign"))
				}
			}
			before, _ := devices.Device(cfg.iface)
			err = restoreReceiveRoot(ctx, devices, cfg, key, "mesh", "node")
			if fault != "none" {
				if err == nil {
					t.Fatal("unsafe root recovery accepted")
				}
				after, _ := devices.Device(cfg.iface)
				if !reflect.DeepEqual(before, after) {
					t.Fatal("rejected root recovery changed native identity")
				}
				return
			}
			must(err)
			must(verifyReceiveReversePathFiltering(cfg.iface))
			link, err := netlink.LinkByName(cfg.iface)
			must(err)
			// Existing peers and addresses survive repeated maintenance.
			peer, err := wgtypes.GeneratePrivateKey()
			must(err)
			_, allowed, err := net.ParseCIDR("10.77.0.2/32")
			must(err)
			must(devices.ConfigureDevice(cfg.iface, wgtypes.Config{Peers: []wgtypes.PeerConfig{{PublicKey: peer.PublicKey(), AllowedIPs: []net.IPNet{*allowed}}}}))
			address, err := netlink.ParseAddr("10.77.0.1/32")
			must(err)
			must(netlink.AddrAdd(link, address))
			// Older roots predate ownership aliases; their exact identity must
			// still be admitted without changing peers or addresses.
			must(netlink.LinkSetAlias(link, ""))
			before, err = devices.Device(cfg.iface)
			must(err)
			addresses, err := netlink.AddrList(link, netlink.FAMILY_ALL)
			must(err)
			must(restoreReceiveRoot(ctx, devices, cfg, key, "mesh", "node"))
			after, err := devices.Device(cfg.iface)
			must(err)
			afterAddresses, err := netlink.AddrList(link, netlink.FAMILY_ALL)
			must(err)
			if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(addresses, afterAddresses) {
				t.Fatal("root recovery changed an existing peer or address")
			}
			must(netlink.LinkDel(link))
			must(restoreReceiveRoot(ctx, devices, cfg, key, "mesh", "node"))
			after, err = devices.Device(cfg.iface)
			must(err)
			if len(after.Peers) != 0 {
				t.Fatal("root recreation replayed peers without authority")
			}
		})
	}
}
