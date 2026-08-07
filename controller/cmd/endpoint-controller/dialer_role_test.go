package main

import (
	"context"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
)

// Every dialer pod runs as one ServiceAccount, and what that account
// may touch is decided here, not in the chart: the chart cannot name
// the per-machine adoption Secrets, because machines come and go after
// install. So the controller derives the Role's resourceNames from the
// same machine list that drives the adoption Secrets themselves, and a
// Role that grants get/patch on every Secret in the namespace -- helm
// release records, TLS keys, whatever else shares it -- is the defect.
func TestTheDialerRoleNamesExactlyTheSecretsTheDialerTouches(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	scheme.AddKnownTypeWithName(machineGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(machineGVK.GroupVersion().WithKind(machineGVK.Kind+"List"), &unstructured.UnstructuredList{})

	machineOf := func(name string) *unstructured.Unstructured {
		m := &unstructured.Unstructured{}
		m.SetGroupVersionKind(machineGVK)
		m.SetName(name)
		m.SetNamespace("cloud-provisioning")
		m.SetLabels(map[string]string{"cloud-provisioning.appmana.com/role": "cloud-worker"})
		return m
	}

	meshSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "peers", Namespace: "cloud-provisioning"},
	}
	// The Role as the chart's static floor renders it: the verbs are
	// right, the scope is the whole namespace.
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "cloud-provisioning-dialer", Namespace: "cloud-provisioning"},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"secrets"},
			Verbs:     []string{"get", "patch"},
		}},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(meshSecret, role).
		WithRuntimeObjects(machineOf("remote1"), machineOf("remote2")).
		Build()
	selector, err := labels.Parse("cloud-provisioning.appmana.com/role=cloud-worker")
	if err != nil {
		t.Fatal(err)
	}
	r := &meshReconciler{
		Client:               c,
		reader:               c,
		machineSelector:      selector,
		secretNamespace:      "cloud-provisioning",
		secretName:           "peers",
		dialerServiceAccount: "cloud-provisioning-dialer",
	}

	if err := r.refreshAdoptionConfigs(context.Background()); err != nil {
		t.Fatalf("refreshAdoptionConfigs: %v", err)
	}

	got := &rbacv1.Role{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "cloud-provisioning", Name: "cloud-provisioning-dialer"}, got); err != nil {
		t.Fatal(err)
	}
	want := []rbacv1.PolicyRule{{
		APIGroups: []string{""},
		Resources: []string{"secrets"},
		Verbs:     []string{"get", "patch"},
		ResourceNames: []string{
			"peers",
			tunnel.AdoptionSecretName("remote1"),
			tunnel.AdoptionSecretName("remote2"),
		},
	}}
	if !reflect.DeepEqual(got.Rules, want) {
		t.Fatalf("dialer Role rules:\n got %+v\nwant %+v", got.Rules, want)
	}
}

// A machine that is gone must fall out of the Role on the next pass:
// the names are a derivation of the machine list, not an append-only
// ledger, or the scope quietly grows back to everything over time.
func TestADeletedMachineFallsOutOfTheDialerRole(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	scheme.AddKnownTypeWithName(machineGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(machineGVK.GroupVersion().WithKind(machineGVK.Kind+"List"), &unstructured.UnstructuredList{})

	m := &unstructured.Unstructured{}
	m.SetGroupVersionKind(machineGVK)
	m.SetName("remote1")
	m.SetNamespace("cloud-provisioning")
	m.SetLabels(map[string]string{"cloud-provisioning.appmana.com/role": "cloud-worker"})

	meshSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "peers", Namespace: "cloud-provisioning"},
	}
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "cloud-provisioning-dialer", Namespace: "cloud-provisioning"},
		Rules: []rbacv1.PolicyRule{{
			APIGroups:     []string{""},
			Resources:     []string{"secrets"},
			Verbs:         []string{"get", "patch"},
			ResourceNames: []string{"peers", tunnel.AdoptionSecretName("remote1"), tunnel.AdoptionSecretName("departed")},
		}},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(meshSecret, role).
		WithRuntimeObjects(m).
		Build()
	selector, err := labels.Parse("cloud-provisioning.appmana.com/role=cloud-worker")
	if err != nil {
		t.Fatal(err)
	}
	r := &meshReconciler{
		Client:               c,
		reader:               c,
		machineSelector:      selector,
		secretNamespace:      "cloud-provisioning",
		secretName:           "peers",
		dialerServiceAccount: "cloud-provisioning-dialer",
	}

	if err := r.refreshAdoptionConfigs(context.Background()); err != nil {
		t.Fatalf("refreshAdoptionConfigs: %v", err)
	}

	got := &rbacv1.Role{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "cloud-provisioning", Name: "cloud-provisioning-dialer"}, got); err != nil {
		t.Fatal(err)
	}
	want := []string{"peers", tunnel.AdoptionSecretName("remote1")}
	if len(got.Rules) != 1 || !reflect.DeepEqual(got.Rules[0].ResourceNames, want) {
		t.Fatalf("dialer Role resourceNames: got %+v want %v", got.Rules, want)
	}
}
