package main

import (
	"context"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestReconcilePinsRelayBeforeRestoringLowerAddressReservation(t *testing.T) {
	data := map[string][]byte{
		tunnel.NodePublicKeyPrefix + "w1":            []byte("key-one"),
		tunnel.NodeTunnelAddressPrefix + "w1":        []byte("10.100.0.2/24"),
		tunnel.NodeAddressesPrefix + "w1":            []byte("10.101.0.2"),
		tunnel.NodePublicKeyPrefix + "w2":            []byte("key-two"),
		tunnel.TunnelAddressReservationPrefix + "w2": []byte("10.100.0.1/24"),
		tunnel.SiteAddressesPrefix + "w2":            []byte("10.101.0.3"),
	}
	r, c := dualStackReconciler(t, "",
		dualStackWorker("w1", "10.101.0.2", "fd00::2", "fd00::2/64"),
		dualStackWorker("w2", "10.101.0.3", "fd00::3", "fd00::3/64"),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "cloud-provisioning", Name: "peers"}, Data: data})
	if _, err := r.reconcileTunnelEndpoints(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := meshData(t, c)
	if string(got[tunnel.NodeTunnelAddressPrefix+"w2"]) != "10.100.0.1/24" {
		t.Fatal("reserved address not restored")
	}
	if string(got[tunnel.RelayPublicKeyKey]) != "key-one" {
		t.Fatal("new allocation replaced the current relay")
	}
	if _, err := r.reconcileTunnelEndpoints(context.Background()); err != nil {
		t.Fatal(err)
	}
	if string(meshData(t, c)[tunnel.RelayPublicKeyKey]) != "key-one" {
		t.Fatal("subsequent reconciliation changed relay")
	}
}
