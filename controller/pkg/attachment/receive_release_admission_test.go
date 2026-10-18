package attachment

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Reuse actual-token completion/finalization evidence. Release admission and
// native observations are synthetic. Historical readback after unrelated mesh
// changes does not establish current owner absence, safe reuse or reboot recovery.
func verifyReleasedJournalAdmission(t *testing.T, api kubernetes.Interface, store handover.Store, mesh *corev1.Secret, machineUID, inventoryUID string) {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	read := func() (ReceiveReleasedJournal, bool, error) {
		return ReadReleasedReceiveJournal(ctx, api, store, mesh.Namespace, mesh.Name, string(mesh.UID), machineUID, inventoryUID, time.Minute)
	}
	absent, ok, err := read()
	must(err)
	if ok || !reflect.DeepEqual(absent, ReceiveReleasedJournal{}) {
		t.Fatal("active finalization treated as released")
	}
	source, ok, err := ReadReceiveRetirementFinalization(ctx, api, store, mesh.Namespace, mesh.Name, string(mesh.UID), machineUID, inventoryUID, time.Minute)
	must(err)
	if !ok {
		t.Fatal("missing authenticated finalization fixture")
	}
	publication := source.Inventory.Publications[machineUID]
	must(tunnel.ReleaseReceiveJournal(ctx, store.ConfigMaps, store.ClusterUID, source.Metadata, source.Mesh, source.Journals[machineUID], publication, func() error { return nil }))
	historical, ok, err := read()
	must(err)
	if !ok || slices.Contains(historical.Inventory.MachineUIDs, machineUID) || historical.Inventory.Publications[machineUID].JournalUID != "" || !reflect.DeepEqual(historical.Inventory.Released[machineUID], publication) {
		t.Fatal("historical reader exposed active ownership or lost evidence")
	}
	must(historical.Recheck(ctx, api, store))
	// A subsequent allocation changes mesh data; the old record remains history,
	// not authority to mutate that new owner's publication or network state.
	currentMesh, err := api.CoreV1().Secrets(mesh.Namespace).Get(ctx, mesh.Name, metav1.GetOptions{})
	must(err)
	if currentMesh.Data == nil {
		currentMesh.Data = map[string][]byte{}
	}
	currentMesh.Data[tunnel.PeerPublicKeyPrefix+"later-worker"] = []byte(source.Withdrawal.PublicKey)
	_, err = api.CoreV1().Secrets(mesh.Namespace).Update(ctx, currentMesh, metav1.UpdateOptions{})
	must(err)
	if err := historical.Recheck(ctx, api, store); err == nil {
		t.Fatal("old read admitted a changed mesh revision")
	}
	historical, ok, err = read()
	must(err)
	if !ok {
		t.Fatal("later mesh data erased historical consumption")
	}
	for _, fault := range []string{"inventory-uid", "journal-uid", "age-policy", "consumed-evidence", "withdrawal-history"} {
		t.Run(fault, func(t *testing.T) {
			candidate, uid, age := store, inventoryUID, time.Minute
			if fault == "inventory-uid" {
				uid = "replacement"
			}
			if fault == "journal-uid" {
				candidate.ExpectedUID = "replacement"
			}
			if fault == "age-policy" {
				age += time.Second
			}
			if fault == "consumed-evidence" || fault == "withdrawal-history" {
				inventory, obj, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, mesh.Namespace, mesh.Name, string(mesh.UID))
				must(err)
				original := obj.DeepCopy()
				t.Cleanup(func() {
					current, e := store.ConfigMaps.Get(ctx, original.Name, metav1.GetOptions{})
					if e != nil {
						t.Error(e)
						return
					}
					current.Data = original.Data
					if _, e = store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{}); e != nil {
						t.Error(e)
					}
				})
				p := inventory.Released[machineUID]
				if fault == "consumed-evidence" {
					p.RetirementCompletion = json.RawMessage(`{"changed":true}`)
				} else {
					var w receiveWithdrawalRecord
					must(json.Unmarshal(p.Retirement, &w))
					w.JournalSHA256 = tunnel.HashPeerList([]byte("changed history"))
					p.Retirement, err = json.Marshal(w)
					must(err)
				}
				inventory.Released[machineUID] = p
				raw, err := json.Marshal(inventory)
				must(err)
				obj.Data["inventory.json"] = string(raw)
				_, err = store.ConfigMaps.Update(ctx, obj, metav1.UpdateOptions{})
				must(err)
			}
			result, ok, err := ReadReleasedReceiveJournal(ctx, api, candidate, mesh.Namespace, mesh.Name, string(mesh.UID), machineUID, uid, age)
			if err == nil || ok || !reflect.DeepEqual(result, ReceiveReleasedJournal{}) {
				t.Fatal("changed historical evidence admitted", fault, err)
			}
		})
	}
	// Exact retained journal identity remains mandatory even after release.
	must(store.ConfigMaps.Delete(ctx, store.Name, metav1.DeleteOptions{}))
	result, ok, err := read()
	if err == nil || ok || !reflect.DeepEqual(result, ReceiveReleasedJournal{}) {
		t.Fatal("missing retained journal accepted")
	}
}
