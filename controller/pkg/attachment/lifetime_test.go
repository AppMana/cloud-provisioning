package attachment

import (
	"context"
	"fmt"
	"testing"
)

type lifetimeProbe struct {
	retiring                bool
	holdError, releaseError error
	releases                int
	onRelease               func()
}

func (p *lifetimeProbe) Protect(context.Context, Record) (bool, error) {
	return p.retiring, p.holdError
}
func (p *lifetimeProbe) Release(context.Context, Record) error {
	p.releases++
	if p.onRelease != nil {
		p.onRelease()
	}
	return p.releaseError
}

func TestLifetimeHoldsBeforePreparationAndRetriesAfterDurableCompletion(t *testing.T) {
	ctx := context.Background()
	r, b, store := fixture(t)
	life := &lifetimeProbe{holdError: fmt.Errorf("hook write failed")}
	r.Lifetime = life
	desired := observedRequest()
	if _, err := r.Step(ctx, "worker", &desired); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Step(ctx, "worker", &desired); err == nil || len(b.calls) != 0 {
		t.Fatal("prepared without lifetime hold")
	}
	life.holdError = nil
	b.forwarding = true
	if phase, err := r.Step(ctx, "worker", &desired); err != nil || phase != Publishing {
		t.Fatal(phase, err)
	}
	life.retiring = true
	if phase, err := r.Step(ctx, "worker", &desired); err != nil || phase != Withdrawing {
		t.Fatal(phase, err)
	}
	// A withdrawing lease must not try to install hooks on a deleting Machine.
	life.holdError = fmt.Errorf("machine is deleting")
	if phase, err := r.Step(ctx, "worker", nil); err != nil || phase != Withdrawing || life.releases != 0 {
		t.Fatal(phase, err)
	}
	b.withdrawn = true
	if phase, err := r.Step(ctx, "worker", nil); err != nil || phase != Releasing {
		t.Fatal(phase, err)
	}
	if _, err := r.Step(ctx, "worker", nil); err != nil || life.releases != 0 {
		t.Fatal("hooks released before provider resources")
	}
	b.released = true
	store.fail = true
	if _, err := r.Step(ctx, "worker", nil); err == nil || life.releases != 0 {
		t.Fatal("hooks released before durable completion")
	}
	life.releaseError = fmt.Errorf("second participant patch failed")
	life.onRelease = func() {
		record, err := store.Load(ctx, "worker")
		if err != nil || record.Phase != Complete {
			t.Fatal("completion not persisted before releasing hooks")
		}
	}
	if phase, err := r.Step(ctx, "worker", nil); err == nil || phase != Complete {
		t.Fatal("missing partial release failure")
	}
	life.releaseError = nil
	if phase, err := r.Step(ctx, "worker", nil); err != nil || phase != Complete || life.releases != 2 {
		t.Fatal("could not retry after CAPI may have removed first participant", phase, err)
	}
}
