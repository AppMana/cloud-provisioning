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

// ReceiveReleasedJournalSet shares one historical admission and its revision
// fence. It is not an active allocation or current native absence permit.
type ReceiveReleasedJournalSet struct {
	Records map[string]ReceiveReleasedJournal
	source  ReceiveRetirementSet
}

func (s ReceiveReleasedJournalSet) Recheck(ctx context.Context, api kubernetes.Interface, store handover.Store) error {
	return s.source.Recheck(ctx, api, store)
}

// ReadReleasedReceiveJournal validates retained history and consumed evidence
// after active membership is removed. Later mesh data and consumer boots need
// not match an old withdrawal's captured state: this proves historical identity
// only. It cannot be used to clean up new owners, restore an old peer, release
// another reservation or assert current native absence. Missing records return
// false and a zero result. The frozen journal and its ancestry must still exist.
func ReadReleasedReceiveJournal(ctx context.Context, api kubernetes.Interface, store handover.Store, namespace, meshName, meshUID, machineUID, inventoryUID string, maxAge time.Duration) (ReceiveReleasedJournal, bool, error) {
	if machineUID == "" || maxAge <= 0 || store.Name != tunnel.ReceiveTransitionName(machineUID) {
		return ReceiveReleasedJournal{}, false, fmt.Errorf("released journal requires exact historical scope and policy")
	}
	set, err := readReleasedReceiveJournals(ctx, api, store, namespace, meshName, meshUID, inventoryUID, map[string]time.Duration{machineUID: maxAge}, 0)
	if err != nil {
		return ReceiveReleasedJournal{}, false, err
	}
	record, found := set.Records[machineUID]
	return record, found, nil
}

// ReadReleasedReceiveJournals admits all retained histories together. A zero
// maxAge selects each journal's frozen lifetime; a positive value requires that
// explicit historical policy, as with ReadReleasedReceiveJournal. Store.Name and ExpectedUID select no
// member here: the exact inventory binds every journal incarnation. Reads are
// scoped to this call; Recheck always goes back to the live API.
func ReadReleasedReceiveJournals(ctx context.Context, api kubernetes.Interface, store handover.Store, namespace, meshName, meshUID, inventoryUID string, maxAge time.Duration) (ReceiveReleasedJournalSet, error) {
	return readReleasedReceiveJournals(ctx, api, store, namespace, meshName, meshUID, inventoryUID, nil, maxAge)
}

func readReleasedReceiveJournals(ctx context.Context, api kubernetes.Interface, store handover.Store, namespace, meshName, meshUID, inventoryUID string, requested map[string]time.Duration, defaultAge time.Duration) (ReceiveReleasedJournalSet, error) {
	fail := func(err error) (ReceiveReleasedJournalSet, error) { return ReceiveReleasedJournalSet{}, err }
	if api == nil || store.ConfigMaps == nil || inventoryUID == "" || defaultAge < 0 {
		return fail(fmt.Errorf("released journal requires exact historical scope and policy"))
	}
	inventory, index, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, namespace, meshName, meshUID)
	if err != nil {
		return fail(err)
	}
	if string(index.UID) != inventoryUID {
		return fail(fmt.Errorf("released inventory incarnation changed"))
	}
	ages := map[string]time.Duration{}
	if requested == nil {
		for uid := range inventory.Released {
			ages[uid] = defaultAge
		}
	} else {
		for uid, age := range requested {
			publication, found := inventory.Released[uid]
			if !found {
				continue
			}
			if store.ExpectedUID != "" && store.ExpectedUID != publication.JournalUID {
				return fail(fmt.Errorf("released journal incarnation differs"))
			}
			ages[uid] = age
		}
	}
	result := ReceiveReleasedJournalSet{Records: map[string]ReceiveReleasedJournal{}}
	if len(ages) == 0 {
		return result, nil
	}
	pending := []string{}
	queued := map[string]bool{}
	for uid := range ages {
		pending = append(pending, uid)
		queued[uid] = true
	}
	slices.Sort(pending)
	journals := map[string]handover.Snapshot{}
	for pos := 0; pos < len(pending); pos++ {
		uid := pending[pos]
		publication, found := inventory.Released[uid]
		if !found {
			return fail(fmt.Errorf("released joint history omitted a member"))
		}
		memberStore := store
		memberStore.Name, memberStore.ExpectedUID = tunnel.ReceiveTransitionName(uid), publication.JournalUID
		snapshot, err := memberStore.Load(ctx)
		if err != nil {
			return fail(err)
		}
		if !snapshot.Frozen() {
			return fail(fmt.Errorf("released journal is not frozen"))
		}
		if err := tunnel.ValidateReceiveTransportHistory(ctx, memberStore, snapshot); err != nil {
			return fail(err)
		}
		journals[uid] = snapshot
		var completion receiveRetirementCompletion
		if err := decodeRetirementEvidence(publication.RetirementCompletion, &completion); err != nil {
			return fail(err)
		}
		if completion.Version == receiveRetirementCohortCompletionVersion {
			if len(completion.Cohort) < 2 || !slices.Contains(completion.Cohort, uid) {
				return fail(fmt.Errorf("released joint completion has invalid membership"))
			}
			for _, member := range completion.Cohort {
				if !queued[member] {
					pending = append(pending, member)
					queued[member] = true
				}
			}
		}
	}
	mesh, err := api.CoreV1().Secrets(namespace).Get(ctx, meshName, metav1.GetOptions{})
	if err != nil {
		return fail(err)
	}
	if string(mesh.UID) != meshUID || mesh.DeletionTimestamp != nil || mesh.ResourceVersion == "" {
		return fail(fmt.Errorf("released mesh incarnation changed"))
	}
	source := ReceiveRetirementSet{Inventory: inventory, Metadata: *index.ObjectMeta.DeepCopy(), MeshMetadata: *mesh.ObjectMeta.DeepCopy(), Mesh: mesh, Journals: journals}
	for uid, age := range ages {
		if age == 0 {
			view, err := journals[uid].Transition().View()
			if err != nil {
				return fail(err)
			}
			age = view.Round.MaxReceiptAge
		}
		record, err := releasedJournalFromSource(store, source, uid, age)
		if err != nil {
			return fail(err)
		}
		result.Records[uid] = record
	}
	if err := source.Recheck(ctx, api, store); err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	result.source = source
	return result, nil
}

func releasedJournalFromSource(store handover.Store, source ReceiveRetirementSet, machineUID string, maxAge time.Duration) (ReceiveReleasedJournal, error) {
	fail := func(err error) (ReceiveReleasedJournal, error) { return ReceiveReleasedJournal{}, err }
	publication := source.Inventory.Released[machineUID]
	snapshot := source.Journals[machineUID]
	w, plan, err := retainedReceiveWithdrawal(source.Inventory, machineUID, snapshot, publication)
	if err != nil {
		return fail(err)
	}
	source.Withdrawal = w
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
			member, found := source.Inventory.Released[uid]
			if !found {
				return fail(fmt.Errorf("released joint history omitted a member"))
			}
			retained, found := source.Journals[uid]
			if !found {
				return fail(fmt.Errorf("released joint journal omitted a member"))
			}
			if _, _, err := retainedReceiveWithdrawal(source.Inventory, uid, retained, member); err != nil {
				return fail(err)
			}
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
		withdrawal, _, err := retainedReceiveWithdrawal(source.Inventory, uid, source.Journals[uid], member)
		if err != nil {
			return fail(err)
		}
		projected.Withdrawal = withdrawal
		ok, err := validateReceiveRetirementFinalization(projected, store.ClusterUID, uid, string(source.Metadata.UID), maxAge, time.Now())
		if err != nil {
			return fail(err)
		}
		if !ok {
			return fail(fmt.Errorf("released journal lacks verified historical finalization"))
		}
	}
	return ReceiveReleasedJournal{Inventory: source.Inventory, Metadata: *source.Metadata.DeepCopy(), Journal: snapshot, Plan: plan, Withdrawal: w, source: source}, nil
}
