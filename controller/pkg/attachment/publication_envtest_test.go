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

// Real API concurrency and finalizer behavior cannot be established by the
// fake client. This starts dedicated API/etcd processes, never a user cluster.
func TestAPIPublicationRetainsRecipientsUntilRetirement(t *testing.T) {
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

	store := ConfigMapPublicationStore{Client: c, Namespace: "attachments"}
	intent := &PublicationIntent{Lease: "worker/epoch", Consumers: []ConsumerTarget{{NodeName: "worker", NodeUID: "uid", PublicKey: "key"}}}
	if err := store.Save(ctx, nil, intent); err != nil {
		t.Fatal(err)
	}
	old, err := store.Load(ctx, intent.Lease)
	if err != nil {
		t.Fatal(err)
	}
	shortened := *old
	shortened.Consumers = nil
	if err := store.Save(ctx, old, &shortened); err == nil {
		t.Fatal("discarded required consumer")
	}
	expanded := *old
	expanded.Consumers = append(append([]ConsumerTarget(nil), old.Consumers...), ConsumerTarget{NodeName: "replacement", NodeUID: "new-uid", PublicKey: "new-key"})
	if err := store.Save(ctx, old, &expanded); err != nil {
		t.Fatalf("recording added recipient: %v", err)
	}
	if err := store.Save(ctx, old, &expanded); err == nil {
		t.Fatal("stale writer overwrote recipient history")
	}
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, store.key(intent.Lease), cm); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, cm); err != nil {
		t.Fatal(err)
	}
	current, err := store.Load(ctx, intent.Lease)
	if err != nil || current == nil || !current.Deleting || len(current.Consumers) != 2 {
		t.Fatal("deletion discarded recipient identities")
	}
	retired := *old
	retired.Retired = true
	if err := store.Save(ctx, old, &retired); err == nil {
		t.Fatal("stale writer retired publication")
	}
	retired = *current
	retired.Retired = true
	if err := store.Save(ctx, current, &retired); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, store.key(intent.Lease), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Fatalf("retired publication retained finalizer %v", err)
	}
}
