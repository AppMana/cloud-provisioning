package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// Filesystem only: bootstrap identity and API endpoints survive removal of a
// foreign remote peer. Exact native ownership, live admission and receipt
// authentication are separate gates; these tests do not establish cloud cleanup.
func TestWithdrawnCloudCaches(t *testing.T) {
	for _, fault := range []string{"none", "interrupted", "key", "allocation", "site-peer", "changed", "mode", "symlink", "trailing", "missing-cache"} {
		t.Run(fault, func(t *testing.T) {
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			key, _ := wgtypes.GeneratePrivateKey()
			target, _ := wgtypes.GeneratePrivateKey()
			site, _ := wgtypes.GeneratePrivateKey()
			cfg := config{iface: "cloud", peersFile: filepath.Join(t.TempDir(), "peers.json")}
			peers := []tunnel.PeerSpec{{PublicKey: target.PublicKey().String(), Remote: true, WGAllowedIPs: []string{"172.29.0.1/32"}}, {PublicKey: site.PublicKey().String(), WGAllowedIPs: []string{"10.90.0.0/24"}}}
			doc := tunnel.PeersFileDoc{PrivateKey: key.String(), LocalAddress: "172.29.0.3/32", Peers: peers, APIServers: []string{"10.90.0.1:6443"}}
			cached := tunnel.PeerListDoc{Peers: peers, APIServers: []string{"10.90.0.2:6443"}}
			switch fault {
			case "key":
				doc.PrivateKey = site.String()
			case "allocation":
				doc.LocalAddress = "172.29.0.4/32"
			case "site-peer":
				doc.Peers[0].Remote = false
			}
			must(writeDurableJSON(cfg.peersFile, doc))
			must(writeDurableJSON(cachePath(cfg), cached))
			switch fault {
			case "mode":
				must(os.Chmod(cfg.peersFile, 0644))
			case "symlink":
				must(os.Rename(cfg.peersFile, cfg.peersFile+".target"))
				must(os.Symlink(cfg.peersFile+".target", cfg.peersFile))
			case "trailing":
				raw, err := os.ReadFile(cfg.peersFile)
				must(err)
				must(os.WriteFile(cfg.peersFile, append(raw, []byte(" {}")...), 0600))
			case "missing-cache":
				must(os.Remove(cachePath(cfg)))
			}
			caches, err := prepareWithdrawnCloudCaches(cfg, key, []string{"172.29.0.3/32"}, target.PublicKey().String())
			valid := fault == "none" || fault == "interrupted" || fault == "changed" || fault == "missing-cache"
			if !valid {
				if err == nil {
					t.Fatal("invalid cloud cache accepted")
				}
				return
			}
			must(err)
			if fault == "interrupted" {
				// Simulate process loss after the bootstrap write but before
				// the optional cache write. A newly admitted retry must still
				// import the target permissions from the untouched cache.
				must(writeDurableJSON(caches[0].path, caches[0].next))
				caches, err = prepareWithdrawnCloudCaches(cfg, key, []string{"172.29.0.3/32"}, target.PublicKey().String())
				must(err)
				found := false
				for _, cache := range caches {
					for _, peer := range cache.peers {
						found = found || peer.PublicKey == target.PublicKey().String()
					}
				}
				if !found {
					t.Fatal("interrupted write lost remaining native permission evidence")
				}
			}
			if fault == "changed" {
				must(os.WriteFile(cachePath(cfg), []byte("{}"), 0600))
			}
			before, err := os.ReadFile(cfg.peersFile)
			must(err)
			err = commitWithdrawnCloudCaches(caches)
			if fault == "changed" {
				if err == nil {
					t.Fatal("changed cache committed")
				}
				after, e := os.ReadFile(cfg.peersFile)
				must(e)
				if string(before) != string(after) {
					t.Fatal("failed validation changed identity file")
				}
				return
			}
			must(err)
			got, err := readPeersFileDoc(cfg.peersFile)
			must(err)
			doc.Peers = doc.Peers[1:]
			if !reflect.DeepEqual(got, doc) {
				t.Fatal("identity, API endpoints or survivor changed")
			}
			if fault != "missing-cache" {
				raw, err := os.ReadFile(cachePath(cfg))
				must(err)
				var got tunnel.PeerListDoc
				must(json.Unmarshal(raw, &got))
				cached.Peers = cached.Peers[1:]
				if !reflect.DeepEqual(got, cached) {
					t.Fatal("cached survivor or API endpoints changed")
				}
			}
			caches, err = prepareWithdrawnCloudCaches(cfg, key, []string{"172.29.0.3/32"}, target.PublicKey().String())
			must(err)
			must(commitWithdrawnCloudCaches(caches))
		})
	}
}
