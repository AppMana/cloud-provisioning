package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
)

func TestBootNeighborCheckpointAdmission(t *testing.T) {
	for _, fault := range []string{"valid", "identity", "pending", "candidate", "withdrawn", "corrupt", "public", "symlink"} {
		t.Run(fault, func(t *testing.T) {
			lock := filepath.Join(t.TempDir(), "native.lock")
			path := lock + ".neighbors.json.retiring"
			inventory := receiveInventoryCheckpoint{InventoryUID: "inventory", NodeUID: "node", PublicKey: "own", Inventory: tunnel.ReceiveInventory{MeshUID: "mesh"}}
			saved := receiveNeighborCheckpoint{Version: 1, InventoryUID: "inventory", MeshUID: "mesh", NodeUID: "node", PublicKey: "own", BootID: "previous", Peers: []tunnel.PeerSpec{{PublicKey: "survivor"}}}
			switch fault {
			case "identity":
				saved.NodeUID = "replacement"
			case "pending":
				saved.Retiring = []tunnel.PeerSpec{{PublicKey: "pending"}}
			case "candidate":
				saved.Candidates = []tunnel.PeerSpec{{PublicKey: "pending"}}
			case "withdrawn":
				saved.Peers[0].PublicKey = "withdrawn"
			}
			if err := writeDurableJSON(path, saved); err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "corrupt":
				if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			case "public":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(path, path+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".original", path); err != nil {
					t.Fatal(err)
				}
			}
			files, err := readReceiveBootNeighborFiles(lock, inventory, map[string]string{"retired": "withdrawn"})
			if fault == "valid" {
				if err != nil || len(files) != 1 || len(files[path]) == 0 {
					t.Fatal("completed neighbor checkpoint rejected", err)
				}
			} else if err == nil {
				t.Fatal("unsafe neighbor checkpoint admitted")
			}
		})
	}
}
