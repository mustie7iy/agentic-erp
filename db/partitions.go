package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Partitioned tables need partitions ahead of time (05-scalability §5.3):
// audit.audit_event by month and the journal by fiscal year. The server
// keeps them ahead with EnsurePartitions and readiness checks Horizon, so a
// maintenance job that stopped shows before a posting fails (05 §5.7).

// AuditMonthsAhead and LedgerYearsAhead are how far EnsurePartitions
// creates partitions beyond the current month and year.
const (
	AuditMonthsAhead = 6
	LedgerYearsAhead = 3
)

// MinAuditMonths and MinLedgerYears are the horizons readiness requires.
const (
	MinAuditMonths = 3
	MinLedgerYears = 2
)

// Horizon counts the future partitions: audit months after the current
// month and journal years after the current year.
func Horizon(ctx context.Context, conn *pgx.Conn, now time.Time) (auditMonths, ledgerYears int, err error) {
	month := now.UTC().Format("200601")
	year := fmt.Sprint(now.UTC().Year())
	err = conn.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = 'audit' AND c.relname ~ '^audit_event_[0-9]{6}$' AND substr(c.relname, 13) > $1),
		(SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = 'ledger' AND c.relname ~ '^journal_entry_y[0-9]{4}$' AND substr(c.relname, 16) > $2)`, month, year).Scan(&auditMonths, &ledgerYears)
	if err != nil {
		return 0, 0, fmt.Errorf("db: partition horizon: %w", err)
	}
	return auditMonths, ledgerYears, nil
}

// EnsurePartitions creates the partitions missing up to the horizons in the
// transaction and returns their names. It runs as the migrations' role,
// which owns the tables; replicas starting together serialize on the
// migration lock for the transaction.
func EnsurePartitions(ctx context.Context, tx pgx.Tx, now time.Time) ([]string, error) {
	var created []string
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, lockKey); err != nil {
		return nil, fmt.Errorf("db: ensure partitions: %w", err)
	}
	first := time.Date(now.UTC().Year(), now.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i <= AuditMonthsAhead; i++ {
		m := first.AddDate(0, i, 0)
		name := "audit_event_" + m.Format("200601")
		if err := create(ctx, tx, &created, "audit", name, fmt.Sprintf(`CREATE TABLE audit.%s PARTITION OF audit.audit_event FOR VALUES FROM ('%s') TO ('%s')`,
			name, m.Format(time.DateOnly), m.AddDate(0, 1, 0).Format(time.DateOnly))); err != nil {
			return nil, fmt.Errorf("db: ensure partitions: %w", err)
		}
	}
	for y := now.UTC().Year(); y <= now.UTC().Year()+LedgerYearsAhead; y++ {
		for _, table := range []string{"journal_entry", "journal_line"} {
			name := fmt.Sprintf("%s_y%d", table, y)
			if err := create(ctx, tx, &created, "ledger", name, fmt.Sprintf(`CREATE TABLE ledger.%s PARTITION OF ledger.%s FOR VALUES FROM (%d) TO (%d)`,
				name, table, y, y+1)); err != nil {
				return nil, fmt.Errorf("db: ensure partitions: %w", err)
			}
		}
	}
	return created, nil
}

// create runs ddl unless the partition exists, and records what it created.
func create(ctx context.Context, tx pgx.Tx, created *[]string, schema, name, ddl string) error {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1 AND c.relname = $2)`,
		schema, name).Scan(&exists); err != nil {
		return fmt.Errorf("check partition %s.%s: %w", schema, name, err)
	}
	if exists {
		return nil
	}
	if _, err := tx.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("create partition %s.%s: %w", schema, name, err)
	}
	*created = append(*created, schema+"."+name)
	return nil
}
