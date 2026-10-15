// Package worker runs a background worker's rounds until its context ends:
// the in-process polling workers of ADR-0012 (proposals, releases,
// simulations, dispatch), the outbox relay of ADR-0007, the balance
// projector, and the periodic audit, offboarding, directory, key rotation
// and contract expiry rounds. Each worker keeps its own RunOnce; this
// package is the loop they all repeated (CLAUDE.md: never copy a helper).
package worker

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"
)

// Round does one round of work. again asks for the next round at once
// instead of after the interval: a round that filled its batch has more
// waiting.
type Round func(ctx context.Context) (again bool, err error)

// Logger is where Loop reports failed rounds; *slog.Logger is one.
type Logger interface {
	ErrorContext(ctx context.Context, msg string, args ...any)
}

// Loop runs round until ctx ends, waiting interval between rounds, or
// fallback when interval is not positive. A failed round is logged as msg
// with the error (Report) and retried after the interval; an error after
// ctx ended is the shutdown, not a failure, and is not logged. A round that
// panics fails like any other (Recover), logged once with its stack: one
// bad row must not crash the replica serving every tenant.
func Loop(ctx context.Context, interval, fallback time.Duration, round Round, log Logger, msg string) {
	if interval <= 0 {
		interval = fallback
	}
	for {
		var again bool
		err := Recover(func() error {
			var rerr error
			again, rerr = round(ctx)
			return rerr
		})
		if Failed(ctx, err) {
			Report(ctx, log, msg, err)
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

// Panic is a panic Recover caught: its value and the stack it was raised
// on, which the error text alone would lose.
type Panic struct {
	Value any
	Stack []byte
}

func (p *Panic) Error() string { return fmt.Sprintf("panic: %v", p.Value) }

// Recover runs fn and returns what it returns, or a panic in it as a
// *Panic. CLAUDE.md bans panic, not recover: a background worker has no
// HTTP middleware above it (telemetry recovers a request's panic), so
// without this a panic in any worker would end the process and every
// tenant's requests with it. Recover does not log: its caller reports the
// error once (Report), stack included, where logging here too would log
// the panic twice, once more as the caller's failure.
func Recover(fn func() error) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = &Panic{Value: rec, Stack: debug.Stack()}
		}
	}()
	return fn()
}

// Report logs err as msg with args, and with the stack when err is a
// recovered panic (Recover).
func Report(ctx context.Context, log Logger, msg string, err error, args ...any) {
	args = append(args, "error", err)
	var p *Panic
	if errors.As(err, &p) {
		args = append(args, "stack", string(p.Stack))
	}
	log.ErrorContext(ctx, msg, args...)
}

// Failed reports whether err is a failure to log: an error while ctx is
// still live. A round reporting its work uses it to stay silent exactly
// when Loop logs the failure.
func Failed(ctx context.Context, err error) bool {
	return err != nil && ctx.Err() == nil
}
