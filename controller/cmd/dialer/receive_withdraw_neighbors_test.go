package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// Filesystem and callback ordering only: no kernel, API receipt or traffic
// continuity is implied. Native/authority failure must retain retry evidence;
// multi-file completion is deliberately not claimed to be crash-atomic.
func TestWithdrawnNeighborCaches(t *testing.T) {
	for _, fault := range []string{"none", "native", "authority", "changed", "identity", "mode", "corrupt", "cancelled", "republished", "symlink", "late-authority"} {
		t.Run(fault, func(t *testing.T) {
			root, _ := wgtypes.GeneratePrivateKey()
			site, _ := wgtypes.GeneratePrivateKey()
			other, _ := wgtypes.GeneratePrivateKey()
			plan := receiveRestoreTestPlan(t, root, site)
			target := plan.SiteDesired.Peers[0]
			for _, peer := range plan.SiteDesired.Peers {
				if peer.PublicKey == root.PublicKey().String() {
					target = peer
				}
			}
			target.Remote = true
			survivor := tunnel.PeerSpec{PublicKey: other.PublicKey().String(), Remote: true, Endpoint: "198.51.100.200:51820", WGAllowedIPs: []string{"172.31.99.1/32"}}
			current := receiveNeighborCheckpoint{Version: 1, InventoryUID: "inventory", MeshUID: plan.MeshUID, MeshVersion: "new", NodeUID: "site-uid", BootID: "new-boot", PublicKey: site.PublicKey().String(), Peers: []tunnel.PeerSpec{survivor}}
			saved := current
			saved.BootID = "old-boot"
			saved.Peers = []tunnel.PeerSpec{target, survivor}
			intent := saved
			intent.Retiring = []tunnel.PeerSpec{survivor}
			intent.Candidates = []tunnel.PeerSpec{target, survivor}
			path := filepath.Join(t.TempDir(), "neighbors.json")
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			must(writeDurableJSON(path, saved))
			must(writeDurableJSON(path+".retiring", intent))
			before, err := os.ReadFile(path)
			must(err)
			switch fault {
			case "identity":
				intent.NodeUID = "replacement"
				must(writeDurableJSON(path+".retiring", intent))
			case "symlink":
				must(os.Remove(path + ".retiring"))
				must(os.Symlink(path, path+".retiring"))
			case "mode":
				must(os.Chmod(path+".retiring", 0644))
			case "corrupt":
				must(os.WriteFile(path+".retiring", []byte("{}"), 0600))
			case "republished":
				current.Peers = append(current.Peers, target)
			}
			sideBefore, err := os.ReadFile(path + ".retiring")
			must(err)
			read := func(name string) receiveNeighborCheckpoint {
				t.Helper()
				raw, err := os.ReadFile(name)
				must(err)
				var value receiveNeighborCheckpoint
				must(json.Unmarshal(raw, &value))
				return value
			}
			calls, checks := 0, 0
			check := func() error {
				checks++
				if (fault == "authority" && checks == 2) || (fault == "late-authority" && checks == 3) {
					return errors.New("authority changed")
				}
				return nil
			}
			native := func(peers []tunnel.PeerSpec) error {
				calls++
				if len(peers) == 0 {
					t.Fatal("lost target ownership")
				}
				for _, peer := range peers {
					if peer.PublicKey != target.PublicKey {
						t.Fatal("selected another owner")
					}
				}
				persisted := read(path + ".retiring")
				found := false
				for _, peer := range persisted.Retiring {
					found = found || peer.PublicKey == target.PublicKey
				}
				if !found {
					t.Fatal("native cleanup before durable target intent")
				}
				if fault == "native" {
					return errors.New("native dependency")
				}
				if fault == "changed" {
					must(os.WriteFile(path, []byte("{}"), 0600))
				}
				return nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if fault == "cancelled" {
				cancel()
			}
			err = retireWithdrawnNeighborCaches(ctx, path, current, []tunnel.ReceiveTransitionPlan{plan}, target.PublicKey, nil, check, native)
			if fault != "none" {
				if err == nil {
					t.Fatal("accepted failure")
				}
				if fault != "changed" {
					raw, e := os.ReadFile(path)
					must(e)
					if string(raw) != string(before) {
						t.Fatal("failure rewrote executable cache")
					}
				}
				if fault == "native" || fault == "authority" || fault == "changed" || fault == "late-authority" {
					retained := read(path + ".retiring")
					found := false
					for _, peer := range retained.Retiring {
						found = found || peer.PublicKey == target.PublicKey
					}
					if !found {
						t.Fatal("failure discarded withdrawal intent")
					}
				} else {
					raw, e := os.ReadFile(path + ".retiring")
					must(e)
					if calls != 0 || string(raw) != string(sideBefore) {
						t.Fatal("invalid input mutated state")
					}
				}
				return
			}
			if calls != 1 {
				t.Fatal("missing native cleanup")
			}
			main, side := read(path), read(path+".retiring")
			if !reflect.DeepEqual(main.Peers, []tunnel.PeerSpec{survivor}) || !reflect.DeepEqual(side.Peers, main.Peers) || !reflect.DeepEqual(side.Retiring, []tunnel.PeerSpec{survivor}) || !reflect.DeepEqual(side.Candidates, []tunnel.PeerSpec{survivor}) {
				t.Fatal("target retained or survivor intent lost")
			}
			if main.BootID != saved.BootID || side.MeshVersion != intent.MeshVersion {
				t.Fatal("invented fresh serving authority")
			}
			must(retireWithdrawnNeighborCaches(ctx, path, current, []tunnel.ReceiveTransitionPlan{plan}, target.PublicKey, nil, check, func(peers []tunnel.PeerSpec) error {
				if len(peers) != 0 {
					t.Fatal("retry resurrected target")
				}
				return nil
			}))
		})
	}
}
