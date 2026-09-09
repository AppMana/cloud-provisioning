package claim

import (
	"context"
	"fmt"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Read the group through the uncached API before installing the claim finalizer.
// A child created by a stale group reconciler after deletion cannot start compute.
// Standalone claims continue through their existing provider-independent path.
func (r *Reconciler) groupAllowsProvisioning(ctx context.Context, claim *v1alpha1.ProvisionedNodeClaim) (bool, error) {
	var parent *metav1.OwnerReference
	for _, owner := range claim.OwnerReferences {
		if owner.Kind != "ProvisionedNodeGroupClaim" {
			continue
		}
		if parent != nil || owner.APIVersion != v1alpha1.GroupVersion.String() || owner.UID == "" || owner.Name == "" {
			return false, fmt.Errorf("invalid or ambiguous group ownership")
		}
		parent = &owner
	}
	if parent == nil {
		return true, nil
	}
	if r.Reader == nil {
		return false, fmt.Errorf("direct group API reader required")
	}
	group := &v1alpha1.ProvisionedNodeGroupClaim{}
	if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: claim.Namespace, Name: parent.Name}, group); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return group.UID == parent.UID && group.DeletionTimestamp.IsZero(), nil
}
