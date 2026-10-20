package main

import (
	"context"
	"fmt"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// reconcileOrphanReceiveWithdrawals records recovery intent for journals whose
// old lifecycle objects and publication are already gone. Each pass performs
// at most one durable change. It never removes a reservation or treats absent
// compute as a native retirement receipt. Pending preparations remain fenced.
func (r *meshReconciler) reconcileOrphanReceiveWithdrawals(ctx context.Context, mesh *corev1.Secret) (bool, error) {
	if r.receivePreparation == nil || r.receivePreparation.Journal == nil || r.receivePreparation.Journal.Receipts == nil {
		return false, nil
	}
	inventory, index, err := tunnel.ReadReceiveInventory(ctx, r.receiveJournal.ConfigMaps, r.receiveJournal.ClusterUID, mesh.Namespace, mesh.Name, string(mesh.UID))
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// An authoritative empty active set has no orphan work. Released records
	// remain in the inventory; this no-op grants no release or route authority.
	if len(inventory.MachineUIDs) == 0 {
		return false, nil
	}
	snapshots := map[string]handover.Snapshot{}
	plans := map[string]tunnel.ReceiveTransitionPlan{}
	var all []handover.Snapshot
	for _, uid := range inventory.MachineUIDs {
		pub := inventory.Publications[uid]
		if pub.JournalUID == "" || len(pub.Plan) != 0 {
			return false, nil
		}
		store := r.receiveJournal
		store.Name = tunnel.ReceiveTransitionName(uid)
		store.ExpectedUID = pub.JournalUID
		snapshot, err := store.Load(ctx)
		if err != nil {
			return false, err
		}
		if err := tunnel.ValidateReceiveTransportHistory(ctx, store, snapshot); err != nil {
			return false, err
		}
		plan, err := tunnel.DecodeReceiveTransition(snapshot, store.ClusterUID)
		if err != nil {
			return false, err
		}
		if plan.MachineUID != uid || plan.Namespace != mesh.Namespace || plan.MeshName != mesh.Name || plan.MeshUID != string(mesh.UID) {
			return false, fmt.Errorf("orphan journal scope changed")
		}
		snapshots[uid], plans[uid] = snapshot, plan
		all = append(all, snapshot)
	}
	if _, err := tunnel.PlanReceiveJournalRoutes(all, r.receiveJournal.ClusterUID); err != nil {
		return false, err
	}
	resolver := attachment.MeshConsumerResolver{Reader: r.reader, Namespace: mesh.Namespace, SecretName: mesh.Name, SecretUID: string(mesh.UID)}
	for _, uid := range inventory.MachineUIDs {
		snapshot, plan := snapshots[uid], plans[uid]
		machine := &unstructured.Unstructured{}
		machine.SetGroupVersionKind(machineGVK)
		err := r.reader.Get(ctx, client.ObjectKey{Namespace: mesh.Namespace, Name: plan.MachineName}, machine)
		if err == nil {
			continue
		}
		if !apierrors.IsNotFound(err) {
			return false, err
		}
		if len(inventory.Publications[uid].Retirement) != 0 {
			continue
		} // Never recapture persisted consumers.
		if err := resolver.CheckReceiveOrphanAbsent(ctx, plan); err != nil {
			return false, err
		}
		if _, present, err := tunnel.WithdrawRemotePeer(mesh.Data, plan.MachineName, plan.RootPublicKey); err != nil {
			return false, err
		} else if present {
			continue
		} // A still-published owner requires a separate removal protocol.
		view, err := snapshot.View()
		if err != nil {
			return false, err
		}
		if !view.Complete {
			return false, fmt.Errorf("orphan receiver transition is unfinished")
		}
		// Recheck all source revisions immediately before the durable mutation.
		currentMesh := &corev1.Secret{}
		if err := r.reader.Get(ctx, client.ObjectKeyFromObject(mesh), currentMesh); err != nil {
			return false, err
		}
		if currentMesh.UID != mesh.UID || currentMesh.ResourceVersion != mesh.ResourceVersion || currentMesh.DeletionTimestamp != nil {
			return false, fmt.Errorf("orphan mesh changed during admission")
		}
		_, currentIndex, err := tunnel.ReadReceiveInventory(ctx, r.receiveJournal.ConfigMaps, r.receiveJournal.ClusterUID, mesh.Namespace, mesh.Name, string(mesh.UID))
		if err != nil {
			return false, err
		}
		if currentIndex.UID != index.UID || currentIndex.ResourceVersion != index.ResourceVersion {
			return false, fmt.Errorf("orphan inventory changed during admission")
		}
		for other, observed := range snapshots {
			check := r.receiveJournal
			check.Name = tunnel.ReceiveTransitionName(other)
			check.ExpectedUID = string(observed.Metadata().UID)
			current, err := check.Load(ctx)
			if err != nil {
				return false, err
			}
			if current.Metadata().ResourceVersion != observed.Metadata().ResourceVersion {
				return false, fmt.Errorf("orphan reserved history changed during admission")
			}
		}
		store := r.receiveJournal
		store.Name = tunnel.ReceiveTransitionName(uid)
		store.ExpectedUID = string(snapshot.Metadata().UID)
		if !snapshot.Frozen() {
			if err := resolver.CheckReceiveOrphanAbsent(ctx, plan); err != nil {
				return false, err
			}
			_, err := store.FreezeCompleted(ctx, snapshot)
			return err == nil, err
		}
		intent, err := resolver.PrepareOrphanReceiveWithdrawal(ctx, snapshot, inventory, currentMesh)
		if err != nil {
			return false, err
		}
		err = intent.ReserveReceiveWithdrawal(ctx, store.ConfigMaps, store.ClusterUID, string(index.UID), currentMesh, snapshot)
		return err == nil, err
	}
	return false, nil
}
