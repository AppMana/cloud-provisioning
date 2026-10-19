package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// Synthetic completed journal and previous-boot observations; no kernel work.
// The marker must remain a replay fence, never become forwarding authority.
func TestReceiveBootLegacyImportFence(t *testing.T) {
	for _, fault := range []string{"valid", "absent", "inventory", "mesh", "node", "key", "base", "pending", "missing-neighbors", "public", "symlink", "unknown", "trailing", "changed-during-native"} {
		t.Run(fault, func(t *testing.T) {
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			root, err := wgtypes.GeneratePrivateKey()
			must(err)
			site, err := wgtypes.GeneratePrivateKey()
			must(err)
			plan := receiveRestoreTestPlan(t, root, site)
			cfg := config{iface: "bootroot", peersFile: filepath.Join(t.TempDir(), "peers.json")}
			inventoryPath, _, _ := writeReceiveBootFixture(t, cfg, root, plan, false)
			raw, err := os.ReadFile(inventoryPath)
			must(err)
			var inventory receiveInventoryCheckpoint
			must(json.Unmarshal(raw, &inventory))
			lock := strings.TrimSuffix(inventoryPath, ".inventory.json")
			neighborPath := lock + ".neighbors.json"
			neighbors := receiveNeighborCheckpoint{Version: 1, InventoryUID: inventory.InventoryUID, MeshUID: plan.MeshUID, MeshVersion: "1", NodeUID: plan.CloudNodeUID, BootID: inventory.BootID, PublicKey: plan.RootPublicKey, Peers: []tunnel.PeerSpec{}}
			if fault == "pending" {
				neighbors.Retiring = []tunnel.PeerSpec{{PublicKey: site.PublicKey().String()}}
			}
			must(writeDurableJSON(neighborPath, neighbors))
			marker := receiveCloudLegacyImport{Version: 1, InventoryUID: inventory.InventoryUID, MeshUID: plan.MeshUID, NodeUID: plan.CloudNodeUID, PublicKey: plan.RootPublicKey, BaseHash: tunnel.HashPeerList(plan.Base)}
			markerPath := neighborPath + ".cloud-legacy-" + marker.BaseHash + ".json"
			switch fault {
			case "inventory":
				marker.InventoryUID = "replacement"
			case "mesh":
				marker.MeshUID = "replacement"
			case "node":
				marker.NodeUID = "replacement"
			case "key":
				marker.PublicKey = site.PublicKey().String()
			case "base":
				marker.BaseHash = "replacement"
			}
			must(writeDurableJSON(markerPath, marker))
			switch fault {
			case "absent":
				must(os.Remove(markerPath))
			case "missing-neighbors":
				must(os.Remove(neighborPath))
			case "public":
				must(os.Chmod(markerPath, 0644))
			case "symlink":
				must(os.Rename(markerPath, markerPath+".original"))
				must(os.Symlink(markerPath+".original", markerPath))
			case "unknown":
				must(writeDurableJSON(neighborPath+".cloud-legacy-untracked.json", marker))
			case "trailing":
				raw, err := os.ReadFile(markerPath)
				must(err)
				must(os.WriteFile(markerPath, append(raw, []byte(" {}")...), 0600))
			}
			original, _ := os.ReadFile(markerPath)
			calls := 0
			handled, err := admitReceiveBoot(context.Background(), cfg, func(state receiveBootState, check func() error) error {
				calls++
				if len(state.Routes.Plans) != 1 || state.Plan.MachineUID != plan.MachineUID {
					t.Fatal("marker altered recovery authority")
				}
				if fault == "changed-during-native" {
					must(os.WriteFile(markerPath, []byte("{}"), 0600))
				}
				return check()
			})
			if !handled {
				t.Fatal("boot admission did not handle retained state")
			}
			valid := fault == "valid" || fault == "absent"
			if valid {
				must(err)
				if calls != 1 {
					t.Fatal("valid fence blocked boot")
				}
			} else if err == nil {
				t.Fatal("unsafe fence admitted")
			}
			if !valid && fault != "changed-during-native" && calls != 0 {
				t.Fatal("unsafe marker reached native recovery")
			}
			if fault == "changed-during-native" && calls != 1 {
				t.Fatal("marker mutation was not exercised")
			}
			after, _ := os.ReadFile(markerPath)
			if fault != "changed-during-native" && string(after) != string(original) {
				t.Fatal("boot changed retained marker")
			}
		})
	}
}
