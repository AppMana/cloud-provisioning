package handover

import (
	"context"
	"crypto/sha256"
	"fmt"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
)

// Uses the real API fixture in TestStoreAPIServerCAS. This tests the journal
// fence, not withdrawal authority, participant retirement, inventory release,
// native cleanup or traffic continuity. No elapsed time stands in for receipts.
func verifyCompletedFreeze(t *testing.T, store Store) {
	ctx := context.Background()
	completed := func(t *testing.T, name, direction string) (Store, Snapshot) {
		t.Helper()
		s := store
		s.Name = name
		tr := transportFixture(t)
		plan := []byte(`{"version":1,"transport":"freeze-test"}`)
		spec := view(t, tr).Round.Spec
		spec.PlanHash = fmt.Sprintf("%x", sha256.Sum256(plan))
		tr, err := NewTransition(spec, epoch, tr.j.Policy)
		if err != nil {
			t.Fatal(err)
		}
		current, err := s.CreateWithPlan(ctx, tr, plan)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.FreezeCompleted(ctx, current); err == nil {
			t.Fatal("froze an unfinished transition")
		}
		rolled := false
		for !view(t, current.Transition()).Complete {
			v := view(t, current.Transition())
			at := v.Round.IssuedAt.Add(time.Second)
			var next Transition
			if !rolled && ((direction == "abort" && v.Round.Phase == Prepare) || (direction == "rollback" && v.Round.Phase == Switch)) {
				next, err = current.Transition().Rollback(at)
				if err != nil {
					t.Fatal(err)
				}
				rolled = true
			} else {
				if at.Before(v.DrainUntil) {
					at = v.DrainUntil
				}
				next = step(t, current.Transition(), at)
			}
			current, err = s.Commit(ctx, current, next)
			if err != nil {
				t.Fatal(err)
			}
		}
		return s, current
	}
	successor := func(t *testing.T, s Store, previous Snapshot) (Snapshot, error) {
		t.Helper()
		old := view(t, previous.Transition())
		spec := view(t, transportFixture(t)).Round.Spec
		spec.TransitionUID = "freeze-racing-successor"
		spec.FromGeneration, spec.ToGeneration = old.ServingGeneration, "next-generation"
		plan := []byte(`{"version":2,"transport":"freeze-successor"}`)
		spec.PlanHash = fmt.Sprintf("%x", sha256.Sum256(plan))
		for i, member := range old.Round.Members {
			spec.Members[i].FromKey, spec.Members[i].FromPeerKey = member.ToKey, member.ToPeerKey
			if old.Round.Phase == Abort {
				spec.Members[i].FromKey, spec.Members[i].FromPeerKey = member.FromKey, member.FromPeerKey
			}
		}
		return s.ReplaceCompleted(ctx, previous, spec, plan, epoch.Add(time.Minute), previous.transition.j.Policy)
	}
	for _, direction := range []string{"forward", "abort", "rollback"} {
		t.Run(direction, func(t *testing.T) {
			s, previous := completed(t, "freeze-"+direction, direction)
			frozen, err := s.FreezeCompleted(ctx, previous)
			if err != nil {
				t.Fatal(err)
			}
			if !frozen.Frozen() || previous.Frozen() || frozen.Metadata().UID != previous.Metadata().UID || !reflect.DeepEqual(frozen.object.Data, previous.object.Data) {
				t.Fatal("freeze changed evidence or failed to persist the fence")
			}
			if err := frozen.ContinuesExecution(previous); err == nil {
				t.Fatal("freeze did not fence in-flight execution")
			}
			if err := frozen.Extends(previous); err != nil {
				t.Fatalf("cannot persist the new fence in a checkpoint: %v", err)
			}
			if err := previous.Extends(frozen); err == nil {
				t.Fatal("checkpoint accepted fence regression")
			}
			raw, err := frozen.Checkpoint()
			if err != nil {
				t.Fatal(err)
			}
			restored, err := s.RestoreCheckpoint(raw)
			if err != nil || !restored.Frozen() {
				t.Fatalf("checkpoint lost the write fence: %v", err)
			}
			if _, err := s.FreezeCompleted(ctx, previous); !apierrors.IsConflict(err) {
				t.Fatalf("stale freeze did not conflict: %v", err)
			}
			frozen, err = s.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.FreezeCompleted(ctx, frozen); err != nil {
				t.Fatalf("retry after reloading lost response: %v", err)
			}
			if _, err := successor(t, s, frozen); err == nil {
				t.Fatal("frozen journal admitted a successor")
			}
			// Bypass the new Store checks to exercise the API's enforcement for
			// older writers, using a fresh resource version for each attempt.
			for _, mutation := range []string{"unfreeze", "data"} {
				object, err := s.ConfigMaps.Get(ctx, s.Name, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if mutation == "unfreeze" {
					mutable := false
					object.Immutable = &mutable
				} else {
					object.Data[journalKey] += " "
				}
				if _, err := s.ConfigMaps.Update(ctx, object, metav1.UpdateOptions{}); !apierrors.IsInvalid(err) {
					t.Fatalf("API did not reject %s with a current version: %v", mutation, err)
				}
			}
			if err := s.ConfigMaps.Delete(ctx, s.Name, metav1.DeleteOptions{}); err != nil {
				t.Fatal(err)
			}
			recreated := frozen.object.DeepCopy()
			recreated.UID, recreated.ResourceVersion = "", ""
			if _, err := s.ConfigMaps.Create(ctx, recreated, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.FreezeCompleted(ctx, frozen); err == nil {
				t.Fatal("same-name recreation accepted an old freeze")
			}
		})
	}
	for _, winner := range []string{"freeze", "successor"} {
		t.Run(winner+"-wins", func(t *testing.T) {
			s, previous := completed(t, "freeze-race-"+winner, "forward")
			maps := &freezeRaceMaps{ConfigMapInterface: s.ConfigMaps, name: s.Name}
			racing := s
			racing.ConfigMaps = maps
			if winner == "freeze" {
				maps.beforeUpdate = func() error { _, err := s.FreezeCompleted(ctx, previous); return err }
				if _, err := successor(t, racing, previous); !apierrors.IsConflict(err) {
					t.Fatalf("successor passed a concurrent freeze: %v", err)
				}
			} else {
				maps.beforeUpdate = func() error { _, err := successor(t, s, previous); return err }
				if _, err := racing.FreezeCompleted(ctx, previous); !apierrors.IsConflict(err) {
					t.Fatalf("freeze accepted a concurrent successor: %v", err)
				}
			}
			current, err := s.Load(ctx)
			if err != nil || !maps.fired || current.Frozen() != (winner == "freeze") || view(t, current.Transition()).Complete != (winner == "freeze") {
				t.Fatalf("race did not preserve its winning state: %v", err)
			}
		})
	}
}

type freezeRaceMaps struct {
	typedcore.ConfigMapInterface
	name         string
	beforeUpdate func() error
	fired        bool
}

func (m *freezeRaceMaps) Update(ctx context.Context, object *corev1.ConfigMap, options metav1.UpdateOptions) (*corev1.ConfigMap, error) {
	if object.Name == m.name && !m.fired {
		m.fired = true
		if err := m.beforeUpdate(); err != nil {
			return nil, err
		}
	}
	return m.ConfigMapInterface.Update(ctx, object, options)
}
