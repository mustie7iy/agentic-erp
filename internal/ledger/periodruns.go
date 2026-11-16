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
// runs, revenue recognition runs; ADR-0073 §9): their table, the advisory
// lock the database's rules on that table take, and the context's
// refusals, whose codes and texts its consumers already know.
type PeriodRuns struct {
	// Table holds the runs: legal_entity_id, fiscal_year, fiscal_period and
	// posting_date (the period's last day).
	Table pgx.Identifier
	// Lock is the advisory lock's namespace: the key is
	// '<Lock>:<tenant>:<company>', as the table's rules compute it.
	Lock string
	// Name names the runs in errors: "leases: lease runs".
	Name string
	// Exists refuses a second run for the period of the company's last.
	Exists func(company string, year, number int16) error
	// NotLater refuses a run for a period before the last run's, which
	// was posted on day.
	NotLater func(company string, year, number int16, day time.Time) error
}

// InOrder holds the company's runs until the transaction ends and refuses
// a run for period that does not follow them: one for the period of the
// last run (Exists) or for an earlier one (NotLater).
func (r PeriodRuns) InOrder(ctx context.Context, tx pgx.Tx, tenant, company uuid.UUID, code string, period FiscalPeriod) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text || ':' || $2::uuid::text || ':' || $3::uuid::text, 0))`,
		r.Lock, tenant, company); err != nil {
		return fmt.Errorf("%s: hold those of company %s: %w", r.Name, code, err)
	}
	var year, number int16
	var day time.Time
	err := tx.QueryRow(ctx, `SELECT fiscal_year, fiscal_period, posting_date FROM `+r.Table.Sanitize()+` WHERE legal_entity_id = $1
		ORDER BY posting_date DESC LIMIT 1`, company).Scan(&year, &number, &day)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("%s: the last of company %s: %w", r.Name, code, err)
	case year == period.FiscalYear && number == period.Number:
		return r.Exists(code, year, number)
	case !day.Before(period.End):
		return r.NotLater(code, year, number, day)
	}
	return nil
}
