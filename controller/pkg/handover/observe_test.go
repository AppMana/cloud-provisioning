package handover

import (
	"reflect"
	"testing"
	"time"
)

func TestObservePreservesRoundAndRequiresFreshCompleteAdvance(t *testing.T) {
	tr := step(t, transitionFixture(t), epoch.Add(time.Second))
	original := view(t, tr).Round
	now := epoch.Add(2 * time.Second)
	subset := receipts(t, original, now)[:1]
	next, err := tr.Observe(now, subset)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := next.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	next, err = DecodeTransition(raw)
	if err != nil {
		t.Fatal(err)
	}
	v := view(t, next)
	if !reflect.DeepEqual(v.Round, original) || len(v.Observed) != 1 || v.Complete {
		t.Fatal("observation changed phase authority")
	}
	if err := next.Extends(tr); err != nil {
		t.Fatal(err)
	}
	if _, err := next.Advance(now, subset); err == nil {
		t.Fatal("partial observations advanced phase")
	}
	stale := receipts(t, original, epoch.Add(time.Second))[:1]
	if _, err := next.Observe(now, stale); err == nil {
		t.Fatal("member evidence regressed")
	}
	for _, mode := range []string{"advance", "rollback", "renew"} {
		var changed Transition
		switch mode {
		case "advance":
			changed, err = next.Advance(now, receipts(t, original, now))
		case "rollback":
			changed, err = next.Rollback(now)
		case "renew":
			changed, err = next.Renew(original.ExpiresAt)
		}
		if err != nil {
			t.Fatal(mode, err)
		}
		current := view(t, changed)
		if len(current.Observed) != 0 {
			t.Fatal("observations leaked across round", mode)
		}
		if _, err := changed.Observe(current.Round.IssuedAt, subset); err == nil {
			t.Fatal("previous round evidence accepted", mode)
		}
	}
}

func TestObserveRejectsInvalidSubsets(t *testing.T) {
	tr := transitionFixture(t)
	r := view(t, tr).Round
	now := epoch.Add(time.Second)
	for _, fault := range []string{"empty", "duplicate", "identity", "device", "direction", "future", "stale", "expired"} {
		t.Run(fault, func(t *testing.T) {
			partial := receipts(t, r, now)[:1]
			at := now
			switch fault {
			case "empty":
				partial = nil
			case "duplicate":
				partial = append(partial, partial[0])
			case "identity":
				partial[0].AuthenticatedNodeUID = "another"
			case "device":
				partial[0].Receipt.DeviceID = "another"
			case "direction":
				partial[0].Receipt.ToKey = "another"
			case "future":
				partial[0].Receipt.ObservedAt = now.Add(time.Second)
			case "stale":
				at = now.Add(r.MaxReceiptAge + time.Second)
			case "expired":
				at = r.ExpiresAt
			}
			if _, err := tr.Observe(at, partial); err == nil {
				t.Fatal("invalid observation committed")
			}
		})
	}
}
