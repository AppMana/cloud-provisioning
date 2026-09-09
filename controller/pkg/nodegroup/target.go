package nodegroup

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var errUnresolvedNode = errors.New("CAPI Machine has no resolved Node identity")

// BindDrainNode records the Machine incarnation even while its workload Node
// remains unresolved. A later registration fills the Node identity without
// replacing any previously recorded identity.
// Management and workload clients must read directly from their respective APIs.
// The CAPI GVK is explicit to support the served version in each installation.
func BindDrainNode(ctx context.Context, management client.Client, workload client.Reader, gvk schema.GroupVersionKind, group *v1alpha1.ProvisionedNodeGroupClaim) (*v1alpha1.ProvisionedNodeGroupClaim, error) {
	if management == nil || workload == nil || group == nil || group.ResourceVersion == "" || gvk.Group != "cluster.x-k8s.io" || gvk.Kind != "Machine" || (gvk.Version != "v1beta1" && gvk.Version != "v1beta2") {
		return nil, fmt.Errorf("group and CAPI/workload API scope required")
	}
	a := group.Status.PendingAction
	if a == nil || a.Type != DrainAction || a.ID == "" || a.ChildUID == "" || a.Template != nil {
		return nil, fmt.Errorf("reserved drain required")
	}
	child := &v1alpha1.ProvisionedNodeClaim{}
	key := client.ObjectKey{Namespace: group.Namespace, Name: a.ChildName}
	if err := management.Get(ctx, key, child); err != nil {
		return nil, err
	}
	observed, err := ObserveChild(group, child)
	if err != nil {
		return nil, err
	}
	if string(child.UID) != a.ChildUID || observed.Ordinal != int(a.Ordinal) || observed.Terminating {
		return nil, fmt.Errorf("drain child changed")
	}
	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(gvk)
	if err := management.Get(ctx, key, machine); err != nil {
		return nil, err
	}

	if !machineOwnedByClaim(machine, child) || machine.GetUID() == "" || !machine.GetDeletionTimestamp().IsZero() {
		return nil, fmt.Errorf("CAPI Machine ownership missing or changed")
	}
	if a.MachineUID != "" && a.MachineUID != string(machine.GetUID()) {
		return nil, fmt.Errorf("persisted drain Machine changed")
	}
	name, _, _ := unstructured.NestedString(machine.Object, "status", "nodeRef", "name")
	provider, _, _ := unstructured.NestedString(machine.Object, "spec", "providerID")
	if a.ProviderID != "" && a.ProviderID != provider {
		return nil, fmt.Errorf("persisted drain provider changed")
	}
	if name == "" || provider == "" {
		if a.NodeName != "" || a.NodeUID != "" {
			return nil, fmt.Errorf("persisted drain Node association disappeared")
		}
		bound := group.DeepCopy()
		bound.Status.PendingAction.MachineUID = string(machine.GetUID())
		bound.Status.PendingAction.ProviderID = provider
		bound, err := persistDrainTarget(ctx, management, group, bound)
		if err != nil {
			return nil, err
		}
		return bound, errUnresolvedNode
	}
	node := &corev1.Node{}
	if err := workload.Get(ctx, client.ObjectKey{Name: name}, node); err != nil {
		if apierrors.IsNotFound(err) && a.NodeName == "" && a.NodeUID == "" {
			bound := group.DeepCopy()
			bound.Status.PendingAction.MachineUID = string(machine.GetUID())
			bound.Status.PendingAction.ProviderID = provider
			bound, persistErr := persistDrainTarget(ctx, management, group, bound)
			if persistErr != nil {
				return nil, persistErr
			}
			return bound, errUnresolvedNode
		}
		return nil, err
	}
	refUID, _, _ := unstructured.NestedString(machine.Object, "status", "nodeRef", "uid")
	if node.UID == "" || (refUID != "" && refUID != string(node.UID)) || node.Spec.ProviderID != provider || !node.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("workload Node association changed")
	}
	bound := group.DeepCopy()
	target := bound.Status.PendingAction
	target.NodeName = name
	target.NodeUID = string(node.UID)
	target.MachineUID = string(machine.GetUID())
	target.ProviderID = provider
	if a.NodeName != "" || a.NodeUID != "" {
		if !reflect.DeepEqual(a, target) {
			return nil, fmt.Errorf("persisted drain target changed")
		}
		return bound, nil
	}
	return persistDrainTarget(ctx, management, group, bound)
}

func persistDrainTarget(ctx context.Context, management client.Client, group, bound *v1alpha1.ProvisionedNodeGroupClaim) (*v1alpha1.ProvisionedNodeGroupClaim, error) {
	if reflect.DeepEqual(group.Status.PendingAction, bound.Status.PendingAction) {
		return bound, nil
	}
	expected := *bound.Status.PendingAction
	if err := management.Status().Update(ctx, bound); err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(bound.Status.PendingAction, &expected) {
		return nil, fmt.Errorf("API did not retain drain target")
	}
	return bound, nil
}
