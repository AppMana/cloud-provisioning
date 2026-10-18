package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Filesystem and pure route projection only. No kernel removal, live follower
// admission, underlay reachability, BGP convergence or traffic deadline proof.
// Missing/changed relay evidence must reject; successful retries must retain
// the same target scope even after the executable cache no longer has the peer.
func TestWithdrawnTransitProjection(t *testing.T) {
	for _, fault := range []string{"none", "interrupted-bgp", "missing", "direct", "mesh", "alias", "overlap", "no-relay", "no-ipv6", "changed", "changed-election"} {
		t.Run(fault, func(t *testing.T) {
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			key, _ := wgtypes.GeneratePrivateKey()
			root, _ := wgtypes.GeneratePrivateKey()
			site, _ := wgtypes.GeneratePrivateKey()
			survivor, _ := wgtypes.GeneratePrivateKey()
			plan := receiveRestoreTestPlan(t, root, site)
			cfg := config{iface: "follower", nodeName: "follower", secretNamespace: "test", secretName: "mesh", privateKeyFile: filepath.Join(t.TempDir(), "key"), routeTable: 518, fwmark: 518}
			mesh := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "mesh", UID: "restore-mesh", ResourceVersion: "1"}, Data: map[string][]byte{
				tunnel.NodePublicKeyPrefix + "site": []byte(site.PublicKey().String()), tunnel.NodeTunnelAddressPrefix + "site": []byte("10.90.0.2/24"), tunnel.NodeAddressesPrefix + "site": []byte("10.90.0.2,fd90::2"),
				tunnel.PeerPublicKeyPrefix + "worker": []byte(root.PublicKey().String()), tunnel.PeerEndpointPrefix + "worker": []byte("198.51.100.1:51820"), tunnel.PeerRouteHostsPrefix + "worker": []byte("172.29.0.1,fd29::1"), tunnel.PeerAllowedIPsPrefix + "worker": []byte("172.29.0.1/32,fd29::1/128"),
				tunnel.PeerPublicKeyPrefix + "survivor": []byte(survivor.PublicKey().String()), tunnel.PeerEndpointPrefix + "survivor": []byte("198.51.100.3:51820"), tunnel.PeerRouteHostsPrefix + "survivor": []byte("172.29.0.3,fd29::3"), tunnel.PeerAllowedIPsPrefix + "survivor": []byte("172.29.0.3/32,fd29::3/128"),
			}}
			own := []string{"10.90.0.4", "fd90::4"}
			switch fault {
			case "direct":
				mesh.Data[tunnel.NodePublicKeyPrefix+cfg.nodeName] = []byte(key.PublicKey().String())
				mesh.Data[tunnel.NodeTunnelAddressPrefix+cfg.nodeName] = []byte("10.90.0.4/24")
			case "alias":
				mesh.Data[tunnel.PeerPublicKeyPrefix+"survivor"] = []byte(root.PublicKey().String())
			case "overlap":
				mesh.Data[tunnel.PeerAllowedIPsPrefix+"survivor"] = []byte("172.29.0.0/24,fd29::/64")
			case "no-relay":
				delete(mesh.Data, tunnel.NodeAddressesPrefix+"site")
			case "no-ipv6":
				mesh.Data[tunnel.NodeAddressesPrefix+"site"] = []byte("10.90.0.2")
			}
			must(saveSiteForwardingCache(cfg, key, mesh, map[string]bool{"offline": true}, map[string]bool{"worker": true, "survivor": true}, own))
			before, err := os.ReadFile(siteForwardingCachePath(cfg))
			must(err)
			if fault == "missing" {
				must(os.Remove(siteForwardingCachePath(cfg)))
			}
			uid := plan.MeshUID
			if fault == "mesh" {
				uid = "replacement"
			}
			cache, err := prepareWithdrawnTransitCache(cfg, key, uid, plan.MachineName, plan.RootPublicKey)
			if fault == "direct" || fault == "mesh" || fault == "alias" {
				if err == nil {
					t.Fatal("invalid follower cache admitted")
				}
				return
			}
			must(err)
			if fault == "changed-election" {
				cache.next.NotReady = map[string]bool{}
				cache.next.Data[tunnel.NodeAddressesPrefix+"site"] = []byte("10.90.0.99,fd90::99")
			}
			routes, err := planWithdrawnTransitRoutes(cache, []tunnel.ReceiveTransitionPlan{plan}, plan.RootPublicKey)
			if fault == "missing" || fault == "overlap" || fault == "no-relay" || fault == "no-ipv6" || fault == "changed-election" {
				if err == nil {
					t.Fatal("unowned or ambiguous routes admitted", routes)
				}
				return
			}
			must(err)
			expected := map[string]string{"172.29.0.1/32": "10.90.0.2", "fd29::1/128": "fd90::2"}
			if !reflect.DeepEqual(routes, expected) {
				t.Fatal("wrong withdrawal scope", routes)
			}
			after, err := os.ReadFile(siteForwardingCachePath(cfg))
			must(err)
			if string(before) != string(after) {
				t.Fatal("preparation changed executable cache")
			}
			if fault == "changed" {
				must(os.WriteFile(siteForwardingCachePath(cfg), []byte("{}"), 0600))
				if commitWithdrawnForwardingCache(cache) == nil {
					t.Fatal("changed cache committed")
				}
				return
			}
			if _, err := prepareWithdrawnForwardingCache(cfg, key, uid, plan.MachineName, plan.RootPublicKey); err == nil {
				t.Fatal("direct path accepted follower cache")
			}
			must(os.WriteFile(cfg.privateKeyFile, []byte(key.String()), 0600))
			speaker := &transitSpeaker{asn: 64512, nextHop: own[0], nextHop6: own[1]}
			must(saveTransitAdvertisements(cfg, speaker, mesh, []string{"10.90.0.2"}))
			advertisements, err := prepareWithdrawnAdvertisementCache(cfg, speaker, cache)
			must(err)
			if fault == "interrupted-bgp" {
				must(writeDurableJSON(advertisements.path, advertisements.next))
				if _, _, err := readTransitAdvertisements(cfg, speaker); err == nil {
					t.Fatal("interrupted follower cache pair is replayable")
				}
				advertisements, err = prepareWithdrawnAdvertisementCache(cfg, speaker, cache)
				must(err)
			}
			must(commitWithdrawnSiteCaches(cache, advertisements))
			cached, _, err := readTransitAdvertisements(cfg, speaker)
			must(err)
			if !reflect.DeepEqual(cached.Peers, []string{"10.90.0.2"}) {
				t.Fatal("follower BGP peers changed")
			}
			saved, err := readSiteForwardingCache(cfg, key)
			must(err)
			if !saved.Transit || !reflect.DeepEqual(saved.Own, own) || !saved.NotReady["offline"] || !reflect.DeepEqual(saved.PeerAcked, map[string]bool{"survivor": true}) {
				t.Fatal("follower identity or election evidence changed")
			}
			expectedData, _, err := tunnel.WithdrawRemotePeer(mesh.Data, plan.MachineName, plan.RootPublicKey)
			must(err)
			if !reflect.DeepEqual(saved.Data, expectedData) {
				t.Fatal("survivor publication changed")
			}
			retry, err := prepareWithdrawnTransitCache(cfg, key, uid, plan.MachineName, plan.RootPublicKey)
			must(err)
			retried, err := planWithdrawnTransitRoutes(retry, []tunnel.ReceiveTransitionPlan{plan}, plan.RootPublicKey)
			must(err)
			if !reflect.DeepEqual(retried, expected) {
				t.Fatal("retry lost target route ownership")
			}
			must(commitWithdrawnForwardingCache(retry))
		})
	}
}
