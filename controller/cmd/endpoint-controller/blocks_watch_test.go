package main

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// A remote's pod blocks are allocated after it joins, one family at a
// time: on the dual-stack AWS row Calico confirmed a remote's IPv4 block
// a second before its IPv6 one. A pass in between published the IPv4
// block alone and, finding a block, stopped coming back; the Machine
// stopped changing, so its IPv6 pods stayed unreachable from the site.
// A block allocated later, of either family, must bring its Machine back.
func TestABlockAllocatedLaterBringsItsMachineBack(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	machine := func(name, node string, role string) *unstructured.Unstructured {
		m := &unstructured.Unstructured{}
		m.SetGroupVersionKind(machineGVK)
		m.SetNamespace("cloud-provisioning")
		m.SetName(name)
		m.SetLabels(map[string]string{cloudWorkerRoleLabel: role})
		_ = unstructured.SetNestedField(m.Object, node, "status", "nodeRef", "name")
		return m
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		machine("aws-k0s-1", "ip-172-29-0-93", cloudWorkerRoleValue),
		machine("aws-k0s-2", "ip-172-29-0-40", cloudWorkerRoleValue),
		machine("someone-elses", "ip-172-29-0-93", "other"),
	).Build()
	selector := labels.SelectorFromSet(labels.Set{cloudWorkerRoleLabel: cloudWorkerRoleValue})
	r := &meshReconciler{Client: c, reader: c, machineSelector: selector}

	block := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"node": "ip-172-29-0-93", "cidr": "fd8f:cf26:522a:128:677f:4f0c:7ba9:2340/122", "state": "confirmed"},
	}}
	got := r.machinesForBlock(context.Background(), block)
	want := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "cloud-provisioning", Name: "aws-k0s-1"}}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("a block on ip-172-29-0-93 enqueued %v, want only %v", got, want)
	}
	site := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"node": "w1"}}}
	if got := r.machinesForBlock(context.Background(), site); len(got) != 0 {
		t.Fatalf("a site node's block enqueued %v", got)
	}
}
