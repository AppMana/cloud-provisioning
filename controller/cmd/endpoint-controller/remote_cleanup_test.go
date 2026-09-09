package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type failedMachineRead struct{ client.Reader }

func (failedMachineRead) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return fmt.Errorf("API unavailable")
}

// Observed in the k0s/kube-router VM lifecycle run: all remote1 resources and
// compute were gone, but its four peer fields and adoption Secret survived.
func TestDeletedRemoteRevokesItsPeerAndAdoption(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		scheme := runtime.NewScheme()
		_ = corev1.AddToScheme(scheme)
		scheme.AddKnownTypeWithName(machineGVK, &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(machineGVK.GroupVersion().WithKind("MachineList"), &unstructured.UnstructuredList{})
		live := &unstructured.Unstructured{}
		live.SetGroupVersionKind(machineGVK)
		live.SetName("remote2")
		live.SetNamespace("cloud-provisioning")
		adoption := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: tunnel.AdoptionSecretName("remote1"), Namespace: "cloud-provisioning", UID: types.UID("old-adoption")}}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(live, adoption).Build()
		r := &meshReconciler{Client: c, reader: c, secretNamespace: "cloud-provisioning", tunnelSubnet: "10.100.0.0/24"}
		if unavailable {
			r.reader = failedMachineRead{Reader: c}
		}
		peer := &corev1.Secret{Data: map[string][]byte{
			"peer-public-key-remote1":  []byte("redacted public key"),
			"peer-endpoint-remote1":    []byte("203.0.113.10:51820"),
			"peer-allowed-ips-remote1": []byte("10.100.0.128/32,203.0.113.10/32,10.244.5.0/24"),
			"peer-route-hosts-remote1": []byte("10.100.0.128,203.0.113.10"),
			"peer-public-key-remote2":  []byte("surviving public key"),
		}}
		changed, err := r.pruneDeletedMachines(context.Background(), peer)
		if unavailable {
			if err == nil || changed || len(peer.Data) != 5 {
				t.Fatalf("failed observation mutated peers: changed=%v err=%v", changed, err)
			}
			continue
		}
		if err != nil || !changed {
			t.Fatalf("changed=%v err=%v", changed, err)
		}
		for _, prefix := range []string{tunnel.PeerPublicKeyPrefix, tunnel.PeerEndpointPrefix, tunnel.PeerAllowedIPsPrefix, tunnel.PeerRouteHostsPrefix} {
			if _, ok := peer.Data[prefix+"remote1"]; ok {
				t.Fatalf("stale %s", prefix)
			}
		}
		if string(peer.Data[tunnel.PeerPublicKeyPrefix+"remote2"]) != "surviving public key" {
			t.Fatal("live peer removed")
		}
		if got := string(peer.Data[tunnel.RetiredTunnelAddressesKey]); got != "10.100.0.128" {
			t.Fatalf("retired addresses=%q", got)
		}
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(adoption), &corev1.Secret{}); !apierrors.IsNotFound(err) {
			t.Fatalf("adoption remained: %v", err)
		}
	}
}
