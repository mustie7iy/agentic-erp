package backoff_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/backoff"
)

func TestRetry(t *testing.T) {
	failing := errors.New("not yet")
	cases := map[string]struct {
		attempts, succeedAt int // succeedAt 0: never
		wantCalls           int
		wantErr             error
	}{
		"first attempt":    {attempts: 3, succeedAt: 1, wantCalls: 1},
		"third attempt":    {attempts: 3, succeedAt: 3, wantCalls: 3},
		"every one failed": {attempts: 3, wantCalls: 3, wantErr: failing},
		"no attempt":       {attempts: 0, wantCalls: 0},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			calls := 0
			err := backoff.Retry(context.Background(), c.attempts, backoff.Constant(time.Millisecond), func() error {
				calls++
				if calls == c.succeedAt {
					return nil
				}
				return failing
			})
			if !errors.Is(err, c.wantErr) || (c.wantErr == nil && err != nil) {
				t.Errorf("error: got %v, want %v", err, c.wantErr)
			}
			if calls != c.wantCalls {
				t.Errorf("calls: got %d, want %d", calls, c.wantCalls)
			}
		})
	}
}

// TestRetryEndsWithItsContext: a context that ends while Retry waits ends
// the retries with the context's error.
func TestRetryEndsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := backoff.Retry(ctx, 5, backoff.Constant(time.Hour), func() error {
		calls++
		cancel()
		return errors.New("not yet")
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error: got %v, want %v", err, context.Canceled)
	}
	if calls != 1 {
		t.Errorf("calls: got %d, want 1", calls)
	}
}

func TestSchedules(t *testing.T) {
	cases := map[string]struct {
		got, want time.Duration
	}{
		"linear, first":   {backoff.Linear(250 * time.Millisecond)(1), 250 * time.Millisecond},
		"linear, third":   {backoff.Linear(250 * time.Millisecond)(3), 750 * time.Millisecond},
		"constant, fifth": {backoff.Constant(200 * time.Millisecond)(5), 200 * time.Millisecond},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if c.got != c.want {
				t.Errorf("delay: got %v, want %v", c.got, c.want)
			}
		})
	}
}

func TestWait(t *testing.T) {
	if err := backoff.Wait(context.Background(), time.Millisecond); err != nil {
		t.Errorf("wait: got %v, want nil", err)
	}
}
