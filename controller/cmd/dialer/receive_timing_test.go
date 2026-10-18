package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestReceiveTiming(t *testing.T) {
	for _, success := range []bool{false, true} {
		var out bytes.Buffer
		start := time.Unix(100, 0)
		timer := &receiveTiming{Operation: "retirement", started: start, last: start, stage: "validation"}
		timer.advance("admission", start.Add(2*time.Millisecond))
		timer.advance("native", start.Add(9*time.Millisecond))
		timer.write(&out, success, start.Add(14*time.Millisecond))
		line := out.String()
		if !strings.HasPrefix(line, "receive timing: ") || strings.Count(line, "\n") != 1 {
			t.Fatal("invalid diagnostic framing")
		}
		var got receiveTiming
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "receive timing: ")), &got); err != nil {
			t.Fatal(err)
		}
		if got.Operation != "retirement" || got.Success != success || got.ElapsedMillis != 14 || got.Truncated || len(got.Stages) != 3 {
			t.Fatalf("incorrect timing: %+v", got)
		}
		for i, want := range []receiveStageTiming{{"validation", 2}, {"admission", 7}, {"native", 5}} {
			if got.Stages[i] != want {
				t.Fatalf("stage %d = %+v", i, got.Stages[i])
			}
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "receive timing: ")), &fields); err != nil {
			t.Fatal(err)
		}
		if len(fields) != 5 {
			t.Fatal("diagnostic exposes unexpected fields")
		}
	}
}

func TestReceiveTimingBoundedAndDisabled(t *testing.T) {
	var out bytes.Buffer
	disabled := newReceiveTiming(false, "phase")
	disabled.step("native")
	disabled.write(&out, false, time.Now())
	if out.Len() != 0 {
		t.Fatal("disabled diagnostics wrote output")
	}
	timer := newReceiveTiming(true, "neighbors")
	for range 40 {
		timer.step("recheck")
	}
	timer.write(&out, false, time.Now())
	if !timer.Truncated || len(timer.Stages) != 16 {
		t.Fatal("unbounded stage diagnostics")
	}
}
