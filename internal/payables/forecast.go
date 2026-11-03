package payables

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
)

// What the liquidity forecast reads of payables (ADR-0051 §4, T53e2): the
// invoices not posted yet, as the open items they would become, and the
// open down payment requests. Both go to the cash context through the
// payments context's port, adapted in internal/app, so the forecast judges
// them by the payment run's own rules.

// Unposted are the legal entity's invoices, credit memos and subsequent
// debits and credits parked or on hold, each as the supplier open item
// posting it now would give (ledger.PayableItem, without an item
// reference): its supplier, invoice date and currency, what the supplier
// is owed after the withholding tax deducted at invoice, the payment terms
// it would post with (invoiceTerms) and its withholding taxes (withholding),
// ordered by supplier. Posting reads the supplier, the order and the codes
// the same way. What would refuse the posting does not refuse the
// forecast: a payment term that is not configured leaves the item without
// terms (due on its supplier's net days, as an item posted without terms
// is), and withholding tax codes that would refuse it leave it without
// withholding; a supplier not set up in the legal entity has neither (no
// term, no codes).
func (e *Endpoints) Unposted(ctx context.Context, legalEntity uuid.UUID) ([]ledger.PayableItem, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, invoiceSelect+` WHERE i.legal_entity_id = $1 AND i.status IN ('parked', 'on_hold') ORDER BY i.supplier_id, i.id`, legalEntity)
	invoices, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Invoice, error) { return scanInvoice(r) })
	if err != nil {
		return nil, fmt.Errorf("payables: the unposted invoices of %s: %w", legalEntity, err)
	}
	u := unposted{e: e, tx: tx, suppliers: map[uuid.UUID]partner.Supplier{}, orders: map[uuid.UUID]*procurement.Order{}}
	out := make([]ledger.PayableItem, len(invoices))
	for i, v := range invoices {
		if out[i], err = u.item(ctx, v); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// unposted reads the suppliers and orders of the unposted invoices once
// each.
type unposted struct {
	e         *Endpoints
	tx        pgx.Tx
	suppliers map[uuid.UUID]partner.Supplier
	orders    map[uuid.UUID]*procurement.Order
}

// item is the open item the invoice would post.
func (u unposted) item(ctx context.Context, v Invoice) (ledger.PayableItem, error) {
	it := ledger.PayableItem{PartnerID: v.SupplierID, DocumentDate: v.InvoiceDate, Currency: v.Currency, Amount: -signed(v, v.Gross)}
	sup, err := u.supplier(ctx, v)
	if err != nil {
		return it, err
	}
	order, err := u.order(ctx, v)
	if err != nil {
		return it, err
	}
	terms, err := u.e.invoiceTerms(ctx, u.tx, v, sup, order)
	switch {
	case refused(err): // a term not configured: none, as an item posted without terms
	case err != nil:
		return it, err
	default:
		it.Terms = terms
	}
	ws, _, withheld, err := u.e.withholding(ctx, v, sup)
	switch {
	case refused(err):
		return it, nil
	case err != nil:
		return it, err
	}
	it.Amount, it.Withholding = -signed(v, v.Gross-withheld), ws
	return it, nil
}

// supplier is the invoice's supplier in its legal entity: without a
// payment term or withholding tax codes when it is not set up there.
func (u unposted) supplier(ctx context.Context, v Invoice) (partner.Supplier, error) {
	if s, ok := u.suppliers[v.SupplierID]; ok {
		return s, nil
	}
	// A partner that is not there is no supplier: no term, no codes, as
	// the zero value is.
	s, _, err := u.e.Suppliers.Supplier(ctx, v.SupplierID, v.LegalEntityID)
	if err != nil {
		return partner.Supplier{}, err
	}
	u.suppliers[v.SupplierID] = s
	return s, nil
}

// order is the invoice's purchase order, nil without one: its payment term
// is the invoice's.
func (u unposted) order(ctx context.Context, v Invoice) (*procurement.Order, error) {
	if v.PurchaseOrderID == nil {
		return nil, nil
	}
	if o, ok := u.orders[*v.PurchaseOrderID]; ok {
		return o, nil
	}
	o, found, err := u.e.Orders.Order(ctx, *v.PurchaseOrderID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("payables: purchase order %s of invoice %s not found", *v.PurchaseOrderID, v.ID)
	}
	u.orders[*v.PurchaseOrderID] = &o
	return &o, nil
}

// refused reports whether the error is a refusal of the document (a 422
// problem), which the forecast passes over, rather than a failure.
func refused(err error) bool {
	var p *problem.Problem
	return errors.As(err, &p) && p.Status == http.StatusUnprocessableEntity
}
