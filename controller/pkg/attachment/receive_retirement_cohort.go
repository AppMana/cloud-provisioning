package attachment

import (
	"context"
	"fmt"
	"reflect"
	"slices"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
)

// ReceiveRetirementCohort is joint cleanup intent, not completion. Captured
// consumers are preserved in each withdrawal. Every returned consumer must
// verify native absence for every member before issuing a joint observation.
type ReceiveRetirementCohort struct {
	MachineUIDs []string
	Consumers   []ConsumerTarget
}

// RetirementCohort admits only already-published withdrawals with the same exact
// original capture. It never uses Node readiness or provider absence. Each member
// must capture all other members by their committed identities and have exactly
// the same remaining consumers. An unpublished withdrawal stays a consumer.
func (s ReceiveRetirementSet) RetirementCohort() (ReceiveRetirementCohort, error) {
	target := s.Withdrawal.MachineUID
	record, err := decodeReceiveWithdrawalRecord(s.Inventory.Publications[target].Retirement, target)
	if err != nil {
		return ReceiveRetirementCohort{}, err
	}
	members := []string{target}
	if record.Version == 2 && !record.Withdrawal.Orphaned {
		for uid, pub := range s.Inventory.Publications {
			if uid == target || len(pub.Retirement) == 0 {
				continue
			}
			other, err := decodeReceiveWithdrawalRecord(pub.Retirement, uid)
			if err != nil {
				return ReceiveRetirementCohort{}, err
			}
			if other.Version != 2 || other.Withdrawal.Orphaned || other.Withdrawal.SourceDataHash != record.Withdrawal.SourceDataHash {
				continue
			}
			journal, ok := s.Journals[uid]
			if !ok {
				return ReceiveRetirementCohort{}, fmt.Errorf("joint retirement omitted a candidate journal")
			}
			if _, err := ReadReceiveWithdrawal(s.Inventory, uid, journal, s.Mesh); err != nil {
				if _, err := ReadPreparedReceiveWithdrawal(s.Inventory, uid, journal, s.Mesh); err != nil {
					return ReceiveRetirementCohort{}, err
				}
				continue
			}
			members = append(members, uid)
		}
	}
	slices.Sort(members)
	return s.retirementCohortFor(members)
}

// The explicit member set is also used to validate historically consumed joint
// evidence. Later withdrawals do not retroactively change an old observation.
func (s ReceiveRetirementSet) retirementCohortFor(members []string) (ReceiveRetirementCohort, error) {
	fail := func(err error) (ReceiveRetirementCohort, error) { return ReceiveRetirementCohort{}, err }
	if len(members) == 0 || !slices.IsSorted(members) || !slices.Contains(members, s.Withdrawal.MachineUID) {
		return fail(fmt.Errorf("invalid joint retirement member set"))
	}
	plans := map[string]tunnel.ReceiveTransitionPlan{}
	withdrawals := map[string]RemoteWithdrawalIntent{}
	sourceHash := ""
	for i, uid := range members {
		if uid == "" || (i > 0 && members[i-1] == uid) {
			return fail(fmt.Errorf("duplicate joint retirement member"))
		}
		snapshot, ok := s.Journals[uid]
		if !ok {
			return fail(fmt.Errorf("joint retirement journal missing"))
		}
		w, err := ReadReceiveWithdrawal(s.Inventory, uid, snapshot, s.Mesh)
		if err != nil {
			return fail(err)
		}
		plan, err := tunnel.DecodeReceiveTransition(snapshot, s.Inventory.ClusterUID)
		if err != nil {
			return fail(err)
		}
		if _, exists := plans[plan.CloudNodeUID]; exists {
			return fail(fmt.Errorf("joint retirement reuses a worker identity"))
		}
		if len(members) > 1 {
			record, err := decodeReceiveWithdrawalRecord(s.Inventory.Publications[uid].Retirement, uid)
			if err != nil {
				return fail(err)
			}
			if record.Version != 2 || w.Orphaned || w.SourceDataHash == "" {
				return fail(fmt.Errorf("joint retirement requires exact captured withdrawals"))
			}
			if sourceHash == "" {
				sourceHash = w.SourceDataHash
			} else if sourceHash != w.SourceDataHash {
				return fail(fmt.Errorf("joint retirement captures differ"))
			}
		}
		plans[plan.CloudNodeUID] = plan
		withdrawals[uid] = w
	}
	var consumers []ConsumerTarget
	for _, uid := range members {
		outside := []ConsumerTarget{}
		departing := map[string]bool{}
		for _, c := range withdrawals[uid].Consumers {
			owner, retiring := plans[c.NodeUID]
			if !retiring {
				outside = append(outside, c)
				continue
			}
			if owner.MachineUID == uid || c.Site || c.NodeName != owner.CloudNodeName || c.MachineName != owner.MachineName || c.PublicKey != owner.RootPublicKey || c.SecretUID != owner.AdoptionUID || departing[c.NodeUID] {
				return fail(fmt.Errorf("joint retirement consumer does not match its departing owner"))
			}
			departing[c.NodeUID] = true
		}
		if len(departing) != len(members)-1 || len(outside) == 0 {
			return fail(fmt.Errorf("joint retirement lacks complete captured membership or surviving consumers"))
		}
		slices.SortFunc(outside, func(a, b ConsumerTarget) int {
			if a.NodeUID < b.NodeUID {
				return -1
			}
			if a.NodeUID > b.NodeUID {
				return 1
			}
			return 0
		})
		if consumers == nil {
			consumers = outside
		} else if !reflect.DeepEqual(consumers, outside) {
			return fail(fmt.Errorf("joint retirement surviving consumer sets differ"))
		}
	}
	return ReceiveRetirementCohort{MachineUIDs: slices.Clone(members), Consumers: slices.Clone(consumers)}, nil
}

// Receipt scopes do not carry an API client. Load matching candidate journals
// from the same controller-owned storage, validating ancestry and publication.
// Callers still perform their full source admission and boundary rechecks.
func loadReceiptRetirementCohort(ctx context.Context, store handover.Store, s ReceiveRetirementSet) (ReceiveRetirementCohort, error) {
	target := s.Withdrawal.MachineUID
	record, err := decodeReceiveWithdrawalRecord(s.Inventory.Publications[target].Retirement, target)
	if err != nil {
		return ReceiveRetirementCohort{}, err
	}
	if record.Version == 2 && !record.Withdrawal.Orphaned {
		for uid, pub := range s.Inventory.Publications {
			if uid == target || len(pub.Retirement) == 0 {
				continue
			}
			other, err := decodeReceiveWithdrawalRecord(pub.Retirement, uid)
			if err != nil {
				return ReceiveRetirementCohort{}, err
			}
			if other.Version != 2 || other.Withdrawal.Orphaned || other.Withdrawal.SourceDataHash != record.Withdrawal.SourceDataHash {
				continue
			}
			member := store
			member.Name = tunnel.ReceiveTransitionName(uid)
			member.ExpectedUID = pub.JournalUID
			snapshot, err := member.Load(ctx)
			if err != nil {
				return ReceiveRetirementCohort{}, err
			}
			if err := tunnel.ValidateReceiveTransportHistory(ctx, member, snapshot); err != nil {
				return ReceiveRetirementCohort{}, err
			}
			s.Journals[uid] = snapshot
		}
	}
	return s.RetirementCohort()
}

// Historical evidence names its original members explicitly. A later withdrawal
// must not silently expand the obligations that were authenticated previously.
func (s ReceiveRetirementSet) completionConsumers(completion receiveRetirementCompletion) ([]ConsumerTarget, error) {
	if completion.Version != receiveRetirementCohortCompletionVersion {
		if len(completion.Cohort) != 0 {
			return nil, fmt.Errorf("legacy completion cannot name a retirement cohort")
		}
		return s.Withdrawal.Consumers, nil
	}
	if len(completion.Cohort) < 2 {
		return nil, fmt.Errorf("joint completion requires multiple members")
	}
	cohort, err := s.retirementCohortFor(completion.Cohort)
	if err != nil {
		return nil, err
	}
	return cohort.Consumers, nil
}
