package main

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Real Windows publisher validation observed these two API address orders
// with unchanged endpoint membership. The duplicated VIP also appeared in
// apiServers. Neither should invalidate the exact-content acknowledgement.
func TestObservedAPIOrderDoesNotInvalidateAdoption(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	peer := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "peers", Namespace: "test"}, Data: map[string][]byte{
		tunnel.NodePublicKeyPrefix + "w1":     []byte("observed-public-key"),
		tunnel.NodeTunnelAddressPrefix + "w1": []byte("10.100.0.1/24"),
		tunnel.APIServersKey:                  []byte("10.10.0.14,10.10.0.13,10.10.0.10"),
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(peer).Build()
	r := &meshReconciler{Client: c, reader: c, secretNamespace: "test", secretName: "peers", apiVIP: "10.10.0.10", apiServerPort: "6443"}
	machine := &unstructured.Unstructured{}
	machine.SetName("aws-win2022")
	if err := r.ensureAdoptionConfig(ctx, machine); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKey{Namespace: "test", Name: tunnel.AdoptionSecretName(machine.GetName())}
	before := &corev1.Secret{}
	if err := c.Get(ctx, key, before); err != nil {
		t.Fatal(err)
	}
	before.Annotations = map[string]string{tunnel.AppliedListAnnotation: tunnel.HashPeerList(before.Data[tunnel.CloudPeersKey])}
	if err := c.Update(ctx, before); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(peer), peer); err != nil {
		t.Fatal(err)
	}
	peer.Data[tunnel.APIServersKey] = []byte("10.10.0.13,10.10.0.10,10.10.0.14")
	if err := c.Update(ctx, peer); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureAdoptionConfig(ctx, machine); err != nil {
		t.Fatal(err)
	}
	after := &corev1.Secret{}
	if err := c.Get(ctx, key, after); err != nil {
		t.Fatal(err)
	}
	if after.ResourceVersion != before.ResourceVersion || !reflect.DeepEqual(after.Data, before.Data) {
		t.Fatal("unchanged API membership rewrote the peer payload and invalidated its acknowledgement")
	}
	var doc tunnel.PeerListDoc
	if err := json.Unmarshal(after.Data[tunnel.CloudPeersKey], &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.APIServers) != 3 {
		t.Fatalf("duplicated API VIP: %v", doc.APIServers)
	}
	// A real endpoint removal must still publish and invalidate the old hash.
	peer.Data[tunnel.APIServersKey] = []byte("10.10.0.10,10.10.0.14")
	if err := c.Update(ctx, peer); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureAdoptionConfig(ctx, machine); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, after); err != nil {
		t.Fatal(err)
	}
	if tunnel.HashPeerList(after.Data[tunnel.CloudPeersKey]) == before.Annotations[tunnel.AppliedListAnnotation] {
		t.Fatal("real API membership change retained the old payload hash")
	}
}
