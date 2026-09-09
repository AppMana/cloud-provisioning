package nodegroup

import (
	"context"
	"fmt"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"reflect"
	"strings"

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

// ApplyPeerWithdrawal removes the captured peer and checks native application
// receipts from every recorded survivor. It never deletes the child claim.
func ApplyPeerWithdrawal(ctx context.Context, api client.Client, gvk schema.GroupVersionKind, group *v1alpha1.ProvisionedNodeGroupClaim, apiVIP, apiPort string) (bool, error) {
	if _, err := drainWorker(group); err != nil {
		return false, err
	}
	a := group.Status.PendingAction
	if api == nil || a.Withdrawal == nil || a.Gateways == nil || apiPort == "" {
		return false, fmt.Errorf("persisted withdrawal inventory and API port required")
	}
	w := a.Withdrawal
	if w.MeshName != a.Gateways.Mesh || w.MeshUID == "" || w.SourceVersion == "" || w.PublicKey == "" || len(w.Consumers) == 0 {
		return false, fmt.Errorf("invalid withdrawal inventory")
	}
	current := &v1alpha1.ProvisionedNodeGroupClaim{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(group), current); err != nil {
		return false, err
	}
	if current.UID != group.UID || current.ResourceVersion != group.ResourceVersion || !reflect.DeepEqual(current.Status.PendingAction, a) {
		return false, fmt.Errorf("drain action changed before publication")
	}
	done, err := RetireGatewayInventory(ctx, api, group)
	if err != nil || !done {
		return false, err
	}
	frozen, err := FreezeGatewayAttachments(ctx, api, gvk, group)
	if err != nil || !frozen {
		return false, err
	}
	mesh := &corev1.Secret{}
	if err := api.Get(ctx, client.ObjectKey{Namespace: group.Namespace, Name: w.MeshName}, mesh); err != nil {
		return false, err
	}
	if string(mesh.UID) != w.MeshUID || !mesh.DeletionTimestamp.IsZero() {
		return false, fmt.Errorf("withdrawal mesh identity changed")
	}
	targets := make([]attachment.ConsumerTarget, 0, len(w.Consumers))
	sites, remotes := map[string]bool{}, map[string]bool{a.ChildName: true}
	for _, c := range w.Consumers {
		if c.NodeUID == a.NodeUID || c.PublicKey == w.PublicKey {
			return false, fmt.Errorf("retiring peer overlaps a survivor")
		}
		targets = append(targets, attachment.ConsumerTarget{NodeName: c.NodeName, NodeUID: c.NodeUID, Site: c.Site, MachineName: c.MachineName, TunnelAddress: c.TunnelAddress, PublicKey: c.PublicKey, SecretUID: c.SecretUID})
		if c.Site {
			sites[c.NodeName] = true
		} else {
			remotes[c.MachineName] = true
		}
	}
	// A new published participant needs a durable inventory extension before
	// withdrawal can complete. Never silently omit it from the acknowledgement set.
	for key := range mesh.Data {
		for _, prefix := range []string{tunnel.NodePublicKeyPrefix, tunnel.SiteAddressesPrefix} {
			if strings.HasPrefix(key, prefix) && !sites[strings.TrimPrefix(key, prefix)] {
				return false, fmt.Errorf("published site membership changed during withdrawal")
			}
		}
		if strings.HasPrefix(key, tunnel.PeerPublicKeyPrefix) && !remotes[strings.TrimPrefix(key, tunnel.PeerPublicKeyPrefix)] {
			return false, fmt.Errorf("published remote membership changed during withdrawal")
		}
	}
	data, _, err := tunnel.WithdrawRemotePeer(mesh.Data, a.ChildName, w.PublicKey)
	if err != nil {
		return false, err
	}
	expected := mesh.DeepCopy()
	expected.Data = data
	consumers, err := attachment.SnapshotConsumers(expected, targets, apiVIP, apiPort)
	if err != nil {
		return false, err
	}
	published, err := attachment.PublishRemoteWithdrawal(ctx, api, mesh, a.ChildName, w.PublicKey)
	if err != nil {
		return false, err
	}
	applied, err := (attachment.ConsumerVerifier{Reader: api}).Applied(ctx, published, consumers)
	if err != nil || !applied {
		return false, err
	}
	if err := api.Get(ctx, client.ObjectKeyFromObject(group), current); err != nil {
		return false, err
	}
	if current.UID != group.UID || current.ResourceVersion != group.ResourceVersion {
		return false, fmt.Errorf("drain action changed during acknowledgement")
	}
	return true, nil
}

// ExtendPeerWithdrawal appends newly published consumers before their receipts
// can satisfy removal. Previously captured recipients are never removed or
// replaced, including when they disappear from the current published mesh.
func ExtendPeerWithdrawal(ctx context.Context, api client.Client, group *v1alpha1.ProvisionedNodeGroupClaim) (*v1alpha1.ProvisionedNodeGroupClaim, bool, error) {
	if _, err := drainWorker(group); err != nil {
		return nil, false, err
	}
	a := group.Status.PendingAction
	if api == nil || a.Removing || a.Withdrawal == nil || a.Gateways == nil || a.Withdrawal.MeshName != a.Gateways.Mesh {
		return nil, false, fmt.Errorf("active withdrawal inventory required")
	}
	w := a.Withdrawal
	mesh := &corev1.Secret{}
	if err := api.Get(ctx, client.ObjectKey{Namespace: group.Namespace, Name: w.MeshName}, mesh); err != nil {
		return nil, false, err
	}
	resolver := attachment.MeshConsumerResolver{Reader: api, Namespace: group.Namespace, SecretName: w.MeshName, SecretUID: w.MeshUID}
	targets, err := resolver.PublishedConsumers(ctx, mesh)
	if err != nil {
		return nil, false, err
	}
	next := group.DeepCopy()
	for _, c := range targets {
		if !c.Site && c.MachineName == a.ChildName {
			if c.NodeUID != a.NodeUID || c.PublicKey != w.PublicKey {
				return nil, false, fmt.Errorf("retiring peer identity changed")
			}
			continue
		}
		if c.NodeUID == a.NodeUID || c.PublicKey == w.PublicKey {
			return nil, false, fmt.Errorf("retiring identity overlaps new consumer")
		}
		value := v1alpha1.GroupPeerConsumer{NodeName: c.NodeName, NodeUID: c.NodeUID, Site: c.Site, MachineName: c.MachineName, TunnelAddress: c.TunnelAddress, PublicKey: c.PublicKey, SecretUID: c.SecretUID}
		found := false
		for _, old := range next.Status.PendingAction.Withdrawal.Consumers {
			if old.NodeName == value.NodeName || old.NodeUID == value.NodeUID || (old.MachineName != "" && old.MachineName == value.MachineName) || (old.PublicKey != "" && old.PublicKey == value.PublicKey) {
				if !reflect.DeepEqual(old, value) {
					return nil, false, fmt.Errorf("retained consumer identity changed")
				}
				found = true
				break
			}
		}
		if !found {
			next.Status.PendingAction.Withdrawal.Consumers = append(next.Status.PendingAction.Withdrawal.Consumers, value)
		}
	}
	if len(next.Status.PendingAction.Withdrawal.Consumers) == len(w.Consumers) {
		return group.DeepCopy(), false, nil
	}
	expected := next.DeepCopy()
	if err := api.Status().Update(ctx, next); err != nil {
		return nil, false, err
	}
	if !reflect.DeepEqual(next.Status.PendingAction, expected.Status.PendingAction) {
		return nil, false, fmt.Errorf("API did not retain extended consumer inventory")
	}
	return next, true, nil
}
