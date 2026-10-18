package attachment

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// Scope hashing only: the sibling evidence is opaque and synthetic. Full
// source admission, token authentication and live retirement are separate.
func TestRetirementScopeHashSurvivesSiblingCompletion(t *testing.T) {
	ctx := context.Background()
	w, snapshot, inventory, mesh := retirementFixture(t, "forward", true)
	history, err := snapshot.Transition().Bytes()
	if err != nil {
		t.Fatal(err)
	}
	retirement, err := json.Marshal(receiveWithdrawalRecord{Version: 1, JournalSHA256: tunnel.HashPeerList(history), Withdrawal: w})
	if err != nil {
		t.Fatal(err)
	}
	publication := inventory.Publications[w.MachineUID]
	publication.Retirement = retirement
	inventory.Publications[w.MachineUID] = publication
	inventory.MachineUIDs = append(inventory.MachineUIDs, "sibling")
	inventory.Publications["sibling"] = tunnel.ReceiveJournalPublication{JournalUID: "sibling-journal", Retirement: json.RawMessage(`{"intent":"retained"}`)}
	raw, err := json.Marshal(inventory)
	if err != nil {
		t.Fatal(err)
	}
	object := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: tunnel.ReceiveInventoryName(string(mesh.UID)), Namespace: mesh.Namespace, UID: "inventory", ResourceVersion: "1"}, Data: map[string]string{"inventory.json": string(raw)}}
	api := fake.NewClientset(object)
	maps := api.CoreV1().ConfigMaps(mesh.Namespace)
	before, err := NewReceiveRetirementReceiptScope(ctx, maps, "cluster", "inventory", snapshot, mesh)
	if err != nil {
		t.Fatal(err)
	}
	sibling := inventory.Publications["sibling"]
	sibling.RetirementCompletion = json.RawMessage(`{"evidence":"new"}`)
	inventory.Publications["sibling"] = sibling
	raw, err = json.Marshal(inventory)
	if err != nil {
		t.Fatal(err)
	}
	object.Data["inventory.json"] = string(raw)
	object.ResourceVersion = "2"
	if _, err := maps.Update(ctx, object, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	after, err := NewReceiveRetirementReceiptScope(ctx, maps, "cluster", "inventory", snapshot, mesh)
	if err != nil {
		t.Fatal(err)
	}
	if before.inventoryHash != after.inventoryHash {
		t.Fatal("sibling completion changed retirement authority hash")
	}
	// Completion evidence is not withdrawal authority. A sibling finishing
	// must not reset collection; the final write still pins its live revision.
	if before.Digest() != after.Digest() {
		t.Fatal("sibling completion invalidated in-flight submissions")
	}
}

func TestRetirementInventoryHashKeepsAuthorityAndLegacySemantics(t *testing.T) {
	inventory := tunnel.ReceiveInventory{Version: 1, ClusterUID: "cluster", Namespace: "test", MeshName: "mesh", MeshUID: "mesh-uid", MachineUIDs: []string{"one", "two"}, Publications: map[string]tunnel.ReceiveJournalPublication{
		"one": {JournalUID: "journal-one", Retirement: json.RawMessage(`{"intent":"one"}`), RetirementCompletion: json.RawMessage(`{"proof":"one"}`)},
		"two": {JournalUID: "journal-two", Retirement: json.RawMessage(`{"intent":"two"}`), RetirementCompletion: json.RawMessage(`{"proof":"two"}`)},
	}}
	original, _ := json.Marshal(inventory)
	hash := func(v tunnel.ReceiveInventory, version int) string {
		t.Helper()
		h, e := receiveRetirementInventoryHash(v, "one", version)
		if e != nil {
			t.Fatal(e)
		}
		return h
	}
	old, newHash := hash(inventory, 2), hash(inventory, 3)
	if hash(inventory, 4) != newHash {
		t.Fatal("current evidence changed inventory authority semantics")
	}
	for _, change := range []string{"own-evidence", "sibling-evidence", "sibling-withdrawal", "sibling-journal", "membership", "mesh", "cluster", "namespace", "pending-plan"} {
		t.Run(change, func(t *testing.T) {
			var copy tunnel.ReceiveInventory
			if err := json.Unmarshal(original, &copy); err != nil {
				t.Fatal(err)
			}
			sibling := copy.Publications["two"]
			switch change {
			case "own-evidence":
				p := copy.Publications["one"]
				p.RetirementCompletion = json.RawMessage(`{"proof":"refreshed"}`)
				copy.Publications["one"] = p
			case "sibling-evidence":
				sibling.RetirementCompletion = json.RawMessage(`{"proof":"refreshed"}`)
			case "sibling-withdrawal":
				sibling.Retirement = json.RawMessage(`{"intent":"changed"}`)
			case "sibling-journal":
				sibling.JournalUID = "replacement"
			case "membership":
				copy.MachineUIDs = copy.MachineUIDs[:1]
				delete(copy.Publications, "two")
			case "mesh":
				copy.MeshUID = "replacement"
			case "cluster":
				copy.ClusterUID = "replacement"
			case "namespace":
				copy.Namespace = "replacement"
			case "pending-plan":
				sibling.Plan = json.RawMessage(`{"plan":"pending"}`)
			}
			if change != "membership" {
				copy.Publications["two"] = sibling
			}
			equivalent := change == "own-evidence" || change == "sibling-evidence"
			for _, version := range []int{3, 4} {
				if (hash(copy, version) == newHash) != equivalent {
					t.Fatal("completion exclusion changed authority semantics")
				}
			}
			if (hash(copy, 2) == old) != (change == "own-evidence") {
				t.Fatal("legacy hash semantics changed")
			}
		})
	}
	after, _ := json.Marshal(inventory)
	if string(original) != string(after) {
		t.Fatal("hashing mutated source")
	}
	if _, err := receiveRetirementInventoryHash(inventory, "one", 5); err == nil {
		t.Fatal("unknown completion version admitted")
	}
}
