package main

import (
	"bytes"
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func TestReceiveCachePreservesOnlyValidatedPublicPreparation(t *testing.T) {
	root, _ := wgtypes.GeneratePrivateKey()
	peer, _ := wgtypes.GeneratePrivateKey()
	cfg := config{iface: "wgtest", peersFile: filepath.Join(t.TempDir(), "peers.json"), nodeName: "worker"}
	base := []byte(`{"peers":[]}`)
	request := tunnel.ReceivePrepare{Version: 1, MeshUID: "mesh", NodeName: "worker", NodeUID: "node", BootID: "previous-boot", RootPublicKey: root.PublicKey().String(), BaseHash: tunnel.HashPeerList(base), Ports: map[string]int{peer.PublicKey().String(): 51821}, Desired: tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{{PublicKey: peer.PublicKey().String(), WGAllowedIPs: []string{"10.90.0.10/32"}}}}, Native: []netip.Prefix{netip.MustParsePrefix("172.29.0.0/24")}}
	if err := cacheReceivePreparation(cfg, root, base, request); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(receiveCachePath(cfg))
	if err != nil || bytes.Contains(raw, []byte(root.String())) {
		t.Fatal("cache missing or contains private key", err)
	}
	info, err := os.Stat(receiveCachePath(cfg))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("cache is not private", err)
	}
	if _, _, err := decodeReceiveCache(raw, cfg, root); err != nil {
		t.Fatal("prepared state cannot survive a different kernel boot", err)
	}
	request.ReceiverGeneration = strings.Repeat("a", 64)
	if err := cacheReceivePreparation(cfg, root, base, request); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(receiveCachePath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	decoded, _, err := decodeReceiveCache(raw, cfg, root)
	if err != nil || decoded.Request.ReceiverGeneration != request.ReceiverGeneration {
		t.Fatalf("cache lost generation: %v", err)
	}
	for _, fault := range []string{"version", "generation", "node", "root", "base", "ports", "unknown", "trailing"} {
		t.Run(fault, func(t *testing.T) {
			var cached receivePreparationCache
			if err := json.Unmarshal(raw, &cached); err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "generation":
				cached.Request.ReceiverGeneration = "invalid"
			case "version":
				cached.Version++
			case "node":
				cached.Request.NodeName = "another-node"
			case "root":
				cached.Request.RootPublicKey = peer.PublicKey().String()
			case "base":
				cached.Base = []byte(`{"peers":null}`)
			case "ports":
				cached.Request.Ports = nil
			}
			invalid, _ := json.Marshal(cached)
			if fault == "unknown" {
				invalid = append([]byte(`{"activation":true,`), invalid[1:]...)
			}
			if fault == "trailing" {
				invalid = append(invalid, []byte(` {}`)...)
			}
			if _, _, err := decodeReceiveCache(invalid, cfg, root); err == nil {
				t.Fatal("unsafe recovery accepted")
			}
		})
	}
	request.BaseHash = "invalid"
	if err := cacheReceivePreparation(cfg, root, base, request); err == nil {
		t.Fatal("invalid preparation replaced durable state")
	}
	after, _ := os.ReadFile(receiveCachePath(cfg))
	if !bytes.Equal(raw, after) {
		t.Fatal("rejected request changed cache")
	}
	// A newer authoritative forwarding document cannot revive stale preparation.
	if err := restoreCachedReceivePreparation(cfg, nil, root, tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{{PublicKey: peer.PublicKey().String()}}}); err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < 2; pass++ {
		if err := clearReceivePreparationCache(cfg); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(receiveCachePath(cfg)); !os.IsNotExist(err) {
			t.Fatal("withdrawn preparation remains cached", err)
		}
		if err := restoreCachedReceivePreparation(cfg, nil, root, tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{}}); err != nil {
			t.Fatal("withdrawal did not survive offline recovery", err)
		}
	}
}
