package nodegroup

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// readyChildren measures observed capacity, independently of desired replicas.
// It requires a current CAPI/Node association and acknowledgement of the peer
// document rendered from this mesh. Receipts attest applied content, not uptime
// or continued reachability; those require the native workload checks.
func (r *Reconciler) readyChildren(ctx context.Context, group *v1alpha1.ProvisionedNodeGroupClaim, children []v1alpha1.ProvisionedNodeClaim) (int32, error) {
	if r.Workload == nil || r.MeshName == "" || r.APIPort == "" || len(children) == 0 {
		return 0, nil
	}
	if r.MachineGVK.Group != "cluster.x-k8s.io" || r.MachineGVK.Kind != "Machine" || (r.MachineGVK.Version != "v1beta1" && r.MachineGVK.Version != "v1beta2") {
		return 0, fmt.Errorf("CAPI scope required for group readiness")
	}
	mesh := &corev1.Secret{}
	if err := r.API.Get(ctx, client.ObjectKey{Namespace: group.Namespace, Name: r.MeshName}, mesh); err != nil {
		return 0, client.IgnoreNotFound(err)
	}
	if mesh.UID == "" || !mesh.DeletionTimestamp.IsZero() {
		return 0, nil
	}
	byNode := map[string]int{}
	for i := range children {
		child := &children[i]
		if _, err := ObserveChild(group, child); err != nil {
			return 0, err
		}
		if !child.DeletionTimestamp.IsZero() || (group.Status.PendingAction != nil && group.Status.PendingAction.Type == DrainAction && group.Status.PendingAction.ChildUID == string(child.UID)) {
			continue
		}
		nodeUID, err := r.readyChild(ctx, mesh, child)
		if err != nil {
			return 0, err
		}
		if nodeUID != "" {
			byNode[nodeUID]++
		}
	}
	last := &corev1.Secret{}
	if err := r.API.Get(ctx, client.ObjectKeyFromObject(mesh), last); err != nil {
		return 0, client.IgnoreNotFound(err)
	}
	if last.UID != mesh.UID || last.ResourceVersion != mesh.ResourceVersion || !last.DeletionTimestamp.IsZero() {
		return 0, nil
	}
	var ready int32
	for _, count := range byNode {
		if count == 1 {
			ready++
		}
	}
	return ready, nil
}

func (r *Reconciler) readyChild(ctx context.Context, mesh *corev1.Secret, child *v1alpha1.ProvisionedNodeClaim) (string, error) {
	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(r.MachineGVK)
	if err := r.API.Get(ctx, client.ObjectKeyFromObject(child), machine); err != nil {
		return "", client.IgnoreNotFound(err)
	}

	if !machineOwnedByClaim(machine, child) || machine.GetUID() == "" || !machine.GetDeletionTimestamp().IsZero() || machine.GetAnnotations()[attachment.DrainIntentAnnotation] != "" || !machineReady(machine) {
		return "", nil
	}
	name, _, _ := unstructured.NestedString(machine.Object, "status", "nodeRef", "name")
	provider, _, _ := unstructured.NestedString(machine.Object, "spec", "providerID")
	if name == "" || provider == "" {
		return "", nil
	}
	node := &corev1.Node{}
	if err := r.Workload.Get(ctx, client.ObjectKey{Name: name}, node); err != nil {
		return "", client.IgnoreNotFound(err)
	}
	refUID, _, _ := unstructured.NestedString(machine.Object, "status", "nodeRef", "uid")
	if node.UID == "" || (refUID != "" && refUID != string(node.UID)) || node.Spec.ProviderID != provider || node.Spec.Unschedulable || !node.DeletionTimestamp.IsZero() {
		return "", nil
	}
	ready := false
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
			ready = true
		}
	}
	if !ready {
		return "", nil
	}
	secret := &corev1.Secret{}
	if err := r.API.Get(ctx, client.ObjectKey{Namespace: child.Namespace, Name: tunnel.AdoptionSecretName(child.Name)}, secret); err != nil {
		return "", client.IgnoreNotFound(err)
	}
	if secret.UID == "" || !secret.DeletionTimestamp.IsZero() {
		return "", nil
	}
	target := attachment.ConsumerTarget{NodeName: name, NodeUID: string(node.UID), MachineName: child.Name, SecretUID: string(secret.UID), PublicKey: string(mesh.Data[tunnel.PeerPublicKeyPrefix+child.Name]), TunnelAddress: strings.SplitN(machine.GetAnnotations()["cloud-provisioning.appmana.com/wireguard-addr4"], "/", 2)[0]}
	// A peer may not yet be published, or may already be withdrawing.
	if target.PublicKey == "" || target.TunnelAddress == "" {
		return "", nil
	}
	consumers, err := attachment.SnapshotConsumers(mesh, []attachment.ConsumerTarget{target}, r.APIVIP, r.APIPort)
	if err != nil {
		// An incomplete publication is unready capacity. It must not prevent
		// the group's independent creation or removal actions from advancing.
		return "", nil
	}
	doc := consumers[0].Document
	if !bytes.Equal(secret.Data[tunnel.CloudPeersKey], doc) || secret.Annotations[tunnel.AppliedListAnnotation] != tunnel.HashPeerList(doc) {
		return "", nil
	}
	// An association change or cordon during observation invalidates this count.
	lastMachine := &unstructured.Unstructured{}
	lastMachine.SetGroupVersionKind(r.MachineGVK)
	if err := r.API.Get(ctx, client.ObjectKeyFromObject(machine), lastMachine); err != nil {
		return "", client.IgnoreNotFound(err)
	}
	lastNode := &corev1.Node{}
	if err := r.Workload.Get(ctx, client.ObjectKeyFromObject(node), lastNode); err != nil {
		return "", client.IgnoreNotFound(err)
	}
	lastSecret := &corev1.Secret{}
	if err := r.API.Get(ctx, client.ObjectKeyFromObject(secret), lastSecret); err != nil {
		return "", client.IgnoreNotFound(err)
	}
	if lastMachine.GetUID() != machine.GetUID() || lastMachine.GetResourceVersion() != machine.GetResourceVersion() || lastNode.UID != node.UID || lastNode.ResourceVersion != node.ResourceVersion || lastSecret.UID != secret.UID || lastSecret.ResourceVersion != secret.ResourceVersion {
		return "", nil
	}
	return string(node.UID), nil
}

func machineReady(machine *unstructured.Unstructured) bool {
	conditions, _, _ := unstructured.NestedSlice(machine.Object, "status", "conditions")
	for _, raw := range conditions {
		condition, ok := raw.(map[string]interface{})
		if !ok || condition["type"] != "Ready" || condition["status"] != "True" {
			continue
		}
		generation, found, err := unstructured.NestedInt64(condition, "observedGeneration")
		// Legacy v1beta1 conditions do not carry observedGeneration.
		return err == nil && ((machine.GroupVersionKind().Version == "v1beta1" && !found) || (found && generation >= machine.GetGeneration()))
	}
	return false
}
