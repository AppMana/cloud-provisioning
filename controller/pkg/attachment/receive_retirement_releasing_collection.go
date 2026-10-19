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
func (shared ReceiveRetirementSet) ReceiptCollectionWithRelease(ctx context.Context, api kubernetes.Interface, store handover.Store, policies map[string]handover.PodReceiptAuthenticator, maxAge time.Duration, recheck func() error) (handover.ReceiptCollection, error) {
	machineUID := shared.Withdrawal.MachineUID
	verified, err := validateReceiveRetirementFinalization(shared, store.ClusterUID, machineUID, string(shared.Metadata.UID), maxAge, time.Now())
	if err != nil {
		return handover.ReceiptCollection{}, err
	}
	if !verified {
		return handover.ReceiptCollection{}, fmt.Errorf("release collection requires verified finalization")
	}
	target, err := shared.ReceiptCollection(ctx, api, store, policies, maxAge, recheck)
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
		proofs, err := scope.AuthenticateComplete(ctx, entries, policies, maxAge)
		if err != nil {
			return err
		}
		check := func(ReceiveRetirementSet) error {
			if err := recheck(); err != nil {
				return err
			}
			// Pin the source used for authentication, not only the later source read
			// by release admission. Any intervening inventory or mesh write fences it.
			return shared.Recheck(ctx, api, store)
		}
		finalCheck := func() error {
			for _, proof := range proofs {
				policy := policies[proof.AuthenticatedNodeUID]
				if err := handover.VerifyObservationIncarnation(ctx, policy.API, policy.Namespace, scope.nodes[proof.AuthenticatedNodeUID], proof.Receipt); err != nil {
					return err
				}
			}
			return scope.validateSubmissions(entries, time.Now(), maxAge)
		}
		return releaseFinalizedReceiveRetirement(ctx, api, store, shared.Mesh.Namespace, shared.Mesh.Name, string(shared.Mesh.UID), machineUID, string(shared.Metadata.UID), maxAge, check, finalCheck)
	}
	return target, nil
}
