package ledger_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestPeriodRunsInOrder: a company's first run and a run for a later
// period follow; a second run for the last run's period is the context's
// Exists refusal and one for an earlier period its NotLater; a failed lock
// or read is returned with the runs' name.
func TestPeriodRunsInOrder(t *testing.T) {
	exists, notLater := errors.New("exists"), errors.New("not later")
	runs := ledger.PeriodRuns{Table: pgx.Identifier{"pg_temp", "run"}, Lock: "test.run", Name: "test: runs",
		Exists:   func(string, int16, int16) error { return exists },
		NotLater: func(string, int16, int16, time.Time) error { return notLater }}
	company := uuid.New()
	march := ledger.FiscalPeriod{FiscalYear: 2026, Number: 3, End: time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC)}
	april := ledger.FiscalPeriod{FiscalYear: 2026, Number: 4, End: time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)}
	february := ledger.FiscalPeriod{FiscalYear: 2026, Number: 2, End: time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC)}
	inTx := func(t *testing.T, fn func(ctx context.Context, tx pgx.Tx)) {
		t.Helper()
		ctx := context.Background()
		tx, err := pgtest.Pool(t).Begin(ctx, database.Scope{})
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `CREATE TEMP TABLE run (legal_entity_id uuid, fiscal_year smallint, fiscal_period smallint, posting_date date) ON COMMIT DROP`); err != nil {
			t.Fatalf("create the runs: %v", err)
		}
		fn(ctx, tx)
	}
	t.Run("first run", func(t *testing.T) {
		inTx(t, func(ctx context.Context, tx pgx.Tx) {
			if err := runs.InOrder(ctx, tx, uuid.New(), company, "1000", march); err != nil {
				t.Errorf("got %v, want nil", err)
			}
		})
	})
	for name, c := range map[string]struct {
		period ledger.FiscalPeriod
		want   error
	}{"later": {april, nil}, "same": {march, exists}, "earlier": {february, notLater}} {
		t.Run(name, func(t *testing.T) {
			inTx(t, func(ctx context.Context, tx pgx.Tx) {
				if _, err := tx.Exec(ctx, `INSERT INTO run VALUES ($1, 2026, 3, '2026-03-31')`, company); err != nil {
					t.Fatalf("insert a run: %v", err)
				}
				if err := runs.InOrder(ctx, tx, uuid.New(), company, "1000", c.period); !errors.Is(err, c.want) {
					t.Errorf("got %v, want %v", err, c.want)
				}
			})
		})
	}
	for name, at := range map[string]int{"lock": 1, "last run": 2} {
		t.Run("failed "+name, func(t *testing.T) {
			inTx(t, func(ctx context.Context, tx pgx.Tx) {
				err := runs.InOrder(ctx, &pgtest.FailingTx{Tx: tx, FailAt: at}, uuid.New(), company, "1000", march)
				if !errors.Is(err, pgtest.ErrInjected) {
					t.Errorf("got %v, want the injected failure", err)
				}
			})
		})
	}
}
