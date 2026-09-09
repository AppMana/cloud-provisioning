package nodegroup

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// labelNodes publishes the group incarnation for workload node selectors.
// It owns only GroupUIDLabel and never transfers a Node from another group.
// Existing labels survive drain until normal claim teardown removes the Node.
func (r *Reconciler) labelNodes(ctx context.Context, group *v1alpha1.ProvisionedNodeGroupClaim, children []v1alpha1.ProvisionedNodeClaim) (bool, error) {
	if r.Workload == nil || !group.DeletionTimestamp.IsZero() || len(children) == 0 {
		return false, nil
	}
	if r.MachineGVK.Group != "cluster.x-k8s.io" || r.MachineGVK.Kind != "Machine" || (r.MachineGVK.Version != "v1beta1" && r.MachineGVK.Version != "v1beta2") {
		return false, fmt.Errorf("CAPI scope required for group Node labels")
	}
	for i := range children {
		child := &children[i]
		if _, err := ObserveChild(group, child); err != nil {
			return false, err
		}
		if !child.DeletionTimestamp.IsZero() || (group.Status.PendingAction != nil && group.Status.PendingAction.Type == DrainAction && group.Status.PendingAction.ChildUID == string(child.UID)) {
			continue
		}
		machine := &unstructured.Unstructured{}
		machine.SetGroupVersionKind(r.MachineGVK)
		if err := r.API.Get(ctx, client.ObjectKeyFromObject(child), machine); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return false, err
		}
		if !machineOwnedByClaim(machine, child) || machine.GetUID() == "" || !machine.GetDeletionTimestamp().IsZero() || machine.GetAnnotations()[attachment.DrainIntentAnnotation] != "" {
			continue
		}
		name, _, _ := unstructured.NestedString(machine.Object, "status", "nodeRef", "name")
		provider, _, _ := unstructured.NestedString(machine.Object, "spec", "providerID")
		if name == "" || provider == "" {
			continue
		}
		node := &corev1.Node{}
		if err := r.Workload.Get(ctx, client.ObjectKey{Name: name}, node); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return false, err
		}
		refUID, _, _ := unstructured.NestedString(machine.Object, "status", "nodeRef", "uid")
		if node.UID == "" || (refUID != "" && refUID != string(node.UID)) || node.Spec.ProviderID != provider || !node.DeletionTimestamp.IsZero() {
			continue
		}
		if current := node.Labels[GroupUIDLabel]; current != "" {
			if current != string(group.UID) {
				return false, fmt.Errorf("Node %s already belongs to another group", node.Name)
			}
			continue
		}
		// Recheck management identities after reading the workload Node. The
		// final Node patch then rejects replacement and concurrent Node changes.
		currentGroup := &v1alpha1.ProvisionedNodeGroupClaim{}
		if err := r.API.Get(ctx, client.ObjectKeyFromObject(group), currentGroup); err != nil {
			return false, err
		}
		if currentGroup.UID != group.UID || currentGroup.ResourceVersion != group.ResourceVersion || !currentGroup.DeletionTimestamp.IsZero() {
			return false, fmt.Errorf("group changed before Node labeling")
		}
		currentChild := &v1alpha1.ProvisionedNodeClaim{}
		if err := r.API.Get(ctx, client.ObjectKeyFromObject(child), currentChild); err != nil {
			return false, err
		}
		if currentChild.UID != child.UID || currentChild.ResourceVersion != child.ResourceVersion || !currentChild.DeletionTimestamp.IsZero() {
			return false, fmt.Errorf("child changed before Node labeling")
		}
		currentMachine := &unstructured.Unstructured{}
		currentMachine.SetGroupVersionKind(r.MachineGVK)
		if err := r.API.Get(ctx, client.ObjectKeyFromObject(machine), currentMachine); err != nil {
			return false, err
		}
		if currentMachine.GetUID() != machine.GetUID() || currentMachine.GetResourceVersion() != machine.GetResourceVersion() {
			return false, fmt.Errorf("Machine changed before Node labeling")
		}
		if node.Labels == nil {
			node.Labels = map[string]string{}
		}
		node.Labels[GroupUIDLabel] = string(group.UID)
		patch, err := json.Marshal([]map[string]interface{}{
			{"op": "test", "path": "/metadata/uid", "value": string(node.UID)},
			{"op": "test", "path": "/metadata/resourceVersion", "value": node.ResourceVersion},
			{"op": "add", "path": "/metadata/labels", "value": node.Labels},
		})
		if err != nil {
			return false, err
		}
		if err := r.Workload.Patch(ctx, node, client.RawPatch(types.JSONPatchType, patch)); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}
