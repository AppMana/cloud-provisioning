package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	ctrl "sigs.k8s.io/controller-runtime"
)

func TestRetiredAdoptionRefreshUsesPendingCadence(t *testing.T) {
	for _, existing := range []time.Duration{0, 2 * time.Second, 30 * time.Second} {
		for _, cause := range []error{attachment.ErrRetirementObservationExpired, fmt.Errorf("completion: %w", attachment.ErrRetirementObservationExpired)} {
			got, err := retiredReceiveAdoptionResult(context.Background(), ctrl.Result{RequeueAfter: existing}, false, cause)
			want := 5 * time.Second
			if existing > 0 && existing < want {
				want = existing
			}
			if err != nil || got.RequeueAfter != want {
				t.Fatalf("refresh wait entered error backoff or delayed an earlier deadline: result=%+v err=%v", got, err)
			}
		}
	}
}

func TestRetiredAdoptionRetryPreservesFailuresAndProgress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	broken := errors.New("API unavailable")
	for _, tc := range []struct {
		ctx context.Context
		err error
	}{
		{context.Background(), broken},
		{ctx, attachment.ErrRetirementObservationExpired},
		{ctx, context.Canceled},
	} {
		got, err := retiredReceiveAdoptionResult(tc.ctx, ctrl.Result{RequeueAfter: 30 * time.Second}, false, tc.err)
		if !errors.Is(err, tc.err) || got != (ctrl.Result{}) {
			t.Fatalf("failure hidden or unusable requeue returned alongside error: result=%+v err=%v", got, err)
		}
	}
	for _, progress := range []bool{false, true} {
		got, err := retiredReceiveAdoptionResult(context.Background(), ctrl.Result{RequeueAfter: 30 * time.Second}, progress, nil)
		want := 30 * time.Second
		if progress {
			want = time.Second
		}
		if err != nil || got.RequeueAfter != want {
			t.Fatalf("existing progress cadence changed: result=%+v err=%v", got, err)
		}
	}
}
