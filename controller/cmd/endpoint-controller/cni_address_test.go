package main

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// The address the CNI peers on is stated by this operator, and the
// CNI's own address monitor restates its autodetected one whenever an
// interface changes underneath it, which is exactly when tunnels move.
// The only signal that overwrite produces is a Node event, and a Node
// event reconciles with no machine name. So the nameless pass has to
// re-assert every machine's address: the Machine holding the right
// value has not changed, and nothing else will ever say it again.
//
// A wrong address here is not cosmetic. The CNI programs the remote's
// pod blocks on every site node via the address it believes the remote
// has, fighting the dialer for the same route slot, and a site node
// with no tunnel of its own then routes those blocks toward an address
// that resolves through the default gateway. The remote's accept list
// drops everything that arrives any way but the right one.
func TestAClobberedCNIAddressIsRepairedOnTheEventTheClobberProduces(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	scheme.AddKnownTypeWithName(machineGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(machineGVK.GroupVersion().WithKind(machineGVK.Kind+"List"), &unstructured.UnstructuredList{})

	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(machineGVK)
	machine.SetName("remote1")
	machine.SetNamespace("cloud-provisioning")
	machine.SetLabels(map[string]string{"cloud-provisioning.appmana.com/role": "cloud-worker"})
	machine.SetAnnotations(map[string]string{
		"cloud-provisioning.appmana.com/wireguard-addr4": "10.100.0.128/24",
	})
	if err := unstructured.SetNestedField(machine.Object, "remote1", "status", "nodeRef", "name"); err != nil {
		t.Fatal(err)
	}

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "remote1",
			// What the CNI's monitor writes when it re-detects on the
			// interface that faces the internet.
			Annotations: map[string]string{calicoIPv4Annotation: "203.0.113.10/24"},
		},
	}

	// An empty mesh secret renders no peers, which keeps this test
	// about the address: the peer list refresh has nothing to say, and
	// the address must be corrected anyway.
	meshSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "peers", Namespace: "cloud-provisioning"},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(node, meshSecret).
		WithRuntimeObjects(machine).
		Build()
	selector, err := labels.Parse("cloud-provisioning.appmana.com/role=cloud-worker")
	if err != nil {
		t.Fatal(err)
	}
	r := &meshReconciler{
		Client:          c,
		reader:          c,
		machineSelector: selector,
		secretNamespace: "cloud-provisioning",
		secretName:      "peers",
	}

	if err := r.refreshAdoptionConfigs(context.Background()); err != nil {
		t.Fatalf("refreshAdoptionConfigs: %v", err)
	}

	got := &corev1.Node{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "remote1"}, got); err != nil {
		t.Fatal(err)
	}
	if want := "10.100.0.128/32"; got.Annotations[calicoIPv4Annotation] != want {
		t.Errorf("node's CNI address is %q after the nameless pass, want %q: the clobber stands until an unrelated Machine event",
			got.Annotations[calicoIPv4Annotation], want)
	}
}
