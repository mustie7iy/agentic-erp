package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ensure runs EnsurePartitions in a transaction of its own.
func ensure(ctx context.Context, conn *pgx.Conn, now time.Time) (created []string, err error) {
	err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		created, err = EnsurePartitions(ctx, tx, now)
		return err
	})
	return created, err
}

// failingTx fails its nth statement.
type failingTx struct {
	pgx.Tx
	failAt, calls int
}

var errInjected = errors.New("injected")

func (f *failingTx) fail() bool { f.calls++; return f.calls == f.failAt }

func (f *failingTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if f.fail() {
		return pgconn.CommandTag{}, errInjected
	}
	return f.Tx.Exec(ctx, sql, args...)
}

type errRow struct{}

func (errRow) Scan(...any) error { return errInjected }

func (f *failingTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if f.fail() {
		return errRow{}
	}
	return f.Tx.QueryRow(ctx, sql, args...)
}

func TestPartitionsStayAhead(t *testing.T) {
	conn := emptyDatabase(t)
	ctx := context.Background()
	if _, err := Migrate(ctx, conn); err != nil {
		t.Fatal(err)
	}
	// The migrations create audit months to the end of 2027 and journal years to 2035.
	months, years, err := Horizon(ctx, conn, time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC))
	if err != nil || months != 14 || years != 9 {
		t.Fatalf("horizon in 2026-10: %d months, %d years, %v", months, years, err)
	}
	// Late in 2027 the audit horizon is too short for readiness; maintenance extends it.
	late := time.Date(2027, 11, 15, 0, 0, 0, 0, time.UTC)
	if months, _, _ = Horizon(ctx, conn, late); months >= MinAuditMonths {
		t.Fatalf("horizon in 2027-11: %d months", months)
	}
	created, err := ensure(ctx, conn, late)
	if err != nil || len(created) != AuditMonthsAhead-1 || created[0] != "audit.audit_event_202801" {
		t.Fatalf("created %v, %v", created, err)
	}
	if months, years, _ = Horizon(ctx, conn, late); months != AuditMonthsAhead || years != 8 {
		t.Fatalf("horizon after maintenance: %d months, %d years", months, years)
	}
	// Journal years are created the same way; a second run creates nothing.
	far := time.Date(2034, 6, 1, 0, 0, 0, 0, time.UTC)
	if created, err = ensure(ctx, conn, far); err != nil || len(created) != 2*(2037-2035)+AuditMonthsAhead+1 {
		t.Fatalf("created %v, %v", created, err)
	}
	if _, years, _ = Horizon(ctx, conn, far); years != LedgerYearsAhead {
		t.Fatalf("ledger horizon: %d", years)
	}
	if created, err = ensure(ctx, conn, far); err != nil || len(created) != 0 {
		t.Fatalf("second run created %v, %v", created, err)
	}
	// Database errors are reported: the horizon without a database, the
	// maintenance without the schema, and every statement of the
	// maintenance failing in turn.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := Horizon(cancelled, conn, far); err == nil {
		t.Fatal("horizon without a database")
	}
	if _, err := ensure(ctx, emptyDatabase(t), far); err == nil {
		t.Fatal("maintenance without the schema")
	}
	next := time.Date(2038, 1, 1, 0, 0, 0, 0, time.UTC) // everything is missing: every statement runs
	for n := 1; ; n++ {
		var err error
		if txErr := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			_, err = EnsurePartitions(ctx, &failingTx{Tx: tx, failAt: n}, next)
			return errInjected // keep the database as it was
		}); !errors.Is(txErr, errInjected) {
			t.Fatalf("statement %d: transaction: %v", n, txErr)
		}
		if err == nil {
			if n < 10 {
				t.Fatalf("succeeded after only %d statements", n)
			}
			break
		}
		if !errors.Is(err, errInjected) {
			t.Fatalf("statement %d: %v", n, err)
		}
	}
}
