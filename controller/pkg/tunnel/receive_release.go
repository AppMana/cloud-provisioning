package tunnel

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
)

// ReleaseReceiveJournal is a storage transition, not release admission. The
// caller must verify finalization, durable consumer fences, compatible recovery
// and current owner absence before calling, and recheck live release authority
// at beforeWrite. No live reconciler may invoke this until that protocol and
// compatible rollback are qualified. The journal itself is never deleted.
//
// Move the exact consumed publication into permanent retired-owner storage and
// remove its active reservation in one inventory CAS. A lost response requires
// fresh admission; an identical retry preserves the retired record. Neither a
// conflict nor an ambiguous response is retried internally.
func ReleaseReceiveJournal(ctx context.Context, maps typedcore.ConfigMapInterface, clusterUID string, expectedInventory metav1.ObjectMeta, mesh *corev1.Secret, snapshot handover.Snapshot, consumed ReceiveJournalPublication, beforeWrite func() error) error {
	if maps == nil || mesh == nil || mesh.UID == "" || mesh.ResourceVersion == "" || mesh.DeletionTimestamp != nil || expectedInventory.UID == "" || expectedInventory.ResourceVersion == "" || expectedInventory.DeletionTimestamp != nil || beforeWrite == nil {
		return fmt.Errorf("release requires exact inventory, live mesh and a write-boundary authority check")
	}
	meta := snapshot.Metadata()
	if !snapshot.Frozen() || meta.UID == "" || meta.ResourceVersion == "" || meta.DeletionTimestamp != nil || consumed.JournalUID != string(meta.UID) || len(consumed.Plan) != 0 || len(consumed.RetirementFinalization) == 0 {
		return fmt.Errorf("release requires a finalized frozen journal incarnation")
	}
	plan, err := DecodeReceiveTransition(snapshot, clusterUID)
	if err != nil {
		return err
	}
	view, err := snapshot.Transition().View()
	if err != nil || !view.Complete || view.ServingGeneration == "" {
		return fmt.Errorf("release requires terminal transport history")
	}
	inventory, object, err := ReadReceiveInventory(ctx, maps, clusterUID, mesh.Namespace, mesh.Name, string(mesh.UID))
	if err != nil {
		return err
	}
	if object.UID != expectedInventory.UID || object.ResourceVersion != expectedInventory.ResourceVersion || object.Name != expectedInventory.Name || object.Namespace != expectedInventory.Namespace || plan.Namespace != inventory.Namespace || plan.MeshName != inventory.MeshName || plan.MeshUID != inventory.MeshUID {
		return fmt.Errorf("release inventory revision or scope changed")
	}
	if retired, exists := inventory.Released[plan.MachineUID]; exists {
		if !reflect.DeepEqual(retired, consumed) {
			return fmt.Errorf("released journal evidence cannot be replaced")
		}
	} else {
		if err := inventory.CheckJournal(plan.MachineUID, string(meta.UID)); err != nil {
			return err
		}
		if !reflect.DeepEqual(inventory.Publications[plan.MachineUID], consumed) {
			return fmt.Errorf("release consumed publication changed")
		}
		if inventory.Released == nil {
			inventory.Released = map[string]ReceiveJournalPublication{}
		}
		inventory.Released[plan.MachineUID] = consumed
		delete(inventory.Publications, plan.MachineUID)
		inventory.MachineUIDs = slices.DeleteFunc(inventory.MachineUIDs, func(uid string) bool { return uid == plan.MachineUID })
	}
	if err := inventory.validate(); err != nil {
		return err
	}
	store := handover.Store{ConfigMaps: maps, ClusterUID: clusterUID, Name: meta.Name, ExpectedUID: string(meta.UID)}
	current, err := store.Load(ctx)
	if err != nil {
		return err
	}
	if err := current.ContinuesExecution(snapshot); err != nil {
		return fmt.Errorf("release journal changed: %w", err)
	}
	raw, err := json.Marshal(inventory)
	if err != nil {
		return err
	}
	object.Data[receiveInventoryKey] = string(raw)
	if err := beforeWrite(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	stored, err := maps.Update(ctx, object, metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	if stored.UID != object.UID || stored.DeletionTimestamp != nil || stored.Data[receiveInventoryKey] != string(raw) {
		return fmt.Errorf("persisted release differs from admitted evidence")
	}
	return nil
}
