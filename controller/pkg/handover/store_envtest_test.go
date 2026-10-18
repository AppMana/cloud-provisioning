package handover

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"os"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"testing"
	"time"
)

func TestStoreAPIServerCAS(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS for isolated API validation")
	}
	env := &envtest.Environment{}
	cfg, e := env.Start()
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := env.Stop(); e != nil {
			t.Error(e)
		}
	})
	client, e := kubernetes.NewForConfig(cfg)
	if e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	_, e = client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "handover-test"}}, metav1.CreateOptions{})
	if e != nil {
		t.Fatal(e)
	}
	for _, model := range []string{"generation", "transport", "planned-transport"} {
		s := Store{ConfigMaps: client.CoreV1().ConfigMaps("handover-test"), Name: "transition-" + model, ClusterUID: "cluster-uid"}
		tr := transitionFixture(t)
		if model != "generation" {
			tr = transportFixture(t)
		}
		plan := []byte(`{"version":1,"transport":"isolated-test"}`)
		if model == "planned-transport" {
			spec := view(t, tr).Round.Spec
			spec.PlanHash = fmt.Sprintf("%x", sha256.Sum256(plan))
			tr, e = NewTransition(spec, epoch, tr.j.Policy)
			if e != nil {
				t.Fatal(e)
			}
			if _, err := s.CreateWithPlan(ctx, tr, []byte("wrong-plan")); err == nil {
				t.Fatal("mismatched plan was persisted")
			}
			if _, err := s.Load(ctx); !apierrors.IsNotFound(err) {
				t.Fatalf("rejected plan created a journal: %v", err)
			}
		}
		create := func() (Snapshot, error) {
			if model == "planned-transport" {
				return s.CreateWithPlan(ctx, tr, plan)
			}
			return s.Create(ctx, tr)
		}
		original, e := create()
		if e != nil {
			t.Fatal(e)
		}
		if _, e := s.Create(ctx, tr); !apierrors.IsAlreadyExists(e) {
			t.Fatalf("create overwrote state: %v", e)
		}
		candidate := step(t, original.Transition(), epoch.Add(time.Second))
		committed, e := s.Commit(ctx, original, candidate)
		if e != nil {
			t.Fatal(e)
		}
		if _, e := s.Commit(ctx, original, candidate); !apierrors.IsConflict(e) {
			t.Fatalf("stale writer accepted: %v", e)
		}
		loaded, e := s.Load(ctx)
		if e != nil {
			t.Fatal(e)
		}
		if view(t, loaded.Transition()).Round.Phase != Switch {
			t.Fatal("restart lost phase")
		}
		if model == "planned-transport" && !bytes.Equal(loaded.Plan(), plan) {
			t.Fatal("restart lost the exact public plan")
		}

		advanced := step(t, candidate, epoch.Add(2*time.Second))
		if _, e := s.Commit(ctx, original, advanced); e == nil {
			t.Fatal("multiple events committed")
		}
		raw, _ := candidate.Bytes()
		rewritten, e := DecodeTransition(raw)
		if e != nil {
			t.Fatal(e)
		}
		rewritten.j.Policy.DrainInterval = time.Second
		rewritten = step(t, rewritten, epoch.Add(2*time.Second))
		if _, e := s.Commit(ctx, committed, rewritten); e == nil {
			t.Fatal("history rewrite accepted")
		}
		foreign := s
		foreign.ClusterUID = "another-cluster"
		if _, e := foreign.Load(ctx); e == nil {
			t.Fatal("foreign cluster adopted")
		}
		if model == "planned-transport" {
			for _, bad := range []string{"changed", "missing"} {
				object, err := s.ConfigMaps.Get(ctx, s.Name, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if bad == "changed" {
					object.Data[planKey] = "changed-plan"
				} else {
					delete(object.Data, planKey)
				}
				object, err = s.ConfigMaps.Update(ctx, object, metav1.UpdateOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.Load(ctx); err == nil {
					t.Fatalf("journal accepted %s public plan", bad)
				}
				object.Data[planKey] = string(plan)
				if _, err := s.ConfigMaps.Update(ctx, object, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
		}
		if e = s.ConfigMaps.Delete(ctx, s.Name, metav1.DeleteOptions{}); e != nil {
			t.Fatal(e)
		}
		if _, e = create(); e != nil {
			t.Fatal(e)
		}
		if _, e = s.Commit(ctx, committed, advanced); e == nil {
			t.Fatal("deleted/recreated journal accepted stale UID")
		}
	}
	t.Run("completed-archives", func(t *testing.T) {
		verifyCompletedArchives(t, Store{ConfigMaps: client.CoreV1().ConfigMaps("handover-test"), ClusterUID: "cluster-uid"})
	})
	t.Run("completed-freeze", func(t *testing.T) {
		verifyCompletedFreeze(t, Store{ConfigMaps: client.CoreV1().ConfigMaps("handover-test"), ClusterUID: "cluster-uid"})
	})
}

// Real API coverage: completion, immutable bytes, retry after a lost response,
// stale source rejection and archive-name collision. This does not exercise
// native forwarding, generation publication or crash recovery on a host.
func verifyCompletedArchives(t *testing.T, store Store) {
	t.Helper()
	ctx := context.Background()
	for _, direction := range []string{"forward", "abort", "rollback"} {
		t.Run(direction, func(t *testing.T) {
			s := store
			s.Name = "archive-source-" + direction
			tr := transportFixture(t)
			plan := []byte(`{"version":1,"transport":"archive-test"}`)
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
			if _, err := s.ArchiveCompleted(ctx, current); err == nil {
				t.Fatal("archived unfinished transition")
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
			archived, err := s.ArchiveCompleted(ctx, current)
			if err != nil {
				t.Fatal(err)
			}
			again, err := s.ArchiveCompleted(ctx, current)
			if err != nil || again.Metadata().UID != archived.Metadata().UID {
				t.Fatalf("retry changed archive: %v", err)
			}
			if !bytes.Equal(archived.Plan(), current.Plan()) || archived.object.Data[journalKey] != current.object.Data[journalKey] {
				t.Fatal("archive lost exact state")
			}
			changed := archived.object.DeepCopy()
			changed.Data[planKey] = "replacement"
			if _, err := s.ConfigMaps.Update(ctx, changed, metav1.UpdateOptions{}); err == nil {
				t.Fatal("API accepted archive data mutation")
			}
			live, err := s.Load(ctx)
			if err != nil || live.Metadata().ResourceVersion != current.Metadata().ResourceVersion {
				t.Fatalf("archival changed source: %v", err)
			}
			object := live.object.DeepCopy()
			object.Annotations = map[string]string{"changed": "true"}
			if _, err := s.ConfigMaps.Update(ctx, object, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ArchiveCompleted(ctx, current); err == nil {
				t.Fatal("accepted stale source")
			}
			if err := s.ConfigMaps.Delete(ctx, archived.Metadata().Name, metav1.DeleteOptions{}); err != nil {
				t.Fatal(err)
			}
			collision := archived.object.DeepCopy()
			collision.UID, collision.ResourceVersion = "", ""
			collision.Data[planKey] = "foreign-plan"
			live, err = s.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			collision.Annotations["handover.cloud-provisioning.io/source-resource-version"] = live.Metadata().ResourceVersion
			if _, err := s.ConfigMaps.Create(ctx, collision, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ArchiveCompleted(ctx, live); err == nil {
				t.Fatal("accepted foreign archive")
			}
			if err := s.ConfigMaps.Delete(ctx, archived.Metadata().Name, metav1.DeleteOptions{}); err != nil {
				t.Fatal(err)
			}
			verifyStoredSuccessor(t, s, live)
		})
	}
}
