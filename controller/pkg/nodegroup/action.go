package nodegroup

import (
	"context"
	"fmt"
	"reflect"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const CreateAction = "Create"
const DrainAction = "Drain"

// ProposeAction derives one operation from authoritative observations. It
// freezes creation intent so a later template update cannot change a retry.
func ProposeAction(group *v1alpha1.ProvisionedNodeGroupClaim, claims []v1alpha1.ProvisionedNodeClaim) (*v1alpha1.NodeGroupAction, error) {
	if group == nil || group.ResourceVersion == "" || group.Generation < 1 {
		return nil, fmt.Errorf("persisted group required")
	}
	if _, err := childName(group, 0); err != nil {
		return nil, err
	}
	if group.Status.PendingAction != nil {
		return nil, fmt.Errorf("resume pending action before planning")
	}
	desired := 1
	if group.Spec.Replicas != nil {
		desired = int(*group.Spec.Replicas)
	}
	if !group.DeletionTimestamp.IsZero() {
		desired = 0
	}
	children := make([]Child, len(claims))
	for i := range claims {
		observed, e := ObserveChild(group, &claims[i])
		if e != nil {
			return nil, e
		}
		children[i] = observed
	}
	plan, e := Next(string(group.UID), desired, children)
	if e != nil {
		return nil, e
	}
	if plan.CreateOrdinal == nil && plan.Drain == "" {
		return nil, nil
	}
	action := &v1alpha1.NodeGroupAction{ID: string(uuid.NewUUID()), Generation: group.Generation}
	if plan.CreateOrdinal != nil {
		action.Type = CreateAction
		action.Ordinal = int32(*plan.CreateOrdinal)
		child, e := BuildChild(group, *plan.CreateOrdinal)
		if e != nil {
			return nil, e
		}
		action.ChildName = child.Name
		action.Template = &v1alpha1.ProvisionedNodeClaimTemplate{Spec: child.Spec}
	} else {
		action.Type = DrainAction
		action.ChildName = plan.Drain
		for i := range children {
			if children[i].Name == plan.Drain {
				action.Ordinal = int32(children[i].Ordinal)
				action.ChildUID = string(claims[i].UID)
			}
		}
	}
	return action, nil
}

// ReserveAction uses the group's resourceVersion as a compare-and-swap. It
// never retries conflicts or changes the caller's object. Only the returned
// persisted group may authorize subsequent child/native operations.
func ReserveAction(ctx context.Context, writer client.Client, group *v1alpha1.ProvisionedNodeGroupClaim, action *v1alpha1.NodeGroupAction) (*v1alpha1.ProvisionedNodeGroupClaim, error) {
	if writer == nil || group == nil || action == nil || group.ResourceVersion == "" || group.Status.PendingAction != nil || action.Generation != group.Generation || action.Generation < 1 || action.ID == "" {
		return nil, fmt.Errorf("invalid action reservation")
	}
	if action.Gateways != nil || action.NodeName != "" || action.NodeUID != "" || action.MachineUID != "" || action.ProviderID != "" {
		return nil, fmt.Errorf("new reservation must resolve its target separately")
	}
	name, e := childName(group, int(action.Ordinal))
	if e != nil {
		return nil, e
	}
	if name != action.ChildName {
		return nil, fmt.Errorf("action has a different child identity")
	}
	switch action.Type {
	case CreateAction:
		if action.Template == nil || action.ChildUID != "" || !group.DeletionTimestamp.IsZero() || !reflect.DeepEqual(action.Template.Spec, group.Spec.Template.Spec) {
			return nil, fmt.Errorf("invalid creation intent")
		}
	case DrainAction:
		if action.Template != nil || action.ChildUID == "" {
			return nil, fmt.Errorf("drain must identify an existing child")
		}
	default:
		return nil, fmt.Errorf("unknown action type")
	}
	// Copy through the API's DeepCopy method, including nested bootstrap fields.
	candidate := group.DeepCopy()
	candidate.Status.PendingAction = action
	candidate = candidate.DeepCopy()
	if e = writer.Status().Update(ctx, candidate); e != nil {
		return nil, e
	}
	if !reflect.DeepEqual(candidate.Status.PendingAction, action) {
		return nil, fmt.Errorf("persisted action differs from requested intent")
	}
	return candidate, nil
}
