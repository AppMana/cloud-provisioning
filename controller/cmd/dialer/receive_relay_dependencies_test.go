package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func TestRelaySwitchRequiresCurrentSuccessorObservation(t *testing.T) {
	key := func() string {
		k, err := wgtypes.GeneratePrivateKey()
		if err != nil {
			t.Fatal(err)
		}
		return k.PublicKey().String()
	}
	at := time.Now()
	tr, err := handover.NewTransition(handover.Spec{ClusterUID: "cluster", TransitionUID: "transition", PlanHash: strings.Repeat("a", 64), FromGeneration: "old", ToGeneration: "new", Phase: handover.Prepare, Members: []handover.Member{{NodeUID: "old", FromKey: key(), ToKey: key()}, {NodeUID: "new", FromKey: key(), ToKey: key()}}}, at, handover.Policy{RoundLifetime: time.Minute, MaxReceiptAge: 10 * time.Second, DrainInterval: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	proofs := func(v handover.View, now time.Time, node string) []handover.AuthenticatedReceipt {
		hash, err := v.Round.Digest()
		if err != nil {
			t.Fatal(err)
		}
		var out []handover.AuthenticatedReceipt
		for _, m := range v.Round.Members {
			if node != "" && node != m.NodeUID {
				continue
			}
			out = append(out, handover.AuthenticatedReceipt{AuthenticatedNodeUID: m.NodeUID, Receipt: handover.Receipt{NodeUID: m.NodeUID, DeviceID: m.DeviceID, RoundHash: hash, FromKey: m.FromKey, ToKey: m.ToKey, FromPeerKey: m.FromPeerKey, ToPeerKey: m.ToPeerKey, ObservedAt: now}})
		}
		return out
	}
	v, _ := tr.View()
	tr, err = tr.Advance(at.Add(time.Second), proofs(v, at.Add(time.Second), ""))
	if err != nil {
		t.Fatal(err)
	}
	plan := tunnel.ReceiveTransitionPlan{MachineUID: "worker", Sites: map[string]tunnel.ReceiveTransitionSite{
		"old": {NodeUID: "old", Legacy: &tunnel.ReceiveSiteLegacy{PeerPublicKey: "old"}, Forward: &tunnel.ReceiveSitePath{PeerPublicKey: "new", Transit: &tunnel.TransitSpec{Via: "10.90.0.2"}}},
		"new": {NodeUID: "new", Legacy: &tunnel.ReceiveSiteLegacy{PeerPublicKey: "old", Transit: &tunnel.TransitSpec{Via: "10.90.0.1"}}, Forward: &tunnel.ReceiveSitePath{PeerPublicKey: "new"}},
	}}
	intent := tunnel.ReceiveJournalRoutes{Plans: map[string]tunnel.ReceiveTransitionPlan{"worker": plan}, Legacy: map[string]tunnel.ReceiveTransitionPlan{}}
	for _, reverse := range []bool{false, true} {
		predecessor, successor := "old", "new"
		if reverse {
			tr, err = tr.Rollback(at.Add(4 * time.Second))
			if err != nil {
				t.Fatal(err)
			}
			predecessor, successor = "new", "old"
			intent.Legacy["worker"] = plan
		}
		v, _ = tr.View()
		now := at.Add(5 * time.Second)
		intent.Views = map[string]handover.View{"worker": v}
		if err := verifyReceiveRelayDependencies(intent, predecessor, "worker", v, now); err == nil {
			t.Fatal("follower switched before relay")
		}
		if err := verifyReceiveRelayDependencies(intent, successor, "worker", v, now); err != nil {
			t.Fatal("direct successor was blocked", err)
		}
		observedAt := at.Add(2 * time.Second)
		if reverse {
			observedAt = at.Add(5 * time.Second)
		}
		tr, err = tr.Observe(observedAt, proofs(v, observedAt, successor))
		if err != nil {
			t.Fatal(err)
		}
		v, _ = tr.View()
		intent.Views["worker"] = v
		if err := verifyReceiveRelayDependencies(intent, predecessor, "worker", v, now); err != nil {
			t.Fatal("verified successor rejected", err)
		}
		checks := 0
		restarted := func(owner tunnel.ReceiveTransitionSite, receipt handover.Receipt) error {
			checks++
			if owner.NodeUID != successor || receipt.NodeUID != successor {
				t.Fatal("checked the wrong relay")
			}
			return fmt.Errorf("relay restarted")
		}
		if err := visitReceiveRelayDependencies(intent, predecessor, "worker", v, now, restarted); err == nil || checks != 1 {
			t.Fatal("restarted relay did not block follower", err, checks)
		}
		if err := visitReceiveRelayDependencies(intent, successor, "worker", v, now, restarted); err != nil || checks != 1 {
			t.Fatal("direct owner unexpectedly depends on follower", err, checks)
		}
		if err := verifyReceiveRelayDependencies(intent, predecessor, "worker", v, at.Add(30*time.Second)); err == nil {
			t.Fatal("stale successor accepted")
		}
		delete(intent.Views, "worker")
		if err := verifyReceiveRelayDependencies(intent, predecessor, "worker", v, now); err == nil {
			t.Fatal("missing committed evidence accepted")
		}
	}
}
