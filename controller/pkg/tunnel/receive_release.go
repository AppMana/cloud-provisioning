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
	plan, err := DecodeReceiveTransition(snapshot, clusterUID)
	if err != nil {
		return err
	}
	return ReleaseReceiveJournals(ctx, maps, clusterUID, expectedInventory, mesh, map[string]handover.Snapshot{plan.MachineUID: snapshot}, map[string]ReceiveJournalPublication{plan.MachineUID: consumed}, beforeWrite)
}

// ReleaseReceiveJournals performs one inventory CAS for an explicitly admitted
// group. It is storage only: each member needs independently verified history,
// owner absence, durable consumer fencing and compatible recovery. The caller
// must recheck the entire group at beforeWrite. No journal is deleted. Partial
// prior releases are rejected; an ambiguous response requires fresh readback.
func ReleaseReceiveJournals(ctx context.Context, maps typedcore.ConfigMapInterface, clusterUID string, expectedInventory metav1.ObjectMeta, mesh *corev1.Secret, snapshots map[string]handover.Snapshot, consumed map[string]ReceiveJournalPublication, beforeWrite func() error) error {
	if maps == nil || mesh == nil || mesh.UID == "" || mesh.ResourceVersion == "" || mesh.DeletionTimestamp != nil || expectedInventory.UID == "" || expectedInventory.ResourceVersion == "" || expectedInventory.DeletionTimestamp != nil || beforeWrite == nil || len(snapshots) == 0 || len(snapshots) != len(consumed) {
		return fmt.Errorf("release requires exact inventory, complete member evidence and a write-boundary authority check")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	inventory, object, err := ReadReceiveInventory(ctx, maps, clusterUID, mesh.Namespace, mesh.Name, string(mesh.UID))
	if err != nil {
		return err
	}
	if object.UID != expectedInventory.UID || object.ResourceVersion != expectedInventory.ResourceVersion || object.Name != expectedInventory.Name || object.Namespace != expectedInventory.Namespace {
		return fmt.Errorf("release inventory revision or scope changed")
	}
	uids := make([]string, 0, len(snapshots))
	for uid := range snapshots {
		uids = append(uids, uid)
	}
	slices.Sort(uids)
	released := 0
	for _, uid := range uids {
		snapshot, publication := snapshots[uid], consumed[uid]
		meta := snapshot.Metadata()
		if uid == "" || !snapshot.RetirementFrozen() || meta.UID == "" || meta.ResourceVersion == "" || meta.DeletionTimestamp != nil || publication.JournalUID != string(meta.UID) || len(publication.Plan) != 0 || len(publication.RetirementFinalization) == 0 {
			return fmt.Errorf("release requires each finalized frozen journal incarnation")
		}
		plan, err := DecodeReceiveTransition(snapshot, clusterUID)
		if err != nil {
			return err
		}
		if plan.MachineUID != uid || plan.Namespace != inventory.Namespace || plan.MeshName != inventory.MeshName || plan.MeshUID != inventory.MeshUID {
			return fmt.Errorf("release journal scope changed")
		}
		if retired, exists := inventory.Released[uid]; exists {
			if !reflect.DeepEqual(retired, publication) {
				return fmt.Errorf("released journal evidence cannot be replaced")
			}
			released++
		} else {
			if err := inventory.CheckJournal(uid, string(meta.UID)); err != nil {
				return err
			}
			if !reflect.DeepEqual(inventory.Publications[uid], publication) {
				return fmt.Errorf("release consumed publication changed")
			}
		}
		store := handover.Store{ConfigMaps: maps, ClusterUID: clusterUID, Name: meta.Name, ExpectedUID: string(meta.UID)}
		current, err := store.Load(ctx)
		if err != nil {
			return err
		}
		if err := current.ContinuesExecution(snapshot); err != nil {
			return fmt.Errorf("release journal changed: %w", err)
		}
	}
	if released != 0 && released != len(uids) {
		return fmt.Errorf("release group is partially retired")
	}
	if inventory.Released == nil {
		inventory.Released = map[string]ReceiveJournalPublication{}
	}
	for _, uid := range uids {
		inventory.Released[uid] = consumed[uid]
		delete(inventory.Publications, uid)
	}
	inventory.MachineUIDs = slices.DeleteFunc(inventory.MachineUIDs, func(uid string) bool { _, found := snapshots[uid]; return found })
	if err := inventory.validate(); err != nil {
		return err
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
