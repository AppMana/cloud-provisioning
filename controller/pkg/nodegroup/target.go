package nodegroup

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var errUnresolvedNode = errors.New("CAPI Machine has no resolved Node identity")

// BindDrainNode records the Machine and workload Node incarnation before drain.
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
	owned := false
	for _, owner := range machine.GetOwnerReferences() {
		if owner.APIVersion == v1alpha1.GroupVersion.String() && owner.Kind == "ProvisionedNodeClaim" && owner.Name == child.Name && owner.UID == child.UID {
			owned = true
		}
	}
	if !owned || machine.GetUID() == "" || !machine.GetDeletionTimestamp().IsZero() {
		return nil, fmt.Errorf("CAPI Machine ownership missing or changed")
	}
	name, _, _ := unstructured.NestedString(machine.Object, "status", "nodeRef", "name")
	provider, _, _ := unstructured.NestedString(machine.Object, "spec", "providerID")
	if name == "" || provider == "" {
		return nil, errUnresolvedNode
	}
	node := &corev1.Node{}
	if err := workload.Get(ctx, client.ObjectKey{Name: name}, node); err != nil {
		return nil, err
	}
	if node.UID == "" || node.Spec.ProviderID != provider || !node.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("workload Node association changed")
	}
	bound := group.DeepCopy()
	target := bound.Status.PendingAction
	target.NodeName = name
	target.NodeUID = string(node.UID)
	target.MachineUID = string(machine.GetUID())
	target.ProviderID = provider
	if a.NodeName != "" || a.NodeUID != "" || a.MachineUID != "" || a.ProviderID != "" {
		if !reflect.DeepEqual(a, target) {
			return nil, fmt.Errorf("persisted drain target changed")
		}
		return bound, nil
	}
	expected := *target
	if err := management.Status().Update(ctx, bound); err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(bound.Status.PendingAction, &expected) {
		return nil, fmt.Errorf("API did not retain drain target")
	}
	return bound, nil
}
