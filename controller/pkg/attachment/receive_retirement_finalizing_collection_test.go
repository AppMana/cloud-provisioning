package attachment

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Real API/token fixture, synthetic native observations. This establishes
// authenticated completion followed by conditional finalization, not orphan
// discovery, native cleanup, live timing, mixed-version rollout or release.
func verifyFinalizingRetirementCollection(t *testing.T, api kubernetes.Interface, store handover.Store, mesh *corev1.Secret, machineUID string, policy handover.PodReceiptAuthenticator, submission handover.PodReceipt) {
	t.Helper()
	ctx := context.Background()
	for _, fault := range []string{"policy", "authentication", "owner-after-completion", "none"} {
		t.Run("finalizing-collection/"+fault, func(t *testing.T) {
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			source, err := ReadReceiveRetirementSet(ctx, api, store, mesh.Namespace, mesh.Name, string(mesh.UID), machineUID)
			must(err)
			original, err := store.ConfigMaps.Get(ctx, source.Metadata.Name, metav1.GetOptions{})
			must(err)
			// Restore only this isolated fixture's data so subsequent cases exercise
			// first finalization independently; production never erases finalization.
			t.Cleanup(func() {
				current, err := store.ConfigMaps.Get(ctx, original.Name, metav1.GetOptions{})
				if err != nil {
					t.Error(err)
					return
				}
				current.Data = original.Data
				if _, err := store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{}); err != nil {
					t.Error(err)
				}
			})
			view, err := source.Journals[machineUID].Transition().View()
			must(err)
			age := view.Round.MaxReceiptAge
			if fault == "policy" {
				age += time.Second
			}
			denied := errors.New("owner reappeared after completion")
			rejectedOwner := false
			check := func() error {
				if fault != "owner-after-completion" {
					return nil
				}
				current, _, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, mesh.Namespace, mesh.Name, string(mesh.UID))
				if err != nil {
					return err
				}
				if !bytes.Equal(current.Publications[machineUID].RetirementCompletion, source.Inventory.Publications[machineUID].RetirementCompletion) {
					rejectedOwner = true
					return denied
				}
				return nil
			}
			collection, err := source.ReceiptCollectionWithFinalization(ctx, api, store, map[string]handover.PodReceiptAuthenticator{submission.Receipt.NodeUID: policy}, age, check)
			if fault == "policy" {
				if err == nil {
					t.Fatal("mismatched policy admitted")
				}
				return
			}
			must(err)
			entry := submission
			entry.Receipt.RoundHash = collection.Digest
			entry.Receipt.ObservedAt = time.Now()
			if fault == "authentication" {
				entry.BearerToken = "invalid-token"
			}
			err = collection.Complete(ctx, []handover.PodReceipt{entry})
			if fault == "none" {
				must(err)
			} else if err == nil {
				t.Fatal("failed admission finalized")
			}
			if fault == "owner-after-completion" && (!rejectedOwner || !errors.Is(err, denied)) {
				t.Fatal("post-completion owner race not exercised", err)
			}
			current, _, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, mesh.Namespace, mesh.Name, string(mesh.UID))
			must(err)
			if fault != "none" {
				if len(current.Publications[machineUID].RetirementFinalization) != 0 {
					t.Fatal("failure persisted finalization")
				}
				return
			}
			_, verified, err := ReadReceiveRetirementFinalization(ctx, api, store, mesh.Namespace, mesh.Name, string(mesh.UID), machineUID, string(source.Metadata.UID), age)
			must(err)
			if !verified || len(current.MachineUIDs) != len(source.Inventory.MachineUIDs) {
				t.Fatal("finalization missing or reservations changed")
			}
			if err := collection.Complete(ctx, []handover.PodReceipt{entry}); err == nil {
				t.Fatal("consumed completion replay accepted")
			}
			verifyReleasingRetirementCollection(t, api, store, mesh, machineUID, policy, entry)
		})
	}
}
