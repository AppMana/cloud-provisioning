package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
)

// PublishReceiveJournal opens a prepared journal to execution only after its
// exact API UID is committed. Drop the seed in the same update: it must never
// be used to recreate a journal that participants could already have executed.
func PublishReceiveJournal(ctx context.Context, maps typedcore.ConfigMapInterface, clusterUID string, mesh *corev1.Secret, snapshot handover.Snapshot, now time.Time) (handover.Snapshot, error) {
	fail := func(err error) (handover.Snapshot, error) { return handover.Snapshot{}, err }
	plan, err := DecodeReceiveTransition(snapshot, clusterUID)
	if err != nil {
		return fail(err)
	}
	inventory, object, err := ReadReceiveInventory(ctx, maps, clusterUID, mesh.Namespace, mesh.Name, string(mesh.UID))
	if err != nil {
		return fail(err)
	}
	if plan.MeshUID != inventory.MeshUID || plan.MeshName != inventory.MeshName || plan.Namespace != inventory.Namespace {
		return fail(fmt.Errorf("publication scope differs"))
	}
	publication, found := inventory.Publications[plan.MachineUID]
	if !found || publication.JournalUID != "" {
		return snapshot, inventory.CheckJournal(plan.MachineUID, string(snapshot.Metadata().UID))
	}
	if !bytes.Equal(publication.Plan, snapshot.Plan()) {
		return fail(fmt.Errorf("journal differs from reserved preparation"))
	}
	view, err := snapshot.View()
	if err != nil {
		return fail(err)
	}
	if view.Complete || view.Round.Phase != handover.Prepare || view.RollbackRequested || len(view.Observed) != 0 {
		return fail(fmt.Errorf("unpublished journal has already progressed"))
	}
	store := handover.Store{ConfigMaps: maps, ClusterUID: clusterUID, Name: snapshot.Metadata().Name, ExpectedUID: string(snapshot.Metadata().UID)}
	current, err := store.Load(ctx)
	if err != nil {
		return fail(err)
	}
	if current.Metadata().ResourceVersion != snapshot.Metadata().ResourceVersion {
		return fail(fmt.Errorf("journal changed before publication"))
	}
	if now.Before(view.Round.IssuedAt) {
		return fail(fmt.Errorf("unpublished journal is not yet valid"))
	}
	if !now.Before(view.Round.ExpiresAt) {
		renewed, err := current.Transition().Renew(now)
		if err != nil {
			return fail(err)
		}
		current, err = store.Commit(ctx, current, renewed)
		if err != nil {
			return fail(err)
		}
	}
	inventory.Publications[plan.MachineUID] = ReceiveJournalPublication{JournalUID: string(snapshot.Metadata().UID)}
	raw, err := json.Marshal(inventory)
	if err != nil {
		return fail(err)
	}
	object.Data[receiveInventoryKey] = string(raw)
	_, err = maps.Update(ctx, object, metav1.UpdateOptions{})
	if err != nil {
		return fail(err)
	}
	return current, nil
}
