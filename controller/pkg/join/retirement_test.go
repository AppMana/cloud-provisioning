package join

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRemoteAllocationDoesNotReuseRetiredIdentity(t *testing.T) {
	scheme := runtime.NewScheme()
	gvk := schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Machine"}
	scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind("MachineList"), &unstructured.UnstructuredList{})
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &Reconciler{Client: c, Reader: c, WireGuardAddress: "10.100.0.128/24"}
	got, err := r.allocateWireGuardAddress(context.Background(), []string{"10.100.0.130", "203.0.113.10"})
	if err != nil || got != "10.100.0.131/24" {
		t.Fatalf("allocation=%q err=%v", got, err)
	}
	if _, err := r.allocateWireGuardAddress(context.Background(), []string{"10.100.0.254"}); err == nil {
		t.Fatal("exhausted address range accepted")
	}
}
