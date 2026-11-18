package ledger

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Customer items open on a past date (T75l, ADR-0063 §10 As built): the
// AR aging as it stood on a key date reads the customer items as the
// due date analysis at a key date does in SAP (S_ALR_87012178, FBL5N "open
// at key date"; from memory, not checked against a source): posted on or
// before the date and not closed by then, which their dates say:
//
//   - a clearing (ledger.clearing_item) closes its items on its
//     clearing_date, never before they were posted (notBeforeItems);
//   - a clearing that was reset (T75i) is none: its items left
//     ledger.clearing_item, and its clearing entry was reversed on its own
//     posting date (Postings.ResetClearing), so the reset undoes it from
//     the day it was dated, as SAP's FBRA leaves its items no clearing
//     date;
//   - a reversal closes both its original's lines and its own on its
//     posting date, as Receivable leaves both out once it exists; one
//     dated before its original (the ledger allows it) leaves the pair
//     open on no day, as it nets to nothing.

// ReceivableOn returns the legal entity's customer items open on the
// date, of the customers named or, without any, of every customer, as
// Receivable returns the items open now: ordered by customer and item, in
// every currency, with the terms each was posted with. The legal entity is
// not checked against the caller's companies: the caller's context does
// that.
func (OpenItems) ReceivableOn(ctx context.Context, legalEntity uuid.UUID, customers []uuid.UUID, on time.Time) ([]ReceivableItem, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, receivableSelect+` WHERE l.legal_entity_id = $1 AND (coalesce(cardinality($2::uuid[]), 0) = 0 OR l.partner_id = ANY($2))
			AND l.posting_date <= $3 AND `+receivableOpenOn+`
		ORDER BY l.partner_id, l.fiscal_year, l.journal_entry_id, l.line_no`, legalEntity, customers, on)
	items, err := pgx.CollectRows(rows, scanReceivable)
	if err != nil {
		return nil, fmt.Errorf("ledger: customer items of %s open on %s: %w", legalEntity, on.Format(time.DateOnly), err)
	}
	return items, nil
}

// receivableOpenOn keeps the lines open on the date $3: not cleared by a
// clearing dated on or before it, and not of an entry reversed, or of a
// reversal posted, on or before it.
const receivableOpenOn = `NOT EXISTS (SELECT FROM ledger.clearing_item c
				JOIN ledger.clearing k ON k.tenant_id = c.tenant_id AND k.id = c.clearing_id
			WHERE c.tenant_id = l.tenant_id AND c.fiscal_year = l.fiscal_year AND c.journal_entry_id = l.journal_entry_id AND c.line_no = l.line_no
				AND k.clearing_date <= $3)
		AND NOT EXISTS (SELECT FROM ledger.reversal r
				JOIN ledger.journal_entry v ON v.tenant_id = r.tenant_id AND v.fiscal_year = r.reversal_year AND v.id = r.reversal_entry_id
			WHERE r.tenant_id = l.tenant_id AND ((r.original_year = l.fiscal_year AND r.original_entry_id = l.journal_entry_id)
					OR (r.reversal_year = l.fiscal_year AND r.reversal_entry_id = l.journal_entry_id))
				AND v.posting_date <= $3)`
