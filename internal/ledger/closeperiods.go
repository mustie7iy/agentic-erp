package ledger

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// ClosePeriods is the ledger's port for the posting periods a financial
// close reads (ADR-0058 §2, §3): the close calendar is one row per regular
// period of a company's leading ledger.
type ClosePeriods struct{}

// Leading returns the leading ledger's regular posting periods of the
// legal entity in the fiscal year, by period: none for a legal entity the
// caller may not act in. Special periods are the year-end close's, which
// T64 leaves out (ADR-0058 §1).
func (ClosePeriods) Leading(ctx context.Context, legalEntity uuid.UUID, fiscalYear int16) ([]Period, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, periodSelect+` WHERE ($1 OR legal_entity_id = ANY($2)) AND legal_entity_id = $3 AND fiscal_year = $4 AND NOT is_special
		AND ledger_id = (SELECT g.id FROM ledger.ledger g WHERE g.is_leading) ORDER BY period`, access.All, access.IDs, legalEntity, fiscalYear)
	periods, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Period, error) { return scanPeriod(row) })
	if err != nil {
		return nil, fmt.Errorf("ledger: periods of %s in fiscal year %d: %w", legalEntity, fiscalYear, err)
	}
	return periods, nil
}
