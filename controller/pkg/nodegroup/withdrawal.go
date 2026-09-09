package nodegroup

import (
	"context"
	"fmt"
	"reflect"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CapturePeerWithdrawal persists public survivor identities before any direct
// peer removal. The source version binds capture to a specific mesh snapshot;
// publication and acknowledgement remain subsequent operations.
func CapturePeerWithdrawal(ctx context.Context, api client.Client, group *v1alpha1.ProvisionedNodeGroupClaim) (*v1alpha1.ProvisionedNodeGroupClaim, error) {
	worker, err := drainWorker(group)
	if err != nil {
		return nil, err
	}
	a := group.Status.PendingAction
	if api == nil || a.Gateways == nil || a.Gateways.Mesh == "" || a.Withdrawal != nil {
		return nil, fmt.Errorf("gateway inventory and new withdrawal capture required")
	}
	done, err := RetireGatewayInventory(ctx, api, group)
	if err != nil {
		return nil, err
	}
	if !done {
		return nil, fmt.Errorf("gateway retirement must finish before direct withdrawal capture")
	}
	mesh := &corev1.Secret{}
	if err := api.Get(ctx, client.ObjectKey{Namespace: group.Namespace, Name: a.Gateways.Mesh}, mesh); err != nil {
		return nil, err
	}
	resolver := attachment.MeshConsumerResolver{Reader: api, Namespace: group.Namespace, SecretName: mesh.Name, SecretUID: string(mesh.UID)}
	intent, err := resolver.PrepareRemoteWithdrawal(ctx, a.ChildName, worker, string(group.UID)+"/"+a.ID)
	if err != nil {
		return nil, err
	}
	inventory := &v1alpha1.GroupPeerWithdrawal{MeshName: intent.MeshName, MeshUID: intent.MeshUID, SourceVersion: intent.SourceVersion, PublicKey: intent.PublicKey}
	for _, c := range intent.Consumers {
		inventory.Consumers = append(inventory.Consumers, v1alpha1.GroupPeerConsumer{NodeName: c.NodeName, NodeUID: c.NodeUID, Site: c.Site, MachineName: c.MachineName, TunnelAddress: c.TunnelAddress, PublicKey: c.PublicKey, SecretUID: c.SecretUID})
	}
	next := group.DeepCopy()
	next.Status.PendingAction.Withdrawal = inventory
	expected := next.DeepCopy()
	if err := api.Status().Update(ctx, next); err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(next.Status.PendingAction, expected.Status.PendingAction) {
		return nil, fmt.Errorf("API did not retain direct withdrawal inventory")
	}
	return next, nil
}
