package nodegroup

import (
	"context"
	"fmt"
	"reflect"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const ActionAnnotation = "cloud-provisioning.appmana.com/node-group-action"

// ResumeCreation reads intent directly from the API and creates its deterministic
// child. Repeating the operation after a lost response returns the same child.
// The client must bypass the manager cache. The pending action remains reserved
// until the caller records completion; this function never starts a new action.
func ResumeCreation(ctx context.Context, api client.Client, key types.NamespacedName, groupUID types.UID, actionID string) (*v1alpha1.ProvisionedNodeClaim, error) {
	if api == nil || groupUID == "" || actionID == "" {
		return nil, fmt.Errorf("API client and reserved identity required")
	}
	group := &v1alpha1.ProvisionedNodeGroupClaim{}
	if err := api.Get(ctx, key, group); err != nil {
		return nil, err
	}
	action := group.Status.PendingAction
	if group.UID != groupUID || action == nil || action.ID != actionID || action.Type != CreateAction || action.Template == nil || action.ChildUID != "" || action.Generation < 1 || action.Generation > group.Generation {
		return nil, fmt.Errorf("creation reservation does not match live group")
	}
	// A later template or scale update does not rewrite committed creation intent.
	frozen := group.DeepCopy()
	action.Template.Spec.DeepCopyInto(&frozen.Spec.Template.Spec)
	expected, err := BuildChild(frozen, int(action.Ordinal))
	if err != nil {
		return nil, err
	}
	if expected.Name != action.ChildName {
		return nil, fmt.Errorf("reserved child name does not match group")
	}
	expected.Annotations[ActionAnnotation] = action.ID
	actual := &v1alpha1.ProvisionedNodeClaim{}
	childKey := client.ObjectKeyFromObject(expected)
	err = api.Get(ctx, childKey, actual)
	if apierrors.IsNotFound(err) {
		actual = expected.DeepCopy()
		err = api.Create(ctx, actual)
		if apierrors.IsAlreadyExists(err) {
			// Another worker may have completed the same reservation while we read.
			actual = &v1alpha1.ProvisionedNodeClaim{}
			err = api.Get(ctx, childKey, actual)
		}
	}
	if err != nil {
		return nil, err
	}
	if _, err = ObserveChild(group, actual); err != nil {
		return nil, err
	}
	if actual.Annotations[ActionAnnotation] != action.ID || !reflect.DeepEqual(actual.Spec, expected.Spec) {
		return nil, fmt.Errorf("existing child differs from reserved creation")
	}
	if !actual.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("reserved child is terminating")
	}
	return actual, nil
}
