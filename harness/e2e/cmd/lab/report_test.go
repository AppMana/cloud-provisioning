package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/check"
)

type journalDNS struct{ calls int }

func (*journalDNS) HTTPGet(context.Context, string, string) ([]byte, error) {
	panic("unexpected HTTP probe")
}
func (p *journalDNS) Resolve(context.Context, string, string) error {
	p.calls++
	if p.calls == 1 {
		return errors.New("transient DNS failure")
	}
	return nil
}

func TestJournalRetainsFailedPassAndSeparatesConvergenceWindows(t *testing.T) {
	oldPath, oldSeries := reportPath, matrixSeries
	defer func() { reportPath, matrixSeries = oldPath, oldSeries }()
	reportPath = filepath.Join(t.TempDir(), "events.jsonl")
	matrixSeries = 0
	opts := matrixOptions()
	opts.ExternalURL = ""
	p := &journalDNS{}
	for range 2 {
		m := check.Converge(context.Background(), p, []check.Target{{Node: "remote1"}}, opts, time.Second, time.Millisecond)
		if !m.OK() {
			t.Fatal(m.Report())
		}
	}
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 {
		t.Fatalf("events=%d", len(lines))
	}
	for i, line := range lines {
		var e struct {
			Event string
			Value struct {
				Series, Attempt, Failed int
				Results                 []struct {
					From, Kind, Error string
					Passed            bool
				}
			}
		}
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		wantSeries, wantAttempt := 1, i+1
		if i == 2 {
			wantSeries, wantAttempt = 2, 1
		}
		if e.Event != "matrix-pass" || e.Value.Series != wantSeries || e.Value.Attempt != wantAttempt {
			t.Fatalf("wrong event: %s", line)
		}
		if i == 0 && (e.Value.Failed != 1 || len(e.Value.Results) != 1 || e.Value.Results[0].Error != "transient DNS failure" || e.Value.Results[0].From != "remote1" || e.Value.Results[0].Kind != "dns" || e.Value.Results[0].Passed) {
			t.Fatalf("lost failed probe: %s", line)
		}
		if i > 0 && e.Value.Failed != 0 {
			t.Fatalf("unexpected failure: %s", line)
		}
	}
}
