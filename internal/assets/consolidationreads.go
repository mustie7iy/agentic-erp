package assets

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// ConsolidationReads are the assets context's published reads for group
// reporting (ADR-0057 As built, the consolidated sheet; ADR-0059 §2): the
// composition root adapts them to the consolidation context's port, so
// neither leaf imports the other. They run in the request's transaction
// under row-level security (ADR-0003) and apply no company access: the
// consolidation context applies it, to a group as a whole (ADR-0059 §12).
type ConsolidationReads struct{}

// ClassTotal is one asset class's sums in a company's ledger, in its
// local currency, by the history sheet's columns (ADR-0057 §9, signed as
// the sheet shows them): of every fiscal year before the year read
// (Period 0), whose cost and accumulated depreciation are the class's
// opening values, or of one period of that year (1-16), whose profit and
// loss columns also carry their booked group amounts (the *GC fields;
// zero for Period 0).
type ClassTotal struct {
	Class                                                     string
	Period                                                    int16
	Acquisitions, Retirements, Transfers                      money.Minor
	Depreciation, ImpairmentLosses, ImpairmentReversals       money.Minor
	RetirementsAccumulated, TransfersAccumulated              money.Minor
	DepreciationGC, ImpairmentLossesGC, ImpairmentReversalsGC money.Minor
}

// ClassTotals are the company's class totals in the ledger before the
// fiscal year (Period 0) and in each of its periods with a movement, by
// class code and period: at most classes × (16 + 1) rows, read from the
// class year totals and the class period totals kept with every posting
// (keepTotals), never from the movements.
func (ConsolidationReads) ClassTotals(ctx context.Context, legalEntity, ledgerID uuid.UUID, year int16) ([]ClassTotal, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT class_code, 0::smallint, sum(acquisitions), sum(retired_cost), sum(transferred_cost), sum(depreciation),
			sum(impairment_losses), sum(impairment_reversals), sum(retired_accumulated), sum(transferred_accumulated), 0, 0, 0
		FROM assets.class_year_total WHERE legal_entity_id = $1 AND ledger_id = $2 AND fiscal_year < $3 GROUP BY class_code
		UNION ALL
		SELECT class_code, fiscal_period, acquisitions, retired_cost, transferred_cost, depreciation, impairment_losses, impairment_reversals,
			retired_accumulated, transferred_accumulated, depreciation_gc, impairment_losses_gc, impairment_reversals_gc
		FROM assets.class_period_total WHERE legal_entity_id = $1 AND ledger_id = $2 AND fiscal_year = $3
		ORDER BY 1, 2`, legalEntity, ledgerID, year)
	var out []ClassTotal
	var c ClassTotal
	_, err = pgx.ForEachRow(rows, []any{&c.Class, &c.Period, &c.Acquisitions, &c.Retirements, &c.Transfers, &c.Depreciation, &c.ImpairmentLosses,
		&c.ImpairmentReversals, &c.RetirementsAccumulated, &c.TransfersAccumulated, &c.DepreciationGC, &c.ImpairmentLossesGC, &c.ImpairmentReversalsGC},
		func() error { out = append(out, c); return nil })
	return out, errs.Wrapf(err, "assets: class totals of %s in %d", legalEntity, year)
}
