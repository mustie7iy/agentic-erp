package receivables

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Issued is a posted customer invoice or credit memo as another context
// lists it: its number, kind, date, customer and gross amount in its
// currency (minor units of Currency, whose digits are Exponent).
type Issued struct {
	ID          uuid.UUID
	Number      string
	Kind        string
	InvoiceDate time.Time
	CustomerID  uuid.UUID
	Currency    string
	Exponent    int
	Gross       money.Minor
}

// IssuedInvoices lists a company's posted customer invoices and credit
// memos dated from through to, in the request's transaction, by date and
// id: the documents the einvoicing context's worklist of e-invoices not
// transmitted reads (ADR-0061 As built E3). A reversed document and a
// debit memo are not listed. The caller applies company access.
func IssuedInvoices(ctx context.Context, legalEntity uuid.UUID, from, to time.Time) ([]Issued, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT i.id, i.document_number, i.kind, i.invoice_date, i.customer_id, i.currency, c.exponent, i.gross_amount
		FROM receivables.customer_invoice i JOIN ref.currency c ON c.code = i.currency
		WHERE i.legal_entity_id = $1 AND i.status = 'posted' AND i.kind IN ('invoice', 'credit_memo') AND i.invoice_date BETWEEN $2 AND $3
		ORDER BY i.invoice_date, i.id`, legalEntity, from, to)
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Issued, error) {
		var v Issued
		return v, r.Scan(&v.ID, &v.Number, &v.Kind, &v.InvoiceDate, &v.CustomerID, &v.Currency, &v.Exponent, &v.Gross)
	})
	if err != nil {
		return nil, fmt.Errorf("receivables: list the posted documents of %s: %w", legalEntity, err)
	}
	return out, nil
}
