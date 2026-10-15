package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSiteForwardingCacheIdentityAndRelayObservations(t *testing.T) {
	key, _ := wgtypes.GeneratePrivateKey()
	cfg := config{iface: "site", secretNamespace: "system", secretName: "mesh", nodeName: "site-node", privateKeyFile: filepath.Join(t.TempDir(), "key"), routeTable: 518, fwmark: 518}
	mesh := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: cfg.secretNamespace, Name: cfg.secretName, UID: "mesh-uid", ResourceVersion: "1"}, Data: map[string][]byte{tunnel.NodePublicKeyPrefix + cfg.nodeName: []byte(key.PublicKey().String()), tunnel.NodeTunnelAddressPrefix + cfg.nodeName: []byte("10.88.0.4/32")}}
	unavailable := map[string]bool{"other-site": true}
	applied := map[string]bool{"cloud-a": true, "cloud-b": false}
	if err := saveSiteForwardingCache(cfg, key, mesh, unavailable, applied); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(siteForwardingCachePath(cfg))
	if err != nil || bytes.Contains(raw, []byte(key.String())) {
		t.Fatal("missing or private recovery input", err)
	}
	saved, err := readSiteForwardingCache(cfg, key)
	if err != nil || !reflect.DeepEqual(saved.NotReady, unavailable) || !reflect.DeepEqual(saved.PeerAcked, applied) {
		t.Fatal("relay observations changed", err)
	}
	info, err := os.Stat(siteForwardingCachePath(cfg))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("cache permissions", err)
	}
	for _, fault := range []string{"version", "namespace", "secret", "mesh", "node", "key", "table", "mark", "interface", "address", "acknowledgements", "unknown", "trailing"} {
		t.Run(fault, func(t *testing.T) {
			var broken siteForwardingCache
			if err := json.Unmarshal(raw, &broken); err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "version":
				broken.Version++
			case "namespace":
				broken.Namespace = "other"
			case "secret":
				broken.SecretName = "other"
			case "mesh":
				broken.MeshUID = ""
			case "node":
				broken.NodeName = "other"
			case "key":
				broken.PublicKey = "invalid"
			case "table":
				broken.Table++
			case "mark":
				broken.Mark++
			case "interface":
				broken.Interface = "other"
			case "address":
				delete(broken.Data, tunnel.NodeTunnelAddressPrefix+cfg.nodeName)
			case "acknowledgements":
				broken.PeerAcked = nil
			}
			invalid, _ := json.Marshal(broken)
			if fault == "unknown" {
				invalid = append([]byte(`{"activation":true,`), invalid[1:]...)
			}
			if fault == "trailing" {
				invalid = append(invalid, []byte(` {}`)...)
			}
			if _, err := decodeSiteForwardingCache(invalid, cfg, key); err == nil {
				t.Fatal("invalid recovery accepted")
			}
		})
	}
	mesh.UID = ""
	if err := saveSiteForwardingCache(cfg, key, mesh, nil, map[string]bool{}); err == nil {
		t.Fatal("unversioned state accepted")
	}
	after, _ := os.ReadFile(siteForwardingCachePath(cfg))
	if !bytes.Equal(raw, after) {
		t.Fatal("rejected input replaced recovery state")
	}
}
