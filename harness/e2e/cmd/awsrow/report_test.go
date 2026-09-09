package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/check"
)

type recoveryProbe struct{ calls int }

func (*recoveryProbe) HTTPGet(context.Context, string, string) ([]byte, error) {
	panic("unexpected HTTP probe")
}
func (p *recoveryProbe) Resolve(context.Context, string, string) error {
	p.calls++
	if p.calls == 1 {
		return errors.New("observed DNS timeout")
	}
	return nil
}

func TestAWSRowPreservesFailedAttemptBeforeSuccess(t *testing.T) {
	var output bytes.Buffer
	m, err := recordedMatrix(context.Background(), &recoveryProbe{}, []check.Target{{Node: "survivor"}}, check.Options{}, time.Second, time.Millisecond, &output)
	if err != nil || !m.OK() {
		t.Fatalf("matrix=%s error=%v", m.Report(), err)
	}
	dec := json.NewDecoder(&output)
	for attempt := 1; attempt <= 2; attempt++ {
		var d struct {
			Attempt, Failed, Passed int
			Results                 []struct {
				From, Kind, Error string
				Passed            bool
			}
		}
		if err := dec.Decode(&d); err != nil {
			t.Fatal(err)
		}
		if d.Attempt != attempt || len(d.Results) != 1 || d.Results[0].From != "survivor" {
			t.Fatalf("lost attempt: %+v", d)
		}
		if attempt == 1 && (d.Failed != 1 || d.Results[0].Error != "observed DNS timeout" || d.Results[0].Passed) {
			t.Fatalf("lost failure: %+v", d)
		}
		if attempt == 2 && (d.Failed != 0 || d.Passed != 1 || !d.Results[0].Passed) {
			t.Fatalf("lost recovery: %+v", d)
		}
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		t.Fatalf("extra attempt: %v", err)
	}
}

type failedJournal struct{ err error }

func (w failedJournal) Write([]byte) (int, error) { return 0, w.err }
func TestAWSRowFailsWhenAttemptEvidenceCannotBeWritten(t *testing.T) {
	want := errors.New("disk full")
	p := &recoveryProbe{}
	_, err := recordedMatrix(context.Background(), p, []check.Target{{Node: "survivor"}}, check.Options{}, time.Second, time.Millisecond, failedJournal{want})
	if !errors.Is(err, want) || p.calls != 1 {
		t.Fatalf("error=%v calls=%d", err, p.calls)
	}
}
