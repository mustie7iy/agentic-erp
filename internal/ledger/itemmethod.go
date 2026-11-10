package ledger

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// methodColumn is the payment method an open item fixes (ADR-0038, SAP
// BSEG-ZLSCH), null for none, for ledger.journal_line l.
const methodColumn = `(SELECT pm.payment_method FROM ledger.open_item_payment_method pm WHERE pm.tenant_id = l.tenant_id
			AND pm.fiscal_year = l.fiscal_year AND pm.journal_entry_id = l.journal_entry_id AND pm.line_no = l.line_no)`

// keepPaymentMethod keeps the payment method the document fixes with the
// entry's supplier open items, as keepTerms keeps their terms: each line
// on a supplier reconciliation account but special G/L items, which no
// run pays (ADR-0037). The ledger keeps the code as the posting context
// checked it (payables, through the payments context's port); a method
// for an entry without a supplier open item is refused, rather than kept
// for nothing.
func keepPaymentMethod(ctx context.Context, tx pgx.Tx, entry Entry, method *string) error {
	if method == nil {
		return nil
	}
	tag, err := tx.Exec(ctx, `INSERT INTO ledger.open_item_payment_method (tenant_id, fiscal_year, journal_entry_id, line_no, payment_method)
		SELECT l.tenant_id, l.fiscal_year, l.journal_entry_id, l.line_no, $3
		FROM ledger.journal_line l JOIN ledger.gl_account a ON a.tenant_id = l.tenant_id AND a.id = l.account_id AND a.reconciliation_kind = 'supplier'
		WHERE l.fiscal_year = $1 AND l.journal_entry_id = $2 AND l.special_gl IS NULL`, entry.FiscalYear, entry.ID, *method)
	if err != nil {
		return fmt.Errorf("ledger: keep the payment method of entry %s: %w", entry.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("ledger: payment method of entry %s, which has no supplier open item", entry.ID)
	}
	return nil
}
