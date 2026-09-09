package handover

import (
	"encoding/json"
	"testing"
	"time"
)

func transitionFixture(t *testing.T) Transition {
	t.Helper()
	tr, err := NewTransition(fixture(t).Spec, epoch, Policy{time.Minute, 10 * time.Second, 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return tr
}
func view(t *testing.T, tr Transition) View {
	t.Helper()
	v, e := tr.View()
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func step(t *testing.T, tr Transition, at time.Time) Transition {
	t.Helper()
	next, e := tr.Advance(at, receipts(t, view(t, tr).Round, at))
	if e != nil {
		t.Fatal(e)
	}
	raw, e := next.Bytes()
	if e != nil {
		t.Fatal(e)
	}
	restarted, e := DecodeTransition(raw)
	if e != nil {
		t.Fatal(e)
	}
	return restarted
}
func TestTransitionRestartDrainAndCompletion(t *testing.T) {
	tr := transitionFixture(t)
	for i, phase := range []Phase{Switch, Drain} {
		tr = step(t, tr, epoch.Add(time.Duration(i+1)*time.Second))
		if view(t, tr).Round.Phase != phase {
			t.Fatal("wrong phase")
		}
	}
	v := view(t, tr)
	if !v.DrainUntil.Equal(epoch.Add(7 * time.Second)) {
		t.Fatal("incorrect drain deadline")
	}
	for _, tc := range []struct {
		name          string
		now, observed time.Time
	}{
		{"early", epoch.Add(6 * time.Second), epoch.Add(6 * time.Second)},
		{"predeadline-receipts", v.DrainUntil, epoch.Add(6 * time.Second)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, e := tr.Advance(tc.now, receipts(t, v.Round, tc.observed)); e == nil {
				t.Fatal("premature retirement")
			}
		})
	}
	if _, e := tr.Advance(v.DrainUntil, nil); e == nil {
		t.Fatal("timer retired without receipts")
	}
	tr = step(t, tr, v.DrainUntil)
	if _, e := tr.Rollback(v.DrainUntil); e == nil {
		t.Fatal("rollback after retirement authorized")
	}
	tr = step(t, tr, v.DrainUntil.Add(time.Second))
	final := view(t, tr)
	if !final.Complete || final.ServingGeneration != "B" {
		t.Fatal(final)
	}
	if _, e := tr.Advance(v.DrainUntil.Add(time.Second), nil); e == nil {
		t.Fatal("advanced terminal state")
	}
	if _, e := tr.Renew(epoch.Add(time.Hour)); e == nil {
		t.Fatal("renewed terminal state")
	}
}
func TestRollbackFromEachRecoverablePhase(t *testing.T) {
	for _, phase := range []Phase{Prepare, Switch, Drain} {
		t.Run(string(phase), func(t *testing.T) {
			tr := transitionFixture(t)
			at := epoch
			for view(t, tr).Round.Phase != phase {
				at = at.Add(time.Second)
				tr = step(t, tr, at)
			}
			before := view(t, tr).Round
			at = at.Add(time.Second)
			next, e := tr.Rollback(at)
			if e != nil {
				t.Fatal(e)
			}
			v := view(t, next)
			if !v.RollbackRequested || !v.DrainUntil.IsZero() {
				t.Fatal(v)
			}
			if _, e := next.Rollback(at); e == nil {
				t.Fatal("repeated rollback")
			}
			if _, e := next.Advance(at, receipts(t, before, at)); e == nil {
				t.Fatal("forward receipts authorized rollback")
			}
			if phase == Prepare {
				if v.Round.Phase != Abort {
					t.Fatal(v)
				}
			} else {
				if v.Round.Phase != Switch || v.Round.FromGeneration != "B" || v.Round.ToGeneration != "A" || v.Round.Members[0].ToKey != before.Members[0].FromKey {
					t.Fatal("rollback direction")
				}
			}
			for !view(t, next).Complete {
				v = view(t, next)
				at = at.Add(time.Second)
				if v.Round.Phase == Drain {
					at = v.DrainUntil
				}
				next = step(t, next, at)
			}
			if view(t, next).ServingGeneration != "A" {
				t.Fatal("rollback lost A")
			}
		})
	}
}
func TestRenewRequiresNewReceiptsAndPreservesDrain(t *testing.T) {
	tr := transitionFixture(t)
	tr = step(t, tr, epoch.Add(time.Second))
	tr = step(t, tr, epoch.Add(2*time.Second))
	old := view(t, tr)
	if _, e := tr.Renew(old.Round.ExpiresAt.Add(-time.Nanosecond)); e == nil {
		t.Fatal("early renewal")
	}
	next, e := tr.Renew(old.Round.ExpiresAt)
	if e != nil {
		t.Fatal(e)
	}
	v := view(t, next)
	if v.Round.Nonce == old.Round.Nonce || !v.DrainUntil.Equal(old.DrainUntil) {
		t.Fatal("renewal changed drain or reused nonce")
	}
	if _, e := next.Advance(v.Round.IssuedAt, receipts(t, old.Round, old.Round.ExpiresAt.Add(-time.Second))); e == nil {
		t.Fatal("renewal replay")
	}
	next = step(t, next, v.Round.IssuedAt)
	if view(t, next).Round.Phase != Retire {
		t.Fatal("cannot finish renewed drain")
	}
}
func TestTransitionRejectsCorruptHistory(t *testing.T) {
	tr := transitionFixture(t)
	tr = step(t, tr, epoch.Add(time.Second))
	for _, tc := range []struct {
		name   string
		change func(*journal)
	}{
		{"version", func(j *journal) { j.Version = 2 }},
		{"policy", func(j *journal) { j.Policy.DrainInterval = 0 }},
		{"skip-phase", func(j *journal) { j.Events[0].Next.Phase = Retire }},
		{"missing-proof", func(j *journal) { j.Events[0].Receipts = nil }},
		{"backdate", func(j *journal) { j.Events[0].At = epoch.Add(-time.Second) }},
		{"reuse-nonce", func(j *journal) { j.Events[0].Next.Nonce = j.Initial.Nonce }},
		{"changed-membership", func(j *journal) { j.Events[0].Next.Members[0].NodeUID = "replacement" }},
		{"missing-next", func(j *journal) { j.Events[0].Next = nil }},
		{"unknown-operation", func(j *journal) { j.Events[0].Operation = "skip" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := tr.Bytes()
			var j journal
			if e := json.Unmarshal(raw, &j); e != nil {
				t.Fatal(e)
			}
			tc.change(&j)
			raw, _ = json.Marshal(j)
			if _, e := DecodeTransition(raw); e == nil {
				t.Fatal("accepted corrupt journal")
			}
		})
	}
	raw, _ := tr.Bytes()
	for _, bad := range [][]byte{[]byte("null"), append(raw, []byte(" {}")...), []byte(`{"unknown":true}`)} {
		if _, e := DecodeTransition(bad); e == nil {
			t.Fatal("accepted invalid JSON state")
		}
	}
	before, _ := tr.Bytes()
	v := view(t, tr)
	v.Round.Members[0].NodeUID = "mutated"
	_, _ = tr.Rollback(epoch.Add(2 * time.Second))
	after, _ := tr.Bytes()
	if string(before) != string(after) {
		t.Fatal("candidate mutated committed state")
	}
}
