package check

import (
	"context"
	"encoding/json"
	"fmt"
	neturl "net/url"
	"os"
	"testing"
)

type observedTransferRow struct {
	From   string
	Checks []struct {
		To                                         string
		PayloadBytes, ExpectedBytes, ReceivedBytes int64
		OK                                         bool
	}
}

// This replays measured transfer outcomes, not Windows networking internals.
// Evidence and the temporary MTU restoration are recorded in
// docs/validation/windows-gateway-transfer-mtu-results.json.
type observedWindowsTransfers map[string]map[string]struct {
	size int64
	err  error
}

func (p observedWindowsTransfers) HTTPGet(context.Context, string, string) ([]byte, error) {
	return []byte("ok"), nil
}
func (p observedWindowsTransfers) Resolve(context.Context, string, string) error { return nil }
func (p observedWindowsTransfers) HTTPSize(_ context.Context, from, target string) (int64, error) {
	u, err := neturl.Parse(target)
	if err != nil {
		return 0, err
	}
	r, ok := p[from][u.Hostname()]
	if !ok {
		return 0, fmt.Errorf("unobserved transfer %s to %s", from, target)
	}
	return r.size, r.err
}

func TestObservedWindowsMTUFailureCannotPassOnSmallRequests(t *testing.T) {
	data, err := os.ReadFile("testdata/windows-gateway-mtu.json")
	if err != nil {
		t.Fatal(err)
	}
	var runs map[string][]observedTransferRow
	if err := json.Unmarshal(data, &runs); err != nil {
		t.Fatal(err)
	}
	for name, wantFailures := range map[string]int{"baseline": 4, "reducedMTU": 0} {
		t.Run(name, func(t *testing.T) {
			p := observedWindowsTransfers{}
			var targets []Target
			for _, row := range runs[name] {
				targets = append(targets, Target{Node: row.From, PodIP: row.From, ServiceIP: row.From})
				p[row.From] = map[string]struct {
					size int64
					err  error
				}{}
				for _, c := range row.Checks {
					if c.PayloadBytes != TransferBytes {
						continue
					}
					var probeErr error
					if !c.OK {
						probeErr = fmt.Errorf("observed curl transfer timeout")
					}
					// Normalize only the successful Windows pipeline's CRLF.
					size := c.ReceivedBytes
					if c.OK {
						size -= c.ExpectedBytes - c.PayloadBytes
					}
					p[row.From][c.To] = struct {
						size int64
						err  error
					}{size, probeErr}
				}
			}
			if len(targets) != 3 {
				t.Fatalf("want three observed nodes, got %d", len(targets))
			}
			m := Run(context.Background(), p, targets, Options{Port: 8080})
			if m.Total() != 21 {
				t.Fatalf("want six pod, six Service, six transfer and three DNS checks, got %d", m.Total())
			}
			failures := m.Failures()
			if len(failures) != wantFailures {
				t.Fatalf("failures=%v, want %d", failures, wantFailures)
			}
			for _, f := range failures {
				if f.Kind != Transfer || (f.From != "w1" && f.To != "w1") {
					t.Fatalf("unexpected failed path: %+v", f)
				}
			}
			if m.OK() != (wantFailures == 0) {
				t.Fatalf("incorrect matrix verdict: %v", m.OK())
			}
		})
	}
}
