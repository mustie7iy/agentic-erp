package extract_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// TestSchedulerRefreshed: a run that published a manifest calls Refreshed
// with its tenant, and its failure is the round's; a run that found
// nothing new calls nothing.
func TestSchedulerRefreshed(t *testing.T) {
	c := newScheduled(t)
	var called []uuid.UUID
	down := errors.New("down")
	c.s.Refreshed = func(_ context.Context, tenant uuid.UUID) error {
		called = append(called, tenant)
		return down
	}
	c.post(t, "2026-03-02", "1.00")
	c.mark(t, "ledger")
	if _, err := c.s.RunOnce(context.Background()); !errors.Is(err, down) {
		t.Fatalf("round: got %v, want Refreshed's failure", err)
	}
	if len(called) != 1 || called[0] != c.Tenant.ID {
		t.Fatalf("refreshed: got %v, want the tenant once", called)
	}
	c.mark(t, "ledger")
	c.aged(t)
	c.runOnce(t)
	if len(called) != 1 {
		t.Errorf("a run with nothing new: got %d calls, want none more", len(called)-1)
	}
}
