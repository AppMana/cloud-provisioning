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
	// Construct the expected identity even during deletion so an existing
	// child can complete its creation journal and enter the drain plan.
	// The live group's deletion timestamp still forbids a new API create.
	frozen.DeletionTimestamp = nil
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
		if !group.DeletionTimestamp.IsZero() {
			return nil, fmt.Errorf("deleting group has an unfulfilled creation reservation")
		}
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

// CompleteCreation releases a reservation only after directly observing the
// expected child UID. The group resource version fences concurrent scale and
// action changes; conflicts require a new observation rather than a blind retry.
// This records claim creation, not machine readiness.
func CompleteCreation(ctx context.Context, api client.Client, group *v1alpha1.ProvisionedNodeGroupClaim, childUID types.UID) (*v1alpha1.ProvisionedNodeGroupClaim, error) {
	if api == nil || group == nil || group.ResourceVersion == "" || childUID == "" {
		return nil, fmt.Errorf("persisted group and child UID required")
	}
	action := group.Status.PendingAction
	if action == nil || action.ID == "" || action.Type != CreateAction || action.Template == nil || action.ChildUID != "" || action.Generation < 1 || action.Generation > group.Generation {
		return nil, fmt.Errorf("pending creation required")
	}
	name, err := childName(group, int(action.Ordinal))
	if err != nil {
		return nil, err
	}
	if name != action.ChildName {
		return nil, fmt.Errorf("reserved child name does not match group")
	}
	child := &v1alpha1.ProvisionedNodeClaim{}
	if err = api.Get(ctx, types.NamespacedName{Namespace: group.Namespace, Name: name}, child); err != nil {
		return nil, err
	}
	if _, err = ObserveChild(group, child); err != nil {
		return nil, err
	}
	if child.UID != childUID || child.Annotations[ActionAnnotation] != action.ID || !reflect.DeepEqual(child.Spec, action.Template.Spec) || !child.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("child does not confirm reserved creation")
	}
	completed := group.DeepCopy()
	completed.Status.PendingAction = nil
	if err = api.Status().Update(ctx, completed); err != nil {
		return nil, err
	}
	return completed, nil
}
