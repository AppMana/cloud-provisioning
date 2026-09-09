package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// Run explicitly inside an otherwise empty network namespace in a QEMU VM.
// Ordinary unit runs skip this test; opting in on the host namespace is refused.
func TestNativeEmptyPeerWithdrawal(t *testing.T) {
	if os.Getenv("CLDT_NATIVE_EMPTY_PEERS") != "1" {
		t.Skip("requires isolated VM network namespace")
	}
	self, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	init, err := os.Readlink("/proc/1/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	if self == init {
		t.Fatal("refusing the guest host network namespace")
	}
	links, err := netlink.LinkList()
	if err != nil {
		t.Fatal(err)
	}
	for _, link := range links {
		if link.Attrs().Name != "lo" {
			t.Fatal("namespace is not empty")
		}
	}
	for _, source := range []string{"bootstrap", "cache", "site-rendered", "cache-corrupt", "adoption-repair", "adoption-api"} {
		t.Run(source, func(t *testing.T) {
			cfg := config{iface: "cldtemptytest", peersFile: filepath.Join(t.TempDir(), "identity.json"), mtu: 1400, routeTable: 518, fwmark: 518, pollInterval: time.Second}
			key, err := wgtypes.GeneratePrivateKey()
			if err != nil {
				t.Fatal(err)
			}
			peer, err := wgtypes.GeneratePrivateKey()
			if err != nil {
				t.Fatal(err)
			}
			doc := tunnel.PeersFileDoc{PrivateKey: key.String(), LocalAddress: "10.253.253.1/32", Peers: []tunnel.PeerSpec{{PublicKey: peer.PublicKey().String(), Endpoint: "127.0.0.1:51821", WGAllowedIPs: []string{"10.253.253.2/32"}, RouteHosts: []string{"10.253.253.2"}}}}
			write := func() {
				t.Helper()
				raw, err := json.Marshal(doc)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(cfg.peersFile, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			write()
			client, err := wgctrl.New()
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			t.Cleanup(func() {
				if link, err := netlink.LinkByName(cfg.iface); err == nil {
					if err = netlink.LinkDel(link); err != nil {
						t.Error(err)
					}
				}
			})
			if err = reconcile(context.Background(), nil, client, cfg); err != nil {
				t.Fatalf("initial native reconciliation: %v", err)
			}
			device, err := client.Device(cfg.iface)
			if err != nil {
				t.Fatal(err)
			}
			if len(device.Peers) != 1 {
				t.Fatal("initial peer missing")
			}
			initialLink, err := netlink.LinkByName(cfg.iface)
			if err != nil {
				t.Fatal(err)
			}
			initialRoutes, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: cfg.routeTable, LinkIndex: initialLink.Attrs().Index}, netlink.RT_FILTER_TABLE|netlink.RT_FILTER_OIF)
			if err != nil {
				t.Fatal(err)
			}
			if len(initialRoutes) != 1 || initialRoutes[0].Dst == nil || initialRoutes[0].Dst.String() != "10.253.253.2/32" {
				t.Fatal("initial peer route missing or ambiguous")
			}
			empty := []tunnel.PeerSpec{}
			if source == "cache" || source == "cache-corrupt" || source == "adoption-repair" || source == "adoption-api" {
				if err = writeCachedPeers(cachePath(cfg), tunnel.PeerListDoc{Peers: empty}); err != nil {
					t.Fatal(err)
				}
			} else {
				doc.Peers = empty
				if source == "site-rendered" {
					// Exercise the shared producer used after a successful mesh read.
					// API delivery itself remains outside this namespace canary.
					doc.Peers, err = tunnel.SitePeers(map[string][]byte{})
					if err != nil {
						t.Fatal(err)
					}
				}
				write()
			}
			if err = reconcile(context.Background(), nil, client, cfg); err != nil {
				t.Fatalf("empty desired state was not applied: %v", err)
			}
			device, err = client.Device(cfg.iface)
			if err != nil {
				t.Fatal(err)
			}
			if len(device.Peers) != 0 {
				t.Fatalf("empty %s resurrected or retained %d native peers", source, len(device.Peers))
			}
			link, err := netlink.LinkByName(cfg.iface)
			if err != nil {
				t.Fatal(err)
			}
			routes, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: cfg.routeTable, LinkIndex: link.Attrs().Index}, netlink.RT_FILTER_TABLE|netlink.RT_FILTER_OIF)
			if err != nil {
				t.Fatal(err)
			}
			if len(routes) != 0 {
				t.Fatalf("removed peer retained %d routes", len(routes))
			}
			// Reconstruct kernel state from durable input, without claiming a VM reboot.
			if err = netlink.LinkDel(link); err != nil {
				t.Fatal(err)
			}
			if err = reconcile(context.Background(), nil, client, cfg); err != nil {
				t.Fatal(err)
			}
			device, err = client.Device(cfg.iface)
			if err != nil {
				t.Fatal(err)
			}
			if len(device.Peers) != 0 {
				t.Fatal("kernel reconstruction resurrected a removed peer")
			}
			if source == "adoption-api" {
				nativeAdoptionAPI(t, cfg, client, doc)
			}
			if source == "adoption-repair" {
				nativeAdoptionRecovery(t, cfg, client, doc)
			}
			if source == "cache-corrupt" {
				for _, raw := range []string{"invalid", `{}`, `{"peers":null}`} {
					if err = os.WriteFile(cachePath(cfg), []byte(raw), 0600); err != nil {
						t.Fatal(err)
					}
					if err = reconcile(context.Background(), nil, client, cfg); err == nil {
						t.Fatalf("corrupt cache %q accepted", raw)
					}
					device, err = client.Device(cfg.iface)
					if err != nil {
						t.Fatal(err)
					}
					if len(device.Peers) != 0 {
						t.Fatal("corrupt cache resurrected bootstrap peer")
					}
				}
			}

		})
	}
}
