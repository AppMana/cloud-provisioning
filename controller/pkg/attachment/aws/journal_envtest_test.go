package aws

import (
	"context"
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestAPIJournalRetainsRouteUntilLastLeaseIsReleased(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS for isolated API validation")
	}
	environment := &envtest.Environment{}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "routes"}}); err != nil {
		t.Fatal(err)
	}
	r, e, _, target := routeFixture(t)
	journal := ConfigMapJournal{Client: c, Namespace: "routes"}
	r.Journal = journal
	for _, lease := range []string{"2022", "2025"} {
		if ready, err := r.Acquire(ctx, lease, target); err != nil || !ready {
			t.Fatalf("%v %v", ready, err)
		}
	}
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, journal.key(r.key(target)), cm); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, cm); err != nil {
		t.Fatal(err)
	}
	if ready, err := r.Acquire(ctx, "new", target); err == nil || ready {
		t.Fatal("deleting journal admitted new lease")
	}
	if done, err := r.Release(ctx, "2022", target); err != nil || !done || e.deletes != 0 {
		t.Fatalf("first lease release %v %v", done, err)
	}
	current, err := journal.Load(ctx, r.key(target))
	if err != nil || !current.Leases["2025"] || !current.Retiring {
		t.Fatal("shared retiring intent disappeared")
	}
	if done, err := r.Release(ctx, "2025", target); err != nil || !done || e.deletes != 1 {
		t.Fatalf("final lease release %v %v", done, err)
	}
	if err := c.Get(ctx, journal.key(r.key(target)), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Fatalf("released route retained finalizer: %v", err)
	}
}
