package attachment

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Real tokens, group admission and one actual inventory CAS. Native observations,
// owner absence and rollback qualification are synthetic. This does not qualify
// kernel cleanup, live allocation reuse, reboot or production activation.
func verifyJointRetirementRelease(t *testing.T, api kubernetes.Interface, store handover.Store, mesh *corev1.Secret, machineUID string, policies map[string]handover.PodReceiptAuthenticator, consumed []handover.PodReceipt) {
	t.Helper()
	ctx := context.Background()
	for _, fault := range []string{"partial-prior-release", "missing-check", "historical-age-policy", "zero-observation-age", "longer-window", "missing-member-finalization", "authentication", "incomplete", "consumed-replay", "stale", "second-owner-at-write", "inventory-at-write", "boot-after-authentication", "expires-at-write", "cancel-at-write", "CAS", "lost-response", "none"} {
		t.Run("joint-release/"+fault, func(t *testing.T) {
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
			cohort, err := source.RetirementCohort()
			must(err)
			original, err := store.ConfigMaps.Get(ctx, source.Metadata.Name, metav1.GetOptions{})
			must(err)
			nodeName := ""
			for _, c := range cohort.Consumers {
				if c.NodeUID == consumed[0].Receipt.NodeUID {
					nodeName = c.NodeName
				}
			}
			node, err := api.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
			must(err)
			if fault != "none" {
				// Only isolated fixture state is restored; production release history must
				// never be erased to retry an operation after an ambiguous response.
				t.Cleanup(func() {
					current, e := store.ConfigMaps.Get(ctx, original.Name, metav1.GetOptions{})
					must(e)
					current.Data, current.Annotations = original.Data, original.Annotations
					_, e = store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{})
					must(e)
					currentNode, e := api.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
					must(e)
					currentNode.Status.NodeInfo.BootID = node.Status.NodeInfo.BootID
					_, e = api.CoreV1().Nodes().UpdateStatus(ctx, currentNode, metav1.UpdateOptions{})
					must(e)
				})
			}
			if fault == "partial-prior-release" {
				var partial tunnel.ReceiveInventory
				must(json.Unmarshal([]byte(original.Data["inventory.json"]), &partial))
				uid := cohort.MachineUIDs[0]
				partial.Released = map[string]tunnel.ReceiveJournalPublication{uid: partial.Publications[uid]}
				delete(partial.Publications, uid)
				partial.MachineUIDs = slices.DeleteFunc(partial.MachineUIDs, func(member string) bool { return member == uid })
				raw, e := json.Marshal(partial)
				must(e)
				changed := original.DeepCopy()
				changed.Data["inventory.json"] = string(raw)
				changed, e = store.ConfigMaps.Update(ctx, changed, metav1.UpdateOptions{})
				must(e)
				e = tunnel.ReleaseReceiveJournals(ctx, store.ConfigMaps, store.ClusterUID, changed.ObjectMeta, mesh, source.Journals, source.Inventory.Publications, func() error { t.Fatal("partial group reached write boundary"); return nil })
				if e == nil || !strings.Contains(e.Error(), "partially retired") {
					t.Fatalf("partial group was not rejected: %v", e)
				}
				after, e := store.ConfigMaps.Get(ctx, original.Name, metav1.GetOptions{})
				must(e)
				if after.ResourceVersion != changed.ResourceVersion {
					t.Fatal("partial group rejection changed inventory")
				}
				return
			}
			if fault == "missing-member-finalization" {
				uid := cohort.MachineUIDs[1]
				publication := source.Inventory.Publications[uid]
				publication.RetirementFinalization = nil
				source.Inventory.Publications[uid] = publication
			}
			var oldCompletion receiveRetirementCompletion
			must(json.Unmarshal(source.Inventory.Publications[machineUID].RetirementCompletion, &oldCompletion))
			oldBatch := slices.Clone(consumed)
			for n := range oldBatch {
				found := false
				for _, receipt := range oldCompletion.Receipts {
					if receipt.NodeUID == oldBatch[n].Receipt.NodeUID {
						oldBatch[n].Receipt = receipt
						found = true
					}
				}
				if !found {
					t.Fatal("fixture omitted consumed receipt")
				}
			}
			batch := slices.Clone(oldBatch)
			historicalAge, observationAge := time.Minute, 2*time.Minute
			if fault == "none" {
				observationAge = historicalAge
			}
			if fault == "historical-age-policy" {
				historicalAge += time.Second
			}
			if fault == "zero-observation-age" {
				observationAge = 0
			}
			success := fault == "none" || fault == "longer-window"
			calls := map[string]int{}
			denied := errors.New("joint owner not retired")
			changeInventory := func() error {
				current, e := store.ConfigMaps.Get(ctx, original.Name, metav1.GetOptions{})
				if e != nil {
					return e
				}
				current.Annotations = map[string]string{"joint-release-race": "changed"}
				_, e = store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{})
				return e
			}
			check := func(member ReceiveRetirementSet) error {
				uid := member.Withdrawal.MachineUID
				if !slices.Contains(cohort.MachineUIDs, uid) {
					return errors.New("unadmitted owner reached release policy")
				}
				calls[uid]++
				if uid != cohort.MachineUIDs[1] {
					return nil
				}
				if fault == "boot-after-authentication" && calls[uid] == 2 {
					current, e := api.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
					if e != nil {
						return e
					}
					current.Status.NodeInfo.BootID = "replacement-boot"
					_, e = api.CoreV1().Nodes().UpdateStatus(ctx, current, metav1.UpdateOptions{})
					return e
				}
				if calls[uid] != 3 {
					return nil
				}
				switch fault {
				case "second-owner-at-write":
					return denied
				case "inventory-at-write":
					return changeInventory()
				case "expires-at-write":
					batch[0].Receipt.ObservedAt = time.Now().Add(-2 * observationAge)
				case "cancel-at-write":
					cancel()
				}
				return nil
			}
			if fault == "missing-check" {
				check = nil
			}
			candidate := store
			writes := 0
			candidate.ConfigMaps = retirementInventoryWriter{ConfigMapInterface: store.ConfigMaps, before: func() error { writes++; return nil }}
			if fault == "CAS" {
				candidate.ConfigMaps = retirementInventoryWriter{ConfigMapInterface: store.ConfigMaps, before: func() error {
					writes++
					return tunnel.ReserveReceiveJournal(ctx, store.ConfigMaps, store.ClusterUID, mesh, "unrelated-concurrent-owner", nil)
				}}
			}
			if fault == "lost-response" {
				candidate.ConfigMaps = retirementInventoryWriter{ConfigMapInterface: store.ConfigMaps, before: func() error { writes++; return nil }, lostResponse: true}
			}
			collection, err := source.ReceiptCollectionWithCohortRelease(attempt, api, candidate, policies, historicalAge, observationAge, check)
			if fault == "missing-check" || fault == "missing-member-finalization" || fault == "historical-age-policy" || fault == "zero-observation-age" {
				if err == nil {
					t.Fatal("invalid joint release admitted")
				}
				return
			}
			must(err)
			if collection.MaxAge != observationAge {
				t.Fatal("joint release observation age changed")
			}
			for n := range batch {
				batch[n].Receipt.RoundHash = collection.Digest
				batch[n].Receipt.ObservedAt = time.Now()
			}
			switch fault {
			case "longer-window":
				for n := range batch {
					batch[n].Receipt.ObservedAt = time.Now().Add(-90 * time.Second)
				}
			case "authentication":
				batch[0].BearerToken = "invalid"
			case "incomplete":
				batch = batch[:1]
			case "consumed-replay":
				batch = slices.Clone(oldBatch)
			case "stale":
				batch[0].Receipt.ObservedAt = time.Now().Add(-2 * observationAge)
			}
			if success {
				must(collection.Observe(attempt, batch))
			}
			calls = map[string]int{}
			err = collection.Complete(attempt, batch)
			if success {
				must(err)
			} else if err == nil {
				t.Fatal("unsafe joint release accepted")
			}
			if fault == "second-owner-at-write" && (!errors.Is(err, denied) || calls[cohort.MachineUIDs[1]] != 3) {
				t.Fatal("second owner write boundary not checked", calls, err)
			}
			if fault == "CAS" && !apierrors.IsConflict(err) {
				t.Fatal("group CAS conflict lost", err)
			}
			if fault == "cancel-at-write" && !errors.Is(err, context.Canceled) {
				t.Fatal("group cancellation lost", err)
			}
			if fault == "expires-at-write" && calls[cohort.MachineUIDs[1]] != 3 {
				t.Fatal("group expiry boundary missed", calls, err)
			}
			inventory, _, readErr := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, mesh.Namespace, mesh.Name, string(mesh.UID))
			must(readErr)
			if !success && fault != "lost-response" {
				wantWrites := 0
				if fault == "CAS" {
					wantWrites = 1
				}
				if writes != wantWrites {
					t.Fatalf("unexpected inventory write attempts: %d", writes)
				}
				if len(inventory.Released) != 0 {
					t.Fatal("failed group release partially changed ownership")
				}
				for _, uid := range cohort.MachineUIDs {
					if !reflect.DeepEqual(inventory.Publications[uid], source.Inventory.Publications[uid]) || !slices.Contains(inventory.MachineUIDs, uid) {
						t.Fatal("failed group release lost member evidence")
					}
				}
				if fault == "CAS" && !slices.Contains(inventory.MachineUIDs, "unrelated-concurrent-owner") {
					t.Fatal("conflict lost concurrent reservation")
				}
				return
			}
			if writes != 1 {
				t.Fatalf("group release used %d inventory writes", writes)
			}
			for _, uid := range cohort.MachineUIDs {
				if calls[uid] != 3 {
					t.Fatalf("owner %s missed a release boundary: %d checks", uid, calls[uid])
				}
			}
			if len(inventory.MachineUIDs) != 0 || len(inventory.Publications) != 0 || len(inventory.Released) != len(cohort.MachineUIDs) {
				t.Fatal("release was not all-or-nothing")
			}
			for _, uid := range cohort.MachineUIDs {
				memberStore := store
				memberStore.Name = tunnel.ReceiveTransitionName(uid)
				memberStore.ExpectedUID = source.Inventory.Publications[uid].JournalUID
				released, ok, e := ReadReleasedReceiveJournal(ctx, api, memberStore, mesh.Namespace, mesh.Name, string(mesh.UID), uid, string(source.Metadata.UID), time.Minute)
				must(e)
				if !ok || !reflect.DeepEqual(released.Inventory.Released[uid], source.Inventory.Publications[uid]) {
					t.Fatal("group release lost historical proof")
				}
				if e := tunnel.ReserveReceiveJournal(ctx, store.ConfigMaps, store.ClusterUID, mesh, uid, nil); e == nil {
					t.Fatal("retired owner restored its reservation")
				}
				if _, e := tunnel.PublishReceiveJournal(ctx, store.ConfigMaps, store.ClusterUID, mesh, source.Journals[uid], time.Now()); e == nil {
					t.Fatal("retired journal republished ownership")
				}
			}
			if err := collection.Complete(ctx, batch); err == nil {
				t.Fatal("stale joint collection retried release")
			}
		})
	}
}
