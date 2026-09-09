package nodegroup

import (
	"context"
	"fmt"
	"reflect"
	"slices"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	claimcontroller "github.com/appmana/cloud-provisioning/controller/pkg/claim"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// cancelUnstartedChildren handles children that have not crossed the claim
// controller's finalizer boundary. A competing admission changes the child's
// resourceVersion and rejects deletion. Established drain actions retain their
// own removal gates, even if another actor removes a claim finalizer.
func cancelUnstartedChildren(ctx context.Context, api client.Client, group *v1alpha1.ProvisionedNodeGroupClaim, children []v1alpha1.ProvisionedNodeClaim) (bool, error) {
	if group.DeletionTimestamp.IsZero() {
		return false, nil
	}
	for i := range children {
		child := &children[i]
		if _, err := ObserveChild(group, child); err != nil {
			return false, err
		}
		if !child.DeletionTimestamp.IsZero() || slices.Contains(child.Finalizers, claimcontroller.Finalizer) {
			continue
		}
		if action := group.Status.PendingAction; action != nil && action.Type == DrainAction && action.ChildUID == string(child.UID) {
			continue
		}
		uid, version := child.UID, child.ResourceVersion
		if err := api.Delete(ctx, child, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &version}}); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		return true, nil
	}
	return false, nil
}

// CancelUnfulfilledCreation clears only an absent child's creation reservation
// after group deletion begins. A late child cannot provision: the claim
// controller checks the live parent before installing its lifecycle finalizer.
func CancelUnfulfilledCreation(ctx context.Context, api client.Client, group *v1alpha1.ProvisionedNodeGroupClaim) (bool, error) {
	if api == nil || group == nil || group.UID == "" || group.ResourceVersion == "" || group.DeletionTimestamp.IsZero() {
		return false, fmt.Errorf("persisted deleting group required")
	}
	a := group.Status.PendingAction
	if a == nil || a.Type != CreateAction || a.ID == "" || a.Template == nil || a.ChildUID != "" || a.Generation < 1 || a.Generation > group.Generation {
		return false, fmt.Errorf("unfulfilled creation reservation required")
	}
	name, err := childName(group, int(a.Ordinal))
	if err != nil {
		return false, err
	}
	if name != a.ChildName {
		return false, fmt.Errorf("reserved child identity changed")
	}
	current := &v1alpha1.ProvisionedNodeGroupClaim{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(group), current); err != nil {
		return false, err
	}
	if current.UID != group.UID || current.ResourceVersion != group.ResourceVersion || current.DeletionTimestamp.IsZero() || !reflect.DeepEqual(current.Status.PendingAction, a) {
		return false, fmt.Errorf("creation cancellation intent changed")
	}
	child := &v1alpha1.ProvisionedNodeClaim{}
	if err := api.Get(ctx, client.ObjectKey{Namespace: group.Namespace, Name: name}, child); err == nil {
		return false, nil
	} else if !apierrors.IsNotFound(err) {
		return false, err
	}
	next := group.DeepCopy()
	next.Status.PendingAction = nil
	if err := api.Status().Update(ctx, next); err != nil {
		return false, err
	}
	if next.Status.PendingAction != nil {
		return false, fmt.Errorf("API did not retain creation cancellation")
	}
	return true, nil
}
