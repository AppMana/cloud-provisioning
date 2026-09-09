package nodegroup

import (
	"context"
	"fmt"
	"reflect"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// BeginRemoval commits teardown only after the recorded consumers acknowledge
// direct withdrawal. The caller must finish workload drain before this step.
func BeginRemoval(ctx context.Context, api client.Client, gvk schema.GroupVersionKind, group *v1alpha1.ProvisionedNodeGroupClaim, apiVIP, apiPort string) (bool, error) {
	if group == nil || group.Status.PendingAction == nil || group.Status.PendingAction.Removing {
		return false, fmt.Errorf("new removal transition required")
	}
	applied, err := ApplyPeerWithdrawal(ctx, api, gvk, group, apiVIP, apiPort)
	if err != nil || !applied {
		return false, err
	}
	next := group.DeepCopy()
	next.Status.PendingAction.Removing = true
	expected := next.DeepCopy()
	if err := api.Status().Update(ctx, next); err != nil {
		return false, err
	}
	if !reflect.DeepEqual(next.Status.PendingAction, expected.Status.PendingAction) {
		return false, fmt.Errorf("API did not retain removal transition")
	}
	return true, nil
}

// ResumeRemoval delegates compute teardown to the child claim controller and
// waits for the exact claim, CAPI Machine and workload Node to disappear. It
// never deletes Machines, Nodes, or same-name replacement claims itself.
func ResumeRemoval(ctx context.Context, api client.Client, workload client.Reader, gvk schema.GroupVersionKind, group *v1alpha1.ProvisionedNodeGroupClaim) (bool, error) {
	if _, err := drainWorker(group); err != nil {
		return false, err
	}
	a := group.Status.PendingAction
	if api == nil || workload == nil || !a.Removing || a.Withdrawal == nil || gvk.Group != "cluster.x-k8s.io" || gvk.Kind != "Machine" || (gvk.Version != "v1beta1" && gvk.Version != "v1beta2") {
		return false, fmt.Errorf("committed removal and CAPI/workload scope required")
	}
	current := &v1alpha1.ProvisionedNodeGroupClaim{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(group), current); err != nil {
		return false, err
	}
	if current.UID != group.UID || current.ResourceVersion != group.ResourceVersion || !reflect.DeepEqual(current.Status.PendingAction, a) {
		return false, fmt.Errorf("removal action changed")
	}
	key := client.ObjectKey{Namespace: group.Namespace, Name: a.ChildName}
	child := &v1alpha1.ProvisionedNodeClaim{}
	childErr := api.Get(ctx, key, child)
	if childErr != nil && !apierrors.IsNotFound(childErr) {
		return false, childErr
	}
	if childErr == nil {
		observed, err := ObserveChild(group, child)
		if err != nil {
			return false, err
		}
		if string(child.UID) != a.ChildUID || observed.Ordinal != int(a.Ordinal) {
			return false, fmt.Errorf("removal child identity changed")
		}
	}
	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(gvk)
	machineErr := api.Get(ctx, key, machine)
	if machineErr != nil && !apierrors.IsNotFound(machineErr) {
		return false, machineErr
	}
	if machineErr == nil && string(machine.GetUID()) != a.MachineUID {
		return false, fmt.Errorf("removal Machine identity changed")
	}
	node := &corev1.Node{}
	nodeErr := workload.Get(ctx, client.ObjectKey{Name: a.NodeName}, node)
	if nodeErr != nil && !apierrors.IsNotFound(nodeErr) {
		return false, nodeErr
	}
	if nodeErr == nil && (string(node.UID) != a.NodeUID || node.Spec.ProviderID != a.ProviderID) {
		return false, fmt.Errorf("removal Node identity changed")
	}
	if childErr == nil {
		if child.DeletionTimestamp.IsZero() {
			uid, rv := child.UID, child.ResourceVersion
			if err := api.Delete(ctx, child, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}}); err != nil && !apierrors.IsNotFound(err) {
				return false, err
			}
		}
		return false, nil
	}
	if machineErr == nil || nodeErr == nil {
		return false, nil
	}
	next := group.DeepCopy()
	next.Status.PendingAction = nil
	if err := api.Status().Update(ctx, next); err != nil {
		return false, err
	}
	if next.Status.PendingAction != nil {
		return false, fmt.Errorf("API did not retain removal completion")
	}
	return true, nil
}
