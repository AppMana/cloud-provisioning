// Package wait polls conditions with cancellable, time-limited attempts.
// Attempts must honor their context. The retry budget is checked between
// attempts; callers needing an overall deadline must supply one in the context.
package wait

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Interval is how long to leave between attempts.
const Interval = 5 * time.Second

// AttemptShare bounds one attempt as a fraction of the whole wait.
//
// A single attempt must not be able to consume the budget, or the
// wait reports one hung probe instead of the condition it was
// watching; it must also not be so short that a slow machine's honest
// answer is cut off and read as failure.
const AttemptShare = 4

// Fatal marks a failure that waiting cannot fix.
//
// Most failures during a wait are the condition not being true yet,
// and retrying is right. Some are not: a machine whose launcher
// crashes on its own configuration is restarted by its supervisor and
// crashes again, and the only thing waiting adds is the delay before
// anyone is told. An attempt that has established the difference says
// so with this, and the wait ends at once.
func Fatal(err error) error { return &fatal{err: err} }

type fatal struct{ err error }

func (f *fatal) Error() string { return f.err.Error() }
func (f *fatal) Unwrap() error { return f.err }

// Until calls attempt until it succeeds or within elapses.
//
// Each attempt is given a context of its own that expires well before
// the wait does, so a command that never returns costs one attempt
// rather than the whole budget. The last error is reported: a wait
// that says only "timed out" makes every timeout a second
// investigation.
func Until(ctx context.Context, within time.Duration, what string, attempt func(context.Context) error) error {
	perAttempt := within / AttemptShare
	if perAttempt < time.Second {
		perAttempt = time.Second
	}
	deadline := time.Now().Add(within)
	var last error
	for {
		try, cancel := context.WithTimeout(ctx, perAttempt)
		last = attempt(try)
		cancel()
		if last == nil {
			return nil
		}
		var stop *fatal
		if errors.As(last, &stop) {
			return fmt.Errorf("%s: %w", what, stop.err)
		}
		// The caller's own cancellation, not this attempt's budget:
		// a run that is being torn down should say so rather than
		// spend the full wait discovering it.
		if ctx.Err() != nil {
			return outer(ctx, what, last)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s within %s: %w", what, within, last)
		}
		select {
		case <-ctx.Done():
			return outer(ctx, what, last)
		case <-time.After(Interval):
		}
	}
}

// outer reports cancellation together with the pending condition and its
// last failure, so an outer deadline preserves useful wait diagnostics.
func outer(ctx context.Context, what string, last error) error {
	if last == nil {
		return fmt.Errorf("%s: %w", what, ctx.Err())
	}
	return fmt.Errorf("%s: %w (last failure: %v)", what, ctx.Err(), last)
}
