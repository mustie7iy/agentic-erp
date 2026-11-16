package worker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestQueue: a round drains each tenant's queue, leaves a tenant whose item
// fails for the next round while the others go on, returns the failures
// with the queue's name and tenant, fails whole when the tenants cannot be
// listed, and claims nothing once its context ends.
func TestQueue(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	broken := errors.New("broken")
	queued := func(items map[uuid.UUID]int, fail map[uuid.UUID]bool) Queue {
		return Queue{Name: "test queue",
			Tenants: func(context.Context) ([]uuid.UUID, error) { return []uuid.UUID{a, b}, nil },
			One: func(_ context.Context, tenant uuid.UUID) (bool, error) {
				if fail[tenant] {
					return false, broken
				}
				if items[tenant] == 0 {
					return false, nil
				}
				items[tenant]--
				return true, nil
			}}
	}
	t.Run("drains every tenant", func(t *testing.T) {
		items := map[uuid.UUID]int{a: 2, b: 3}
		n, err := queued(items, nil).RunOnce(context.Background())
		if err != nil {
			t.Fatalf("round: %v", err)
		}
		if n != 5 {
			t.Errorf("items taken: got %d, want 5", n)
		}
	})
	t.Run("a failing tenant is left, the others go on", func(t *testing.T) {
		items := map[uuid.UUID]int{a: 2, b: 3}
		n, err := queued(items, map[uuid.UUID]bool{a: true}).RunOnce(context.Background())
		if !errors.Is(err, broken) {
			t.Fatalf("got %v, want %v", err, broken)
		}
		if want := "test queue: tenant " + a.String() + ": broken"; err.Error() != want {
			t.Errorf("error: got %q, want %q", err, want)
		}
		if n != 3 {
			t.Errorf("items taken: got %d, want 3", n)
		}
	})
	t.Run("tenants not listed", func(t *testing.T) {
		q := Queue{Name: "test queue", Tenants: func(context.Context) ([]uuid.UUID, error) { return nil, broken }}
		again, err := q.Round(context.Background())
		if !errors.Is(err, broken) || !strings.HasPrefix(err.Error(), "test queue: ") {
			t.Errorf("got %v, want %v named by the queue", err, broken)
		}
		if again {
			t.Error("again: got true, want false")
		}
	})
	t.Run("a stopping worker claims nothing", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		items := map[uuid.UUID]int{a: 2, b: 3}
		if n, err := queued(items, nil).RunOnce(ctx); n != 0 || err != nil {
			t.Errorf("got %d, %v, want 0, nil", n, err)
		}
	})
}
