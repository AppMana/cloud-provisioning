package cni

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func fixtureObjects(t *testing.T, dir string, names ...string) []client.Object {
	t.Helper()
	var objects []client.Object
	for _, name := range names {
		raw, err := os.ReadFile("testdata/" + dir + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		var list unstructured.UnstructuredList
		if err := json.Unmarshal(raw, &list); err != nil {
			t.Fatal(err)
		}
		for i := range list.Items {
			objects = append(objects, &list.Items[i])
		}
	}
	return objects
}

// Whether the pod network carries IPv6 is read from where the network
// allocates it: the observed dual-stack site has an enabled IPv6 pool,
// and the observed IPv4 sites have none.
func TestObservedDualStackIsReadFromThePools(t *testing.T) {
	for dir, want := range map[string]bool{
		"k0s-v1.36.4-calico-v3.32.1-bird-dualstack": true,
		"k0s-v1.34.1-calico-v3.29.6":                false,
		"k0s-v1.34.1-calico-v3.29.6-aws":            false,
	} {
		reader := newClient(fixtureObjects(t, dir, "ippools.json")...)
		network, err := Detect(context.Background(), reader)
		if err != nil {
			t.Fatal(err)
		}
		got, err := network.DualStack(context.Background(), reader)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s: dual-stack = %v, want %v", dir, got, want)
		}
	}
}

// A disabled IPv6 pool allocates nothing, so it does not make the
// network dual-stack.
func TestADisabledIPv6PoolIsNotDualStack(t *testing.T) {
	pool := ipPool("v6", "fd00:244::/64", "Never", "Never")
	_ = unstructured.SetNestedField(pool.Object, true, "spec", "disabled")
	reader := newClient(ipPool("v4", "10.244.0.0/16", "Never", "Never"), pool)
	got, err := Network{Name: Calico, Encapsulation: Native}.DualStack(context.Background(), reader)
	if err != nil || got {
		t.Fatalf("dual-stack = %v, %v; want false", got, err)
	}
}

// A network without its own address management routes by the blocks
// the controller manager allocates, which on a dual-stack cluster come
// in both families.
func TestNodeAllocatedIPv6BlocksAreDualStack(t *testing.T) {
	reader := newClient(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "w1"},
		Spec:       corev1.NodeSpec{PodCIDRs: []string{"10.244.1.0/24", "fd00:244:1::/64"}},
	})
	got, err := Network{Name: KubeRouter, Encapsulation: Native}.DualStack(context.Background(), reader)
	if err != nil || !got {
		t.Fatalf("dual-stack = %v, %v; want true", got, err)
	}
	reader = newClient(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "w1"},
		Spec:       corev1.NodeSpec{PodCIDRs: []string{"10.244.1.0/24"}},
	})
	got, err = Network{Name: KubeRouter, Encapsulation: Native}.DualStack(context.Background(), reader)
	if err != nil || got {
		t.Fatalf("dual-stack = %v, %v; want false", got, err)
	}
}
