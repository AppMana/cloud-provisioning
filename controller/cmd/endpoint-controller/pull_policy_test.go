package main

import (
	"context"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
)

func TestDialerPullPolicyReconcilesBothDaemonSets(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &meshReconciler{Client: c, reader: c, secretNamespace: "test", secretName: "mesh", dialerDaemonSetName: "site", dialerCloudDaemonSetName: "remote", dialerImage: "cldt:test"}
	for _, policy := range []corev1.PullPolicy{"", corev1.PullNever, corev1.PullAlways, corev1.PullIfNotPresent} {
		r.dialerImagePullPolicy = policy
		if err := r.ensureDialerDaemonSet(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := r.ensureCloudDialerDaemonSet(context.Background()); err != nil {
			t.Fatal(err)
		}
		want := policy
		if want == "" {
			want = corev1.PullIfNotPresent
		}
		for _, name := range []string{"site", "remote"} {
			var ds appsv1.DaemonSet
			if err := c.Get(context.Background(), types.NamespacedName{Namespace: "test", Name: name}, &ds); err != nil {
				t.Fatal(err)
			}
			if got := ds.Spec.Template.Spec.Containers[0].ImagePullPolicy; got != want {
				t.Fatalf("%s policy=%s, want %s", name, got, want)
			}
		}
	}
}

func TestDialerPullPolicyRejectsInvalidInput(t *testing.T) {
	for _, value := range []string{"", "never", "Sometimes"} {
		if _, err := parseDialerPullPolicy(value); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
	for _, value := range []string{"Never", "Always", "IfNotPresent"} {
		if _, err := parseDialerPullPolicy(value); err != nil {
			t.Fatal(err)
		}
	}
}
