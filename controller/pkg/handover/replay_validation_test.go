package handover

import (
	"fmt"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	"strings"
	"testing"
	"time"
)

func replayValidationFixture(t testing.TB) Transition {
	t.Helper()
	members := make([]Member, 7)
	for i := range members {
		var from, to wgtypes.Key
		from[0], to[0] = byte(i*2+1), byte(i*2+2)
		members[i] = Member{NodeUID: fmt.Sprintf("node-%d", i), FromKey: from.String(), ToKey: to.String()}
	}
	tr, err := NewTransition(Spec{ClusterUID: "cluster", TransitionUID: "transition", PlanHash: strings.Repeat("a", 64), FromGeneration: "A", ToGeneration: "B", Phase: Prepare, Members: members}, epoch, Policy{time.Minute, 10 * time.Second, time.Second})
	if err != nil {
		t.Fatal(err)
	}
	at := epoch
	for phase := 0; phase < 2; phase++ {
		v, err := tr.View()
		if err != nil {
			t.Fatal(err)
		}
		digest, err := v.Round.Digest()
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 32; i++ {
			at = at.Add(10 * time.Millisecond)
			proofs := make([]AuthenticatedReceipt, len(v.Round.Members))
			for j, m := range v.Round.Members {
				proofs[j] = AuthenticatedReceipt{AuthenticatedNodeUID: m.NodeUID, Receipt: Receipt{NodeUID: m.NodeUID, FromKey: m.FromKey, ToKey: m.ToKey, RoundHash: digest, ObservedAt: at}}
			}
			if phase == 0 && i == 31 {
				tr, err = tr.Advance(at, proofs)
			} else {
				tr, err = tr.Observe(at, proofs)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	return tr
}

func TestReplayValidatesEveryEventAndRound(t *testing.T) {
	tr := replayValidationFixture(t)
	raw, err := tr.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Transition)
	}{
		{"previous round binding", func(v *Transition) { v.j.Events[63].Receipts[0].RoundHash = v.j.Events[0].Receipts[0].RoundHash }},
		{"late event wrong member", func(v *Transition) { v.j.Events[63].Receipts[0].NodeUID = "replacement" }},
		{"late event wrong key", func(v *Transition) { v.j.Events[63].Receipts[0].FromKey = v.j.Events[63].Receipts[0].ToKey }},
		{"late duplicate", func(v *Transition) { v.j.Events[63].Receipts[1] = v.j.Events[63].Receipts[0] }},
		{"future observation", func(v *Transition) { v.j.Events[63].Receipts[0].ObservedAt = v.j.Events[63].At.Add(time.Second) }},
		{"expired round", func(v *Transition) { v.j.Events[63].At = epoch.Add(2 * time.Minute) }},
		{"invalid next membership", func(v *Transition) { v.j.Events[31].Next.Members[0].FromKey = "invalid" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate, err := DecodeTransition(raw)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = candidate.View(); err != nil {
				t.Fatal(err)
			}
			tc.mutate(&candidate)
			if _, err = candidate.View(); err == nil {
				t.Fatal("accepted corrupted history after successful replay")
			}
		})
	}
}

func BenchmarkTransitionReceiptHistory(b *testing.B) {
	tr := replayValidationFixture(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := tr.View(); err != nil {
			b.Fatal(err)
		}
	}
}
