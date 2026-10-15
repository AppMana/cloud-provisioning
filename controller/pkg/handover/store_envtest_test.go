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
}
