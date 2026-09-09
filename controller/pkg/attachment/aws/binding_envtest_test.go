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

func TestAPIBindingPersistsThroughDeletionAndRestart(t *testing.T) {
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
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "bindings"}}); err != nil {
		t.Fatal(err)
	}

	routes, cloud, _, route := routeFixture(t)
	checks, _, _, gateway := checkFixture(t)
	checks.API = cloud
	cloud.sourceCheck = true
	ingress, _, _, permission := ingressFixture(t)
	store := ConfigMapBindingStore{Client: c, Namespace: "bindings"}
	managed := BoundResources{Store: store, Resources: ForwardingResources{Scope: routes.Scope, Routes: routes, Checks: checks, Ingress: ingress}}
	binding := ForwardingBinding{Lease: "worker/epoch", Scope: routes.Scope, Gateway: gateway, Routes: []RouteTarget{route}, Ingress: []IngressTarget{permission}}
	if ok, err := managed.Ensure(ctx, binding); err != nil || !ok {
		t.Fatalf("setup %v %v", ok, err)
	}
	original, err := store.Load(ctx, binding.Lease)
	if err != nil {
		t.Fatal(err)
	}
	changed := binding
	changed.Gateway.InstanceID = "replacement"
	if _, err := managed.Ensure(ctx, changed); err == nil {
		t.Fatal("accepted changed binding")
	}
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, store.key(binding.Lease), cm); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, cm); err != nil {
		t.Fatal(err)
	}
	if _, err := managed.Ensure(ctx, binding); err == nil {
		t.Fatal("deleting binding acquired resources")
	}
	stale := *original
	stale.Released = true
	if err := store.Save(ctx, original, &stale); err == nil {
		t.Fatal("stale binding removed finalizer")
	}
	// Simulate a controller restart. Cleanup has only the persisted lease ID.
	restarted := BoundResources{Store: store, Resources: managed.Resources}
	if ok, err := restarted.Release(ctx, binding.Lease); err != nil || !ok {
		t.Fatalf("release %v %v", ok, err)
	}
	if err := c.Get(ctx, store.key(binding.Lease), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Fatalf("binding retained after cleanup: %v", err)
	}
	if !cloud.sourceCheck {
		t.Fatal("gateway original setting not restored")
	}
	if ok, err := restarted.Release(ctx, binding.Lease); err != nil || !ok {
		t.Fatal("release not idempotent")
	}
}
