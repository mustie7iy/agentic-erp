package assets

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
)

// totalsOf sums, per key, the movements of each asset's latest posting
// (those whose seq is the asset's last_seq) by the history sheet's columns
// (ADR-0057 §5, §9): the cost of acquisitions, of retirements and of
// transfers in and out, and the accumulated depreciation of depreciation
// (runs and catch-ups), retirements and transfers. Its select list ends
// with those six sums; keys are the columns before them.
const totalsOf = `coalesce(sum(m.cost) FILTER (WHERE m.kind = 'acquisition'), 0),
		coalesce(sum(m.cost) FILTER (WHERE m.kind = 'retirement'), 0),
		coalesce(sum(m.cost) FILTER (WHERE m.kind IN ('transfer_in', 'transfer_out')), 0),
		coalesce(sum(m.accumulated) FILTER (WHERE m.kind = 'depreciation'), 0),
		coalesce(sum(m.accumulated) FILTER (WHERE m.kind = 'retirement'), 0),
		coalesce(sum(m.accumulated) FILTER (WHERE m.kind IN ('transfer_in', 'transfer_out')), 0)
	FROM assets.asset a JOIN assets.movement m ON m.tenant_id = a.tenant_id AND m.asset_id = a.id AND m.seq = a.last_seq
	WHERE a.id = ANY($1)`

// addedTo adds the sums of a totals row being inserted to the row there.
const addedTo = `acquisitions = t.acquisitions + EXCLUDED.acquisitions, retired_cost = t.retired_cost + EXCLUDED.retired_cost,
		transferred_cost = t.transferred_cost + EXCLUDED.transferred_cost, depreciation = t.depreciation + EXCLUDED.depreciation,
		retired_accumulated = t.retired_accumulated + EXCLUDED.retired_accumulated,
		transferred_accumulated = t.transferred_accumulated + EXCLUDED.transferred_accumulated`

// keepTotals adds the movements of the assets' latest posting to the year
// totals the history sheet reads (ADR-0057 §5, §9): each asset's per
// ledger and fiscal year, and its class's per company, ledger and fiscal
// year. It is called once per transaction, after the posting has written
// its movements and set each asset's last_seq to its seq (derive, or a
// run's batch). After it the transaction takes only locks every posting
// takes after its own keepTotals (its own assets' rows, a run's batch and
// Operation, the audit chain's head at commit), never a class row. It
// upserts the class rows first and then the asset rows, each statement in
// key order, so two postings touching the same classes in any order wait
// for each other rather than deadlock (TestYearTotalsInKeyOrder); the
// asset rows are the posting's own, whose assets it holds FOR UPDATE.
// Last it adds the posting's depreciation to the assets' terms
// (keepBooked), rows of the same assets.
func keepTotals(ctx context.Context, tx pgx.Tx, assets ...uuid.UUID) error {
	if _, err := tx.Exec(ctx, `INSERT INTO assets.class_year_total AS t (tenant_id, legal_entity_id, ledger_id, fiscal_year, class_code, acquisitions,
			retired_cost, transferred_cost, depreciation, retired_accumulated, transferred_accumulated)
		SELECT m.tenant_id, m.legal_entity_id, m.ledger_id, m.fiscal_year, m.class_code, `+totalsOf+`
		GROUP BY 1, 2, 3, 4, 5 ORDER BY 1, 2, 3, 4, 5
		ON CONFLICT (tenant_id, legal_entity_id, ledger_id, fiscal_year, class_code) DO UPDATE SET `+addedTo, assets); err != nil {
		return fmt.Errorf("assets: class year totals of %d assets: %w", len(assets), err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO assets.year_total AS t (tenant_id, ledger_id, asset_id, fiscal_year, legal_entity_id, class_code, acquisitions,
			retired_cost, transferred_cost, depreciation, retired_accumulated, transferred_accumulated)
		SELECT m.tenant_id, m.ledger_id, m.asset_id, m.fiscal_year, m.legal_entity_id, m.class_code, `+totalsOf+`
		GROUP BY 1, 2, 3, 4, 5, 6 ORDER BY 1, 2, 3, 4
		ON CONFLICT (tenant_id, ledger_id, asset_id, fiscal_year) DO UPDATE SET `+addedTo, assets); err != nil {
		return fmt.Errorf("assets: year totals of %d assets: %w", len(assets), err)
	}
	return keepBooked(ctx, tx, assets)
}

// keepBooked adds the depreciation of the assets' latest posting to the
// depreciation booked on their terms in each ledger
// (asset_terms.booked_depreciation, minus the sum of the depreciation
// movements: a debit of expense, positive), which a run's batches, the
// preview and a catch-up read rather than the movements (readMoves,
// T57f3). The terms rows are the posting's own assets', which it holds
// FOR UPDATE; they are locked in key order all the same, by a statement of
// their own (a locking CTE joined to the update was estimated at one row
// and compared every pair, 0.2 s a batch of 500), so the update never
// waits in an order another could reverse.
func keepBooked(ctx context.Context, tx pgx.Tx, assets []uuid.UUID) error {
	if _, err := tx.Exec(ctx, `SELECT FROM assets.asset_terms WHERE asset_id = ANY($1) ORDER BY tenant_id, asset_id, ledger_id FOR UPDATE`,
		assets); err != nil {
		return fmt.Errorf("assets: lock the terms of %d assets: %w", len(assets), err)
	}
	_, err := tx.Exec(ctx, `UPDATE assets.asset_terms t SET booked_depreciation = t.booked_depreciation - d.accumulated, version = t.version + 1
		FROM (SELECT m.tenant_id, m.asset_id, m.ledger_id, sum(m.accumulated) AS accumulated
			FROM assets.asset a JOIN assets.movement m ON m.tenant_id = a.tenant_id AND m.asset_id = a.id AND m.seq = a.last_seq
			WHERE a.id = ANY($1) AND m.kind = 'depreciation' GROUP BY 1, 2, 3) d
		WHERE t.tenant_id = d.tenant_id AND t.asset_id = d.asset_id AND t.ledger_id = d.ledger_id`, assets)
	return errs.Wrapf(err, "assets: depreciation booked of %d assets", len(assets))
}
