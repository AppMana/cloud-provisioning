package tunnel

import (
	"fmt"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
)

// ReceivePreparePredecessor identifies terminal authority for additive successor
// preparation. It is not permission to switch routes or retire the predecessor.
// JournalSHA256 includes terminal receipts, not just the public plan.
type ReceivePreparePredecessor struct {
	ClusterUID    string `json:"clusterUID"`
	MachineUID    string `json:"machineUID"`
	JournalUID    string `json:"journalUID"`
	JournalSHA256 string `json:"journalSHA256"`
}

func NewReceivePreparePredecessor(snapshot handover.Snapshot, clusterUID string) (ReceivePreparePredecessor, error) {
	plan, err := DecodeReceiveTransition(snapshot, clusterUID)
	if err != nil {
		return ReceivePreparePredecessor{}, err
	}
	view, err := snapshot.Transition().View()
	if err != nil {
		return ReceivePreparePredecessor{}, err
	}
	meta := snapshot.Metadata()
	if !view.Complete || meta.UID == "" || meta.ResourceVersion == "" {
		return ReceivePreparePredecessor{}, fmt.Errorf("successor preparation requires a completed versioned journal")
	}
	raw, err := snapshot.Transition().Bytes()
	if err != nil {
		return ReceivePreparePredecessor{}, err
	}
	return ReceivePreparePredecessor{ClusterUID: clusterUID, MachineUID: plan.MachineUID, JournalUID: string(meta.UID), JournalSHA256: HashPeerList(raw)}, nil
}

// ValidatePredecessor checks exact terminal authority and cloud identity before
// additive work. The caller must supply a fresh live snapshot and recheck before
// publishing readiness. Native ownership and inventory admission remain required.
func (r ReceivePrepare) ValidatePredecessor(snapshot handover.Snapshot) (ReceiveTransitionPlan, error) {
	fail := func(err error) (ReceiveTransitionPlan, error) { return ReceiveTransitionPlan{}, err }
	if r.Predecessor == nil {
		return fail(fmt.Errorf("missing preparation predecessor"))
	}
	if _, err := r.ReceiveScope(); err != nil {
		return fail(err)
	}
	actual, err := NewReceivePreparePredecessor(snapshot, r.Predecessor.ClusterUID)
	if err != nil {
		return fail(err)
	}
	if actual != *r.Predecessor {
		return fail(fmt.Errorf("preparation predecessor changed"))
	}
	plan, err := DecodeReceiveTransition(snapshot, actual.ClusterUID)
	if err != nil {
		return fail(err)
	}
	if r.Version != 1 || r.ReceiverGeneration == "" || r.ReceiverGeneration == plan.ReceiverGeneration || r.MeshUID != plan.MeshUID || r.NodeName != plan.CloudNodeName || r.NodeUID != plan.CloudNodeUID || r.RootPublicKey != plan.RootPublicKey || r.BaseHash != HashPeerList(plan.Base) {
		return fail(fmt.Errorf("successor preparation identity differs from its predecessor"))
	}
	for _, port := range r.Ports {
		for _, occupied := range plan.Ports {
			if port == occupied {
				return fail(fmt.Errorf("successor preparation reuses a predecessor listener"))
			}
		}
	}
	return plan, nil
}
