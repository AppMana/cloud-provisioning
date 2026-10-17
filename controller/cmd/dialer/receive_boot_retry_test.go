package main

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestReceiveBootRetryIsBoundedAndPreservesOrdinaryPolling(t *testing.T) {
	var retry receiveBootRetry
	start := time.Now()
	normal := 30 * time.Second
	pending := receiveBootPending{errors.New("transport not authenticated yet")}
	for _, elapsed := range []time.Duration{0, time.Second, 59 * time.Second} {
		if got := retry.delay(fmt.Errorf("restore: %w", pending), normal, start.Add(elapsed)); got != time.Second {
			t.Fatalf("retry at %s = %s", elapsed, got)
		}
	}
	for _, elapsed := range []time.Duration{time.Minute, 2 * time.Minute} {
		if got := retry.delay(pending, normal, start.Add(elapsed)); got != normal {
			t.Fatalf("retry window extended at %s: %s", elapsed, got)
		}
	}
	for _, err := range []error{nil, errors.New("checkpoint rejected")} {
		if got := retry.delay(err, normal, start); got != normal || !retry.started.IsZero() {
			t.Fatalf("ordinary reconciliation changed: delay=%s start=%s", got, retry.started)
		}
	}
	if got := retry.delay(pending, 100*time.Millisecond, start); got != 100*time.Millisecond {
		t.Fatalf("short configured polling slowed to %s", got)
	}
	if !errors.Is(pending, pending.error) {
		t.Fatal("retry marker lost original failure")
	}
}
