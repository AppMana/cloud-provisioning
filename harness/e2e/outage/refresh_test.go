package outage

import (
	"context"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/check"
)

// Refresh pod addresses after reboot. A recreated pod can have a new IP
// while its Service address stays stable; probing the old IP would falsely
// report a routing failure.
func TestTheProbesAreReReadAfterTheVictimReturns(t *testing.T) {
	p := &fakeProber{}
	p.onCut = func() { p.unreachable = "c" }
	p.onRestore = func() { p.unreachable = "" }

	d := deps(p)
	// The victim's pod comes back somewhere else. Only the refreshed
	// address answers.
	moved := []check.Target{
		{Node: "a", PodIP: "10.0.0.1", ServiceIP: "10.96.0.1"},
		{Node: "b", PodIP: "10.0.0.2", ServiceIP: "10.96.0.2"},
		{Node: "c", PodIP: "10.0.0.3", ServiceIP: "10.96.0.3"},
	}
	var refreshed int
	var askedFor [][]string
	d.Refresh = func(ctx context.Context, want []string) ([]check.Target, error) {
		refreshed++
		askedFor = append(askedFor, want)
		var out []check.Target
		for _, t := range moved {
			for _, w := range want {
				if t.Node == w {
					out = append(out, t)
				}
			}
		}
		return out, nil
	}

	res := Run(context.Background(), Row{Name: "x", Victim: "c", Mode: Reboot}, d)
	if !res.OK() {
		t.Fatalf("the row failed: %s", res)
	}
	// Once before measuring the survivors, once before measuring the
	// return: both are after something moved.
	if refreshed < 2 {
		t.Errorf("the probes were re-read %d times, want one per measurement after the victim moved", refreshed)
	}
	// While the victim is down its probe cannot be ready, so the
	// survivors' refresh must not wait for it: doing so fails the row
	// for the thing the row is doing on purpose.
	for _, n := range askedFor[0] {
		if n == "c" {
			t.Error("the survivors' refresh waited for the victim's probe, which is down by definition")
		}
	}
	// And the return's refresh does want it back.
	var wantedVictim bool
	for _, n := range askedFor[len(askedFor)-1] {
		if n == "c" {
			wantedVictim = true
		}
	}
	if !wantedVictim {
		t.Error("the return's refresh did not wait for the victim to come back")
	}
}

// With no way to re-read, a row still runs against what it started
// with rather than failing: a caller measuring something that cannot
// move needs no refresh.
func TestARowWithoutARefreshStillRuns(t *testing.T) {
	p := &fakeProber{}
	p.onCut = func() { p.unreachable = "c" }
	p.onRestore = func() { p.unreachable = "" }

	d := deps(p)
	d.Refresh = nil
	if res := Run(context.Background(), Row{Name: "x", Victim: "c", Mode: Cut}, d); !res.OK() {
		t.Errorf("a row with no refresh failed: %s", res)
	}
	if !strings.Contains(strings.ToLower(Row{Name: "x"}.Name), "x") {
		t.Skip()
	}
}
