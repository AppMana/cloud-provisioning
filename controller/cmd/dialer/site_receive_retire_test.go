package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// This exercises durable ownership selection and callback ordering only. Native
// route/device behavior is covered separately in disposable guest namespaces.
func TestWithdrawnSiteReceiverRecords(t *testing.T) {
	for _, fault := range []string{"none", "prepare", "retire", "authority", "republished", "legacy-only", "legacy-digest", "mixed"} {
		t.Run(fault, func(t *testing.T) {
			site, _ := wgtypes.GeneratePrivateKey()
			root, _ := wgtypes.GeneratePrivateKey()
			other, _ := wgtypes.GeneratePrivateKey()
			plan := receiveRestoreTestPlan(t, root, site)
			cfg := config{iface: "site", secretNamespace: "test", secretName: "mesh", nodeName: "site", privateKeyFile: filepath.Join(t.TempDir(), "key")}
			base := siteReceiveCachePath(cfg)
			path := base + ".admitted"
			for i := 0; i < 4; i++ {
				public := root.PublicKey().String()
				if i == 3 {
					public = other.PublicKey().String()
				}
				receiver, _ := wgtypes.GeneratePrivateKey()
				peer := tunnel.PeerSpec{PublicKey: public, Remote: true, Endpoint: "198.51.100.99:51820", WGAllowedIPs: []string{"172.29.0.0/24"}, RouteHosts: []string{"172.29.0.4"}}
				record := siteReceiveCache{Version: 1, Namespace: "test", SecretName: "mesh", MeshUID: plan.MeshUID, NodeName: "site", PublicKey: site.PublicKey().String(), Peers: tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{peer}}, Bindings: map[string]receiveBinding{public: {PeerPublicKey: site.PublicKey().String(), PublicKey: receiver.PublicKey().String(), ListenPort: 51821 + i}}, Generations: map[string]string{public: strings.Repeat(string(rune('a'+i)), 64)}}
				if fault == "mixed" && i == 0 {
					survivor := peer
					survivor.PublicKey = other.PublicKey().String()
					survivor.Endpoint = "198.51.100.100:51820"
					survivor.WGAllowedIPs = []string{"172.30.0.0/24"}
					survivor.RouteHosts = []string{"172.30.0.4"}
					record.Peers.Peers = append(record.Peers.Peers, survivor)
					survivorReceiver, _ := wgtypes.GeneratePrivateKey()
					record.Bindings[survivor.PublicKey] = receiveBinding{PeerPublicKey: site.PublicKey().String(), PublicKey: survivorReceiver.PublicKey().String(), ListenPort: 51830}
				}
				recordPath := path
				if i == 0 || fault == "legacy-only" {
					recordPath = base
				}
				if err := preserveSiteReceiveOwnership(recordPath, record, cfg, site); err != nil {
					t.Fatal(err)
				}
			}
			if fault == "legacy-digest" {
				files, _ := filepath.Glob(base + ".owners/*.json")
				if err := os.WriteFile(files[0], []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			mesh := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mesh", Namespace: "test", UID: "restore-mesh", ResourceVersion: "1"}, Data: map[string][]byte{tunnel.NodePublicKeyPrefix + "site": []byte(site.PublicKey().String())}}
			if fault == "republished" {
				mesh.Data[tunnel.PeerPublicKeyPrefix+"worker"] = []byte(root.PublicKey().String())
				mesh.Data[tunnel.PeerEndpointPrefix+"worker"] = []byte("198.51.100.99:51820")
				mesh.Data[tunnel.PeerAllowedIPsPrefix+"worker"] = []byte("172.29.0.0/24")
			}
			scope := &siteReceiveScope{plans: []tunnel.ReceiveTransitionPlan{plan}, check: func() error {
				if fault == "authority" {
					return errors.New("changed authority")
				}
				return nil
			}}
			prepared, retired := 0, 0
			prepare := func(groups map[string][]receiveRouteSelection, routes map[string][]string) error {
				prepared++
				if len(groups) != 3 || len(routes) != 3 {
					t.Fatal("lost pending generation or selected unrelated root")
				}
				for name, selections := range groups {
					if len(selections) != 1 || !slices.Equal(routes[name], []string{"172.29.0.0/24", "172.29.0.4/32"}) {
						t.Fatal("lost explicit route hosts or exact ownership")
					}
				}
				if fault == "prepare" {
					return errors.New("route withdrawal failed")
				}
				return nil
			}
			retire := func(_ []receiveRouteSelection, _ []string) error {
				retired++
				if prepared != 1 {
					t.Fatal("device retirement preceded route withdrawal")
				}
				if fault == "retire" {
					return errors.New("device dependency remains")
				}
				return nil
			}
			err := retireSiteReceiverRecords(context.Background(), fake.NewClientset(mesh), cfg, mesh, site, path, scope, root.PublicKey().String(), prepare, retire, base)
			remaining, globErr := filepath.Glob(path + ".owners/*.json")
			if globErr != nil {
				t.Fatal(globErr)
			}
			legacy, e := filepath.Glob(base + ".owners/*.json")
			if e != nil {
				t.Fatal(e)
			}
			remaining = append(remaining, legacy...)
			if fault == "none" || fault == "legacy-only" || fault == "mixed" {
				want := 1
				if fault == "mixed" {
					want = 2
				}
				for _, name := range remaining {
					raw, e := os.ReadFile(name)
					if e != nil {
						t.Fatal(e)
					}
					record, e := decodeSiteReceiveCache(raw, cfg, site)
					if e != nil {
						t.Fatal(e)
					}
					if _, found := record.Bindings[root.PublicKey().String()]; found {
						t.Fatal("retained withdrawn binding")
					}
					if _, found := record.Bindings[other.PublicKey().String()]; !found {
						t.Fatal("lost surviving binding")
					}
				}
				if err != nil || prepared != 1 || retired != 3 || len(remaining) != want {
					t.Fatalf("retirement: prepared=%d retired=%d retained=%d err=%v", prepared, retired, len(remaining), err)
				}
			} else {
				if err == nil || len(remaining) != 4 {
					t.Fatal("failed cleanup discarded ownership", err)
				}
				if (fault == "authority" || fault == "republished" || fault == "legacy-digest") && (prepared != 0 || retired != 0) {
					t.Fatal("rejected authority reached native callback")
				}
				if fault == "prepare" && retired != 0 {
					t.Fatal("failed route withdrawal reached device retirement")
				}
			}
		})
	}
}

// Native deletion is simulated here; the dedicated receiver native test covers
// exact ownership, route/address dependencies and idempotent kernel deletion.
func TestSiteReceiverRetirementAdmission(t *testing.T) {
	for _, fault := range []string{"none", "previous-sources", "live-owner", "reserved-owner", "inventory", "publication", "native", "digest", "cancel-after-native"} {
		t.Run(fault, func(t *testing.T) {
			site, _ := wgtypes.GeneratePrivateKey()
			root, _ := wgtypes.GeneratePrivateKey()
			cloud, _ := wgtypes.GeneratePrivateKey()
			receiver, _ := wgtypes.GeneratePrivateKey()
			plan := receiveRestoreTestPlan(t, root, site)
			if fault == "reserved-owner" {
				cloud = root
			}
			cfg := config{iface: "site", secretNamespace: "test", secretName: "mesh", nodeName: "site", privateKeyFile: filepath.Join(t.TempDir(), "key")}
			peer := tunnel.PeerSpec{PublicKey: cloud.PublicKey().String(), Remote: true, Endpoint: "198.51.100.99:51820", WGAllowedIPs: []string{"172.29.99.1/32"}}
			record := siteReceiveCache{Version: 1, Namespace: "test", SecretName: "mesh", MeshUID: plan.MeshUID, NodeName: "site", PublicKey: site.PublicKey().String(), Peers: tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{peer}}, Bindings: map[string]receiveBinding{peer.PublicKey: {PeerPublicKey: site.PublicKey().String(), PublicKey: receiver.PublicKey().String(), ListenPort: 51821}}}

			if fault == "previous-sources" {
				previous := peer
				previous.WGAllowedIPs = []string{"172.29.99.2/32", "2001:db8:99::2/128"}
				record.Previous = map[string]tunnel.PeerListDoc{peer.PublicKey: {Peers: []tunnel.PeerSpec{previous}}}
			}
			path := siteReceiveCachePath(cfg) + ".admitted"
			if err := preserveSiteReceiveOwnership(path, record, cfg, site); err != nil {
				t.Fatal(err)
			}
			mesh := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mesh", Namespace: "test", UID: "restore-mesh", ResourceVersion: "1"}, Data: map[string][]byte{tunnel.NodePublicKeyPrefix + "site": []byte(site.PublicKey().String())}}
			if fault == "live-owner" {
				mesh.Data[tunnel.PeerPublicKeyPrefix+"worker"] = []byte(peer.PublicKey)
				mesh.Data[tunnel.PeerEndpointPrefix+"worker"] = []byte(peer.Endpoint)
				mesh.Data[tunnel.PeerAllowedIPsPrefix+"worker"] = []byte(peer.WGAllowedIPs[0])
				mesh.Data[tunnel.PeerRouteHostsPrefix+"worker"] = []byte("172.29.99.1")
			}
			live := mesh.DeepCopy()
			if fault == "publication" {
				live.ResourceVersion = "2"
			}
			scope := &siteReceiveScope{roots: map[string]bool{}, plans: []tunnel.ReceiveTransitionPlan{plan}, check: func() error {
				if fault == "inventory" {
					return errors.New("inventory changed")
				}
				return nil
			}}
			files, _ := filepath.Glob(path + ".owners/*.json")
			if fault == "digest" {
				if err := os.WriteFile(files[0], []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			retire := func(selections []receiveRouteSelection) error {
				calls++
				if len(selections) != 1 || selections[0].PeerPublicKey != receiver.PublicKey().String() {
					t.Fatal("wrong owner selected")
				}

				if fault == "previous-sources" && !slices.Equal(selections[0].AllowedSources, []string{"172.29.99.1/32", "172.29.99.2/32", "2001:db8:99::2/128"}) {
					t.Fatal("retirement lost prepared predecessor permissions")
				}
				if fault == "native" {
					return errors.New("foreign route")
				}
				if fault == "cancel-after-native" {
					cancel()
				}
				return nil
			}
			err := retireMissingSiteReceivers(ctx, fake.NewClientset(live), cfg, mesh, site, path, scope, retire)
			remaining, _ := filepath.Glob(path + ".owners/*.json")
			if fault == "none" || fault == "previous-sources" || fault == "cancel-after-native" {
				if err != nil || calls != 1 || len(remaining) != 0 {
					t.Fatalf("retirement: calls=%d retained=%d err=%v", calls, len(remaining), err)
				}
				return
			}
			if len(remaining) != 1 {
				t.Fatal("lost ownership evidence on skipped or failed retirement")
			}
			if fault == "live-owner" || fault == "reserved-owner" {
				if err != nil || calls != 0 {
					t.Fatalf("live/reserved owner reached native deletion: %d %v", calls, err)
				}
				return
			}
			expected := 0
			if fault == "native" {
				expected = 1
			}
			if err == nil || calls != expected {
				t.Fatalf("unsafe admission: calls=%d err=%v", calls, err)
			}
		})
	}
}
