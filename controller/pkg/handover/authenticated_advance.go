package handover

import (
	"context"
	"fmt"
	"time"
)

// PodReceipt is a direct transport submission, not a journal record. Bearer
// credentials must remain in request memory and must never be persisted.
type PodReceipt struct {
	BearerToken string  `json:"-"`
	BootID      string  `json:"bootID"`
	Receipt     Receipt `json:"receipt"`
}

// AdvanceWithPodReceipts authenticates a complete round against current API
// identities, then atomically commits its successor and verified receipts.
// policies is trusted controller configuration keyed by Node UID, not input
// supplied by senders. The caller must validate the public transport plan and
// permit phase execution before invoking this method. Native verification is
// the responsibility of the sending agents; token validation cannot prove it.
//
// This never retries a conflict, renews an expired round, or publishes an
// uncommitted successor. A caller may publish only the returned snapshot.
func (s Store) AdvanceWithPodReceipts(ctx context.Context, expectedRoundHash string, submissions []PodReceipt, policies map[string]PodReceiptAuthenticator) (Snapshot, error) {
	previous, err := s.Load(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	view, err := previous.Transition().View()
	if err != nil {
		return Snapshot{}, err
	}
	digest, err := view.Round.Digest()
	if err != nil || expectedRoundHash == "" || digest != expectedRoundHash || view.Complete {
		return Snapshot{}, fmt.Errorf("receipt submission does not target the current unfinished round")
	}
	if len(submissions) != len(view.Round.Members) {
		return Snapshot{}, fmt.Errorf("receipt submission does not cover the complete round")
	}
	members := make(map[[2]string]bool, len(view.Round.Members))
	for _, member := range view.Round.Members {
		members[[2]string{member.NodeUID, member.DeviceID}] = true
	}
	proofs := make([]AuthenticatedReceipt, 0, len(submissions))
	for _, submission := range submissions {
		id := [2]string{submission.Receipt.NodeUID, submission.Receipt.DeviceID}
		policy, found := policies[id[0]]
		if !members[id] || !found || submission.Receipt.RoundHash != digest {
			return Snapshot{}, fmt.Errorf("receipt submission has a missing, duplicate or unexpected member")
		}
		delete(members, id)
		proof, err := policy.Authenticate(ctx, submission.BearerToken, submission.Receipt, submission.BootID)
		if err != nil {
			return Snapshot{}, err
		}
		proofs = append(proofs, proof)
	}
	// Time is sampled after authentication: slow API calls cannot extend a
	// round's validity or turn an observation that aged out into current proof.
	next, err := previous.Transition().Advance(time.Now(), proofs)
	if err != nil {
		return Snapshot{}, err
	}
	return s.Commit(ctx, previous, next)
}
