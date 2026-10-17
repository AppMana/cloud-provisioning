package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestSiteReceiveCacheRejectsInvalidRecoveryBeforeDevices(t *testing.T) {
	key, _ := wgtypes.GeneratePrivateKey()
	cloud, _ := wgtypes.GeneratePrivateKey()
	receiver, _ := wgtypes.GeneratePrivateKey()
	cfg := config{iface: "site", secretNamespace: "mesh-system", secretName: "mesh", nodeName: "site-node", privateKeyFile: filepath.Join(t.TempDir(), "key")}
	valid := siteReceiveCache{Version: 1, Namespace: cfg.secretNamespace, SecretName: cfg.secretName, MeshUID: "mesh-uid", NodeName: cfg.nodeName, PublicKey: key.PublicKey().String(), Peers: tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{{PublicKey: cloud.PublicKey().String(), Remote: true, Endpoint: "198.51.100.10:51820", WGAllowedIPs: []string{"10.88.0.3/32"}}}}, Bindings: map[string]receiveBinding{cloud.PublicKey().String(): {PeerPublicKey: key.PublicKey().String(), PublicKey: receiver.PublicKey().String(), ListenPort: 51821}}}
	raw, _ := json.Marshal(valid)
	if _, err := decodeSiteReceiveCache(raw, cfg, key); err != nil {
		t.Fatal(err)
	}
	for _, fault := range []string{"version", "namespace", "secret", "node", "key", "mesh", "bindings", "port", "sources", "unknown", "trailing"} {
		t.Run(fault, func(t *testing.T) {
			var cached siteReceiveCache
			if err := json.Unmarshal(raw, &cached); err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "version":
				cached.Version++
			case "namespace":
				cached.Namespace = "other"
			case "secret":
				cached.SecretName = "other"
			case "node":
				cached.NodeName = "other"
			case "key":
				cached.PublicKey = cloud.PublicKey().String()
			case "mesh":
				cached.MeshUID = ""
			case "bindings":
				cached.Bindings = nil
			case "port":
				b := cached.Bindings[cloud.PublicKey().String()]
				b.ListenPort = 0
				cached.Bindings[cloud.PublicKey().String()] = b
			case "sources":
				cached.Peers.Peers[0].WGAllowedIPs = []string{"0.0.0.0/0"}
			}
			invalid, _ := json.Marshal(cached)
			if fault == "unknown" {
				invalid = append([]byte(`{"routes":[],`), invalid[1:]...)
			}
			if fault == "trailing" {
				invalid = append(invalid, []byte(` {}`)...)
			}
			if err := os.WriteFile(siteReceiveCachePath(cfg), invalid, 0600); err != nil {
				t.Fatal(err)
			}
			if err := restoreSiteReceiveCache(cfg, nil, key); err == nil {
				t.Fatal("invalid cache reached device recovery")
			}
		})
	}
	valid.Bindings = map[string]receiveBinding{}
	if err := writeDurableJSON(siteReceiveCachePath(cfg), valid); err != nil {
		t.Fatal(err)
	}
	if err := restoreSiteReceiveCache(cfg, nil, key); err != nil {
		t.Fatal("empty preparation cannot recover", err)
	}
	if err := removeDurableCache(siteReceiveCachePath(cfg)); err != nil {
		t.Fatal(err)
	}
	if err := restoreSiteReceiveCache(cfg, nil, key); err != nil {
		t.Fatal("removed preparation cannot recover", err)
	}
}

func TestAdmittedSitePreparationCannotReplaceLegacyRecovery(t *testing.T) {
	key, _ := wgtypes.GeneratePrivateKey()
	cfg := config{iface: "site", secretNamespace: "test", secretName: "mesh", nodeName: "site", privateKeyFile: filepath.Join(t.TempDir(), "key")}
	mesh := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mesh", Namespace: "test", UID: "mesh", ResourceVersion: "1"}, Data: map[string][]byte{tunnel.NodePublicKeyPrefix + "site": []byte(key.PublicKey().String())}}
	k := fake.NewClientset(mesh)
	legacy := []byte("legacy recovery remains independently owned")
	path := siteReceiveCachePath(cfg)
	if err := os.WriteFile(path, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	scope := &siteReceiveScope{roots: map[string]bool{}, check: func() error { return nil }}
	if err := reconcileDurableSiteReceiversScoped(context.Background(), k, nil, cfg, mesh, key, scope); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, legacy) {
		t.Fatal("scoped preparation replaced legacy recovery", err)
	}
	admitted, err := os.ReadFile(path + ".admitted")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeSiteReceiveCache(admitted, cfg, key); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := restoreSiteReceiveCache(cfg, nil, key); err != nil {
		t.Fatal("admitted records entered offline recovery", err)
	}
}

func TestRemovedSiteReceiverRetainsOwnershipEvidence(t *testing.T) {
	key, _ := wgtypes.GeneratePrivateKey()
	cloud, _ := wgtypes.GeneratePrivateKey()
	receiver, _ := wgtypes.GeneratePrivateKey()
	cfg := config{iface: "site", secretNamespace: "test", secretName: "mesh", nodeName: "site", privateKeyFile: filepath.Join(t.TempDir(), "key")}
	prior := siteReceiveCache{Version: 1, Namespace: "test", SecretName: "mesh", MeshUID: "mesh", NodeName: "site", PublicKey: key.PublicKey().String(), Peers: tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{{PublicKey: cloud.PublicKey().String(), Remote: true, Endpoint: "198.51.100.10:51820", WGAllowedIPs: []string{"172.29.0.10/32"}}}}, Bindings: map[string]receiveBinding{cloud.PublicKey().String(): {PeerPublicKey: key.PublicKey().String(), PublicKey: receiver.PublicKey().String(), ListenPort: 51821}}}
	path := siteReceiveCachePath(cfg) + ".admitted"
	if err := writeDurableJSON(path, prior); err != nil {
		t.Fatal(err)
	}
	mesh := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mesh", Namespace: "test", UID: "mesh", ResourceVersion: "2"}, Data: map[string][]byte{tunnel.NodePublicKeyPrefix + "site": []byte(key.PublicKey().String())}}
	api := fake.NewClientset(mesh)
	scope := &siteReceiveScope{roots: map[string]bool{}, check: func() error { return nil }}
	for i := 0; i < 2; i++ {
		if err := reconcileDurableSiteReceiversScoped(context.Background(), api, nil, cfg, mesh, key, scope); err != nil {
			t.Fatal(err)
		}
	}
	records, err := filepath.Glob(path + ".owners/*.json")
	if err != nil || len(records) != 1 {
		t.Fatalf("lost or duplicated ownership records: %v %v", records, err)
	}
	raw, err := os.ReadFile(records[0])
	if err != nil {
		t.Fatal(err)
	}
	preserved, err := decodeSiteReceiveCache(raw, cfg, key)
	if err != nil || len(preserved.Bindings) != 1 || preserved.Bindings[cloud.PublicKey().String()].PublicKey != receiver.PublicKey().String() {
		t.Fatalf("ownership evidence changed: %+v %v", preserved, err)
	}
	// Retention must not resurrect withdrawn preparation during offline boot.
	if err := restoreSiteReceiveCache(cfg, nil, key); err != nil {
		t.Fatal(err)
	}
	if err := preserveSiteReceiveOwnership(path, prior, cfg, key); err != nil {
		t.Fatal("idempotent preservation", err)
	}
	if err := os.WriteFile(records[0], []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := preserveSiteReceiveOwnership(path, prior, cfg, key); err == nil {
		t.Fatal("overwrote changed ownership evidence")
	}
	if err := os.Remove(records[0]); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "foreign"), records[0]); err != nil {
		t.Fatal(err)
	}
	if err := preserveSiteReceiveOwnership(path, prior, cfg, key); err == nil {
		t.Fatal("accepted symlink ownership record")
	}
}
