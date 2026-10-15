package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
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
