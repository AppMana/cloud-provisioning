package handover

import (
	"context"
	"fmt"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const withdrawalIntentKey = "withdrawal-intent-sha256"

// WithdrawalIntentHash binds an interrupted journal's immutable fence to the
// persisted lifecycle intent. It never indicates transport or cleanup success.
func (s Snapshot) WithdrawalIntentHash() string {
	if s.object == nil {
		return ""
	}
	return s.object.Data[withdrawalIntentKey]
}

// RetirementFrozen accepts completed history or an explicitly fenced
// interruption. Callers must separately authenticate the matching withdrawal,
// journal ancestry and native absence before cleanup or release.
func (s Snapshot) RetirementFrozen() bool {
	if !s.Frozen() {
		return false
	}
	view, err := s.transition.View()
	return err == nil && ((view.Complete && view.ServingGeneration != "") || hexToken(s.WithdrawalIntentHash(), 32))
}

// FreezeExpiredWithdrawal fences an expired, incomplete transition without
// fabricating completion or selecting a serving generation. The caller must
// authenticate the persisted drain intent and pass its canonical SHA256 digest.
// All possible native generations remain cleanup obligations.
func (s Store) FreezeExpiredWithdrawal(ctx context.Context, previous Snapshot, intentHash string, now time.Time) (Snapshot, error) {
	if !hexToken(intentHash, 32) {
		return Snapshot{}, fmt.Errorf("withdrawal requires an exact intent digest")
	}
	if err := s.configured(); err != nil {
		return Snapshot{}, err
	}
	if previous.object == nil || previous.object.DeletionTimestamp != nil {
		return Snapshot{}, fmt.Errorf("withdrawal requires a live journal snapshot")
	}
	checked, err := s.snapshot(previous.object)
	if err != nil {
		return Snapshot{}, err
	}
	view, err := checked.Transition().View()
	if err != nil || view.Complete || now.Before(view.Round.ExpiresAt) {
		return Snapshot{}, fmt.Errorf("withdrawal requires an expired unfinished round")
	}
	if checked.Frozen() && checked.WithdrawalIntentHash() != intentHash {
		return Snapshot{}, fmt.Errorf("withdrawal fence has different lifecycle authority")
	}
	object := previous.object.DeepCopy()
	object.Data[withdrawalIntentKey] = intentHash
	return s.freeze(ctx, object)
}

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
	return s.freeze(ctx, previous.object.DeepCopy())
}

func (s Store) freeze(ctx context.Context, object *corev1.ConfigMap) (Snapshot, error) {
	fail := func(err error) (Snapshot, error) { return Snapshot{}, err }
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
		return fail(fmt.Errorf("persisted freeze differs from the admitted journal"))
	}
	return s.snapshot(stored)
}
