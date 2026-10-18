package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// Filesystem-only tests: no native deletion, API authority, crash atomicity or
// completion receipt is implied. All cache inputs must validate before importing
// ownership. A stale snapshot must fail before changing either executable cache;
// retries after a partial durable commit require reloading current state.
func TestRetiringSiteCaches(t *testing.T) {
	for _, fault := range []string{"none", "single-root", "corrupt", "mesh", "mode", "symlink", "changed", "changed-mode", "changed-symlink"} {
		t.Run(fault, func(t *testing.T) {
			site, _ := wgtypes.GeneratePrivateKey()
			root, _ := wgtypes.GeneratePrivateKey()
			other, _ := wgtypes.GeneratePrivateKey()
			cfg := config{iface: "site", secretNamespace: "test", secretName: "mesh", nodeName: "site", privateKeyFile: filepath.Join(t.TempDir(), "key")}
			record := siteReceiveCache{Version: 1, Namespace: "test", SecretName: "mesh", MeshUID: "mesh-uid", NodeName: "site", PublicKey: site.PublicKey().String(), Bindings: map[string]receiveBinding{}, Generations: map[string]string{}, Previous: map[string]tunnel.PeerListDoc{}}
			for i, key := range []wgtypes.Key{root, other} {
				if fault == "single-root" && i == 1 {
					continue
				}
				receiver, _ := wgtypes.GeneratePrivateKey()
				pub := key.PublicKey().String()
				peer := tunnel.PeerSpec{PublicKey: pub, Remote: true, Endpoint: "198.51.100.99:51820", WGAllowedIPs: []string{fmt.Sprintf("172.29.0.%d/32", 4+i)}}
				record.Peers.Peers = append(record.Peers.Peers, peer)
				record.Bindings[pub] = receiveBinding{PeerPublicKey: site.PublicKey().String(), PublicKey: receiver.PublicKey().String(), ListenPort: 51821 + i}
				record.Generations[pub] = strings.Repeat(string(rune('a'+i)), 64)
			}
			// Previous documents intentionally include both roots: retiring one binding
			// must not rewrite the surviving receiver's historical input document.
			for pub := range record.Bindings {
				record.Previous[pub] = record.Peers
			}
			base := siteReceiveCachePath(cfg)
			paths := []string{base, base + ".admitted"}
			for _, path := range paths {
				if err := writeDurableJSON(path, record); err != nil {
					t.Fatal(err)
				}
			}
			original, err := os.ReadFile(base)
			if err != nil {
				t.Fatal(err)
			}
			alter := func(kind string) {
				t.Helper()
				var err error
				switch kind {
				case "corrupt", "changed":
					err = os.WriteFile(paths[1], []byte("{}"), 0600)
				case "mesh":
					changed := record
					changed.MeshUID = "replacement"
					err = writeDurableJSON(paths[1], changed)
				case "mode", "changed-mode":
					err = os.Chmod(paths[1], 0644)
				case "symlink", "changed-symlink":
					if err = os.Remove(paths[1]); err == nil {
						err = os.Symlink(base, paths[1])
					}
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if !strings.HasPrefix(fault, "changed") {
				alter(fault)
			}
			caches, err := preserveRetiringSiteCaches(cfg, site, "mesh-uid")
			rejected := fault == "corrupt" || fault == "mesh" || fault == "mode" || fault == "symlink"
			if rejected {
				if err == nil {
					t.Fatal("accepted invalid second cache")
				}
				for _, path := range paths {
					files, _ := filepath.Glob(path + ".owners/*.json")
					if len(files) != 0 {
						t.Fatal("imported first cache before validating second")
					}
				}
				return
			}
			if err != nil || len(caches) != 2 {
				t.Fatalf("preserve: %v", err)
			}
			for _, path := range paths {
				files, _ := filepath.Glob(path + ".owners/*.json")
				if len(files) != 1 {
					t.Fatal("missing imported ownership")
				}
			}
			if strings.HasPrefix(fault, "changed") {
				alter(fault)
			}
			err = commitRetiredSiteCaches(caches, root.PublicKey().String())
			if strings.HasPrefix(fault, "changed") {
				if err == nil {
					t.Fatal("accepted changed cache")
				}
				raw, e := os.ReadFile(base)
				if e != nil || string(raw) != string(original) {
					t.Fatal("changed first cache before validating second")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range paths {
				raw, err := os.ReadFile(path)
				if fault == "single-root" {
					if !os.IsNotExist(err) {
						t.Fatal("retired executable cache still present", err)
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				got, err := decodeSiteReceiveCache(raw, cfg, site)
				if err != nil {
					t.Fatal(err)
				}
				survivor := other.PublicKey().String()
				if len(got.Bindings) != 1 || !reflect.DeepEqual(got.Bindings[survivor], record.Bindings[survivor]) || len(got.Generations) != 1 || got.Generations[survivor] != record.Generations[survivor] || len(got.Previous) != 1 || !reflect.DeepEqual(got.Previous[survivor], record.Previous[survivor]) || len(got.Peers.Peers) != 1 || got.Peers.Peers[0].PublicKey != survivor {
					t.Fatal("lost survivor or retained retired binding")
				}
			}
			if len(caches[0].record.Bindings) != len(record.Bindings) {
				t.Fatal("mutated retry snapshot")
			}
			if err := commitRetiredSiteCaches(caches, root.PublicKey().String()); err == nil {
				t.Fatal("accepted stale retry snapshot")
			}
			refreshed, err := preserveRetiringSiteCaches(cfg, site, "mesh-uid")
			if err != nil {
				t.Fatal(err)
			}
			if err := commitRetiredSiteCaches(refreshed, root.PublicKey().String()); err != nil {
				t.Fatal("reloaded retry failed", err)
			}
			// The source fixture itself remains unchanged, including shared previous docs.
			raw, _ := json.Marshal(record)
			if _, err := decodeSiteReceiveCache(raw, cfg, site); err != nil {
				t.Fatal(err)
			}
		})
	}
}
