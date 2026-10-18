package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"reflect"
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

// Deleting a Machine must revoke its transport, native-network metadata and
// adoption Secret while preserving live peers.
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
			"peer-native-network-remote1": []byte(`{"id":"aws:account:region:vpc"}`),
			"peer-public-key-remote1":     []byte("redacted public key"),
			"peer-endpoint-remote1":       []byte("203.0.113.10:51820"),
			"peer-allowed-ips-remote1":    []byte("10.100.0.128/32,203.0.113.10/32,10.244.5.0/24"),
			"peer-route-hosts-remote1":    []byte("10.100.0.128,203.0.113.10"),
			"peer-public-key-remote2":     []byte("surviving public key"),
		}}
		changed, err := r.pruneDeletedMachines(context.Background(), peer)
		if unavailable {
			if err == nil || changed || len(peer.Data) != 6 {
				t.Fatalf("failed observation mutated peers: changed=%v err=%v", changed, err)
			}
			continue
		}
		if err != nil || !changed {
			t.Fatalf("changed=%v err=%v", changed, err)
		}
		for _, prefix := range []string{tunnel.PeerPublicKeyPrefix, tunnel.PeerEndpointPrefix, tunnel.PeerAllowedIPsPrefix, tunnel.PeerRouteHostsPrefix, tunnel.PeerNativeNetworkPrefix} {
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

// Fake API admission only: reservations exclude orphan cleanup even if compute
// is absent. Missing/corrupt journal discovery fails before mutation. This does
// not establish native withdrawal, API atomicity or completed orphan recovery.
func TestOrphanCleanupPreservesReceiverReservations(t *testing.T) {
	for _, state := range []string{"prepared", "missing-storage", "missing-journal", "unpublished", "malformed", "wrong-scope", "wrong-index", "foreign-machine"} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			scheme := runtime.NewScheme()
			_ = corev1.AddToScheme(scheme)
			scheme.AddKnownTypeWithName(machineGVK, &unstructured.Unstructured{})
			scheme.AddKnownTypeWithName(machineGVK.GroupVersion().WithKind("MachineList"), &unstructured.UnstructuredList{})
			mesh := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mesh", Namespace: "test", UID: "mesh-uid", ResourceVersion: "1"}, Data: map[string][]byte{tunnel.PeerPublicKeyPrefix + "remote1": []byte("root"), tunnel.PeerRouteHostsPrefix + "remote1": []byte("10.100.0.128")}}
			adoption := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: tunnel.AdoptionSecretName("remote1"), Namespace: mesh.Namespace, UID: "adoption", ResourceVersion: "1"}}
			plan := tunnel.ReceiveTransitionPlan{Version: 1, Namespace: mesh.Namespace, MeshName: mesh.Name, MeshUID: string(mesh.UID), MachineName: "remote1", MachineUID: "machine"}
			if state == "wrong-scope" {
				plan.MeshUID = "other"
			}
			raw, err := json.Marshal(plan)
			if err != nil {
				t.Fatal(err)
			}
			publication := tunnel.ReceiveJournalPublication{Plan: raw}
			if state == "missing-journal" {
				publication = tunnel.ReceiveJournalPublication{JournalUID: "missing"}
			}
			if state == "unpublished" {
				publication = tunnel.ReceiveJournalPublication{}
			}
			inventory := tunnel.ReceiveInventory{Version: 1, ClusterUID: "cluster", Namespace: mesh.Namespace, MeshName: mesh.Name, MeshUID: string(mesh.UID), MachineUIDs: []string{"machine"}, Publications: map[string]tunnel.ReceiveJournalPublication{"machine": publication}}
			raw, err = json.Marshal(inventory)
			if err != nil {
				t.Fatal(err)
			}
			index := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: tunnel.ReceiveInventoryName(string(mesh.UID)), Namespace: mesh.Namespace, UID: "index", ResourceVersion: "1"}, Data: map[string]string{"inventory.json": string(raw)}}
			if state == "malformed" {
				index.Data["inventory.json"] = "{}"
			}
			objects := []client.Object{mesh, adoption, index}
			if state == "foreign-machine" {
				objects = objects[:2]
				other := &unstructured.Unstructured{}
				other.SetGroupVersionKind(machineGVK)
				other.SetName("remote1")
				other.SetNamespace("another-mesh")
				objects = append(objects, other)
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			typedIndex := index.DeepCopy()
			if state == "wrong-index" {
				typedIndex.UID = "replacement"
			}
			core := kubefake.NewClientset(typedIndex)
			r := &meshReconciler{Client: c, reader: c, secretNamespace: mesh.Namespace, tunnelSubnet: "10.100.0.0/24", receiveJournal: handover.Store{ConfigMaps: core.CoreV1().ConfigMaps(mesh.Namespace), ClusterUID: "cluster"}}
			if state == "missing-storage" {
				r.receiveJournal = handover.Store{}
			}
			before := mesh.DeepCopy()
			changed, err := r.pruneDeletedMachines(ctx, mesh)
			if state == "foreign-machine" {
				if err != nil || !changed {
					t.Fatal("unrelated namespace blocked ordinary cleanup", changed, err)
				}
				if err := c.Get(ctx, client.ObjectKeyFromObject(adoption), &corev1.Secret{}); !apierrors.IsNotFound(err) {
					t.Fatal("ordinary orphan adoption not removed", err)
				}
				return
			}
			if changed || !reflect.DeepEqual(before.Data, mesh.Data) {
				t.Fatal("reserved cleanup mutated mesh")
			}
			if (state == "prepared") != (err == nil) {
				t.Fatal("incorrect reserved cleanup admission", err)
			}
			if err := c.Get(ctx, client.ObjectKeyFromObject(adoption), &corev1.Secret{}); err != nil {
				t.Fatal("reserved adoption removed", err)
			}
		})
	}
}
