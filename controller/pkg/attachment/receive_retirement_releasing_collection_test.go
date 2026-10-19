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

// Real API storage and bound service-account tokens; native observations and
// owner/rollout policy are synthetic. This does not qualify live release,
// connectivity, durable host recovery, RBAC propagation or version rollback.
func verifyReleasingRetirementCollection(t *testing.T, api kubernetes.Interface, store handover.Store, mesh *corev1.Secret, machineUID string, policy handover.PodReceiptAuthenticator, consumed handover.PodReceipt) {
	t.Helper()
	ctx := context.Background()
	for _, fault := range []string{"missing-finalization", "missing-check", "age-policy", "authentication", "image-policy", "consumed-replay", "incomplete", "stale", "owner-at-write", "inventory-at-write", "boot-after-authentication", "expires-at-write", "cancel-at-write", "CAS", "lost-response", "none"} {
		t.Run("releasing-collection/"+fault, func(t *testing.T) {
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			attempt, cancel := context.WithCancel(ctx)
			defer cancel()
			source, err := ReadReceiveRetirementSet(ctx, api, store, mesh.Namespace, mesh.Name, string(mesh.UID), machineUID)
			must(err)
			original, err := store.ConfigMaps.Get(ctx, source.Metadata.Name, metav1.GetOptions{})
			must(err)
			nodeName := source.Withdrawal.Consumers[0].NodeName
			originalNode, err := api.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
			must(err)
			// Restore only isolated fixture state so each case starts from the same
			// historical finalization. Production must never erase release history.
			t.Cleanup(func() {
				current, err := store.ConfigMaps.Get(ctx, original.Name, metav1.GetOptions{})
				must(err)
				current.Data, current.Annotations = original.Data, original.Annotations
				_, err = store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{})
				must(err)
				node, err := api.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
				must(err)
				node.Status.NodeInfo.BootID = originalNode.Status.NodeInfo.BootID
				_, err = api.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{})
				must(err)
			})
			view, err := source.Journals[machineUID].Transition().View()
			must(err)
			age := view.Round.MaxReceiptAge
			if fault == "age-policy" {
				age += time.Second
			}
			if fault == "missing-finalization" {
				p := source.Inventory.Publications[machineUID]
				p.RetirementFinalization = nil
				source.Inventory.Publications[machineUID] = p
			}
			calls := 0
			entries := []handover.PodReceipt{consumed}
			denied := errors.New("release owner policy denied")
			changeInventory := func() error {
				current, err := store.ConfigMaps.Get(ctx, original.Name, metav1.GetOptions{})
				if err != nil {
					return err
				}
				current.Annotations = map[string]string{"release-race": "changed"}
				_, err = store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{})
				return err
			}
			check := func() error {
				calls++
				if fault == "boot-after-authentication" && calls == 2 {
					current, err := api.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
					if err != nil {
						return err
					}
					current.Status.NodeInfo.BootID = "replacement-boot"
					_, err = api.CoreV1().Nodes().UpdateStatus(ctx, current, metav1.UpdateOptions{})
					return err
				}
				if calls != 3 {
					return nil
				}
				switch fault {
				case "owner-at-write":
					return denied
				case "inventory-at-write":
					return changeInventory()
				case "expires-at-write":
					entries[0].Receipt.ObservedAt = time.Now().Add(-2 * age)
				case "cancel-at-write":
					cancel()
				}
				return nil
			}
			if fault == "missing-check" {
				check = nil
			}
			candidate := store
			if fault == "CAS" {
				candidate.ConfigMaps = retirementInventoryWriter{ConfigMapInterface: store.ConfigMaps, before: changeInventory}
			}
			if fault == "lost-response" {
				candidate.ConfigMaps = retirementInventoryWriter{ConfigMapInterface: store.ConfigMaps, lostResponse: true}
			}
			currentPolicy := policy
			if fault == "image-policy" {
				currentPolicy.ImageID = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			}
			collection, err := source.ReceiptCollectionWithRelease(attempt, api, candidate, map[string]handover.PodReceiptAuthenticator{consumed.Receipt.NodeUID: currentPolicy}, age, check)
			if fault == "missing-finalization" || fault == "missing-check" || fault == "age-policy" {
				if err == nil {
					t.Fatal("invalid release collection admitted")
				}
				return
			}
			must(err)
			if collection.Digest == consumed.Receipt.RoundHash {
				t.Fatal("finalized release retained consumed receipt binding")
			}
			entries[0].Receipt.RoundHash, entries[0].Receipt.ObservedAt = collection.Digest, time.Now()
			switch fault {
			case "authentication":
				entries[0].BearerToken = "invalid-token"
			case "consumed-replay":
				entries[0] = consumed
			case "incomplete":
				entries = nil
			case "stale":
				entries[0].Receipt.ObservedAt = time.Now().Add(-2 * age)
			}
			if fault == "none" {
				must(collection.Observe(attempt, entries))
			}
			calls = 0
			err = collection.Complete(attempt, entries)
			if fault == "none" {
				must(err)
			} else if err == nil {
				t.Fatal("unsafe release accepted")
			}
			if fault == "owner-at-write" && (!errors.Is(err, denied) || calls != 3) {
				t.Fatal("owner write boundary missed", calls, err)
			}
			if fault == "CAS" && !apierrors.IsConflict(err) {
				t.Fatal("CAS conflict lost", err)
			}
			if fault == "cancel-at-write" && !errors.Is(err, context.Canceled) {
				t.Fatal("cancellation lost", err)
			}
			if fault == "expires-at-write" && calls != 3 {
				t.Fatal("expiry write boundary missed", calls, err)
			}
			inventory, _, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, mesh.Namespace, mesh.Name, string(mesh.UID))
			must(err)
			publication := source.Inventory.Publications[machineUID]
			if fault != "none" && fault != "lost-response" {
				if len(inventory.Released) != 0 || !reflect.DeepEqual(inventory.Publications[machineUID], publication) {
					t.Fatal("failed release changed ownership or historical evidence")
				}
				return
			}
			historical, verified, err := ReadReleasedReceiveJournal(ctx, api, store, mesh.Namespace, mesh.Name, string(mesh.UID), machineUID, string(source.Metadata.UID), age)
			must(err)
			if !verified || !reflect.DeepEqual(historical.Inventory.Released[machineUID], publication) {
				t.Fatal("release lost historical evidence")
			}
			if err := collection.Complete(ctx, entries); err == nil {
				t.Fatal("stale collection released twice")
			}
		})
	}
}
