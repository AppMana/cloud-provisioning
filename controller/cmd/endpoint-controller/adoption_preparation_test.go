package main

import (
	"context"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestAdoptionPreservesPreparationBase(t *testing.T) {
	for _, mode := range []string{"ordinary", "pending", "malformed", "empty", "concurrent"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			machine := &unstructured.Unstructured{}
			machine.SetName("worker")
			base := []byte(`{"peers":[]}`)
			adoption := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: tunnel.AdoptionSecretName("worker"), Namespace: "cloud-provisioning"}, Data: map[string][]byte{tunnel.CloudPeersKey: base}}
			mesh := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "peers", Namespace: adoption.Namespace}, Data: map[string][]byte{
				tunnel.NodePublicKeyPrefix + "site":     []byte(preparationKey(16)),
				tunnel.NodeTunnelAddressPrefix + "site": []byte("10.100.0.2/24"),
			}}
			proposal := []byte(`{"version":1}`)
			switch mode {
			case "pending":
				adoption.Data[tunnel.ReceivePrepareKey] = proposal
			case "malformed":
				adoption.Data[tunnel.ReceivePrepareKey] = []byte("invalid")
			case "empty":
				adoption.Data[tunnel.ReceivePrepareKey] = []byte{}
			}
			r, c := dualStackReconciler(t, "", mesh, adoption)
			if mode == "concurrent" {
				r.Client = &receivePreparationRaceClient{Client: c, beforePatch: func(ctx context.Context) error {
					current := &corev1.Secret{}
					if err := c.Get(ctx, client.ObjectKeyFromObject(adoption), current); err != nil {
						return err
					}
					current.Data[tunnel.ReceivePrepareKey] = proposal
					return c.Update(ctx, current)
				}}
			}
			err := r.ensureAdoptionConfig(ctx, machine)
			if mode == "concurrent" {
				if !apierrors.IsConflict(err) {
					t.Fatalf("concurrent preparation must reject stale forwarding write: %v", err)
				}
				if err := r.ensureAdoptionConfig(ctx, machine); err != nil {
					t.Fatalf("retry after conflict: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			current := &corev1.Secret{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(adoption), current); err != nil {
				t.Fatal(err)
			}
			unchanged := string(current.Data[tunnel.CloudPeersKey]) == string(base)
			if unchanged != (mode != "ordinary") {
				t.Fatalf("forwarding base unchanged=%v in mode %s", unchanged, mode)
			}
		})
	}
}
