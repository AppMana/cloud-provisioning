package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
)

// Synthetic evidence exercises only the durable anti-regression fence. It does
// not authenticate finalization, establish native absence, or qualify reboot
// recovery. Changing BootID models admission after a boot, not a live reboot.
func TestReceiveFinalizationCheckpointFence(t *testing.T) {
	base := receiveInventoryCheckpoint{
		Version: 1, InventoryUID: "inventory", NodeName: "node", NodeUID: "node-uid", BootID: "boot", PublicKey: "key",
		Inventory: tunnel.ReceiveInventory{
			Version: 1, ClusterUID: "cluster", Namespace: "namespace", MeshName: "mesh", MeshUID: "mesh-uid",
			MachineUIDs: []string{"retired", "survivor"},
			Publications: map[string]tunnel.ReceiveJournalPublication{
				"retired":  {JournalUID: "journal", Retirement: json.RawMessage(`{"intent":1}`), RetirementCompletion: json.RawMessage(`{"completion":1}`), RetirementFinalization: json.RawMessage(`{"finalization":1}`)},
				"survivor": {JournalUID: "survivor-journal"},
			},
		},
	}
	for _, boot := range []string{"boot", "next-boot"} {
		for _, fault := range []string{"unchanged", "remove-finalization", "replace-finalization", "remove-completion", "replace-completion", "other-finalization", "initial-finalization", "unfinalized-refresh"} {
			t.Run(boot+"/"+fault, func(t *testing.T) {
				clone := func() receiveInventoryCheckpoint {
					raw, err := json.Marshal(base)
					if err != nil {
						t.Fatal(err)
					}
					var result receiveInventoryCheckpoint
					if err := json.Unmarshal(raw, &result); err != nil {
						t.Fatal(err)
					}
					return result
				}
				saved, next := clone(), clone()
				next.BootID = boot
				p := next.Inventory.Publications["retired"]
				rejected := true
				switch fault {
				case "unchanged":
					rejected = false
				case "remove-finalization":
					p.RetirementFinalization = nil
				case "replace-finalization":
					p.RetirementFinalization = json.RawMessage(`{"finalization":2}`)
				case "remove-completion":
					p.RetirementCompletion = nil
				case "replace-completion":
					p.RetirementCompletion = json.RawMessage(`{"completion":2}`)
				case "other-finalization":
					other := next.Inventory.Publications["survivor"]
					other.Retirement = json.RawMessage(`{"intent":2}`)
					other.RetirementCompletion = json.RawMessage(`{"completion":2}`)
					other.RetirementFinalization = json.RawMessage(`{"finalization":2}`)
					next.Inventory.Publications["survivor"] = other
					rejected = false
				case "initial-finalization", "unfinalized-refresh":
					prior := saved.Inventory.Publications["retired"]
					prior.RetirementFinalization = nil
					saved.Inventory.Publications["retired"] = prior
					if fault == "unfinalized-refresh" {
						p.RetirementFinalization = nil
						p.RetirementCompletion = json.RawMessage(`{"completion":2}`)
					}
					rejected = false
				}
				next.Inventory.Publications["retired"] = p
				path := filepath.Join(t.TempDir(), "inventory.json")
				if err := writeDurableJSON(path, saved); err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				err = validateReceiveInventoryCheckpoint(path, next)
				if rejected {
					if err == nil || !strings.Contains(err.Error(), "finalization") {
						t.Fatalf("finalized evidence regression accepted: %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				after, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if string(before) != string(after) {
					t.Fatal("admission mutated the saved fence")
				}
			})
		}
	}
}
