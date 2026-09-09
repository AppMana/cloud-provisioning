package nodegroup

import (
	"context"
	"fmt"
	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// FreezeGatewayAttachments runs after workload drain and inventory capture.
// CAPILifetime retires requests touching a marked Machine before forwarding can
// be prepared. The mark survives restarts and belongs to exactly one action.
func FreezeGatewayAttachments(ctx context.Context, api client.Client, gvk schema.GroupVersionKind, group *v1alpha1.ProvisionedNodeGroupClaim) (bool, error) {
	worker, err := drainWorker(group)
	if err != nil {
		return false, err
	}
	if api == nil || group.Status.PendingAction.Gateways == nil || gvk.Group != "cluster.x-k8s.io" || gvk.Kind != "Machine" || (gvk.Version != "v1beta1" && gvk.Version != "v1beta2") {
		return false, fmt.Errorf("captured gateway inventory and CAPI scope required")
	}
	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(gvk)
	if err := api.Get(ctx, client.ObjectKey{Namespace: group.Namespace, Name: group.Status.PendingAction.ChildName}, machine); err != nil {
		return false, err
	}
	provider, _, _ := unstructured.NestedString(machine.Object, "spec", "providerID")
	if string(machine.GetUID()) != worker.UID || provider != worker.ProviderID || !machine.GetDeletionTimestamp().IsZero() {
		return false, fmt.Errorf("drain Machine identity changed")
	}
	want := string(group.UID) + "/" + group.Status.PendingAction.ID
	current := machine.GetAnnotations()[attachment.DrainIntentAnnotation]
	if current != "" {
		if current != want {
			return false, fmt.Errorf("Machine belongs to another drain action")
		}
		return true, nil
	}
	before := machine.DeepCopy()
	annotations := machine.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[attachment.DrainIntentAnnotation] = want
	machine.SetAnnotations(annotations)
	if err := api.Patch(ctx, machine, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return false, err
	}
	return false, nil
}
