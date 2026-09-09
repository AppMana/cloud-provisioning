package check

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	neturl "net/url"
	"os"
	"testing"
	"time"
)

type capturedFailure struct {
	From, To string
	Kind     Kind
	Message  string
}
type capturedPass struct {
	Expected struct{ Total, Passed, Failed, NotRequired int }
	Failures []capturedFailure
}
type replayTarget struct {
	node string
	kind Kind
}
type rebootReplay struct {
	failures map[string]string
	targets  map[string]replayTarget
}

func failureKey(from, to string, kind Kind) string { return from + "/" + to + "/" + string(kind) }
func (p *rebootReplay) failure(from, to string, kind Kind) error {
	if message := p.failures[failureKey(from, to, kind)]; message != "" {
		return errors.New(message)
	}
	return nil
}
func (p *rebootReplay) target(raw string) replayTarget {
	u, err := neturl.Parse(raw)
	if err != nil {
		panic(err)
	}
	if t, ok := p.targets[u.Hostname()]; ok {
		return t
	}
	return replayTarget{kind: External}
}
func (p *rebootReplay) HTTPGet(_ context.Context, from, raw string) ([]byte, error) {
	to := p.target(raw)
	return []byte("ok"), p.failure(from, to.node, to.kind)
}
func (p *rebootReplay) HTTPSize(_ context.Context, from, raw string) (int64, error) {
	to := p.target(raw)
	return TransferBytes, p.failure(from, to.node, Transfer)
}
func (p *rebootReplay) Resolve(_ context.Context, from, _ string) error {
	return p.failure(from, "", DNS)
}

// The native returned matrix failed 15 probes before a later pass recovered.
// Replay that observed outcome sequence to ensure convergence and its observer
// retain every failure. This does not simulate the cause or native timing.
func TestObservedRebootFailuresRemainVisibleAfterConvergence(t *testing.T) {
	raw, err := os.ReadFile("testdata/k0s-kuberouter-reboot-convergence.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Nodes  []string
		Passes []capturedPass
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	p := &rebootReplay{targets: map[string]replayTarget{}}
	var targets []Target
	for i, name := range fixture.Nodes {
		pod, service := fmt.Sprintf("10.0.0.%d", i+1), fmt.Sprintf("10.1.0.%d", i+1)
		p.targets[pod] = replayTarget{name, Pod}
		p.targets[service] = replayTarget{name, Service}
		targets = append(targets, Target{Node: name, PodIP: pod, ServiceIP: service})
	}
	selectPass := func(index int) {
		p.failures = map[string]string{}
		for _, f := range fixture.Passes[index].Failures {
			p.failures[failureKey(f.From, f.To, f.Kind)] = f.Message
		}
	}
	selectPass(0)
	observed := 0
	opts := Options{Port: 8080, ExternalURL: ExternalURL, ObservePass: func(attempt int, m *Matrix) {
		observed++
		if attempt > len(fixture.Passes) {
			t.Fatalf("unexpected extra pass %d", attempt)
		}
		expected := fixture.Passes[attempt-1].Expected
		if m.Total() != expected.Total || m.Passed() != expected.Passed || m.Failed() != expected.Failed || m.NotRequired() != expected.NotRequired {
			t.Fatalf("pass %d: %s", attempt, m.Report())
		}
		for _, f := range m.Failures() {
			if f.Err == nil || f.Err.Error() != p.failures[failureKey(f.From, f.To, f.Kind)] {
				t.Fatalf("lost native failure %s", f)
			}
		}
		if attempt < len(fixture.Passes) {
			selectPass(attempt)
		}
	}}
	final := Converge(context.Background(), p, targets, opts, 5*time.Second, time.Millisecond)
	if observed != len(fixture.Passes) || !final.OK() {
		t.Fatalf("observed=%d final=%s", observed, final.Report())
	}
}
