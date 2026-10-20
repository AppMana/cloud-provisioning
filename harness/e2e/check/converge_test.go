package check

import (
	"context"
	"errors"
	"testing"
	"time"
)

type recoveringDNS struct{ calls int }

type delayedDNS struct {
	delay time.Duration
	calls int
}

func (*delayedDNS) HTTPGet(context.Context, string, string) ([]byte, error) {
	panic("unexpected HTTP probe")
}

func (p *delayedDNS) Resolve(ctx context.Context, _, _ string) error {
	p.calls++
	select {
	case <-time.After(p.delay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestConvergeAllowsSlowTransportWithinExplicitPassBudget(t *testing.T) {
	p := &delayedDNS{delay: 900 * time.Millisecond}
	m := Converge(context.Background(), p, []Target{{Node: "worker"}},
		Options{PassTimeout: 2 * time.Second}, 2*time.Second, time.Millisecond)
	if !m.OK() || p.calls != 1 || len(m.PriorPasses) != 0 {
		t.Fatalf("slow successful probe was interrupted or repeated: calls=%d %s", p.calls, m.Report())
	}
}

func TestConvergeExplicitPassBudgetCannotExtendOuterDeadline(t *testing.T) {
	for _, parent := range []bool{false, true} {
		t.Run(map[bool]string{false: "window", true: "parent"}[parent], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			window := 50 * time.Millisecond
			if parent {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, window)
				defer stop()
				window = 2 * time.Second
			}
			p := &delayedDNS{delay: time.Second}
			m := Converge(ctx, p, []Target{{Node: "worker"}},
				Options{PassTimeout: time.Hour}, window, time.Millisecond)
			if m.OK() || p.calls != 1 || m.Cancelled != parent || m.Failed() != 1 {
				t.Fatalf("outer deadline was not enforced: calls=%d %s", p.calls, m.Report())
			}
			if !errors.Is(m.Results()[0].Err, context.DeadlineExceeded) {
				t.Fatalf("deadline failure lost: %+v", m.Results())
			}
		})
	}
}

func (*recoveringDNS) HTTPGet(context.Context, string, string) ([]byte, error) {
	panic("unexpected HTTP probe")
}
func (p *recoveringDNS) Resolve(context.Context, string, string) error {
	p.calls++
	if p.calls == 1 {
		return errors.New("observed DNS timeout")
	}
	return nil
}

func TestConvergePreservesFailedPassBeforeSuccess(t *testing.T) {
	p := &recoveringDNS{}
	var attempts []int
	var results []Result
	final := Converge(context.Background(), p, []Target{{Node: "remote1"}}, Options{ObservePass: func(attempt int, m *Matrix) {
		attempts = append(attempts, attempt)
		results = append(results, m.Results()...)
	}}, time.Second, time.Millisecond)
	if final.OK() || !final.Converged() || len(attempts) != 2 || attempts[0] != 1 || attempts[1] != 2 {
		t.Fatalf("attempts=%v final=%s", attempts, final.Report())
	}
	if len(final.PriorPasses) != 1 || final.PriorPasses[0].Failed() != 1 {
		t.Fatal("result lost failed history without an observer")
	}
	if len(results) != 2 || results[0].OK || results[0].Err == nil || results[0].Err.Error() != "observed DNS timeout" || results[0].From != "remote1" || results[0].Kind != DNS || !results[1].OK {
		t.Fatalf("lost pass evidence: %+v", results)
	}
}

func TestConvergeObservesFailureBeforeCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	final := Converge(ctx, &recoveringDNS{}, []Target{{Node: "remote1"}}, Options{ObservePass: func(attempt int, m *Matrix) {
		calls++
		if attempt != 1 || m.Failed() != 1 {
			t.Errorf("unexpected pass %d: %s", attempt, m.Report())
		}
		cancel()
	}}, time.Second, time.Hour)
	if calls != 1 || !final.Cancelled || final.OK() {
		t.Fatalf("calls=%d cancelled=%v final=%s", calls, final.Cancelled, final.Report())
	}
}

func TestConvergeKeepsFailuresWithoutAnObserver(t *testing.T) {
	final := Converge(context.Background(), &recoveringDNS{}, []Target{{Node: "worker"}}, Options{}, time.Second, time.Millisecond)
	if final.OK() || !final.Converged() || len(final.PriorPasses) != 1 || final.PriorPasses[0].Failed() != 1 {
		t.Fatal(final.Report())
	}
	details := final.Details()
	if details["ok"] != false || details["converged"] != true || len(details["priorPasses"].([]map[string]any)) != 1 {
		t.Fatalf("lost evidence: %+v", details)
	}
}
