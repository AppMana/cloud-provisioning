package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// Native deletion is simulated here; the dedicated receiver native test covers
// exact ownership, route/address dependencies and idempotent kernel deletion.
func TestSiteReceiverRetirementAdmission(t *testing.T) {
	for _, fault := range []string{"none", "live-owner", "reserved-owner", "inventory", "publication", "native", "digest"} {
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
			calls := 0
			retire := func(selections []receiveRouteSelection) error {
				calls++
				if len(selections) != 1 || selections[0].PeerPublicKey != receiver.PublicKey().String() {
					t.Fatal("wrong owner selected")
				}
				if fault == "native" {
					return errors.New("foreign route")
				}
				return nil
			}
			err := retireMissingSiteReceivers(context.Background(), fake.NewClientset(live), cfg, mesh, site, path, scope, retire)
			remaining, _ := filepath.Glob(path + ".owners/*.json")
			if fault == "none" {
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
