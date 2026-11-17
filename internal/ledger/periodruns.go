package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PeriodRuns are a context's runs per company and fiscal period that must
// follow one another, each reading what the runs before it posted (lease
// runs, revenue recognition runs; ADR-0073 §9): their table and the
// advisory lock the database's rules on that table take. Each context
// refuses a run out of order with its own problem codes, which its
// consumers already know, from what InOrder reports: a refusal built in
// the context, not here, keeps each operation's codes its own.
type PeriodRuns struct {
	// Table holds the runs: legal_entity_id, fiscal_year, fiscal_period and
	// posting_date (the period's last day).
	Table pgx.Identifier
	// Lock is the advisory lock's namespace: the key is
	// '<Lock>:<tenant>:<company>', as the table's rules compute it.
	Lock string
	// Name names the runs in errors: "leases: lease runs".
	Name string
}

// RunOrder is where a new run falls against the company's last run.
type RunOrder int

const (
	// RunFollows: no run yet, or the last is for an earlier period.
	RunFollows RunOrder = iota
	// RunSamePeriod: the last run is for the same period.
	RunSamePeriod
	// RunEarlierPeriod: the last run is for a later period than the new one.
	RunEarlierPeriod
)

// LastRun is the company's last run: its period and posting date.
type LastRun struct {
	FiscalYear, Period int16
	PostingDate        time.Time
}

// InOrder holds the company's runs until the transaction ends and tells
// where a run for period falls against the last of them, which it returns
// (zero when there is none) for the caller's refusal.
func (r PeriodRuns) InOrder(ctx context.Context, tx pgx.Tx, tenant, company uuid.UUID, code string, period FiscalPeriod) (LastRun, RunOrder, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text || ':' || $2::uuid::text || ':' || $3::uuid::text, 0))`,
		r.Lock, tenant, company); err != nil {
		return LastRun{}, RunFollows, fmt.Errorf("%s: hold those of company %s: %w", r.Name, code, err)
	}
	var last LastRun
	err := tx.QueryRow(ctx, `SELECT fiscal_year, fiscal_period, posting_date FROM `+r.Table.Sanitize()+` WHERE legal_entity_id = $1
		ORDER BY posting_date DESC LIMIT 1`, company).Scan(&last.FiscalYear, &last.Period, &last.PostingDate)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return LastRun{}, RunFollows, nil
	case err != nil:
		return LastRun{}, RunFollows, fmt.Errorf("%s: the last of company %s: %w", r.Name, code, err)
	case last.FiscalYear == period.FiscalYear && last.Period == period.Number:
		return last, RunSamePeriod, nil
	case !last.PostingDate.Before(period.End):
		return last, RunEarlierPeriod, nil
	}
	return last, RunFollows, nil
}
