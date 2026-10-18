package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
)

// Fixed call-site labels and durations only: never pass object identities,
// configuration, credentials or error text to these diagnostics. Timings include
// nested work and must not be summed across operations. They grant no authority.
type receiveTiming struct {
	Operation     string               `json:"operation"`
	Success       bool                 `json:"success"`
	ElapsedMillis int64                `json:"elapsedMillis"`
	Stages        []receiveStageTiming `json:"stages"`
	Truncated     bool                 `json:"truncated"`
	started, last time.Time
	stage         string
}
type receiveStageTiming struct {
	Stage  string `json:"stage"`
	Millis int64  `json:"millis"`
}

func newReceiveTiming(enabled bool, operation string) *receiveTiming {
	if !enabled {
		return nil
	}
	now := time.Now()
	return &receiveTiming{Operation: operation, started: now, last: now, stage: "validation"}
}
func (t *receiveTiming) step(stage string) { t.advance(stage, time.Now()) }
func (t *receiveTiming) advance(stage string, now time.Time) {
	if t == nil {
		return
	}
	if len(t.Stages) < 16 {
		t.Stages = append(t.Stages, receiveStageTiming{Stage: t.stage, Millis: max(0, now.Sub(t.last).Milliseconds())})
	} else {
		t.Truncated = true
	}
	t.stage, t.last = stage, now
}
func (t *receiveTiming) finish(success bool) { t.write(os.Stderr, success, time.Now()) }
func (t *receiveTiming) write(w io.Writer, success bool, now time.Time) {
	if t == nil {
		return
	}
	t.advance("", now)
	t.Success = success
	t.ElapsedMillis = max(0, now.Sub(t.started).Milliseconds())
	raw, err := json.Marshal(t)
	if err == nil {
		fmt.Fprintf(w, "receive timing: %s\n", raw)
	}
}
