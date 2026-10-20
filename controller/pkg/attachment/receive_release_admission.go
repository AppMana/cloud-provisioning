package attachment

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ReceiveReleasedJournal is historical retired-owner evidence, never an active
// reservation, current absence observation or native mutation permit.
type ReceiveReleasedJournal struct {
	Inventory  tunnel.ReceiveInventory
	Metadata   metav1.ObjectMeta
	Journal    handover.Snapshot
	Plan       tunnel.ReceiveTransitionPlan
	Withdrawal RemoteWithdrawalIntent
	source     ReceiveRetirementSet
}

// Recheck pins the inventory, retained journal and mesh revisions used for this
// historical read. It neither admits active allocations nor checks current owner
// absence; callers must enforce those separately for their intended operation.
func (r ReceiveReleasedJournal) Recheck(ctx context.Context, api kubernetes.Interface, store handover.Store) error {
	return r.source.Recheck(ctx, api, store)
}

// ReadReleasedReceiveJournal validates retained history and consumed evidence
// after active membership is removed. Later mesh data and consumer boots need
// not match an old withdrawal's captured state: this proves historical identity
// only. It cannot be used to clean up new owners, restore an old peer, release
// another reservation or assert current native absence. Missing records return
// false and a zero result. The frozen journal and its ancestry must still exist.
func ReadReleasedReceiveJournal(ctx context.Context, api kubernetes.Interface, store handover.Store, namespace, meshName, meshUID, machineUID, inventoryUID string, maxAge time.Duration) (ReceiveReleasedJournal, bool, error) {
	fail := func(err error) (ReceiveReleasedJournal, bool, error) { return ReceiveReleasedJournal{}, false, err }
	if api == nil || store.ConfigMaps == nil || machineUID == "" || inventoryUID == "" || maxAge <= 0 || store.Name != tunnel.ReceiveTransitionName(machineUID) {
		return fail(fmt.Errorf("released journal requires exact historical scope and policy"))
	}
	inventory, index, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, namespace, meshName, meshUID)
	if err != nil {
		return fail(err)
	}
	if string(index.UID) != inventoryUID {
		return fail(fmt.Errorf("released inventory incarnation changed"))
	}
	publication, found := inventory.Released[machineUID]
	if !found {
		return ReceiveReleasedJournal{}, false, nil
	}
	if store.ExpectedUID != "" && store.ExpectedUID != publication.JournalUID {
		return fail(fmt.Errorf("released journal incarnation differs"))
	}
	store.ExpectedUID = publication.JournalUID
	snapshot, err := store.Load(ctx)
	if err != nil {
		return fail(err)
	}
	if !snapshot.Frozen() {
		return fail(fmt.Errorf("released journal is not frozen"))
	}
	if err := tunnel.ValidateReceiveTransportHistory(ctx, store, snapshot); err != nil {
		return fail(err)
	}
	w, plan, err := retainedReceiveWithdrawal(inventory, machineUID, snapshot, publication)
	if err != nil {
		return fail(err)
	}
	mesh, err := api.CoreV1().Secrets(namespace).Get(ctx, meshName, metav1.GetOptions{})
	if err != nil {
		return fail(err)
	}
	if string(mesh.UID) != meshUID || mesh.DeletionTimestamp != nil || mesh.ResourceVersion == "" {
		return fail(fmt.Errorf("released mesh incarnation changed"))
	}
	source := ReceiveRetirementSet{Inventory: inventory, Metadata: *index.ObjectMeta.DeepCopy(), MeshMetadata: *mesh.ObjectMeta.DeepCopy(), Mesh: mesh, Journals: map[string]handover.Snapshot{machineUID: snapshot}, Withdrawal: w}
	// The common historical validator accepts a publication, but this projection
	// stays private. Never expose the retired owner as active routing authority.
	projected := source
	projected.Inventory.Publications = map[string]tunnel.ReceiveJournalPublication{machineUID: publication}
	var completion receiveRetirementCompletion
	if err := decodeRetirementEvidence(publication.RetirementCompletion, &completion); err != nil {
		return fail(err)
	}
	members := []string{machineUID}
	if completion.Version == receiveRetirementCohortCompletionVersion {
		if len(completion.Cohort) < 2 || !slices.Contains(completion.Cohort, machineUID) {
			return fail(fmt.Errorf("released joint completion has invalid membership"))
		}
		members = completion.Cohort
		for _, uid := range members {
			if uid == machineUID {
				continue
			}
			member, found := inventory.Released[uid]
			if !found {
				return fail(fmt.Errorf("released joint history omitted a member"))
			}
			memberStore := store
			memberStore.Name, memberStore.ExpectedUID = tunnel.ReceiveTransitionName(uid), member.JournalUID
			retained, err := memberStore.Load(ctx)
			if err != nil {
				return fail(err)
			}
			if err := tunnel.ValidateReceiveTransportHistory(ctx, memberStore, retained); err != nil {
				return fail(err)
			}
			if _, _, err := retainedReceiveWithdrawal(inventory, uid, retained, member); err != nil {
				return fail(err)
			}
			source.Journals[uid] = retained
			projected.Inventory.Publications[uid] = member
		}
	}
	for _, uid := range members {
		member := projected.Inventory.Publications[uid]
		var evidence receiveRetirementCompletion
		if err := decodeRetirementEvidence(member.RetirementCompletion, &evidence); err != nil {
			return fail(err)
		}
		if completion.Version == receiveRetirementCohortCompletionVersion && (evidence.Version != completion.Version || !slices.Equal(evidence.Cohort, members)) {
			return fail(fmt.Errorf("released joint completions disagree on membership"))
		}
		withdrawal, _, err := retainedReceiveWithdrawal(inventory, uid, source.Journals[uid], member)
		if err != nil {
			return fail(err)
		}
		projected.Withdrawal = withdrawal
		ok, err := validateReceiveRetirementFinalization(projected, store.ClusterUID, uid, inventoryUID, maxAge, time.Now())
		if err != nil {
			return fail(err)
		}
		if !ok {
			return fail(fmt.Errorf("released journal lacks verified historical finalization"))
		}
	}
	if err := source.Recheck(ctx, api, store); err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	return ReceiveReleasedJournal{Inventory: inventory, Metadata: *index.ObjectMeta.DeepCopy(), Journal: snapshot, Plan: plan, Withdrawal: w, source: source}, true, nil
}
