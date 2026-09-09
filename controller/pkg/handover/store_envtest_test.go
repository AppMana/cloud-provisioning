package handover

import (
	"context"
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
	s := Store{ConfigMaps: client.CoreV1().ConfigMaps("handover-test"), Name: "transition", ClusterUID: "cluster-uid"}
	tr := transitionFixture(t)
	original, e := s.Create(ctx, tr)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := s.Create(ctx, tr); !apierrors.IsAlreadyExists(e) {
		t.Fatalf("create overwrote state: %v", e)
	}
	candidate := step(t, tr, epoch.Add(time.Second))
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
	if e = s.ConfigMaps.Delete(ctx, s.Name, metav1.DeleteOptions{}); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Create(ctx, tr); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Commit(ctx, committed, advanced); e == nil {
		t.Fatal("deleted/recreated journal accepted stale UID")
	}
}
