package main

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// A machine whose node has not joined yet must be come back for.
//
// A node has no blocks until something is scheduled on it, and no
// node exists at all until it joins — both after the Machine stopped
// changing. If nothing schedules a return, the machine is reconciled
// once while the node is absent and never again: the blocks are never
// published, nothing at the site can reach a pod on the remote, and
// every component reports healthy.
//
// This was invisible while the node was found through
// status.nodeRef, because Cluster API writing that field was itself
// the Machine event that brought the reconciler back. The data source
// doubled as the trigger, so removing the dependency on it removed
// the trigger too.
func TestAMachineWithNoNodeYetIsComeBackFor(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	// No nodes at all: the remote has not joined.
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithLists(&corev1.NodeList{}).Build()
	r := &meshReconciler{reader: c}

	machine := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "remote1"},
		"spec":     map[string]any{"providerID": "containernet://remote1"},
	}}
	if got := r.nodeNameForMachine(context.Background(), machine); got != "" {
		t.Fatalf("resolved a node that has not joined: %q", got)
	}

	// The node joins, and from then on it resolves — but only
	// something coming back would ever ask again.
	if err := c.Create(context.Background(), &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "remote1"},
		Spec:       corev1.NodeSpec{ProviderID: "containernet://remote1"},
	}); err != nil {
		t.Fatal(err)
	}
	if got := r.nodeNameForMachine(context.Background(), machine); got != "remote1" {
		t.Errorf("after joining, the machine resolves to %q", got)
	}
}
