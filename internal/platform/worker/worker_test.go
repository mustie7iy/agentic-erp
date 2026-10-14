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
	t.Run("a failed round is logged and retried", func(t *testing.T) {
		rounds := 0
		logged := run(t, time.Millisecond, time.Hour, func(_ context.Context, cancel context.CancelFunc) (bool, error) {
			rounds++
			if rounds == 2 {
				cancel()
				return false, nil
			}
			return true, errors.New("boom")
		})
		if rounds != 2 {
			t.Errorf("got %d rounds, want 2", rounds)
		}
		if !strings.Contains(logged, `msg="round failed" error=boom`) {
			t.Errorf("got log %q, want the failure", logged)
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
