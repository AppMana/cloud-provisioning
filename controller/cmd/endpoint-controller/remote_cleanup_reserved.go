package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// reservedRemoteNames protects both published journals and preparations that
// survived a crash before journal creation. Machine absence does not release
// either reservation. This is a cleanup exclusion, never retirement authority.
func (r *meshReconciler) reservedRemoteNames(ctx context.Context, mesh *corev1.Secret) (map[string]bool, error) {
	names := map[string]bool{}
	index := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: mesh.Namespace, Name: tunnel.ReceiveInventoryName(string(mesh.UID))}
	if err := r.reader.Get(ctx, key, index); apierrors.IsNotFound(err) {
		return names, nil
	} else if err != nil {
		return nil, err
	}
	if r.receiveJournal.ConfigMaps == nil || r.receiveJournal.ClusterUID == "" {
		return nil, fmt.Errorf("reserved peer cleanup requires receiver journal storage")
	}
	inventory, observed, err := tunnel.ReadReceiveInventory(ctx, r.receiveJournal.ConfigMaps, r.receiveJournal.ClusterUID, mesh.Namespace, mesh.Name, string(mesh.UID))
	if err != nil {
		return nil, err
	}
	if observed.UID != index.UID || observed.ResourceVersion != index.ResourceVersion {
		return nil, fmt.Errorf("receiver inventory changed during orphan discovery")
	}
	for _, uid := range inventory.MachineUIDs {
		publication := inventory.Publications[uid]
		var plans []tunnel.ReceiveTransitionPlan
		if len(publication.Plan) > 0 {
			// A pending preparation can coexist with a published journal.
			// Protect both; decoding this record does not authorize execution.
			var plan tunnel.ReceiveTransitionPlan
			if err := json.Unmarshal(publication.Plan, &plan); err != nil {
				return nil, err
			}
			plans = append(plans, plan)
		}
		if publication.JournalUID == "" && len(publication.Plan) == 0 {
			return nil, fmt.Errorf("reserved orphan lacks a published journal identity or preparation")
		}
		if publication.JournalUID != "" {
			store := r.receiveJournal
			store.Name = tunnel.ReceiveTransitionName(uid)
			store.ExpectedUID = publication.JournalUID
			snapshot, err := store.Load(ctx)
			if err != nil {
				return nil, err
			}
			plan, err := tunnel.DecodeReceiveTransition(snapshot, store.ClusterUID)
			if err != nil {
				return nil, err
			}
			plans = append(plans, plan)
		}
		for _, plan := range plans {
			if (plan.Version != 1 && plan.Version != 2) || plan.MachineUID != uid || plan.MachineName == "" || plan.Namespace != mesh.Namespace || plan.MeshName != mesh.Name || plan.MeshUID != string(mesh.UID) {
				return nil, fmt.Errorf("reserved orphan journal scope changed")
			}
			names[plan.MachineName] = true
		}
	}
	current := &corev1.ConfigMap{}
	if err := r.reader.Get(ctx, key, current); err != nil {
		return nil, err
	}
	if current.UID != observed.UID || current.ResourceVersion != observed.ResourceVersion || current.DeletionTimestamp != nil {
		return nil, fmt.Errorf("receiver inventory changed during orphan admission")
	}
	return names, nil
}
