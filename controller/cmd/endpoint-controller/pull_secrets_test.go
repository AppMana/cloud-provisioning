package main

import (
	"context"
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Reconciliation must retain repository-specific credentials for every agent
// platform, including rotation and removal. Fake clients do not prove registry
// authorization or that kubelet successfully pulls the published images.
func TestAgentPullSecretsReconcileCompleteList(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := windowsPublisherFixture()
	r.Client, r.reader = c, c
	r.dialerDaemonSetName = "site"
	r.dialerCloudDaemonSetName = "remote"
	r.dialerImage = "example.invalid/dialer:test"
	ctx := context.Background()
	for _, tc := range []struct {
		input string
		names []string
	}{
		{"controller-pull,dialer-pull", []string{"controller-pull", "dialer-pull"}},
		{"dialer-next, controller-pull,dialer-next,", []string{"dialer-next", "controller-pull"}},
		{"legacy", []string{"legacy"}},
		{" , ", nil},
	} {
		r.dialerImagePullSecret = tc.input
		for _, ensure := range []func(context.Context) error{r.ensureDialerDaemonSet, r.ensureCloudDialerDaemonSet, r.ensureWindowsPublisherDaemonSet} {
			if err := ensure(ctx); err != nil {
				t.Fatal(err)
			}
		}
		var list appsv1.DaemonSetList
		if err := c.List(ctx, &list, client.InNamespace(r.secretNamespace)); err != nil {
			t.Fatal(err)
		}
		if len(list.Items) != 3 {
			t.Fatalf("got %d agent DaemonSets", len(list.Items))
		}
		for _, ds := range list.Items {
			var names []string
			for _, ref := range ds.Spec.Template.Spec.ImagePullSecrets {
				names = append(names, ref.Name)
			}
			if !reflect.DeepEqual(names, tc.names) {
				t.Fatalf("%s: pull secrets %v, want %v", ds.Name, names, tc.names)
			}
		}
	}
}
