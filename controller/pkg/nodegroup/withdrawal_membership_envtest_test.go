package nodegroup

import (
	"context"
	"reflect"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func verifyWithdrawalExpansion(t *testing.T, api client.Client, g *v1alpha1.ProvisionedNodeGroupClaim) *v1alpha1.ProvisionedNodeGroupClaim {
	t.Helper()
	ctx := context.Background()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "zz-new-withdrawal-site"}}
	if err := api.Create(ctx, node); err != nil {
		t.Fatal(err)
	}
	mesh := &corev1.Secret{}
	if err := api.Get(ctx, client.ObjectKey{Namespace: g.Namespace, Name: g.Status.PendingAction.Withdrawal.MeshName}, mesh); err != nil {
		t.Fatal(err)
	}
	mesh.Data[tunnel.NodePublicKeyPrefix+node.Name] = []byte("new-site-key")
	mesh.Data[tunnel.NodeTunnelAddressPrefix+node.Name] = []byte("10.100.0.9")
	mesh.Data[tunnel.NodeAddressesPrefix+node.Name] = []byte("10.10.0.12")
	if err := api.Update(ctx, mesh); err != nil {
		t.Fatal(err)
	}
	old := g.DeepCopy()
	extended, changed, err := ExtendPeerWithdrawal(ctx, api, g)
	if err != nil || !changed {
		t.Fatal("new consumer was not persisted", changed, err)
	}
	count := len(old.Status.PendingAction.Withdrawal.Consumers)
	if !reflect.DeepEqual(old.Status.PendingAction.Withdrawal.Consumers, extended.Status.PendingAction.Withdrawal.Consumers[:count]) || len(extended.Status.PendingAction.Withdrawal.Consumers) != count+1 {
		t.Fatal("extension replaced previous recipients")
	}
	reloaded := &v1alpha1.ProvisionedNodeGroupClaim{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(g), reloaded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(extended.Status.PendingAction.Withdrawal, reloaded.Status.PendingAction.Withdrawal) {
		t.Fatal("API lost extension")
	}
	if _, _, err := ExtendPeerWithdrawal(ctx, api, old); !apierrors.IsConflict(err) {
		t.Fatalf("stale extension accepted: %v", err)
	}
	delete(mesh.Data, tunnel.NodePublicKeyPrefix+node.Name)
	delete(mesh.Data, tunnel.NodeTunnelAddressPrefix+node.Name)
	delete(mesh.Data, tunnel.NodeAddressesPrefix+node.Name)
	if err := api.Update(ctx, mesh); err != nil {
		t.Fatal(err)
	}
	retained, changed, err := ExtendPeerWithdrawal(ctx, api, reloaded)
	if err != nil || changed || !reflect.DeepEqual(retained.Status.PendingAction.Withdrawal, reloaded.Status.PendingAction.Withdrawal) {
		t.Fatal("unpublished recipient dropped", changed, err)
	}
	mesh.Data[tunnel.NodePublicKeyPrefix+node.Name] = []byte("replacement-key")
	mesh.Data[tunnel.NodeTunnelAddressPrefix+node.Name] = []byte("10.100.0.9")
	mesh.Data[tunnel.NodeAddressesPrefix+node.Name] = []byte("10.10.0.12")
	if err := api.Update(ctx, mesh); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ExtendPeerWithdrawal(ctx, api, reloaded); err == nil {
		t.Fatal("changed consumer key accepted")
	}
	mesh.Data[tunnel.NodePublicKeyPrefix+node.Name] = []byte("new-site-key")
	if err := api.Update(ctx, mesh); err != nil {
		t.Fatal(err)
	}
	return reloaded
}
