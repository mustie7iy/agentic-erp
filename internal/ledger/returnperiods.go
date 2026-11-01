package ledger

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ReturnPeriods is the ledger's port for the posting periods a tax return
// touches (ADR-0049 §11, T32): preparing one needs its companies' periods
// in its tax period closed, so no posting is under way, and recording it
// filed locks them.
type ReturnPeriods struct{}

// Overlapping returns the leading ledger's regular posting periods of the
// legal entities, each one the caller may act in, that overlap start to
// end, by company, year and period. Special periods are left out: they
// are the year-end close's, and a quarter's filing does not lock them.
func (ReturnPeriods) Overlapping(ctx context.Context, legalEntities []uuid.UUID, start, end time.Time) ([]Period, error) {
	tx, err := taxItemsOf(ctx, legalEntities)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, periodSelect+` WHERE legal_entity_id = ANY($1) AND NOT is_special AND start_date <= $3 AND end_date >= $2
		AND ledger_id = (SELECT g.id FROM ledger.ledger g WHERE g.is_leading) ORDER BY legal_entity_id, fiscal_year, period`, legalEntities, start, end)
	periods, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Period, error) { return scanPeriod(row) })
	if err != nil {
		return nil, fmt.Errorf("ledger: posting periods from %s to %s: %w", start.Format(time.DateOnly), end.Format(time.DateOnly), err)
	}
	return periods, nil
}

// Lock locks a closed period as lockFiscalPeriod does, audited and
// published as that is. No version is asked: the caller read the period
// in its own transaction, and the lock reads it again FOR UPDATE and
// refuses one that is no longer closed.
func (ReturnPeriods) Lock(ctx context.Context, id uuid.UUID) (Period, error) {
	return LockPeriod(ctx, id, func(int32) error { return nil })
}
