package wait

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// The reason this package exists: an attempt that never returns must
// not be able to outlive the wait. Written as a loop that checks the
// clock between attempts, this test hangs forever.
func TestAHungAttemptDoesNotOutliveTheWait(t *testing.T) {
	done := make(chan error, 1)
	go func() {
		done <- Until(context.Background(), 4*time.Second, "the machine to answer",
			func(ctx context.Context) error {
				// A command that has stopped talking: it returns only
				// when something takes it away.
				<-ctx.Done()
				return ctx.Err()
			})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a wait whose every attempt hung reported success")
		}
		if !strings.Contains(err.Error(), "the machine to answer") {
			t.Errorf("the failure does not say what was being waited for: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the wait outlived its own limit: a hung attempt bounds nothing")
	}
}

// Each attempt gets a slice of the budget, not the whole of it, so
// several are made before the wait gives up.
func TestSeveralAttemptsAreMade(t *testing.T) {
	var attempts int
	_ = Until(context.Background(), 4*time.Second, "something that never happens",
		func(ctx context.Context) error {
			attempts++
			<-ctx.Done()
			return ctx.Err()
		})
	if attempts < 2 {
		t.Errorf("made %d attempts: one hung probe consumed the whole wait", attempts)
	}
}

// The last failure is carried out, because a wait that reports only
// that it timed out makes every timeout a second investigation.
func TestTheLastFailureIsReported(t *testing.T) {
	err := Until(context.Background(), 2*time.Second, "the API to serve",
		func(context.Context) error { return errors.New("connection refused") })
	if err == nil {
		t.Fatal("no error")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("the reason was dropped: %v", err)
	}
}

// A cancelled run says it was cancelled, rather than spending the
// full wait to discover it.
func TestCancellationIsNotATimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Until(ctx, time.Minute, "anything", func(context.Context) error { return errors.New("no") })
	if !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled run reported %v, not its own cancellation", err)
	}
}

func TestSuccessReturnsImmediately(t *testing.T) {
	start := time.Now()
	if err := Until(context.Background(), time.Minute, "x", func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("a condition already true waited anyway")
	}
}

// A wait cut short by the caller's own deadline still says what it
// was waiting for and how that last failed. Returning a bare
// "context deadline exceeded" leaves the caller to work out which of
// a run's several waits it was.
func TestAnOuterDeadlineKeepsTheReason(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := Until(ctx, time.Hour, "every node to register", func(context.Context) error {
		return errors.New("w1, w2 never registered")
	})
	if err == nil {
		t.Fatal("no error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the cancellation is no longer recognisable: %v", err)
	}
	if !strings.Contains(err.Error(), "every node to register") {
		t.Errorf("the failure does not say what was awaited: %v", err)
	}
	if !strings.Contains(err.Error(), "w1, w2 never registered") {
		t.Errorf("the last failure was thrown away: %v", err)
	}
}

// Some failures are not the condition being false yet. A machine
// whose launcher crashes on its own configuration is restarted by its
// supervisor and crashes again, and waiting adds nothing but the
// delay before anyone is told — twelve minutes, in the run that
// prompted this.
func TestAFailureWaitingCannotFixEndsTheWait(t *testing.T) {
	var attempts int
	start := time.Now()
	err := Until(context.Background(), time.Hour, "the machine to answer", func(context.Context) error {
		attempts++
		return Fatal(errors.New("its launcher is crash-looping"))
	})
	if err == nil {
		t.Fatal("no error")
	}
	if attempts != 1 {
		t.Errorf("tried %d times against a failure that cannot change", attempts)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("the wait kept waiting")
	}
	if !strings.Contains(err.Error(), "crash-looping") {
		t.Errorf("the reason was dropped: %v", err)
	}
}
