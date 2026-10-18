package handover

import (
	"context"
	"crypto/sha256"
	"fmt"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
)

const journalKey = "transition.json"
const planKey = "plan.json"

// Store must use controller-owned ConfigMaps, inaccessible to receipt writers.
// API resourceVersion and UID protect against concurrent writes and recreation.
// It does not retry conflicts: reload authoritative state before recomputing.
type Store struct {
	ConfigMaps typedcore.ConfigMapInterface
	Name       string
	ClusterUID string
	// ExpectedUID, when set by admission, rejects a journal recreated between
	// validation and an authenticated observation or advancement.
	ExpectedUID     string
	OwnerReferences []metav1.OwnerReference
}
type Snapshot struct {
	transition Transition
	object     *corev1.ConfigMap
}

func (s Snapshot) Transition() Transition { return s.transition }

// ContinuesExecution accepts only unchanged state or appended observations in
// the same journal incarnation. Observations do not change native authority.
// Renewals, phase changes, history rewrites and other object changes still fence
// an in-flight execution. This is not a replacement for write-side CAS or live
// participant, deadline and receipt-incarnation checks.
func (s Snapshot) ContinuesExecution(previous Snapshot) error {
	if s.object == nil || previous.object == nil || s.object.UID == "" || s.object.ResourceVersion == "" || previous.object.ResourceVersion == "" || s.object.DeletionTimestamp != nil || previous.object.DeletionTimestamp != nil {
		return fmt.Errorf("execution requires live journal snapshots")
	}
	a, b := s.object.DeepCopy(), previous.object.DeepCopy()
	a.ResourceVersion, b.ResourceVersion = "", ""
	a.ManagedFields, b.ManagedFields = nil, nil
	a.Data[journalKey] = b.Data[journalKey]
	if !reflect.DeepEqual(a, b) {
		return fmt.Errorf("journal identity, metadata or public plan changed")
	}
	if err := s.transition.Extends(previous.transition); err != nil {
		return err
	}
	for _, event := range s.transition.j.Events[len(previous.transition.j.Events):] {
		if event.Operation != observe {
			return fmt.Errorf("journal execution authority changed")
		}
	}
	return nil
}

// Metadata returns an independent copy for scope and incarnation checks. It
// exposes no mutable reference to the snapshot used by compare-and-swap.
func (s Snapshot) Metadata() metav1.ObjectMeta {
	if s.object == nil {
		return metav1.ObjectMeta{}
	}
	return *s.object.ObjectMeta.DeepCopy()
}

// Plan returns the exact public plan committed with this transition, if any.
func (s Snapshot) Plan() []byte {
	if s.object == nil {
		return nil
	}
	return []byte(s.object.Data[planKey])
}

func (s Store) snapshot(object *corev1.ConfigMap) (Snapshot, error) {
	if object.Name != s.Name || object.UID == "" || object.ResourceVersion == "" {
		return Snapshot{}, fmt.Errorf("invalid journal object identity")
	}
	if s.ExpectedUID != "" && string(object.UID) != s.ExpectedUID {
		return Snapshot{}, fmt.Errorf("journal incarnation differs from admission")
	}
	t, e := DecodeTransition([]byte(object.Data[journalKey]))
	if e != nil {
		return Snapshot{}, e
	}
	v, e := t.View()
	if e != nil {
		return Snapshot{}, e
	}
	if v.Round.ClusterUID != s.ClusterUID {
		return Snapshot{}, fmt.Errorf("journal belongs to another cluster")
	}
	if t.j.PlanRequired && object.Data[planKey] == "" {
		return Snapshot{}, fmt.Errorf("journal public plan is missing")
	}
	if plan, present := object.Data[planKey]; present {
		if plan == "" || fmt.Sprintf("%x", sha256.Sum256([]byte(plan))) != v.Round.PlanHash {
			return Snapshot{}, fmt.Errorf("journal public plan does not match its committed hash")
		}
	}
	return Snapshot{transition: t, object: object.DeepCopy()}, nil
}
func (s Store) configured() error {
	if s.ConfigMaps == nil || !token(s.Name) || !token(s.ClusterUID) {
		return fmt.Errorf("invalid journal store configuration")
	}
	return nil
}
func (s Store) Create(ctx context.Context, t Transition) (Snapshot, error) {
	return s.create(ctx, t, nil)
}

// CreateWithPlan commits the public plan and initial round atomically. The
// plan remains part of the versioned object throughout all phase transitions.
func (s Store) CreateWithPlan(ctx context.Context, t Transition, plan []byte) (Snapshot, error) {
	if len(plan) == 0 {
		return Snapshot{}, fmt.Errorf("public handover plan is required")
	}
	v, err := t.View()
	if err != nil {
		return Snapshot{}, err
	}
	if fmt.Sprintf("%x", sha256.Sum256(plan)) != v.Round.PlanHash {
		return Snapshot{}, fmt.Errorf("public handover plan hash differs")
	}
	t.j.PlanRequired = true
	return s.create(ctx, t, plan)
}
func (s Store) create(ctx context.Context, t Transition, plan []byte) (Snapshot, error) {
	if e := s.configured(); e != nil {
		return Snapshot{}, e
	}
	if t.j.PlanRequired && len(plan) == 0 {
		return Snapshot{}, fmt.Errorf("journal requires its public plan")
	}
	raw, e := t.Bytes()
	if e != nil {
		return Snapshot{}, e
	}
	if len(t.j.Events) != 0 || t.j.Initial.ClusterUID != s.ClusterUID {
		return Snapshot{}, fmt.Errorf("journal creation requires a new transition for this cluster")
	}
	data := map[string]string{journalKey: string(raw)}
	if plan != nil {
		data[planKey] = string(plan)
	}
	object, e := s.ConfigMaps.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: s.Name, OwnerReferences: append([]metav1.OwnerReference(nil), s.OwnerReferences...)}, Data: data}, metav1.CreateOptions{})
	if e != nil {
		return Snapshot{}, e
	}
	if object.Data[journalKey] != string(raw) || (plan != nil && object.Data[planKey] != string(plan)) {
		return Snapshot{}, fmt.Errorf("persisted journal differs from requested state")
	}
	return s.snapshot(object)
}
func (s Store) Load(ctx context.Context) (Snapshot, error) {
	if e := s.configured(); e != nil {
		return Snapshot{}, e
	}
	object, e := s.ConfigMaps.Get(ctx, s.Name, metav1.GetOptions{})
	if e != nil {
		return Snapshot{}, e
	}
	return s.snapshot(object)
}

// Commit accepts exactly one validated successor event. Callers must publish
// only the returned persisted snapshot, never the uncommitted candidate.
func (s Store) Commit(ctx context.Context, previous Snapshot, next Transition) (Snapshot, error) {
	if e := s.configured(); e != nil {
		return Snapshot{}, e
	}
	if previous.object == nil {
		return Snapshot{}, fmt.Errorf("missing journal snapshot")
	}
	checked, e := s.snapshot(previous.object)
	if e != nil {
		return Snapshot{}, e
	}
	if checked.Frozen() {
		return Snapshot{}, fmt.Errorf("frozen journal cannot append an event")
	}
	if len(next.j.Events) != len(checked.transition.j.Events)+1 {
		return Snapshot{}, fmt.Errorf("commit requires exactly one successor event")
	}
	if e := next.Extends(checked.transition); e != nil {
		return Snapshot{}, e
	}
	raw, e := next.Bytes()
	if e != nil {
		return Snapshot{}, e
	}
	object := previous.object.DeepCopy()
	if object.Data == nil {
		object.Data = map[string]string{}
	}
	object.Data[journalKey] = string(raw)
	updated, e := s.ConfigMaps.Update(ctx, object, metav1.UpdateOptions{})
	if e != nil {
		return Snapshot{}, e
	}
	if updated.Data[journalKey] != string(raw) || updated.Data[planKey] != previous.object.Data[planKey] {
		return Snapshot{}, fmt.Errorf("persisted journal differs from requested state")
	}
	return s.snapshot(updated)
}
