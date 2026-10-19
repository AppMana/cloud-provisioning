package attachment

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Uses real-token completion/finalization. Native absence and owner/compatibility
// policy are synthetic; this tests their binding, not a live release rollout.
func verifyFinalizedRelease(t *testing.T, api kubernetes.Interface, store handover.Store, mesh *corev1.Secret, machineUID, inventoryUID string, publication tunnel.ReceiveJournalPublication) {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	changeInventory := func() error {
		index, err := store.ConfigMaps.Get(ctx, tunnel.ReceiveInventoryName(string(mesh.UID)), metav1.GetOptions{})
		if err != nil {
			return err
		}
		index.Annotations = map[string]string{"release-test": time.Now().String()}
		_, err = store.ConfigMaps.Update(ctx, index, metav1.UpdateOptions{})
		return err
	}
	for _, fault := range []string{"missing-check", "owner-before", "owner-at-write", "inventory-at-write", "mesh-at-write", "journal-at-write", "cancel-at-write", "CAS", "changed-policy", "wrong-inventory"} {
		t.Run("release/"+fault, func(t *testing.T) {
			attempt, cancel := context.WithCancel(ctx)
			defer cancel()
			calls := 0
			candidate := store
			age := time.Minute
			uid := inventoryUID
			denied := errors.New("live release policy denied")
			check := func(source ReceiveRetirementSet) error {
				calls++
				if string(source.Metadata.UID) != inventoryUID || source.Withdrawal.MachineUID != machineUID || !reflect.DeepEqual(source.Inventory.Publications[machineUID], publication) {
					t.Fatal("policy received a different source")
				}
				if fault == "owner-before" {
					return denied
				}
				if calls != 2 {
					return nil
				}
				switch fault {
				case "owner-at-write":
					return denied
				case "inventory-at-write":
					return changeInventory()
				case "mesh-at-write":
					current, err := api.CoreV1().Secrets(mesh.Namespace).Get(ctx, mesh.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}
					current.Annotations = map[string]string{"release-test": time.Now().String()}
					_, err = api.CoreV1().Secrets(mesh.Namespace).Update(ctx, current, metav1.UpdateOptions{})
					return err
				case "journal-at-write":
					current, err := store.ConfigMaps.Get(ctx, store.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}
					current.Annotations = map[string]string{"release-test": time.Now().String()}
					_, err = store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{})
					return err
				case "cancel-at-write":
					cancel()
				}
				return nil
			}
			if fault == "missing-check" {
				check = nil
			}
			if fault == "changed-policy" {
				age += time.Second
			}
			if fault == "wrong-inventory" {
				uid = "replaced"
			}
			if fault == "CAS" {
				candidate.ConfigMaps = retirementInventoryWriter{ConfigMapInterface: store.ConfigMaps, before: changeInventory}
			}
			err := ReleaseFinalizedReceiveRetirement(attempt, api, candidate, mesh.Namespace, mesh.Name, string(mesh.UID), machineUID, uid, age, check)
			if err == nil {
				t.Fatal("unsafe release accepted")
			}
			if (fault == "owner-before" || fault == "owner-at-write") && !errors.Is(err, denied) {
				t.Fatal("policy rejection lost", err)
			}
			if fault == "CAS" && !apierrors.IsConflict(err) {
				t.Fatal("CAS conflict lost", err)
			}
			if fault == "cancel-at-write" && !errors.Is(err, context.Canceled) {
				t.Fatal("cancellation lost", err)
			}
			if (fault == "owner-at-write" || fault == "inventory-at-write" || fault == "mesh-at-write" || fault == "journal-at-write" || fault == "cancel-at-write" || fault == "CAS") && calls != 2 {
				t.Fatal("fault missed write boundary", calls, err)
			}
			inventory, _, readErr := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, mesh.Namespace, mesh.Name, string(mesh.UID))
			must(readErr)
			if len(inventory.Released) != 0 || inventory.CheckJournal(machineUID, publication.JournalUID) != nil || !reflect.DeepEqual(inventory.Publications[machineUID], publication) {
				t.Fatal("failed release changed active ownership")
			}
		})
	}
	calls := 0
	check := func(source ReceiveRetirementSet) error {
		calls++
		if !reflect.DeepEqual(source.Inventory.Publications[machineUID], publication) {
			t.Fatal("release changed consumed evidence")
		}
		return nil
	}
	lost := store
	lost.ConfigMaps = retirementInventoryWriter{ConfigMapInterface: store.ConfigMaps, lostResponse: true}
	if err := ReleaseFinalizedReceiveRetirement(ctx, api, lost, mesh.Namespace, mesh.Name, string(mesh.UID), machineUID, inventoryUID, time.Minute, check); err == nil {
		t.Fatal("lost release response concealed")
	}
	if calls != 2 {
		t.Fatal("release policy not checked twice", calls)
	}
	historical, ok, err := ReadReleasedReceiveJournal(ctx, api, store, mesh.Namespace, mesh.Name, string(mesh.UID), machineUID, inventoryUID, time.Minute)
	must(err)
	if !ok || !reflect.DeepEqual(historical.Inventory.Released[machineUID], publication) {
		t.Fatal("lost response has no verifiable released record")
	}
	if err := ReleaseFinalizedReceiveRetirement(ctx, api, store, mesh.Namespace, mesh.Name, string(mesh.UID), machineUID, inventoryUID, time.Minute, check); err == nil {
		t.Fatal("released history reused as active release authority")
	}
	if calls != 2 {
		t.Fatal("already released record reached active policy")
	}
}
