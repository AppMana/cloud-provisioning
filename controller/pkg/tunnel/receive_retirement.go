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
