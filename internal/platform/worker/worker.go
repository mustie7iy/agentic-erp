// Package worker runs a background worker's rounds until its context ends:
// the in-process polling workers of ADR-0012 (proposals, releases,
// simulations, dispatch), the outbox relay of ADR-0007, the balance
// projector, and the periodic audit, offboarding, directory, key rotation
// and contract expiry rounds. Each worker keeps its own RunOnce; this
// package is the loop they all repeated (CLAUDE.md: never copy a helper).
package worker

import (
	"context"
	"log/slog"
	"time"
)

// Round does one round of work. again asks for the next round at once
// instead of after the interval: a round that filled its batch has more
// waiting.
type Round func(ctx context.Context) (again bool, err error)

// Loop runs round until ctx ends, waiting interval between rounds, or
// fallback when interval is not positive. A failed round is logged as msg
// with the error and retried after the interval; an error after ctx ended
// is the shutdown, not a failure, and is not logged.
func Loop(ctx context.Context, interval, fallback time.Duration, round Round, log *slog.Logger, msg string) {
	if interval <= 0 {
		interval = fallback
	}
	for {
		again, err := round(ctx)
		if Failed(ctx, err) {
			log.ErrorContext(ctx, msg, "error", err)
		}
		if again && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// Failed reports whether err is a failure to log: an error while ctx is
// still live. A round reporting its work uses it to stay silent exactly
// when Loop logs the failure.
func Failed(ctx context.Context, err error) bool {
	return err != nil && ctx.Err() == nil
}
