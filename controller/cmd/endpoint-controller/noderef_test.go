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

// A machine must resolve to its node without Cluster API having a
// connection to the workload cluster.
//
// status.nodeRef only appears if Cluster API holds one, taken from a
// <cluster>-kubeconfig Secret. This product's model is a
// pre-existing, self-managed cluster with no control plane provider
// to write one — examples/aws.yaml is the complete set an operator
// applies, and it contains no such Secret. Depending on nodeRef alone
// half-works in exactly the documented setup: the node joins and goes
// Ready, its pod blocks are never published, the CNI is never told
// which address to peer on, and nothing reports a problem.
func TestAMachineResolvesToItsNodeWithoutNodeRef(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	nodes := &corev1.NodeList{Items: []corev1.Node{
		{ObjectMeta: meta("cp"), Spec: corev1.NodeSpec{ProviderID: "containernet://cp"}},
		{ObjectMeta: meta("remote1"), Spec: corev1.NodeSpec{ProviderID: "containernet://remote1"}},
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithLists(nodes).Build()
	r := &meshReconciler{reader: c}

	machine := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "remote1"},
		"spec":     map[string]any{"providerID": "containernet://remote1"},
	}}
	if got := r.nodeNameForMachine(context.Background(), machine); got != "remote1" {
		t.Errorf("nodeNameForMachine = %q, want the node whose providerID matches", got)
	}

	// nodeRef still wins when Cluster API did set it.
	_ = unstructured.SetNestedField(machine.Object, "somewhere-else", "status", "nodeRef", "name")
	if got := r.nodeNameForMachine(context.Background(), machine); got != "somewhere-else" {
		t.Errorf("nodeNameForMachine = %q, want nodeRef to win when present", got)
	}
}

// A machine whose identity nothing carries resolves to nothing rather
// than to some node: publishing another node's pod blocks under this
// machine's peer entry would send its traffic to the wrong place.
func TestAMachineWithNoIdentityResolvesToNothing(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithLists(&corev1.NodeList{Items: []corev1.Node{
			{ObjectMeta: meta("cp"), Spec: corev1.NodeSpec{ProviderID: "containernet://cp"}},
		}}).Build()
	r := &meshReconciler{reader: c}

	machine := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "remote1"},
	}}
	if got := r.nodeNameForMachine(context.Background(), machine); got != "" {
		t.Errorf("nodeNameForMachine = %q, want nothing", got)
	}
}

func meta(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name} }
