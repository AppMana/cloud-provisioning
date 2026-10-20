package attachment

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"k8s.io/client-go/kubernetes"
)

// ReleaseFinalizedReceiveRetirementCohort releases an explicit joint retirement
// together. check must independently authorize current owner absence, durable
// consumer fences and compatible recovery for EACH supplied member. It runs for
// every member at both boundaries. Historical finalization alone cannot release
// ownership. Lost responses require fresh retained-history readback, not retry.
func ReleaseFinalizedReceiveRetirementCohort(ctx context.Context, api kubernetes.Interface, store handover.Store, namespace, meshName, meshUID, machineUID, inventoryUID string, maxAge time.Duration, check func(ReceiveRetirementSet) error) error {
	return releaseFinalizedReceiveRetirementCohort(ctx, api, store, namespace, meshName, meshUID, machineUID, inventoryUID, maxAge, check, nil)
}

func releaseFinalizedReceiveRetirementCohort(ctx context.Context, api kubernetes.Interface, store handover.Store, namespace, meshName, meshUID, machineUID, inventoryUID string, maxAge time.Duration, check func(ReceiveRetirementSet) error, finalCheck func() error) error {
	if api == nil || store.ConfigMaps == nil || check == nil {
		return fmt.Errorf("joint release requires API and per-member release authority")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	source, verified, err := ReadReceiveRetirementFinalization(ctx, api, store, namespace, meshName, meshUID, machineUID, inventoryUID, maxAge)
	if err != nil {
		return err
	}
	if !verified {
		return fmt.Errorf("joint release requires verified finalization")
	}
	members, err := source.finalizedRetirementCohort(store.ClusterUID, inventoryUID, maxAge)
	if err != nil {
		return err
	}
	snapshots := map[string]handover.Snapshot{}
	publications := map[string]tunnel.ReceiveJournalPublication{}
	for _, member := range members {
		uid := member.Withdrawal.MachineUID
		snapshots[uid] = member.Journals[uid]
		publications[uid] = member.Inventory.Publications[uid]
	}
	recheck := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, member := range members {
			if err := check(member); err != nil {
				return err
			}
		}
		// All member views share this admitted inventory and complete journal set.
		// Reread those revisions once, rather than reconstructing each ancestry.
		if err := source.Recheck(ctx, api, store); err != nil {
			return err
		}
		if finalCheck != nil {
			if err := finalCheck(); err != nil {
				return err
			}
		}
		return ctx.Err()
	}
	if err := recheck(); err != nil {
		return err
	}
	return tunnel.ReleaseReceiveJournals(ctx, store.ConfigMaps, store.ClusterUID, source.Metadata, source.Mesh, snapshots, publications, recheck)
}

// Derive per-owner views from one freshly admitted complete source. Live cohort
// membership must exactly match every consumed joint completion. This helper is
// not source admission and does not grant release or perform API reads.
func (source ReceiveRetirementSet) finalizedRetirementCohort(clusterUID, inventoryUID string, maxAge time.Duration) ([]ReceiveRetirementSet, error) {
	cohort, err := source.RetirementCohort()
	if err != nil {
		return nil, err
	}
	if len(cohort.MachineUIDs) < 2 {
		return nil, fmt.Errorf("joint release requires multiple published withdrawals")
	}
	members := make([]ReceiveRetirementSet, 0, len(cohort.MachineUIDs))
	for _, uid := range cohort.MachineUIDs {
		pub := source.Inventory.Publications[uid]
		var completion receiveRetirementCompletion
		if err := decodeRetirementEvidence(pub.RetirementCompletion, &completion); err != nil {
			return nil, err
		}
		if completion.Version != receiveRetirementCohortCompletionVersion || !slices.Equal(completion.Cohort, cohort.MachineUIDs) {
			return nil, fmt.Errorf("joint release completions disagree on membership")
		}
		w, err := ReadReceiveWithdrawal(source.Inventory, uid, source.Journals[uid], source.Mesh)
		if err != nil {
			return nil, err
		}
		member := source
		member.Withdrawal = w
		verified, err := validateReceiveRetirementFinalization(member, clusterUID, uid, inventoryUID, maxAge, time.Now())
		if err != nil {
			return nil, err
		}
		if !verified {
			return nil, fmt.Errorf("joint release member has no verified finalization")
		}
		members = append(members, member)
	}
	return members, nil
}
