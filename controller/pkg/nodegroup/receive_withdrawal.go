package nodegroup

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// beginReceiveRemoval handles reserved workers before ordinary peer withdrawal.
// A discovered inventory must never fall back on missing storage or invalid
// history. Each durable stage is retryable; reservations remain after completion.
func (r *Reconciler) beginReceiveRemoval(ctx context.Context, group *v1alpha1.ProvisionedNodeGroupClaim) (bool, error) {
	if _, err := drainWorker(group); err != nil {
		return true, err
	}
	a := group.Status.PendingAction
	if a.Withdrawal == nil || a.Gateways == nil {
		return true, fmt.Errorf("captured withdrawal required")
	}
	mesh := &corev1.Secret{}
	if err := r.API.Get(ctx, client.ObjectKey{Namespace: group.Namespace, Name: a.Withdrawal.MeshName}, mesh); err != nil {
		return true, err
	}
	if string(mesh.UID) != a.Withdrawal.MeshUID || mesh.DeletionTimestamp != nil {
		return true, fmt.Errorf("withdrawal mesh identity changed")
	}
	index := &corev1.ConfigMap{}
	if err := r.API.Get(ctx, client.ObjectKey{Namespace: group.Namespace, Name: tunnel.ReceiveInventoryName(string(mesh.UID))}, index); apierrors.IsNotFound(err) {
		return false, nil
	} else if err != nil {
		return true, err
	}
	if r.RetirementAPI == nil {
		return true, fmt.Errorf("reserved receiver removal requires live retirement API")
	}
	cluster, err := r.RetirementAPI.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		return true, err
	}
	if cluster.UID == "" || cluster.DeletionTimestamp != nil {
		return true, fmt.Errorf("retirement cluster identity missing")
	}
	store := handover.Store{ConfigMaps: r.RetirementAPI.CoreV1().ConfigMaps(group.Namespace), ClusterUID: string(cluster.UID), Name: tunnel.ReceiveTransitionName(a.MachineUID)}
	inventory, liveIndex, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, group.Namespace, mesh.Name, string(mesh.UID))
	if err != nil {
		return true, err
	}
	if liveIndex.UID != index.UID || liveIndex.ResourceVersion != index.ResourceVersion {
		return true, fmt.Errorf("retirement discovery changed")
	}
	if !slices.Contains(inventory.MachineUIDs, a.MachineUID) {
		return false, nil
	}
	publication := inventory.Publications[a.MachineUID]
	if publication.JournalUID == "" {
		return true, fmt.Errorf("receiver journal is not fully published")
	}
	store.ExpectedUID = publication.JournalUID
	snapshot, err := store.Load(ctx)
	if err != nil {
		return true, err
	}
	intent := groupReceiveWithdrawalIntent(group)
	authorize := func(ctx context.Context, w attachment.RemoteWithdrawalIntent) error {
		return AuthorizeReceiveWithdrawal(ctx, r.API, r.Workload, r.MachineGVK, group, w)
	}
	if err := authorize(ctx, intent); err != nil {
		return true, err
	}
	plan, err := tunnel.DecodeReceiveTransition(snapshot, store.ClusterUID)
	if err != nil {
		return true, err
	}
	if plan.MachineUID != a.MachineUID || plan.MachineName != a.ChildName || plan.CloudNodeUID != a.NodeUID || plan.CloudNodeName != a.NodeName || plan.Namespace != group.Namespace || plan.MeshName != mesh.Name || plan.MeshUID != string(mesh.UID) || plan.RootPublicKey != intent.PublicKey {
		return true, fmt.Errorf("receiver journal differs from drain owner")
	}
	if !snapshot.Frozen() {
		_, err := store.FreezeCompleted(ctx, snapshot)
		return true, err
	}
	if len(publication.Retirement) == 0 {
		return true, intent.ReserveReceiveWithdrawal(ctx, store.ConfigMaps, store.ClusterUID, string(index.UID), mesh, snapshot)
	}
	if _, err := attachment.PublishReceiveWithdrawal(ctx, r.RetirementAPI, store, group.Namespace, mesh.Name, string(mesh.UID), a.MachineUID, string(index.UID), authorize); err != nil {
		return true, err
	}
	maxAge := r.RetirementMaxAge
	if maxAge <= 0 {
		maxAge = time.Minute
	}
	complete, err := attachment.VerifyReceiveRetirementCompletion(ctx, r.RetirementAPI, store, group.Namespace, mesh.Name, string(mesh.UID), a.MachineUID, string(index.UID), maxAge)
	if err != nil || !complete {
		return true, err
	}
	if err := authorize(ctx, intent); err != nil {
		return true, err
	}
	// Lifecycle reads may take time. Revalidate completion age, incarnations and
	// source after that check rather than carrying an earlier success forward.
	complete, err = attachment.VerifyReceiveRetirementCompletion(ctx, r.RetirementAPI, store, group.Namespace, mesh.Name, string(mesh.UID), a.MachineUID, string(index.UID), maxAge)
	if err != nil || !complete {
		return true, err
	}
	_, err = recordRemoval(ctx, r.API, group)
	return true, err
}
