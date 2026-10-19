package worker

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// run runs Loop until round cancels its context and returns what was logged.
func run(t *testing.T, interval, fallback time.Duration, round func(ctx context.Context, cancel context.CancelFunc) (bool, error)) string {
	t.Helper()
	var out bytes.Buffer
	log := slog.New(slog.NewTextHandler(&out, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		Loop(ctx, interval, fallback, func(ctx context.Context) (bool, error) { return round(ctx, cancel) }, log, "round failed")
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Loop did not return after its context ended")
	}
	return out.String()
}

// TestLoop: rounds repeat after the interval (the fallback when none is
// set) or at once when asked, failures are logged while the context is live,
// and the loop stops when it ends.
func TestLoop(t *testing.T) {
	t.Run("rounds repeat until cancelled, with the fallback interval", func(t *testing.T) {
		rounds := 0
		logged := run(t, 0, time.Millisecond, func(_ context.Context, cancel context.CancelFunc) (bool, error) {
			rounds++
			if rounds == 3 {
				cancel()
			}
			return false, nil
		})
		if rounds != 3 {
			t.Errorf("got %d rounds, want 3", rounds)
		}
		if logged != "" {
			t.Errorf("got log %q, want none", logged)
		}
	})
	t.Run("again runs the next round without waiting", func(t *testing.T) {
		rounds := 0
		run(t, time.Hour, time.Hour, func(_ context.Context, cancel context.CancelFunc) (bool, error) {
			rounds++
			if rounds == 3 {
				cancel()
				return false, nil
			}
			return true, nil
		})
		if rounds != 3 {
			t.Errorf("got %d rounds, want 3", rounds)
		}
	})
	t.Run("a failed round is logged and waits for the interval even when it asks for another", func(t *testing.T) {
		rounds := 0
		logged := run(t, time.Hour, time.Hour, func(_ context.Context, cancel context.CancelFunc) (bool, error) {
			rounds++
			time.AfterFunc(50*time.Millisecond, cancel) // ends the wait: a round 2 would mean no wait
			return true, errors.New("boom")
		})
		if rounds != 1 {
			t.Errorf("got %d rounds, want 1", rounds)
		}
		if !strings.Contains(logged, `msg="round failed" error=boom`) {
			t.Errorf("got log %q, want the failure", logged)
		}
	})
	t.Run("a round that panics is logged with its stack and the next round runs", func(t *testing.T) {
		rounds := 0
		logged := run(t, time.Millisecond, time.Hour, func(_ context.Context, cancel context.CancelFunc) (bool, error) {
			rounds++
			if rounds == 1 {
				panic("boom")
			}
			cancel()
			return false, nil
		})
		if rounds != 2 {
			t.Errorf("got %d rounds, want 2", rounds)
		}
		if !strings.Contains(logged, `msg="round failed" error="panic: boom"`) {
			t.Errorf("got log %q, want the panic as the round's failure", logged)
		}
		if !strings.Contains(logged, "stack=") {
			t.Errorf("got log %q, want the panic's stack", logged)
		}
		if n := strings.Count(logged, "\n"); n != 1 {
			t.Errorf("got %d log records %q, want the panic logged once", n, logged)
		}
	})
	t.Run("an error after the context ended is not logged", func(t *testing.T) {
		logged := run(t, time.Millisecond, time.Hour, func(_ context.Context, cancel context.CancelFunc) (bool, error) {
			cancel()
			return false, context.Canceled
		})
		if logged != "" {
			t.Errorf("got log %q, want none", logged)
		}
	})
}

// TestRecover: a panic comes back as a *Panic with its stack, still
// matching the error it panicked with, if any.
func TestRecover(t *testing.T) {
	boom := errors.New("boom")
	t.Run("an error", func(t *testing.T) {
		err := Recover(func() error { panic(boom) })
		if !errors.Is(err, boom) {
			t.Errorf("got %v, want it to match the error panicked with", err)
		}
		var p *Panic
		if !errors.As(err, &p) || len(p.Stack) == 0 {
			t.Errorf("got %#v, want a *Panic with its stack", err)
		}
	})
	t.Run("another value", func(t *testing.T) {
		err := Recover(func() error { panic("boom") })
		if got := errors.Unwrap(err); got != nil {
			t.Errorf("unwrapped: got %v, want nil", got)
		}
		if got := err.Error(); got != "panic: boom" {
			t.Errorf("got %q, want panic: boom", got)
		}
	})
	t.Run("no panic", func(t *testing.T) {
		if err := Recover(func() error { return boom }); !errors.Is(err, boom) {
			t.Errorf("got %v, want boom", err)
		}
	})
}

// TestFailed: only an error while the context is live is a failure.
func TestFailed(t *testing.T) {
	live := context.Background()
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	for name, c := range map[string]struct {
		ctx  context.Context
		err  error
		want bool
	}{
		"error, live context":  {live, errors.New("boom"), true},
		"error, ended context": {ended, errors.New("boom"), false},
		"no error":             {live, nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := Failed(c.ctx, c.err); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// A timer already due when the context has ended does not start another
// round: the select alone would pick it about half the time.
func TestWaitCancelledWins(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := range 1000 {
		if Wait(ctx, 0) {
			t.Fatalf("wait %d: got run again, want stop after the cancel", i)
		}
	}
	if !Wait(context.Background(), time.Nanosecond) {
		t.Error("wait: got stop, want run again while the context lives")
	}
}
