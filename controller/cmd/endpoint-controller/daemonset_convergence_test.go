package main

import (
	"context"
	"os"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// Exercise API defaulting and persisted revisions, which a fake client cannot
// model. Reconciling an admitted agent must not trigger another rollout.
func TestAgentDaemonSetsConvergeAfterImageUpgrade(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("requires isolated Kubernetes API binaries")
	}
	e := &envtest.Environment{}
	cfg, err := e.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.Stop(); err != nil {
			t.Error(err)
		}
	})
	c, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r := windowsPublisherFixture()
	r.Client, r.reader = c, c
	r.secretName = "peers"
	r.dialerDaemonSetName = "site"
	r.dialerImage = "example.invalid/dialer:previous"
	r.dialerPrivateKeyDir = "/var/lib/cloud-provisioning"
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: r.secretNamespace}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		ensure  func(context.Context) error
		upgrade func()
	}{
		{"site", r.ensureDialerDaemonSet, func() { r.dialerImage = "example.invalid/dialer:site-next" }},
		{"remote", r.ensureCloudDialerDaemonSet, func() { r.dialerImage = "example.invalid/dialer:remote-next" }},
		{"remote-windows", r.ensureWindowsPublisherDaemonSet, func() { r.windowsPublisherImage = "example.invalid/publisher:next" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			read := func() *appsv1.DaemonSet {
				t.Helper()
				ds := &appsv1.DaemonSet{}
				if err := c.Get(ctx, client.ObjectKey{Namespace: r.secretNamespace, Name: tc.name}, ds); err != nil {
					t.Fatal(err)
				}
				return ds
			}
			if err := tc.ensure(ctx); err != nil {
				t.Fatal(err)
			}
			before := read()
			tc.upgrade()
			if err := tc.ensure(ctx); err != nil {
				t.Fatal(err)
			}
			after := read()
			if after.Generation != before.Generation+1 || after.Spec.Template.Spec.Containers[0].Image == before.Spec.Template.Spec.Containers[0].Image {
				t.Fatal("image upgrade did not change the admitted template exactly once")
			}
			for i := 0; i < 3; i++ {
				if err := tc.ensure(ctx); err != nil {
					t.Fatal(err)
				}
				current := read()
				if current.Generation != after.Generation || current.ResourceVersion != after.ResourceVersion {
					t.Fatalf("unchanged reconcile wrote a new revision: generation %d -> %d, resourceVersion %s -> %s", after.Generation, current.Generation, after.ResourceVersion, current.ResourceVersion)
				}
			}
		})
	}
}
