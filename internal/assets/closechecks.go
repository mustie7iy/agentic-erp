package assets

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// CloseChecks is the assets context's published port for a financial
// close's depreciation check (ADR-0058 §2, §6, T64k), run in the
// request's transaction under row-level security.
type CloseChecks struct{}

// PeriodDepreciation is a company's depreciation of a period in a ledger
// as a close's check reads it: its latest posted run of the period, nil
// without one, and, only without one, how many assets a run of the period
// would select.
type PeriodDepreciation struct {
	Run      *uuid.UUID
	Selected int
}

// Depreciation reads the legal entity's depreciation of the fiscal year
// and period, ending on end, in the ledger. Only a posted run counts: a
// failed one (a cancelled one ends failed, T57f) or one still queued or
// running does not. Without a posted run it counts the assets a run would
// select, through the run's and the preview's own selection
// (selectDepreciable), so "nothing to depreciate" means what a run would
// find (ADR-0057 §7 step 1). A legal entity the caller may not act in is
// an error, not "nothing to depreciate": a check must not pass on what it
// could not read.
func (CloseChecks) Depreciation(ctx context.Context, legalEntity, ledger uuid.UUID, fiscalYear, period int16, end time.Time) (PeriodDepreciation, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return PeriodDepreciation{}, fmt.Errorf("assets: depreciation of %s: %w", legalEntity, err)
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return PeriodDepreciation{}, fmt.Errorf("assets: depreciation of %s: %w", legalEntity, err)
	}
	if !access.Allows(legalEntity) {
		return PeriodDepreciation{}, fmt.Errorf("assets: depreciation of %s: not a company the caller may act in", legalEntity)
	}
	var run uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM assets.depreciation_run WHERE legal_entity_id = $1 AND ledger_id = $2 AND fiscal_year = $3
		AND fiscal_period = $4 AND status = 'posted' ORDER BY created_at DESC, id DESC LIMIT 1`, legalEntity, ledger, fiscalYear, period).Scan(&run)
	switch {
	case err == nil:
		return PeriodDepreciation{Run: &run}, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return PeriodDepreciation{}, fmt.Errorf("assets: posted depreciation run of %s in ledger %s, %d/%d: %w", legalEntity, ledger, fiscalYear, period, err)
	}
	cs, err := selectDepreciable(ctx, tx, legalEntity, ledger, Period{FiscalYear: fiscalYear, Number: period, End: end}, nil, nil)
	if err != nil {
		return PeriodDepreciation{}, err
	}
	return PeriodDepreciation{Selected: len(cs)}, nil
}
