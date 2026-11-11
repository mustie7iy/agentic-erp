package ledger

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// methodColumn is the payment method an open item fixes (ADR-0038, SAP
// BSEG-ZLSCH), null for none, from ledger.open_item_payment_method joined
// as pm by methodJoin to ledger.journal_line l. A join, as the terms are
// joined, rather than a subquery per line: a proposal and the liquidity
// forecast read 100,000 items, few of which fix a method.
const (
	methodColumn = `pm.payment_method`
	methodJoin   = `LEFT JOIN ledger.open_item_payment_method pm ON pm.tenant_id = l.tenant_id AND pm.fiscal_year = l.fiscal_year
			AND pm.journal_entry_id = l.journal_entry_id AND pm.line_no = l.line_no`
)

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

// keepPaymentMethods keeps what a posting fixes for its supplier open
// items: one method for all of them (Posting.PaymentMethod), or each
// line's own (PostingLine.PaymentMethod).
func keepPaymentMethods(ctx context.Context, tx pgx.Tx, entry Entry, in Posting) error {
	var nos []int32
	var methods []string
	for i, l := range in.Lines {
		if l.PaymentMethod != nil {
			nos, methods = append(nos, int32(i+1)), append(methods, *l.PaymentMethod)
		}
	}
	if len(nos) == 0 {
		return keepPaymentMethod(ctx, tx, entry, in.PaymentMethod)
	}
	if in.PaymentMethod != nil || len(in.GRIR) > 0 {
		return fmt.Errorf("ledger: payment method of a line of entry %s, which fixes one for all its items or has GR/IR items", entry.ID)
	}
	return keepLineMethods(ctx, tx, entry, nos, methods)
}

// keepLineMethods keeps the payment methods single lines fix, by line
// number: a posting's lines are numbered from 1 in the order given, as a
// migration's clearings rely on too. Each must be a supplier open item as
// keepPaymentMethod takes them; a method on any other line is refused,
// rather than dropped.
func keepLineMethods(ctx context.Context, tx pgx.Tx, entry Entry, nos []int32, methods []string) error {
	tag, err := tx.Exec(ctx, `INSERT INTO ledger.open_item_payment_method (tenant_id, fiscal_year, journal_entry_id, line_no, payment_method)
		SELECT l.tenant_id, l.fiscal_year, l.journal_entry_id, l.line_no, m.method
		FROM unnest($3::int[], $4::text[]) AS m(line_no, method)
		JOIN ledger.journal_line l ON l.fiscal_year = $1 AND l.journal_entry_id = $2 AND l.line_no = m.line_no AND l.special_gl IS NULL
		JOIN ledger.gl_account a ON a.tenant_id = l.tenant_id AND a.id = l.account_id AND a.reconciliation_kind = 'supplier'`,
		entry.FiscalYear, entry.ID, nos, methods)
	if err != nil {
		return fmt.Errorf("ledger: keep the payment methods of the lines of entry %s: %w", entry.ID, err)
	}
	if tag.RowsAffected() != int64(len(nos)) {
		return fmt.Errorf("ledger: payment method of a line of entry %s that is no supplier open item", entry.ID)
	}
	return nil
}
