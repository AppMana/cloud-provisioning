package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
)

// Synthetic retained evidence tests the durable anti-resurrection fence across
// modeled boot identities. It does not admit active-to-released transitions,
// authenticate release, or qualify real reboot, native absence or address reuse.
func TestReceiveReleasedOwnershipCheckpointFence(t *testing.T) {
	publication := tunnel.ReceiveJournalPublication{JournalUID: "retired-journal", Retirement: json.RawMessage(`{"intent":1}`), RetirementCompletion: json.RawMessage(`{"completion":1}`), RetirementFinalization: json.RawMessage(`{"finalization":1}`)}
	base := receiveInventoryCheckpoint{Version: 1, InventoryUID: "inventory", NodeName: "node", NodeUID: "node-uid", BootID: "boot", PublicKey: "key", Inventory: tunnel.ReceiveInventory{Version: 1, ClusterUID: "cluster", Namespace: "test", MeshName: "mesh", MeshUID: "mesh-uid", MachineUIDs: []string{"survivor"}, Publications: map[string]tunnel.ReceiveJournalPublication{"survivor": {JournalUID: "surviving-journal"}}, Released: map[string]tunnel.ReceiveJournalPublication{"retired": publication}}}
	for _, boot := range []string{"boot", "next-boot"} {
		for _, fault := range []string{"unchanged", "drop", "journal", "intent", "completion", "finalization", "resurrect-reservation", "resurrect-publication", "corrupt-saved", "additional-retired-owner", "shrink-with-retired-record"} {
			t.Run(boot+"/"+fault, func(t *testing.T) {
				clone := func() receiveInventoryCheckpoint {
					raw, err := json.Marshal(base)
					if err != nil {
						t.Fatal(err)
					}
					var x receiveInventoryCheckpoint
					if err := json.Unmarshal(raw, &x); err != nil {
						t.Fatal(err)
					}
					return x
				}
				saved, next := clone(), clone()
				next.BootID = boot
				p := next.Inventory.Released["retired"]
				switch fault {
				case "drop":
					delete(next.Inventory.Released, "retired")
				case "journal":
					p.JournalUID = "replacement"
				case "intent":
					p.Retirement = json.RawMessage(`{"intent":2}`)
				case "completion":
					p.RetirementCompletion = json.RawMessage(`{"completion":2}`)
				case "finalization":
					p.RetirementFinalization = json.RawMessage(`{"finalization":2}`)
				case "resurrect-reservation":
					next.Inventory.MachineUIDs = []string{"retired", "survivor"}
				case "resurrect-publication":
					next.Inventory.Publications["retired"] = publication
				case "corrupt-saved":
					broken := saved.Inventory.Released["retired"]
					broken.RetirementFinalization = nil
					saved.Inventory.Released["retired"] = broken
					p = broken
				case "additional-retired-owner":
					next.Inventory.Released["other-retired"] = publication
				case "shrink-with-retired-record":
					saved.Inventory.MachineUIDs = []string{"retired", "survivor"}
					saved.Inventory.Publications["retired"] = publication
					delete(saved.Inventory.Released, "retired")
				}
				if fault != "drop" {
					next.Inventory.Released["retired"] = p
				}
				path := filepath.Join(t.TempDir(), "inventory.json")
				if err := writeDurableJSON(path, saved); err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				err = validateReceiveInventoryCheckpoint(path, next)
				if fault == "unchanged" || fault == "additional-retired-owner" {
					if err != nil {
						t.Fatal(err)
					}
				} else {
					if err == nil {
						t.Fatal("retired ownership fence was bypassed")
					}
					if fault != "shrink-with-retired-record" && !strings.Contains(err.Error(), "released receiver") {
						t.Fatal("wrong rejection", err)
					}
				}
				after, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if string(before) != string(after) {
					t.Fatal("validation rewrote saved evidence")
				}
			})
		}
	}
}
