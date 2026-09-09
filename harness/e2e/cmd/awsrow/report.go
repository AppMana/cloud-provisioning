package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/check"
)

// Record every native probe pass. A later green matrix cannot erase a failed
// attempt, and inability to preserve evidence must not produce a passing row.
func recordedMatrix(ctx context.Context, prober check.Prober, targets []check.Target, opts check.Options, within, every time.Duration, output io.Writer) (*check.Matrix, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var recordErr error
	opts.ObservePass = func(attempt int, matrix *check.Matrix) {
		detail := matrix.Details()
		detail["attempt"] = attempt
		detail["time"] = time.Now().UTC()
		if err := json.NewEncoder(output).Encode(detail); err != nil {
			recordErr = fmt.Errorf("recording matrix attempt: %w", err)
			cancel()
		}
	}
	matrix := check.Converge(ctx, prober, targets, opts, within, every)
	return matrix, recordErr
}
