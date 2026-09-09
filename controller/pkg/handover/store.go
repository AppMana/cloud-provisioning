package handover

import (
	"bytes"
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
)

const journalKey = "transition.json"

// Store must use controller-owned ConfigMaps, inaccessible to receipt writers.
// API resourceVersion and UID protect against concurrent writes and recreation.
// It does not retry conflicts: reload authoritative state before recomputing.
type Store struct {
	ConfigMaps typedcore.ConfigMapInterface
	Name       string
	ClusterUID string
}
type Snapshot struct {
	transition Transition
	object     *corev1.ConfigMap
}

func (s Snapshot) Transition() Transition { return s.transition }

func (s Store) snapshot(object *corev1.ConfigMap) (Snapshot, error) {
	if object.Name != s.Name || object.UID == "" || object.ResourceVersion == "" {
		return Snapshot{}, fmt.Errorf("invalid journal object identity")
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
	return Snapshot{transition: t, object: object.DeepCopy()}, nil
}
func (s Store) configured() error {
	if s.ConfigMaps == nil || !token(s.Name) || !token(s.ClusterUID) {
		return fmt.Errorf("invalid journal store configuration")
	}
	return nil
}
func (s Store) Create(ctx context.Context, t Transition) (Snapshot, error) {
	if e := s.configured(); e != nil {
		return Snapshot{}, e
	}
	raw, e := t.Bytes()
	if e != nil {
		return Snapshot{}, e
	}
	if len(t.j.Events) != 0 || t.j.Initial.ClusterUID != s.ClusterUID {
		return Snapshot{}, fmt.Errorf("journal creation requires a new transition for this cluster")
	}
	object, e := s.ConfigMaps.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: s.Name}, Data: map[string]string{journalKey: string(raw)}}, metav1.CreateOptions{})
	if e != nil {
		return Snapshot{}, e
	}
	if object.Data[journalKey] != string(raw) {
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
	old, e := checked.transition.Bytes()
	if e != nil {
		return Snapshot{}, e
	}
	raw, e := next.Bytes()
	if e != nil {
		return Snapshot{}, e
	}
	if len(next.j.Events) != len(checked.transition.j.Events)+1 {
		return Snapshot{}, fmt.Errorf("commit requires exactly one successor event")
	}
	prefix := next.j
	prefix.Events = prefix.Events[:len(prefix.Events)-1]
	prefixRaw, e := (Transition{prefix}).Bytes()
	if e != nil {
		return Snapshot{}, e
	}
	if !bytes.Equal(old, prefixRaw) {
		return Snapshot{}, fmt.Errorf("candidate rewrites committed transition history")
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
	if updated.Data[journalKey] != string(raw) {
		return Snapshot{}, fmt.Errorf("persisted journal differs from requested state")
	}
	return s.snapshot(updated)
}
