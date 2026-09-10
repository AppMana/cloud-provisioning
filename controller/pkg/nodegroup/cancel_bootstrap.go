package nodegroup

import (
	"context"
	"fmt"
	"reflect"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	claimcontroller "github.com/appmana/cloud-provisioning/controller/pkg/claim"
	"github.com/appmana/cloud-provisioning/controller/pkg/join"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// BootstrapCancellationAnnotation certifies that cancellation won the Machine
// resource-version race against the join controller's address reservation.
// It is recorded together with the drain marker, which blocks future joins.
const BootstrapCancellationAnnotation = "cloud-provisioning.appmana.com/cancel-before-bootstrap"

func (r *Reconciler) cancellationScope(ctx context.Context, group *v1alpha1.ProvisionedNodeGroupClaim) error {
	if r.API == nil || r.Workload == nil || group == nil || group.UID == "" || group.ResourceVersion == "" || r.MachineGVK.Group != "cluster.x-k8s.io" || r.MachineGVK.Kind != "Machine" || (r.MachineGVK.Version != "v1beta1" && r.MachineGVK.Version != "v1beta2") {
		return fmt.Errorf("persisted group and direct CAPI/workload scope required")
	}
	a := group.Status.PendingAction
	if a == nil || a.Type != DrainAction || a.ID == "" || a.ChildUID == "" || a.MachineUID == "" || a.NodeName != "" || a.NodeUID != "" || a.Removing || a.Withdrawal != nil || a.Gateways != nil || a.Template != nil {
		return fmt.Errorf("unregistered Machine removal action required")
	}
	current := &v1alpha1.ProvisionedNodeGroupClaim{}
	if err := r.API.Get(ctx, client.ObjectKeyFromObject(group), current); err != nil {
		return err
	}
	if current.UID != group.UID || current.ResourceVersion != group.ResourceVersion || !reflect.DeepEqual(current.Status.PendingAction, a) {
		return fmt.Errorf("cancellation action changed")
	}
	return nil
}

// bootstrapUnpublished checks the join controller's pre-publication contract.
// Missing providerID/nodeRef alone never proves that bootstrap has not started.
func (r *Reconciler) bootstrapUnpublished(ctx context.Context, group *v1alpha1.ProvisionedNodeGroupClaim, machine *unstructured.Unstructured, proof *v1alpha1.BootstrapCancellation) (bool, error) {
	a := group.Status.PendingAction
	if proof == nil || proof.SecretName == "" || proof.MeshName == "" {
		return false, fmt.Errorf("bootstrap and mesh scope required")
	}
	if machine != nil {
		provider, _, _ := unstructured.NestedString(machine.Object, "spec", "providerID")
		node, _, _ := unstructured.NestedMap(machine.Object, "status", "nodeRef")
		config, _, _ := unstructured.NestedMap(machine.Object, "spec", "bootstrap", "configRef")
		secret, _, _ := unstructured.NestedString(machine.Object, "spec", "bootstrap", "dataSecretName")
		fenced := machine.GetAnnotations()[BootstrapCancellationAnnotation] == string(group.UID)+"/"+a.ID
		if a.ProviderID != "" && provider != a.ProviderID {
			return false, fmt.Errorf("cancelled provider identity changed")
		}
		if (provider != "" && !fenced) || len(node) > 0 || len(config) > 0 || secret != proof.SecretName || machine.GetAnnotations()[join.WireGuardAddrAnnotation] != "" {
			return false, nil
		}
	}
	secret := &corev1.Secret{}
	if err := r.API.Get(ctx, client.ObjectKey{Namespace: group.Namespace, Name: proof.SecretName}, secret); err == nil {
		return false, nil
	} else if !apierrors.IsNotFound(err) {
		return false, err
	}
	mesh := &corev1.Secret{}
	if err := r.API.Get(ctx, client.ObjectKey{Namespace: group.Namespace, Name: proof.MeshName}, mesh); err != nil {
		return false, err
	}
	if mesh.UID == "" || !mesh.DeletionTimestamp.IsZero() || (proof.MeshUID != "" && proof.MeshUID != string(mesh.UID)) {
		return false, fmt.Errorf("cancellation mesh identity changed")
	}
	for _, prefix := range []string{tunnel.PeerPublicKeyPrefix, tunnel.PeerEndpointPrefix, tunnel.PeerAllowedIPsPrefix, tunnel.PeerRouteHostsPrefix, tunnel.PeerRouteHostPrefix, tunnel.TunnelAddressReservationPrefix} {
		if _, exists := mesh.Data[prefix+a.ChildName]; exists {
			return false, nil
		}
	}
	nodes := &corev1.NodeList{}
	if err := r.Workload.List(ctx, nodes); err != nil {
		return false, err
	}
	for _, node := range nodes.Items {
		if node.Annotations[claimcontroller.ClaimAnnotation] == group.Namespace+"/"+a.ChildName {
			return false, nil
		}
	}
	proof.MeshUID = string(mesh.UID)
	return true, nil
}

// beginBootstrapCancellation fences only joins that have not reserved an
// address or published userdata. In-flight reservations use optimistic Machine
// patches, so either their reservation or this marker wins, never both.
func (r *Reconciler) beginBootstrapCancellation(ctx context.Context, group *v1alpha1.ProvisionedNodeGroupClaim) (bool, error) {
	if err := r.cancellationScope(ctx, group); err != nil {
		return false, err
	}
	a := group.Status.PendingAction
	if a.BootstrapCancellation != nil || r.BootstrapSecretNameFormat == "" || r.MeshName == "" {
		return false, fmt.Errorf("new bootstrap cancellation and configured join scope required")
	}
	key := client.ObjectKey{Namespace: group.Namespace, Name: a.ChildName}
	child := &v1alpha1.ProvisionedNodeClaim{}
	if err := r.API.Get(ctx, key, child); err != nil {
		return false, err
	}
	observed, err := ObserveChild(group, child)
	if err != nil {
		return false, err
	}
	if observed.Ordinal != int(a.Ordinal) {
		return false, fmt.Errorf("cancellation child ordinal changed")
	}
	if string(child.UID) != a.ChildUID || !child.DeletionTimestamp.IsZero() {
		return false, fmt.Errorf("cancellation child changed")
	}
	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(r.MachineGVK)
	if err := r.API.Get(ctx, key, machine); err != nil {
		return false, err
	}
	if string(machine.GetUID()) != a.MachineUID || !machineOwnedByClaim(machine, child) || !machine.GetDeletionTimestamp().IsZero() {
		return false, fmt.Errorf("cancellation Machine changed")
	}
	proof := &v1alpha1.BootstrapCancellation{SecretName: fmt.Sprintf(r.BootstrapSecretNameFormat, a.ChildName), MeshName: r.MeshName}
	eligible, err := r.bootstrapUnpublished(ctx, group, machine, proof)
	if err != nil || !eligible {
		return false, err
	}
	want := string(group.UID) + "/" + a.ID
	annotations := machine.GetAnnotations()
	mark, drain := annotations[BootstrapCancellationAnnotation], annotations[attachment.DrainIntentAnnotation]
	if (mark != "" && mark != want) || (drain != "" && drain != want) || (mark == "" && drain != "") {
		return false, fmt.Errorf("Machine has a different removal intent")
	}
	if mark == "" || drain == "" {
		before := machine.DeepCopy()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[BootstrapCancellationAnnotation] = want
		annotations[attachment.DrainIntentAnnotation] = want
		machine.SetAnnotations(annotations)
		if err := r.API.Patch(ctx, machine, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return false, err
		}
		return true, nil
	}
	next := group.DeepCopy()
	next.Status.PendingAction.BootstrapCancellation = proof
	expected := next.DeepCopy()
	if err := r.API.Status().Update(ctx, next); err != nil {
		return false, err
	}
	if !reflect.DeepEqual(next.Status.PendingAction, expected.Status.PendingAction) {
		return false, fmt.Errorf("API did not retain bootstrap cancellation")
	}
	return true, nil
}

// resumeBootstrapCancellation delegates teardown to the claim/CAPI/provider
// lifecycle, including provider finalizers. It retains the action until the
// claim and original Machine are absent and never deletes provider objects.
func (r *Reconciler) resumeBootstrapCancellation(ctx context.Context, group *v1alpha1.ProvisionedNodeGroupClaim) (bool, error) {
	if err := r.cancellationScope(ctx, group); err != nil {
		return false, err
	}
	a := group.Status.PendingAction
	if a.BootstrapCancellation == nil || a.BootstrapCancellation.MeshUID == "" {
		return false, fmt.Errorf("persisted bootstrap cancellation required")
	}
	key := client.ObjectKey{Namespace: group.Namespace, Name: a.ChildName}
	child := &v1alpha1.ProvisionedNodeClaim{}
	childErr := r.API.Get(ctx, key, child)
	if childErr != nil && !apierrors.IsNotFound(childErr) {
		return false, childErr
	}
	if childErr == nil {
		observed, err := ObserveChild(group, child)
		if err != nil {
			return false, err
		}
		if observed.Ordinal != int(a.Ordinal) {
			return false, fmt.Errorf("cancellation child ordinal changed")
		}
		if string(child.UID) != a.ChildUID {
			return false, fmt.Errorf("cancelled claim replaced")
		}
	}
	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(r.MachineGVK)
	machineErr := r.API.Get(ctx, key, machine)
	if machineErr != nil && !apierrors.IsNotFound(machineErr) {
		return false, machineErr
	}
	if machineErr == nil {
		want := string(group.UID) + "/" + a.ID
		if string(machine.GetUID()) != a.MachineUID || machine.GetAnnotations()[BootstrapCancellationAnnotation] != want || machine.GetAnnotations()[attachment.DrainIntentAnnotation] != want {
			return false, fmt.Errorf("cancelled Machine identity or marker changed")
		}
	} else {
		machine = nil
	}
	proof := *a.BootstrapCancellation
	unpublished, err := r.bootstrapUnpublished(ctx, group, machine, &proof)
	if err != nil {
		return false, err
	}
	if !unpublished {
		return false, fmt.Errorf("bootstrap appeared after cancellation; retain lifecycle for inspection")
	}
	if childErr == nil {
		if child.DeletionTimestamp.IsZero() {
			uid, rv := child.UID, child.ResourceVersion
			if err := r.API.Delete(ctx, child, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}}); err != nil && !apierrors.IsNotFound(err) {
				return false, err
			}
		}
		return false, nil
	}
	if machine != nil {
		return false, nil
	}
	next := group.DeepCopy()
	next.Status.PendingAction = nil
	if err := r.API.Status().Update(ctx, next); err != nil {
		return false, err
	}
	if next.Status.PendingAction != nil {
		return false, fmt.Errorf("API did not retain cancellation completion")
	}
	return true, nil
}
