package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
)

// ReserveReceiveRetirement persists withdrawal intent in the existing discovery
// inventory without releasing its journal, destinations or listener reservations.
// The withdrawal owner must validate the intent and its live source separately.
// This storage operation alone grants no native execution authority.
//
// expectedInventoryUID must come from the caller's admitted inventory. Both the
// journal and index retain their original incarnations. A conflicting writer or
// ambiguous response requires reloading; never replace an existing intent with a
// new recipient list because some consumers have since disappeared.
func ReserveReceiveRetirement(ctx context.Context, maps typedcore.ConfigMapInterface, clusterUID, expectedInventoryUID string, mesh *corev1.Secret, snapshot handover.Snapshot, intent json.RawMessage) error {
	if maps == nil || mesh == nil || expectedInventoryUID == "" || mesh.UID == "" || mesh.ResourceVersion == "" || mesh.DeletionTimestamp != nil {
		return fmt.Errorf("retirement requires live storage and admitted inventory identity")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, intent); err != nil || compact.Len() == 0 || compact.Bytes()[0] != '{' {
		return fmt.Errorf("retirement requires an object intent")
	}
	meta := snapshot.Metadata()
	if !snapshot.Frozen() || meta.UID == "" || meta.ResourceVersion == "" || meta.DeletionTimestamp != nil {
		return fmt.Errorf("retirement requires a frozen journal incarnation")
	}
	plan, err := DecodeReceiveTransition(snapshot, clusterUID)
	if err != nil {
		return err
	}
	view, err := snapshot.Transition().View()
	if err != nil || !view.Complete || view.ServingGeneration == "" {
		return fmt.Errorf("retirement requires a completed journal")
	}
	inventory, object, err := ReadReceiveInventory(ctx, maps, clusterUID, mesh.Namespace, mesh.Name, string(mesh.UID))
	if err != nil {
		return err
	}
	if string(object.UID) != expectedInventoryUID || plan.Namespace != inventory.Namespace || plan.MeshName != inventory.MeshName || plan.MeshUID != inventory.MeshUID {
		return fmt.Errorf("retirement inventory identity changed")
	}
	if err := inventory.CheckJournal(plan.MachineUID, string(meta.UID)); err != nil {
		return err
	}
	publication := inventory.Publications[plan.MachineUID]
	if publication.JournalUID != string(meta.UID) {
		return fmt.Errorf("retirement requires explicitly published journal identity")
	}
	if len(publication.Retirement) > 0 && !bytes.Equal(publication.Retirement, compact.Bytes()) {
		return fmt.Errorf("retirement intent already exists with different contents")
	}
	store := handover.Store{ConfigMaps: maps, ClusterUID: clusterUID, Name: meta.Name, ExpectedUID: string(meta.UID)}
	current, err := store.Load(ctx)
	if err != nil {
		return err
	}
	if err := current.ContinuesExecution(snapshot); err != nil {
		return fmt.Errorf("retirement journal changed: %w", err)
	}
	publication.Retirement = bytes.Clone(compact.Bytes())
	inventory.Publications[plan.MachineUID] = publication
	raw, err := json.Marshal(inventory)
	if err != nil {
		return err
	}
	object.Data[receiveInventoryKey] = string(raw)
	// Even retries perform CAS: a stale acknowledgement of storage must not
	// conceal an index replacement or another inventory writer.
	stored, err := maps.Update(ctx, object, metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	if stored.UID != object.UID || stored.DeletionTimestamp != nil || stored.Data[receiveInventoryKey] != string(raw) {
		return fmt.Errorf("persisted retirement differs from admitted intent")
	}
	return nil
}

// RecordReceiveRetirementCompletion is storage only. The controller must first
// authenticate the complete retained consumer batch and re-admit all live source
// state. No agent credentials belong in completion. This operation retains every
// journal and allocation reservation; it never treats completion as release.
//
// expectedInventory pins the revision used for authentication. Even an identical
// retry needs a fresh admitted revision after an ambiguous response. Conflicts
// are returned without retry, and existing evidence cannot be replaced.
func RecordReceiveRetirementCompletion(ctx context.Context, maps typedcore.ConfigMapInterface, clusterUID string, expectedInventory metav1.ObjectMeta, mesh *corev1.Secret, snapshot handover.Snapshot, intent, completion json.RawMessage) error {
	return writeReceiveRetirementCompletion(ctx, maps, clusterUID, expectedInventory, mesh, snapshot, intent, retirementEvidenceWrite{record: completion})
}

// RefreshReceiveRetirementCompletion replaces only the exact observed evidence
// after the controller has authenticated a new complete consumer batch. The old
// record and inventory revision are both compare-and-swap preconditions. No
// participant, intent, journal or allocation reservation is removed.
func RefreshReceiveRetirementCompletion(ctx context.Context, maps typedcore.ConfigMapInterface, clusterUID string, expectedInventory metav1.ObjectMeta, mesh *corev1.Secret, snapshot handover.Snapshot, intent, previous, completion json.RawMessage) error {
	if len(previous) == 0 {
		return fmt.Errorf("completion refresh requires previous evidence")
	}
	return writeReceiveRetirementCompletion(ctx, maps, clusterUID, expectedInventory, mesh, snapshot, intent, retirementEvidenceWrite{previous: previous, record: completion})
}

// RecordReceiveRetirementFinalization is storage only, not a release permit.
// The caller must verify completion and supply live owner, consumer, source and
// freshness checks at beforeWrite. Both the consumed completion and inventory
// revision are exact preconditions. Identical retries require freshly admitted
// metadata; neither a different finalization nor a later completion can replace
// finalized evidence. Every reservation and retired journal remains retained.
func RecordReceiveRetirementFinalization(ctx context.Context, maps typedcore.ConfigMapInterface, clusterUID string, expectedInventory metav1.ObjectMeta, mesh *corev1.Secret, snapshot handover.Snapshot, intent, completion, finalization json.RawMessage, beforeWrite func() error) error {
	if len(completion) == 0 || beforeWrite == nil {
		return fmt.Errorf("finalization requires consumed evidence and a live write-boundary check")
	}
	return writeReceiveRetirementCompletion(ctx, maps, clusterUID, expectedInventory, mesh, snapshot, intent, retirementEvidenceWrite{previous: completion, record: finalization, finalize: true, beforeWrite: beforeWrite})
}

type retirementEvidenceWrite struct {
	previous, record json.RawMessage
	finalize         bool
	beforeWrite      func() error
}

func writeReceiveRetirementCompletion(ctx context.Context, maps typedcore.ConfigMapInterface, clusterUID string, expectedInventory metav1.ObjectMeta, mesh *corev1.Secret, snapshot handover.Snapshot, intent json.RawMessage, write retirementEvidenceWrite) error {
	if maps == nil || mesh == nil || mesh.UID == "" || mesh.ResourceVersion == "" || mesh.DeletionTimestamp != nil || expectedInventory.UID == "" || expectedInventory.ResourceVersion == "" || expectedInventory.DeletionTimestamp != nil {
		return fmt.Errorf("completion requires exact live inventory and mesh revisions")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, write.record); err != nil || compact.Len() == 0 || compact.Bytes()[0] != '{' {
		return fmt.Errorf("completion requires an object evidence record")
	}
	meta := snapshot.Metadata()
	if !snapshot.Frozen() || meta.UID == "" || meta.ResourceVersion == "" || meta.DeletionTimestamp != nil {
		return fmt.Errorf("completion requires a frozen journal incarnation")
	}
	plan, err := DecodeReceiveTransition(snapshot, clusterUID)
	if err != nil {
		return err
	}
	view, err := snapshot.Transition().View()
	if err != nil || !view.Complete || view.ServingGeneration == "" {
		return fmt.Errorf("completion requires terminal transport history")
	}
	inventory, object, err := ReadReceiveInventory(ctx, maps, clusterUID, mesh.Namespace, mesh.Name, string(mesh.UID))
	if err != nil {
		return err
	}
	if object.UID != expectedInventory.UID || object.ResourceVersion != expectedInventory.ResourceVersion || object.Name != expectedInventory.Name || object.Namespace != expectedInventory.Namespace || plan.Namespace != inventory.Namespace || plan.MeshName != inventory.MeshName || plan.MeshUID != inventory.MeshUID {
		return fmt.Errorf("completion inventory revision or scope changed")
	}
	if err := inventory.CheckJournal(plan.MachineUID, string(meta.UID)); err != nil {
		return err
	}
	publication := inventory.Publications[plan.MachineUID]
	if publication.JournalUID != string(meta.UID) || len(publication.Retirement) == 0 || !bytes.Equal(publication.Retirement, intent) {
		return fmt.Errorf("completion withdrawal intent changed")
	}
	if len(publication.RetirementFinalization) > 0 && (!write.finalize || !bytes.Equal(publication.RetirementFinalization, compact.Bytes())) {
		return fmt.Errorf("finalized retirement evidence cannot be replaced")
	}
	if len(write.previous) > 0 {
		if !bytes.Equal(publication.RetirementCompletion, write.previous) {
			return fmt.Errorf("completion refresh evidence changed")
		}
	} else if len(publication.RetirementCompletion) > 0 && !bytes.Equal(publication.RetirementCompletion, compact.Bytes()) {
		return fmt.Errorf("completion evidence already exists with different contents")
	}
	store := handover.Store{ConfigMaps: maps, ClusterUID: clusterUID, Name: meta.Name, ExpectedUID: string(meta.UID)}
	current, err := store.Load(ctx)
	if err != nil {
		return err
	}
	if err := current.ContinuesExecution(snapshot); err != nil {
		return fmt.Errorf("completion journal changed: %w", err)
	}
	if write.finalize {
		publication.RetirementFinalization = bytes.Clone(compact.Bytes())
	} else {
		publication.RetirementCompletion = bytes.Clone(compact.Bytes())
	}
	inventory.Publications[plan.MachineUID] = publication
	raw, err := json.Marshal(inventory)
	if err != nil {
		return err
	}
	object.Data[receiveInventoryKey] = string(raw)
	if write.beforeWrite != nil {
		if err := write.beforeWrite(); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	stored, err := maps.Update(ctx, object, metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	if stored.UID != object.UID || stored.DeletionTimestamp != nil || stored.Data[receiveInventoryKey] != string(raw) {
		return fmt.Errorf("persisted completion differs from admitted evidence")
	}
	return nil
}
