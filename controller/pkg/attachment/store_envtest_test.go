package attachment

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

type concurrentUpdate struct {
	client.Client
	race bool
}

func (c *concurrentUpdate) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if c.race {
		c.race = false
		other := &corev1.ConfigMap{}
		if err := c.Client.Get(ctx, client.ObjectKeyFromObject(obj), other); err != nil {
			return err
		}
		other.Annotations = map[string]string{"concurrent-writer": "observed"}
		if err := c.Client.Update(ctx, other); err != nil {
			return err
		}
	}
	return c.Client.Update(ctx, obj, opts...)
}

// Real API concurrency and finalizer behavior cannot be established by the
// fake client. This starts dedicated API/etcd processes, never a user cluster.
func TestAPIProtectsAttachmentRetirementIntent(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS for isolated API validation")
	}
	env := &envtest.Environment{}
	config, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
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
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "attachments"}}); err != nil {
		t.Fatal(err)
	}
	racing := &concurrentUpdate{Client: c}
	store := ConfigMapStore{Client: racing, Namespace: "attachments"}
	b := &backend{forwarding: true, applied: true, withdrawn: true, released: true}
	r := Reconciler{Store: store, Forwarder: b, Publication: b}
	desired := observedRequest()
	if _, err := r.Step(ctx, "worker", &desired); err != nil {
		t.Fatal(err)
	}
	old, err := store.Load(ctx, "worker")
	if err != nil {
		t.Fatal(err)
	}
	next := *old
	next.Phase = Withdrawing
	racing.race = true
	if err := store.Save(ctx, old, &next); !apierrors.IsConflict(err) {
		t.Fatalf("expected real API conflict, got %v", err)
	}
	current, err := store.Load(ctx, "worker")
	if err != nil || current.Phase != Preparing {
		t.Fatal("stale writer changed durable intent")
	}
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, store.key("worker"), cm); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, cm); err != nil {
		t.Fatal(err)
	}
	retained, err := store.Load(ctx, "worker")
	if err != nil || retained == nil || !retained.Deleting {
		t.Fatal("active intent disappeared on deletion")
	}
	desired.Worker.UID = ""
	for _, want := range []Phase{Withdrawing, Releasing, Complete} {
		got, err := r.Step(ctx, "worker", &desired)
		if err != nil || got != want {
			t.Fatalf("%s %v, want %s", got, err, want)
		}
	}
	if err := c.Get(ctx, store.key("worker"), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Fatalf("released attachment retained finalizer: %v", err)
	}
	if !reflectCalls(b.calls, []string{"withdraw", "release"}) {
		t.Fatalf("unexpected external operations %v", b.calls)
	}
}

func reflectCalls(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
