package check

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// Every ordered pair, both directions, and no node against itself.
// Direction matters because a tunnel is dialled from one side and the
// return path is not the same mechanism.
func TestPairsAreOrderedAndComplete(t *testing.T) {
	got := Pairs([]string{"cp", "w1", "remote1"})
	if len(got) != 6 {
		t.Fatalf("%d pairs for 3 nodes, want 6 (n(n-1))", len(got))
	}
	seen := map[Pair]bool{}
	for _, p := range got {
		if p.From == p.To {
			t.Errorf("a node was paired with itself: %v", p)
		}
		if seen[p] {
			t.Errorf("duplicate pair %v", p)
		}
		seen[p] = true
	}
	// Both directions of the same pair must be present and distinct:
	// a transit pair that works one way and not the other is exactly
	// the failure a placement change produced, and sampling one
	// direction would have missed it.
	if !seen[Pair{"cp", "remote1"}] || !seen[Pair{"remote1", "cp"}] {
		t.Error("a pair is measured in only one direction")
	}
}

// A run with nothing in it is a failure. A harness that reports zero
// checks as success is how a broken configuration stays green, and
// this happened: a namespace that would not delete meant no probe
// pods started, and the row reported no checks ran on a healthy
// network.
func TestNoChecksIsNotAPass(t *testing.T) {
	var m Matrix
	if m.OK() {
		t.Error("an empty run reported success")
	}
	m.Add(Result{From: "cp", To: "w1", Kind: Pod, OK: true})
	if !m.OK() {
		t.Error("a run with one passing check reported failure")
	}
	m.Add(Result{From: "w1", To: "cp", Kind: Pod})
	if m.OK() {
		t.Error("a run with a failure reported success")
	}
}

// The transfer check must assert the length, not merely that
// something came back. A body that arrives short is the packet-size
// failure it exists to catch.
func TestAShortTransferFails(t *testing.T) {
	full := strings.Repeat("x", TransferBytes)
	short := strings.Repeat("x", TransferBytes-1)

	m := Run(context.Background(), &fake{bodies: map[string]string{
		"http://10.0.0.2:8080/big": full,
		"http://10.0.0.3:8080/big": short,
	}, defaultBody: "ok"}, []Target{
		{Node: "a", PodIP: "10.0.0.1", ServiceIP: "10.96.0.1"},
		{Node: "b", PodIP: "10.0.0.2", ServiceIP: "10.96.0.2"},
		{Node: "c", PodIP: "10.0.0.3", ServiceIP: "10.96.0.3"},
	}, Options{Port: 8080})

	var shortFailed bool
	for _, r := range m.Failures() {
		if r.Kind == Transfer && r.To == "c" {
			shortFailed = true
			if !strings.Contains(r.Detail, fmt.Sprint(TransferBytes-1)) {
				t.Errorf("the failure does not say how many bytes arrived: %q", r.Detail)
			}
		}
		if r.Kind == Transfer && r.To == "b" {
			t.Error("a complete transfer was reported as a failure")
		}
	}
	if !shortFailed {
		t.Error("a short transfer passed")
	}
}

// The pairs are independent, so they run at once. This asserts the
// concurrency is real: sequential probing is what made a full
// campaign an overnight job.
func TestProbesRunConcurrently(t *testing.T) {
	f := &fake{defaultBody: "ok", block: make(chan struct{})}
	nodes := []Target{
		{Node: "a", PodIP: "10.0.0.1", ServiceIP: "10.96.0.1"},
		{Node: "b", PodIP: "10.0.0.2", ServiceIP: "10.96.0.2"},
		{Node: "c", PodIP: "10.0.0.3", ServiceIP: "10.96.0.3"},
		{Node: "d", PodIP: "10.0.0.4", ServiceIP: "10.96.0.4"},
	}

	f.started = make(chan struct{}, 64)
	done := make(chan *Matrix)
	go func() {
		done <- Run(context.Background(), f, nodes, Options{Port: 8080, Concurrency: 8, SkipTransfer: true})
	}()

	// Every probe blocks until released, so a second probe can only
	// start if the first did not have to finish first. Waiting on the
	// channel rather than spinning on a counter makes that a fact
	// about the code and not about the scheduler.
	for i := 0; i < 2; i++ {
		<-f.started
	}
	close(f.block)

	m := <-done
	if m.Failed() != 0 {
		t.Errorf("probes failed: %s", m.Report())
	}
	if f.peak.Load() < 2 {
		t.Errorf("peak concurrency was %d: the probes ran one at a time", f.peak.Load())
	}
}

// Results are ordered, so two runs of the same row are comparable
// line by line rather than only in total.
func TestResultsAreDeterministicallyOrdered(t *testing.T) {
	var m Matrix
	m.Add(Result{From: "w1", To: "cp", Kind: Service, OK: true})
	m.Add(Result{From: "cp", To: "w1", Kind: Pod, OK: true})
	m.Add(Result{From: "cp", To: "w1", Kind: Service, OK: true})

	got := m.Results()
	if got[0].From != "cp" || got[0].Kind != Pod {
		t.Errorf("first result is %v", got[0])
	}
	if got[2].From != "w1" {
		t.Errorf("last result is %v", got[2])
	}
}

// A failing check says which pair and what happened. A harness that
// reports only a count makes every failure a second investigation.
func TestAFailureNamesThePairAndTheReason(t *testing.T) {
	r := Result{From: "remote1", To: "remote2", Kind: Pod, Err: errors.New("timed out")}
	line := r.String()
	for _, want := range []string{"FAIL", "remote1", "remote2", "pod", "timed out"} {
		if !strings.Contains(line, want) {
			t.Errorf("the line %q does not mention %q", line, want)
		}
	}
}

// fake answers probes without a cluster.
type fake struct {
	bodies      map[string]string
	defaultBody string
	block       chan struct{}
	started     chan struct{}

	inFlight atomic.Int64
	peak     atomic.Int64
	mu       sync.Mutex
}

func (f *fake) HTTPGet(ctx context.Context, node, url string) ([]byte, error) {
	if f.started != nil {
		f.started <- struct{}{}
	}
	n := f.inFlight.Add(1)
	for {
		peak := f.peak.Load()
		if n <= peak || f.peak.CompareAndSwap(peak, n) {
			break
		}
	}
	defer f.inFlight.Add(-1)

	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	body, ok := f.bodies[url]
	f.mu.Unlock()
	if !ok {
		body = f.defaultBody
	}
	return []byte(body), nil
}

func (f *fake) Resolve(ctx context.Context, node, name string) error { return nil }
