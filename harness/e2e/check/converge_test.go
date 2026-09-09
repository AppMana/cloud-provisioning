package check

import (
	"context"
	"errors"
	"testing"
	"time"
)

type recoveringDNS struct{ calls int }

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
	if !final.OK() || len(attempts) != 2 || attempts[0] != 1 || attempts[1] != 2 {
		t.Fatalf("attempts=%v final=%s", attempts, final.Report())
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
