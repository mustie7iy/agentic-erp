package ledger_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
)

// TestConsolidationReadsOutsideRequest: the ledger's reads for
// consolidation run only in a request's or activity's unit of work; their
// figures and database failures are tested through the consolidation
// runs (internal/consolidation).
func TestConsolidationReadsOutsideRequest(t *testing.T) {
	ctx, r, id := context.Background(), ledger.ConsolidationReads{}, uuid.New()
	for name, read := range map[string]func() error{
		"the leading ledger": func() error { _, err := r.LeadingLedger(ctx); return err },
		"a period":           func() error { _, _, err := r.PeriodOf(ctx, id, id, 2026, 11); return err },
		"periods":            func() error { _, err := r.PeriodsOf(ctx, []uuid.UUID{id}, id, 2026, 11); return err },
		"the accounts":       func() error { _, err := r.AccountsOf(ctx, id); return err },
		"the balances":       func() error { _, err := r.BalancesThrough(ctx, id, id, 2026, 11); return err },
		"a rate":             func() error { _, _, err := r.RateOn(ctx, "average", "USD", "EUR", time.Now()); return err },
	} {
		if err := read(); err == nil {
			t.Errorf("%s outside a unit of work: got no error, want one", name)
		}
	}
}
