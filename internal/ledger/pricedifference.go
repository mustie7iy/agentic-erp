package ledger

// The order item and the tax borne of a price difference line (accounting
// specialist B10, po:2026-11-10; ADR-0057 §6, ADR-0049 §8). An invoiced
// order item's price difference and the input tax it bears post as one
// line on the price_difference account, which names no order item: a
// journal line's order item marks a GR/IR line. The ledger records both
// beside the line (ledger.price_difference_item, insert-only), in the
// transaction that posts it, and changes nothing that posts.

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// priceDifference is what is recorded of an invoiced order item's price
// difference line: the order item and the tax borne in the line, in the
// entry's currency, signed as the line.
type priceDifference struct {
	order    uuid.UUID
	item     int32
	taxBorne money.Minor
}

// keepPriceDifferences records, for each price difference line of the
// posted entry, its order item and the tax borne in it, in local currency
// at the posting's rate as the line itself is converted. lines are the
// entry's as they were posted, in line order. The entry is the posting
// group's first, the one open items and reports refer to; its copies in
// parallel ledgers carry the same lines and are not recorded again.
func (g grirPosting) keepPriceDifferences(ctx context.Context, tx pgx.Tx, e Entry, lines []Line) error {
	var nos, items []int32
	var orders []uuid.UUID
	var tcs, lcs []money.Minor
	for i, l := range lines {
		if l.differs == nil {
			continue
		}
		lc, err := convert(l.differs.taxBorne, g.rates.toLC, g.rates.expTC, g.rates.expLC)
		if err != nil {
			return err
		}
		nos, orders, items = append(nos, int32(i+1)), append(orders, l.differs.order), append(items, l.differs.item)
		tcs, lcs = append(tcs, l.differs.taxBorne), append(lcs, lc)
	}
	if len(nos) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.price_difference_item (tenant_id, fiscal_year, journal_entry_id, line_no, purchase_order_id, po_item_no,
			tax_borne_tc, tax_borne_lc)
		SELECT platform.require_tenant(), $1, $2, n, o, i, tc, lc
		FROM unnest($3::int[], $4::uuid[], $5::int[], $6::bigint[], $7::bigint[]) AS x(n, o, i, tc, lc)`,
		e.FiscalYear, e.ID, nos, orders, items, tcs, lcs); err != nil {
		return fmt.Errorf("ledger: record the price differences of entry %s: %w", e.ID, err)
	}
	return nil
}

// reversePriceDifferences records a reversal's price difference lines as
// its original's with the tax negated: a reversal keeps its original's
// lines in order (reversalLines), so line n reverses line n. An original
// without recorded lines, posted before they were recorded included,
// leaves its reversal's unknown too.
func reversePriceDifferences(ctx context.Context, tx pgx.Tx, orig, rev Entry) error {
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.price_difference_item (tenant_id, fiscal_year, journal_entry_id, line_no, purchase_order_id, po_item_no,
			tax_borne_tc, tax_borne_lc)
		SELECT i.tenant_id, $3, $4, i.line_no, i.purchase_order_id, i.po_item_no, -i.tax_borne_tc, -i.tax_borne_lc
		FROM ledger.price_difference_item i WHERE i.fiscal_year = $1 AND i.journal_entry_id = $2`,
		orig.FiscalYear, orig.ID, rev.FiscalYear, rev.ID); err != nil {
		return fmt.Errorf("ledger: record the price differences of reversal %s: %w", rev.ID, err)
	}
	return nil
}
