package claim

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"slices"
	"time"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	attachmentruntime "github.com/appmana/cloud-provisioning/controller/pkg/attachment/runtime"
	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const receiveRemovalAnnotation = "cloud-provisioning.appmana.com/receive-removal"

type claimReceiveRemoval struct {
	Version      int                                  `json:"version"`
	MachineUID   string                               `json:"machineUID"`
	NodeName     string                               `json:"nodeName"`
	NodeUID      string                               `json:"nodeUID"`
	ProviderID   string                               `json:"providerID"`
	MeshUID      string                               `json:"meshUID"`
	InventoryUID string                               `json:"inventoryUID"`
	Gateways     []attachmentruntime.WorkerRequestRef `json:"gateways"`
	Withdrawal   *attachment.RemoteWithdrawalIntent   `json:"withdrawal,omitempty"`
	Removing     bool                                 `json:"removing,omitempty"`
}

func (r *Reconciler) saveReceiveRemoval(ctx context.Context, claim *v1alpha1.ProvisionedNodeClaim, record claimReceiveRemoval) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	next := claim.DeepCopy()
	if next.Annotations == nil {
		next.Annotations = map[string]string{}
	}
	next.Annotations[receiveRemovalAnnotation] = string(raw)
	return r.Update(ctx, next)
}

// prepareReceiveDeletion retains standalone claims until the shared retirement
// protocol permits compute teardown. The annotation stores public lifecycle
// identities only. Group-owned claims use their group's committed removal path.
func (r *Reconciler) prepareReceiveDeletion(ctx context.Context, claim *v1alpha1.ProvisionedNodeClaim, machine *unstructured.Unstructured) (bool, error) {
	if r.MeshName == "" {
		if claim.Annotations[receiveRemovalAnnotation] != "" {
			return false, fmt.Errorf("captured retirement requires configured mesh scope")
		}
		return true, nil
	}
	var parent *metav1.OwnerReference
	for _, owner := range claim.OwnerReferences {
		if owner.Kind == "ProvisionedNodeGroupClaim" {
			if parent != nil {
				return false, fmt.Errorf("ambiguous removal group ownership")
			}
			copied := owner
			parent = &copied
		}
	}
	if parent != nil {
		return r.groupReceiveDeletionCommitted(ctx, claim, machine, *parent)
	}
	mesh := &corev1.Secret{}
	if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: claim.Namespace, Name: r.MeshName}, mesh); apierrors.IsNotFound(err) {
		if claim.Annotations[receiveRemovalAnnotation] != "" {
			return false, fmt.Errorf("retirement mesh disappeared")
		}
		return true, nil
	} else if err != nil {
		return false, err
	}
	index := &corev1.ConfigMap{}
	if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: claim.Namespace, Name: tunnel.ReceiveInventoryName(string(mesh.UID))}, index); apierrors.IsNotFound(err) {
		if claim.Annotations[receiveRemovalAnnotation] != "" {
			return false, fmt.Errorf("retirement inventory disappeared")
		}
		return true, nil
	} else if err != nil {
		return false, err
	}
	if r.RetirementAPI == nil || r.Workload == nil {
		return false, fmt.Errorf("reserved claim deletion requires live retirement and workload APIs")
	}
	cluster, err := r.RetirementAPI.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	if cluster.UID == "" || cluster.DeletionTimestamp != nil {
		return false, fmt.Errorf("retirement cluster identity changed")
	}
	store := handover.Store{ConfigMaps: r.RetirementAPI.CoreV1().ConfigMaps(claim.Namespace), ClusterUID: string(cluster.UID), Name: tunnel.ReceiveTransitionName(string(machine.GetUID()))}
	inventory, liveIndex, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, claim.Namespace, mesh.Name, string(mesh.UID))
	if err != nil {
		return false, err
	}
	if index.UID != liveIndex.UID || index.ResourceVersion != liveIndex.ResourceVersion {
		return false, fmt.Errorf("retirement discovery changed")
	}
	raw := claim.Annotations[receiveRemovalAnnotation]
	if !slices.Contains(inventory.MachineUIDs, string(machine.GetUID())) {
		if raw != "" {
			return false, fmt.Errorf("claim lost receiver reservation")
		}
		return true, nil
	}
	if !MachineOwnedByClaim(machine, claim) || claim.DeletionTimestamp == nil || !slices.Contains(claim.Finalizers, Finalizer) {
		return false, fmt.Errorf("reserved deletion requires an owned Machine and retained deleting claim")
	}
	provider, _, _ := unstructured.NestedString(machine.Object, "spec", "providerID")
	nodeName, _, _ := unstructured.NestedString(machine.Object, "status", "nodeRef", "name")
	node := &corev1.Node{}
	if err := r.Workload.Get(ctx, client.ObjectKey{Name: nodeName}, node); err != nil {
		return false, err
	}
	if provider == "" || node.UID == "" || node.Spec.ProviderID != provider || node.DeletionTimestamp != nil {
		return false, fmt.Errorf("retiring claim Node association changed")
	}
	refUID, _, _ := unstructured.NestedString(machine.Object, "status", "nodeRef", "uid")
	if refUID != "" && refUID != string(node.UID) {
		return false, fmt.Errorf("retiring Machine Node reference changed")
	}
	worker := attachment.Machine{UID: string(machine.GetUID()), NodeUID: string(node.UID), ProviderID: provider}
	if raw == "" {
		refs, err := attachmentruntime.WorkerRequests(ctx, r.Reader, claim.Namespace, mesh.Name, worker)
		if err != nil {
			return false, err
		}
		return false, r.saveReceiveRemoval(ctx, claim, claimReceiveRemoval{Version: 1, MachineUID: worker.UID, NodeName: node.Name, NodeUID: worker.NodeUID, ProviderID: provider, MeshUID: string(mesh.UID), InventoryUID: string(index.UID), Gateways: refs})
	}
	var record claimReceiveRemoval
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return false, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return false, fmt.Errorf("trailing claim retirement data")
	}
	if record.Version != 1 || record.MachineUID != worker.UID || record.NodeName != node.Name || record.NodeUID != worker.NodeUID || record.ProviderID != provider || record.MeshUID != string(mesh.UID) || record.InventoryUID != string(index.UID) || record.Gateways == nil {
		return false, fmt.Errorf("persisted claim retirement scope changed")
	}
	if w := record.Withdrawal; w != nil && (w.MeshNamespace != claim.Namespace || w.MeshName != mesh.Name || w.MeshUID != record.MeshUID || w.MachineName != claim.Name || w.MachineUID != record.MachineUID || w.PublicKey == "" || w.SourceVersion == "" || w.DrainIntent != string(claim.UID)+"/delete") {
		return false, fmt.Errorf("captured withdrawal differs from claim identity")
	}
	if record.Removing {
		if record.Withdrawal == nil || record.Withdrawal.DrainIntent != string(claim.UID)+"/delete" || machine.GetAnnotations()[attachment.DrainIntentAnnotation] != record.Withdrawal.DrainIntent {
			return false, fmt.Errorf("removing claim lacks captured withdrawal")
		}
		return true, nil
	}
	empty, err := attachment.DrainNode(ctx, r.Workload, node.Name, node.UID)
	if err != nil || !empty {
		return false, err
	}
	marker := string(claim.UID) + "/delete"
	if existing := machine.GetAnnotations()[attachment.DrainIntentAnnotation]; existing != marker {
		if existing != "" {
			return false, fmt.Errorf("Machine has a different drain owner")
		}
		next := machine.DeepCopy()
		annotations := next.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[attachment.DrainIntentAnnotation] = marker
		next.SetAnnotations(annotations)
		return false, r.Patch(ctx, next, client.MergeFromWithOptions(machine, client.MergeFromWithOptimisticLock{}))
	}
	refs, err := attachmentruntime.WorkerRequests(ctx, r.Reader, claim.Namespace, mesh.Name, worker)
	if err != nil {
		return false, err
	}
	for _, ref := range refs {
		if !slices.Contains(record.Gateways, ref) {
			return false, fmt.Errorf("gateway membership changed during claim retirement")
		}
	}
	for _, ref := range record.Gateways {
		done, err := attachmentruntime.RetireWorkerRequest(ctx, r.Client, claim.Namespace, mesh.Name, ref.Name, ref.UID, worker)
		if err != nil || !done {
			return false, err
		}
	}
	if record.Withdrawal == nil {
		resolver := attachment.MeshConsumerResolver{Reader: r.Reader, Namespace: claim.Namespace, SecretName: mesh.Name, SecretUID: string(mesh.UID)}
		record.Withdrawal, err = resolver.PrepareRemoteWithdrawal(ctx, claim.Name, worker, marker)
		if err != nil {
			return false, err
		}
		return false, r.saveReceiveRemoval(ctx, claim, record)
	}
	authorize := func(ctx context.Context, intent attachment.RemoteWithdrawalIntent) error {
		if !reflect.DeepEqual(intent, *record.Withdrawal) || intent.DrainIntent != marker {
			return fmt.Errorf("claim withdrawal authority changed")
		}
		current := &v1alpha1.ProvisionedNodeClaim{}
		if err := r.Reader.Get(ctx, client.ObjectKeyFromObject(claim), current); err != nil {
			return err
		}
		if current.UID != claim.UID || current.ResourceVersion != claim.ResourceVersion || current.DeletionTimestamp == nil || current.Annotations[receiveRemovalAnnotation] != raw {
			return fmt.Errorf("claim changed during retirement")
		}
		m := &unstructured.Unstructured{}
		m.SetGroupVersionKind(machineGVK)
		if err := r.Reader.Get(ctx, client.ObjectKeyFromObject(machine), m); err != nil {
			return err
		}
		if m.GetUID() != machine.GetUID() || m.GetResourceVersion() != machine.GetResourceVersion() || m.GetDeletionTimestamp() != nil || !MachineOwnedByClaim(m, current) || m.GetAnnotations()[attachment.DrainIntentAnnotation] != marker {
			return fmt.Errorf("Machine changed during claim retirement")
		}
		n := &corev1.Node{}
		if err := r.Workload.Get(ctx, client.ObjectKey{Name: record.NodeName}, n); err != nil {
			return err
		}
		if n.UID != types.UID(record.NodeUID) || n.Spec.ProviderID != record.ProviderID || n.DeletionTimestamp != nil || !n.Spec.Unschedulable {
			return fmt.Errorf("claim Node is not drained")
		}
		pods := &corev1.PodList{}
		if err := r.Workload.List(ctx, pods, client.MatchingFields{"spec.nodeName": n.Name}); err != nil {
			return err
		}
		for _, pod := range pods.Items {
			if pod.Spec.NodeName != n.Name || !attachment.DrainPodExempt(&pod) {
				return fmt.Errorf("claim workload remains")
			}
		}
		latest := &corev1.Node{}
		if err := r.Workload.Get(ctx, client.ObjectKeyFromObject(n), latest); err != nil {
			return err
		}
		if latest.UID != n.UID || latest.ResourceVersion != n.ResourceVersion {
			return fmt.Errorf("claim Node changed during workload observation")
		}
		lastMachine := &unstructured.Unstructured{}
		lastMachine.SetGroupVersionKind(machineGVK)
		if err := r.Reader.Get(ctx, client.ObjectKeyFromObject(m), lastMachine); err != nil {
			return err
		}
		lastClaim := &v1alpha1.ProvisionedNodeClaim{}
		if err := r.Reader.Get(ctx, client.ObjectKeyFromObject(current), lastClaim); err != nil {
			return err
		}
		if lastMachine.GetUID() != m.GetUID() || lastMachine.GetResourceVersion() != m.GetResourceVersion() || lastClaim.UID != current.UID || lastClaim.ResourceVersion != current.ResourceVersion {
			return fmt.Errorf("claim ownership changed during workload observation")
		}
		return ctx.Err()
	}
	age := r.RetirementMaxAge
	if age <= 0 {
		age = time.Minute
	}
	ready, err := attachment.AdvanceReceiveRemoval(ctx, r.RetirementAPI, store, liveIndex.ObjectMeta, mesh, *record.Withdrawal, node.Name, string(node.UID), age, authorize)
	if err != nil || !ready {
		return false, err
	}
	record.Removing = true
	return false, r.saveReceiveRemoval(ctx, claim, record)
}

// Group claims already have durable removal or pre-bootstrap cancellation
// authority. A direct child deletion must not bypass that parent's decision.
func (r *Reconciler) groupReceiveDeletionCommitted(ctx context.Context, claim *v1alpha1.ProvisionedNodeClaim, machine *unstructured.Unstructured, owner metav1.OwnerReference) (bool, error) {
	if owner.APIVersion != v1alpha1.GroupVersion.String() || owner.UID == "" || !MachineOwnedByClaim(machine, claim) {
		return false, fmt.Errorf("invalid removal group identity")
	}
	group := &v1alpha1.ProvisionedNodeGroupClaim{}
	if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: claim.Namespace, Name: owner.Name}, group); err != nil {
		return false, err
	}
	a := group.Status.PendingAction
	if group.UID != owner.UID || a == nil || a.Type != "Drain" || a.ChildName != claim.Name || a.ChildUID != string(claim.UID) || a.MachineUID != string(machine.GetUID()) {
		return false, fmt.Errorf("group has not authorized this claim removal")
	}
	marker := string(group.UID) + "/" + a.ID
	if machine.GetAnnotations()[attachment.DrainIntentAnnotation] != marker {
		return false, fmt.Errorf("group drain marker changed")
	}
	if a.Removing && a.Withdrawal != nil {
		provider, _, _ := unstructured.NestedString(machine.Object, "spec", "providerID")
		nodeName, _, _ := unstructured.NestedString(machine.Object, "status", "nodeRef", "name")
		nodeUID, _, _ := unstructured.NestedString(machine.Object, "status", "nodeRef", "uid")
		if a.Withdrawal.MeshName != r.MeshName || provider != a.ProviderID || nodeName != a.NodeName || (nodeUID != "" && nodeUID != a.NodeUID) {
			return false, fmt.Errorf("committed group removal binding changed")
		}
		return true, nil
	}
	if proof := a.BootstrapCancellation; proof != nil && machine.GetAnnotations()["cloud-provisioning.appmana.com/cancel-before-bootstrap"] == marker {
		node, _, _ := unstructured.NestedMap(machine.Object, "status", "nodeRef")
		config, _, _ := unstructured.NestedMap(machine.Object, "spec", "bootstrap", "configRef")
		secret, _, _ := unstructured.NestedString(machine.Object, "spec", "bootstrap", "dataSecretName")
		if a.Removing || a.Withdrawal != nil || a.NodeName != "" || a.NodeUID != "" || proof.MeshName != r.MeshName || proof.SecretName == "" || secret != proof.SecretName || len(node) > 0 || len(config) > 0 || machine.GetAnnotations()["cloud-provisioning.appmana.com/wireguard-addr4"] != "" {
			return false, fmt.Errorf("bootstrap cancellation binding changed")
		}
		return true, nil
	}
	return false, nil
}
