package tunnel

import (
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
)

// Selection only: these synthetic views do not authenticate journal history or
// demonstrate native route changes, traffic continuity or recovery deadlines.
func TestSuccessorTransportRejectsRootAndUnknownGeneration(t *testing.T) {
	old := ReceiveTransitionPlan{ReceiverGeneration: "old"}
	p := ReceiveTransitionPlan{ReceiverGeneration: "new", PreviousReceiver: &ReceivePreviousReceiver{Generation: "receivers:old", Plan: &old}}
	for _, generation := range []string{"legacy:" + HashPeerList(p.Base), "receivers:unknown", ""} {
		v := handover.View{Complete: true, ServingGeneration: generation}
		v.Round.PlanHash = "new"
		if _, _, err := p.SelectedReceiveTransport(v); err == nil {
			t.Fatalf("accepted generation %q outside successor graph", generation)
		}
	}
	v := handover.View{Complete: true, ServingGeneration: "receivers:new"}
	v.Round.PlanHash = "new"
	p.PreviousReceiver.Plan = nil
	if _, _, err := p.SelectedReceiveTransport(v); err == nil {
		t.Fatal("accepted missing predecessor graph")
	}
}
