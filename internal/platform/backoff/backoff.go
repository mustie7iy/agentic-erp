// Package backoff waits between attempts at something not ready yet: a
// database server at its connection limit, a development server starting.
// Test harnesses retry with it rather than sleeping in a loop of their own
// (CLAUDE.md: no time.Sleep in tests), so each wait ends with its context
// and the delays are stated once, as a schedule. It is the harnesses'
// counterpart of the workers' loop (worker.Loop, ADR-0012), which waits
// between rounds of work rather than attempts at one thing.
package backoff

import (
	"context"
	"time"
)

// Wait waits d, or until ctx ends, whose error it then returns.
func Wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Retry calls try until it succeeds, at most attempts times, waiting
// delay(n) after the n-th attempt that failed. It returns nil once try
// does, try's last error when every attempt failed, or ctx's error when
// ctx ends while it waits.
func Retry(ctx context.Context, attempts int, delay func(n int) time.Duration, try func() error) error {
	var err error
	for n := 1; n <= attempts; n++ {
		if err = try(); err == nil || n == attempts {
			return err
		}
		if werr := Wait(ctx, delay(n)); werr != nil {
			return werr
		}
	}
	return err
}

// Linear is the schedule step, 2 step, 3 step, ...
func Linear(step time.Duration) func(n int) time.Duration {
	return func(n int) time.Duration { return time.Duration(n) * step }
}

// Constant is the schedule d, d, d, ...
func Constant(d time.Duration) func(int) time.Duration {
	return func(int) time.Duration { return d }
}
