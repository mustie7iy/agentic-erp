package payables

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// ProcessLog reads supplier documents for the processmining context's
// event logs (T103a, ADR-0080): those on purchase orders, by order, and a
// company's documents without an order, each a case of its own. It reads
// in the caller's transaction and changes nothing.
type ProcessLog struct{}

// LoggedInvoice is a supplier document (an invoice, a credit memo, a
// subsequent debit or credit) as an event log reads it: the orders it is
// on, by its header or its items, and the journal entry it was posted
// with, which a payment clears.
type LoggedInvoice struct {
	ID             uuid.UUID
	DocumentNumber string
	InvoiceDate    time.Time
	Orders         []uuid.UUID
	EntryYear      *int16
	EntryID        *uuid.UUID
}

const loggedSelect = `SELECT v.id, v.document_number, v.invoice_date,
		ARRAY(SELECT DISTINCT o FROM (SELECT v.purchase_order_id AS o UNION ALL SELECT i.purchase_order_id FROM payables.supplier_invoice_item i
			WHERE i.tenant_id = v.tenant_id AND i.supplier_invoice_id = v.id) os WHERE o IS NOT NULL ORDER BY o),
		v.journal_entry_year, v.journal_entry_id
	FROM payables.supplier_invoice v`

func scanLogged(rows pgx.Rows, what string) ([]LoggedInvoice, error) {
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (LoggedInvoice, error) {
		var v LoggedInvoice
		err := r.Scan(&v.ID, &v.DocumentNumber, &v.InvoiceDate, &v.Orders, &v.EntryYear, &v.EntryID)
		return v, errs.Wrapf(err, "scan a supplier document")
	})
	if err != nil {
		return nil, fmt.Errorf("payables: %s for an event log: %w", what, err)
	}
	return out, nil
}

// OfOrders returns the documents on any of the orders, by their header or
// an item, by invoice date and id.
func (ProcessLog) OfOrders(ctx context.Context, orders []uuid.UUID) ([]LoggedInvoice, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, loggedSelect+` WHERE v.purchase_order_id = ANY($1) OR v.id IN (SELECT i.supplier_invoice_id FROM payables.supplier_invoice_item i
		WHERE i.purchase_order_id = ANY($1)) ORDER BY v.invoice_date, v.id`, orders)
	return scanLogged(rows, fmt.Sprintf("the documents of %d orders", len(orders)))
}

// withoutOrder selects a company's documents dated in a period on no
// order, by header or item.
const withoutOrder = ` WHERE v.legal_entity_id = $1 AND v.invoice_date BETWEEN $2 AND $3 AND v.purchase_order_id IS NULL
	AND NOT EXISTS (SELECT 1 FROM payables.supplier_invoice_item i WHERE i.tenant_id = v.tenant_id AND i.supplier_invoice_id = v.id
		AND i.purchase_order_id IS NOT NULL)`

// WithoutOrder returns up to limit documents of the company on no order
// dated from one day to another, both included, by invoice date and id,
// after the document dated afterDate with id afterID (nil: from the
// first).
func (ProcessLog) WithoutOrder(ctx context.Context, legalEntity uuid.UUID, from, to time.Time, afterDate *time.Time, afterID uuid.UUID, limit int) ([]LoggedInvoice, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, loggedSelect+withoutOrder+` AND ($4::date IS NULL OR (v.invoice_date, v.id) > ($4, $5))
		ORDER BY v.invoice_date, v.id LIMIT $6`, legalEntity, from, to, afterDate, afterID, limit)
	return scanLogged(rows, "the documents without an order of "+legalEntity.String())
}

// CountWithoutOrder is how many documents of the company on no order are
// dated from one day to another, both included.
func (ProcessLog) CountWithoutOrder(ctx context.Context, legalEntity uuid.UUID, from, to time.Time) (int, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return 0, err
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM payables.supplier_invoice v`+withoutOrder, legalEntity, from, to).Scan(&n); err != nil {
		return 0, fmt.Errorf("payables: count the documents without an order of %s for an event log: %w", legalEntity, err)
	}
	return n, nil
}
