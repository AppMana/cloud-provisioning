package handover

import (
	"context"
	"fmt"
	"reflect"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Frozen reports the API-enforced write fence on this exact journal. It is
// neither a native retirement acknowledgement nor permission to release its
// inventory reservation. Immutable archives also report true.
func (s Snapshot) Frozen() bool {
	return s.object != nil && s.object.Immutable != nil && *s.object.Immutable
}

// FreezeCompleted prevents further generations in the current incarnation.
// Kubernetes makes the journal's data immutable in the same resource-version
// compare-and-swap as this fence, including for writers unaware of retirement.
// A concurrent successor wins or loses that CAS; it cannot pass the fence with
// a stale snapshot. Reload after a conflict or lost response. Never unfreeze by
// deleting/recreating the journal: participants still own its exact UID.
//
// The caller must establish withdrawal authority separately. Freezing alone
// does not stop native maintenance, retire pending devices, acknowledge any
// participant, remove discovery entries or authorize address reuse.
func (s Store) FreezeCompleted(ctx context.Context, previous Snapshot) (Snapshot, error) {
	fail := func(err error) (Snapshot, error) { return Snapshot{}, err }
	if err := s.configured(); err != nil {
		return fail(err)
	}
	if previous.object == nil || previous.object.DeletionTimestamp != nil {
		return fail(fmt.Errorf("freezing requires a live journal snapshot"))
	}
	checked, err := s.snapshot(previous.object)
	if err != nil {
		return fail(err)
	}
	view, err := checked.Transition().View()
	if err != nil {
		return fail(err)
	}
	if !view.Complete || view.ServingGeneration == "" {
		return fail(fmt.Errorf("freezing requires verified transition completion"))
	}
	object := previous.object.DeepCopy()
	immutable := true
	object.Immutable = &immutable
	// Update even on an idempotent retry: the API must still check the supplied
	// UID and resourceVersion rather than returning an obsolete local snapshot.
	stored, err := s.ConfigMaps.Update(ctx, object, metav1.UpdateOptions{})
	if err != nil {
		return fail(err)
	}
	if stored.UID != object.UID || stored.DeletionTimestamp != nil || stored.Immutable == nil || !*stored.Immutable ||
		!reflect.DeepEqual(stored.Data, object.Data) || !reflect.DeepEqual(stored.BinaryData, object.BinaryData) {
		return fail(fmt.Errorf("persisted freeze differs from the completed journal"))
	}
	return s.snapshot(stored)
}
