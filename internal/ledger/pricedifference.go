package ledger

// The order item and the tax borne of a price difference line (accounting
// specialist B10, po:2026-11-10; ADR-0057 §6, ADR-0049 §8). An invoiced
// order item's price difference and the input tax it bears post as one
// line, on the price_difference account or, for an asset's item, on its
// asset clearing account (assetitem.go), and that line names no order
// item: a journal line's order item marks a GR/IR line. The ledger
// records both beside the line (ledger.price_difference_item,
// insert-only), in the transaction that posts it, whichever account the
// line is on: the record is all that names the line's order item and
// tells its tax from its price difference. Only its lines on a profit
// and loss account were expensed (OrderItemExpenses).

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// priceDifference is what is recorded of an invoiced order item's price
// difference line: the order item and the tax borne in the line, in the
// entry's currency, debit positive as the document (GRIRItem.TaxBorne),
// whatever the line's own sign.
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

// OrderItemExpense is what a company's recorded price difference lines of
// a fiscal year expensed for one order item in the leading ledger, in
// local currency, debit positive: Amount on the lines, TaxBorne of it the
// input tax the item bore; the rest is the price difference. Expensed:
// on a profit and loss account; a recorded line on an asset clearing
// account, a balance sheet account, added to an asset's cost instead.
type OrderItemExpense struct {
	PurchaseOrderID  uuid.UUID
	POItemNo         int32
	Amount, TaxBorne money.Minor
}

// UnrecordedExpense is what an invoice or credit memo entry (or its
// reversal) of the leading ledger put on the price difference account on
// lines without a recorded order item, in local currency: Debits the sum
// of those that are debits, Credits (negative) of those that are credits;
// and the order items the entry's GR/IR lines name, any of which the
// lines may belong to.
type UnrecordedExpense struct {
	EntryID         uuid.UUID
	Debits, Credits money.Minor
	Items           []OrderItemRef
}

// OrderItemCleared is what a company's GR/IR clearings of a fiscal year
// put in profit or loss for one order item in the leading ledger, in
// local currency, debit positive (an expense; negative: a credit, as for
// goods received and never invoiced).
type OrderItemCleared struct {
	PurchaseOrderID uuid.UUID
	POItemNo        int32
	Amount          money.Minor
}

// OrderItemRef names a purchase order item.
type OrderItemRef struct {
	PurchaseOrderID uuid.UUID
	POItemNo        int32
}

// OrderItemExpenses reads what the company's recorded price difference
// lines of the fiscal year expensed per order item (B10; ADR-0057 §6), by
// order and item: exact, as posted, reversals netted where they fall in
// the year. The lines on balance sheet accounts are left out: an asset's
// item's difference posted to its asset clearing account (product owner
// 2026-11-10) is part of the asset's cost, not an expense, and the
// configuration admits only an asset account as a clearing account and
// only an expense account as the price difference account.
func (AssetReads) OrderItemExpenses(ctx context.Context, legalEntity uuid.UUID, year int16) ([]OrderItemExpense, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT i.purchase_order_id, i.po_item_no, sum(l.amount_lc)::bigint, sum(i.tax_borne_lc)::bigint
		FROM ledger.price_difference_item i
		JOIN ledger.journal_line l ON l.tenant_id = i.tenant_id AND l.fiscal_year = i.fiscal_year AND l.journal_entry_id = i.journal_entry_id
			AND l.line_no = i.line_no
		JOIN ledger.ledger g ON g.tenant_id = l.tenant_id AND g.id = l.ledger_id
		JOIN ledger.gl_account a ON a.tenant_id = l.tenant_id AND a.id = l.account_id
		WHERE i.fiscal_year = $2 AND l.legal_entity_id = $1 AND g.is_leading AND NOT a.is_balance_sheet
		GROUP BY i.purchase_order_id, i.po_item_no ORDER BY i.purchase_order_id, i.po_item_no`, legalEntity, year)
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (OrderItemExpense, error) {
		var x OrderItemExpense
		return x, r.Scan(&x.PurchaseOrderID, &x.POItemNo, &x.Amount, &x.TaxBorne)
	})
	return out, errs.Wrapf(err, "ledger: recorded price differences of %s in %d", legalEntity, year)
}

// UnrecordedExpenses reads, per entry, what the company's payables
// entries of the fiscal year in the leading ledger put on its current
// price difference account on lines without a recorded order item, for
// the entries with a line on the GR/IR account, whose order items it
// names: postings made before the record existed and a reversal's
// rounding residue. A company without a price
// difference account has none. GR/IR clearings (procurement's entries)
// and manual entries are not read.
func (AssetReads) UnrecordedExpenses(ctx context.Context, legalEntity uuid.UUID, year int16) ([]UnrecordedExpense, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	account, ok, err := Accounts{}.Determination(ctx, legalEntity, "price_difference")
	if err != nil || !ok {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `WITH u AS (SELECT l.tenant_id, l.legal_entity_id, l.journal_entry_id AS id,
				coalesce(sum(l.amount_lc) FILTER (WHERE l.amount_lc > 0), 0)::bigint AS debits,
				coalesce(sum(l.amount_lc) FILTER (WHERE l.amount_lc < 0), 0)::bigint AS credits
			FROM ledger.journal_line l
			JOIN ledger.journal_entry e ON e.tenant_id = l.tenant_id AND e.fiscal_year = l.fiscal_year AND e.id = l.journal_entry_id
			JOIN ledger.ledger g ON g.tenant_id = l.tenant_id AND g.id = l.ledger_id
			WHERE l.legal_entity_id = $1 AND l.fiscal_year = $2 AND l.account_id = $3 AND g.is_leading AND e.source_context = 'payables'
				AND l.purchase_order_id IS NULL
				AND NOT EXISTS (SELECT FROM ledger.price_difference_item i WHERE i.tenant_id = l.tenant_id AND i.fiscal_year = l.fiscal_year
					AND i.journal_entry_id = l.journal_entry_id AND i.line_no = l.line_no)
			GROUP BY l.tenant_id, l.legal_entity_id, l.journal_entry_id)
		SELECT u.id, u.debits, u.credits, o.purchase_order_id, o.po_item_no FROM u
		JOIN LATERAL (SELECT DISTINCT x.purchase_order_id, x.po_item_no FROM ledger.journal_line x
			JOIN ledger.account_determination d ON d.tenant_id = x.tenant_id AND d.legal_entity_id = u.legal_entity_id AND d.key = 'gr_ir'
				AND d.account_id = x.account_id
			WHERE x.tenant_id = u.tenant_id AND x.fiscal_year = $2 AND x.journal_entry_id = u.id AND x.purchase_order_id IS NOT NULL) o ON true
		ORDER BY u.id, o.purchase_order_id, o.po_item_no`, legalEntity, year, account)
	var out []UnrecordedExpense
	var entry uuid.UUID
	var debits, credits money.Minor
	var item OrderItemRef
	_, err = pgx.ForEachRow(rows, []any{&entry, &debits, &credits, &item.PurchaseOrderID, &item.POItemNo}, func() error {
		if n := len(out); n == 0 || out[n-1].EntryID != entry {
			out = append(out, UnrecordedExpense{EntryID: entry, Debits: debits, Credits: credits})
		}
		out[len(out)-1].Items = append(out[len(out)-1].Items, item)
		return nil
	})
	return out, errs.Wrapf(err, "ledger: unrecorded price differences of %s in %d", legalEntity, year)
}

// ClearedExpenses reads what the company's GR/IR clearings (T72e) of the
// fiscal year put in profit or loss per order item (B10; ADR-0057 §6), by
// order and item. A clearing entry (ledger.gr_ir_clearing_entry) is all
// clearing items, each a GR/IR line naming its order item followed by its
// counter line (itemLines), so the line after a GR/IR line is what that
// item's clearing posted against: exact per item, without a record of
// its own. Counter lines on balance sheet accounts are left out: an
// asset's item cleared against its asset clearing account (product owner
// 2026-11-10) adjusts the asset's cost, not profit or loss. A clearing is
// never reversed (procurement's entry; no path), so none nets out.
func (AssetReads) ClearedExpenses(ctx context.Context, legalEntity uuid.UUID, year int16) ([]OrderItemCleared, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT l.purchase_order_id, l.po_item_no, sum(c.amount_lc)::bigint
		FROM ledger.gr_ir_clearing_entry ce
		JOIN ledger.journal_line l ON l.tenant_id = ce.tenant_id AND l.fiscal_year = ce.fiscal_year AND l.journal_entry_id = ce.journal_entry_id
			AND l.purchase_order_id IS NOT NULL
		JOIN ledger.journal_line c ON c.tenant_id = l.tenant_id AND c.fiscal_year = l.fiscal_year AND c.journal_entry_id = l.journal_entry_id
			AND c.line_no = l.line_no + 1 AND c.purchase_order_id IS NULL
		JOIN ledger.gl_account a ON a.tenant_id = c.tenant_id AND a.id = c.account_id
		JOIN ledger.ledger g ON g.tenant_id = l.tenant_id AND g.id = l.ledger_id
		WHERE ce.fiscal_year = $2 AND l.legal_entity_id = $1 AND g.is_leading AND NOT a.is_balance_sheet
		GROUP BY l.purchase_order_id, l.po_item_no ORDER BY l.purchase_order_id, l.po_item_no`, legalEntity, year)
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (OrderItemCleared, error) {
		var x OrderItemCleared
		return x, r.Scan(&x.PurchaseOrderID, &x.POItemNo, &x.Amount)
	})
	return out, errs.Wrapf(err, "ledger: GR/IR clearings in profit or loss of %s in %d", legalEntity, year)
}
