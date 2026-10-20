package attachment

import (
	"context"
	"fmt"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"k8s.io/client-go/kubernetes"
)

// ReceiptCollectionWithRelease consumes a fresh authenticated native-absence
// batch after historical finalization. It reuses the retirement receipt digest,
// which includes finalization, so the consumed pre-finalization batch cannot be
// replayed. Agents must checkpoint their durable withdrawal fence and finish
// native cleanup before observing this digest. Historical evidence is retained
// unchanged; this operation does not renew or replace consumed completion.
//
// The caller must admit current owner absence and compatible agent/controller
// and rollback releases in recheck. Construction does not enable live release.
// maxAge validates consumed historical evidence; observationAge bounds only the
// fresh release batch. Both are trusted controller policy, never agent input.
func (shared ReceiveRetirementSet) ReceiptCollectionWithRelease(ctx context.Context, api kubernetes.Interface, store handover.Store, policies map[string]handover.PodReceiptAuthenticator, maxAge, observationAge time.Duration, recheck func() error) (handover.ReceiptCollection, error) {
	if recheck == nil {
		return handover.ReceiptCollection{}, fmt.Errorf("release collection requires current release authority")
	}
	release := func(ctx context.Context, finalCheck func() error) error {
		check := func(ReceiveRetirementSet) error {
			if err := recheck(); err != nil {
				return err
			}
			return shared.Recheck(ctx, api, store)
		}
		return releaseFinalizedReceiveRetirement(ctx, api, store, shared.Mesh.Namespace, shared.Mesh.Name, string(shared.Mesh.UID), shared.Withdrawal.MachineUID, string(shared.Metadata.UID), maxAge, check, finalCheck)
	}
	return shared.receiptCollectionWithRelease(ctx, api, store, policies, maxAge, observationAge, recheck, release)
}

// ReceiptCollectionWithCohortRelease requires a fresh joint native observation
// from every survivor after all members finalized. check independently admits
// current absence and compatible recovery for each departing owner. This API is
// explicit: ordinary single-owner release policy cannot authorize other owners.
func (shared ReceiveRetirementSet) ReceiptCollectionWithCohortRelease(ctx context.Context, api kubernetes.Interface, store handover.Store, policies map[string]handover.PodReceiptAuthenticator, maxAge, observationAge time.Duration, check func(ReceiveRetirementSet) error) (handover.ReceiptCollection, error) {
	if check == nil {
		return handover.ReceiptCollection{}, fmt.Errorf("joint release collection requires per-member authority")
	}
	members, err := shared.finalizedRetirementCohort(store.ClusterUID, string(shared.Metadata.UID), maxAge)
	if err != nil {
		return handover.ReceiptCollection{}, err
	}
	recheck := func() error {
		for _, member := range members {
			if err := check(member); err != nil {
				return err
			}
		}
		return ctx.Err()
	}
	release := func(ctx context.Context, finalCheck func() error) error {
		boundCheck := func() error {
			// Pin the exact source used for authentication, even if release
			// admission observes a newer inventory or journal revision.
			if err := shared.Recheck(ctx, api, store); err != nil {
				return err
			}
			return finalCheck()
		}
		return releaseFinalizedReceiveRetirementCohort(ctx, api, store, shared.Mesh.Namespace, shared.Mesh.Name, string(shared.Mesh.UID), shared.Withdrawal.MachineUID, string(shared.Metadata.UID), maxAge, check, boundCheck)
	}
	return shared.receiptCollectionWithRelease(ctx, api, store, policies, maxAge, observationAge, recheck, release)
}

func (shared ReceiveRetirementSet) receiptCollectionWithRelease(ctx context.Context, api kubernetes.Interface, store handover.Store, policies map[string]handover.PodReceiptAuthenticator, maxAge, observationAge time.Duration, recheck func() error, release func(context.Context, func() error) error) (handover.ReceiptCollection, error) {
	machineUID := shared.Withdrawal.MachineUID
	verified, err := validateReceiveRetirementFinalization(shared, store.ClusterUID, machineUID, string(shared.Metadata.UID), maxAge, time.Now())
	if err != nil {
		return handover.ReceiptCollection{}, err
	}
	if !verified {
		return handover.ReceiptCollection{}, fmt.Errorf("release collection requires verified finalization")
	}
	target, err := shared.ReceiptCollection(ctx, api, store, policies, observationAge, recheck)
	if err != nil {
		return handover.ReceiptCollection{}, err
	}
	scope, err := NewReceiveRetirementReceiptScope(ctx, store.ConfigMaps, store.ClusterUID, string(shared.Metadata.UID), shared.Journals[machineUID], shared.Mesh)
	if err != nil {
		return handover.ReceiptCollection{}, err
	}
	if scope.Digest() != target.Digest {
		return handover.ReceiptCollection{}, fmt.Errorf("release collection source changed")
	}
	target.Complete = func(ctx context.Context, entries []handover.PodReceipt) error {
		if err := recheck(); err != nil {
			return err
		}
		if err := shared.Recheck(ctx, api, store); err != nil {
			return err
		}
		proofs, err := scope.AuthenticateComplete(ctx, entries, policies, observationAge)
		if err != nil {
			return err
		}
		finalCheck := func() error {
			for _, proof := range proofs {
				policy := policies[proof.AuthenticatedNodeUID]
				if err := handover.VerifyObservationIncarnation(ctx, policy.API, policy.Namespace, scope.nodes[proof.AuthenticatedNodeUID], proof.Receipt); err != nil {
					return err
				}
			}
			return scope.validateSubmissions(entries, time.Now(), observationAge)
		}
		return release(ctx, finalCheck)
	}
	return target, nil
}
