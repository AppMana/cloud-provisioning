package nodegroup

import (
	"context"
	"fmt"
	"reflect"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	attachmentruntime "github.com/appmana/cloud-provisioning/controller/pkg/attachment/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func drainWorker(group *v1alpha1.ProvisionedNodeGroupClaim) (attachment.Machine, error) {
	if group == nil || group.ResourceVersion == "" || group.UID == "" {
		return attachment.Machine{}, fmt.Errorf("persisted group required")
	}
	a := group.Status.PendingAction
	if a == nil || a.Type != DrainAction || a.ID == "" || a.ChildUID == "" || a.MachineUID == "" || a.NodeUID == "" || a.NodeName == "" || a.ProviderID == "" {
		return attachment.Machine{}, fmt.Errorf("bound drain target required")
	}
	return attachment.Machine{UID: a.MachineUID, NodeUID: a.NodeUID, ProviderID: a.ProviderID}, nil
}

func CaptureGatewayInventory(ctx context.Context, api client.Client, group *v1alpha1.ProvisionedNodeGroupClaim, mesh string) (*v1alpha1.ProvisionedNodeGroupClaim, error) {
	worker, err := drainWorker(group)
	if err != nil {
		return nil, err
	}
	if api == nil || group.Status.PendingAction.Gateways != nil {
		return nil, fmt.Errorf("new gateway inventory required")
	}
	refs, err := attachmentruntime.WorkerRequests(ctx, api, group.Namespace, mesh, worker)
	if err != nil {
		return nil, err
	}
	inventory := &v1alpha1.GroupGatewayInventory{Mesh: mesh, Requests: []v1alpha1.GroupGatewayRequest{}}
	for _, ref := range refs {
		inventory.Requests = append(inventory.Requests, v1alpha1.GroupGatewayRequest{Name: ref.Name, UID: string(ref.UID)})
	}
	updated := group.DeepCopy()
	updated.Status.PendingAction.Gateways = inventory
	candidate := updated.DeepCopy()
	if err := api.Status().Update(ctx, candidate); err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(candidate.Status.PendingAction, updated.Status.PendingAction) {
		return nil, fmt.Errorf("API did not retain gateway inventory")
	}
	return candidate, nil
}

// RetireGatewayInventory covers the recorded gateway requests only. Direct mesh
// peers and other attachment kinds must also be withdrawn before claim deletion.
func RetireGatewayInventory(ctx context.Context, api client.Client, group *v1alpha1.ProvisionedNodeGroupClaim) (bool, error) {
	worker, err := drainWorker(group)
	if err != nil {
		return false, err
	}
	inventory := group.Status.PendingAction.Gateways
	if api == nil || inventory == nil || inventory.Mesh == "" {
		return false, fmt.Errorf("persisted gateway inventory required")
	}
	current, err := attachmentruntime.WorkerRequests(ctx, api, group.Namespace, inventory.Mesh, worker)
	if err != nil {
		return false, err
	}
	known := map[string]string{}
	for _, ref := range inventory.Requests {
		if ref.Name == "" || ref.UID == "" || known[ref.Name] != "" {
			return false, fmt.Errorf("invalid persisted request inventory")
		}
		known[ref.Name] = ref.UID
	}
	for _, ref := range current {
		if known[ref.Name] != string(ref.UID) {
			return false, fmt.Errorf("gateway request inventory changed during drain")
		}
	}
	for _, ref := range inventory.Requests {
		done, err := attachmentruntime.RetireWorkerRequest(ctx, api, group.Namespace, inventory.Mesh, ref.Name, types.UID(ref.UID), worker)
		if err != nil || !done {
			return false, err
		}
	}
	return true, nil
}
