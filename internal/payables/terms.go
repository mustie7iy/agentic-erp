package payables

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// storedTerms are an invoice's payment terms as invoiceSelect reads them
// from payables.supplier_invoice_terms.
type storedTerms struct {
	Code     string  `json:"code"`
	Baseline string  `json:"baseline"`
	NetDays  int     `json:"net_days"`
	Days1    *int    `json:"days1"`
	Pct1     *string `json:"pct1"`
	Days2    *int    `json:"days2"`
	Pct2     *string `json:"pct2"`
}

// terms are the stored terms as the ledger keeps them with the open item.
func (s storedTerms) terms() *ledger.ItemTerms {
	baseline, _ := time.Parse(time.DateOnly, s.Baseline) // a date column, written by json_build_object as YYYY-MM-DD
	t := &ledger.ItemTerms{PaymentTermCode: s.Code, BaselineDate: baseline, NetDays: s.NetDays}
	if s.Days1 != nil { // a tier's days and percent are null together (supplier_invoice_terms_pairs)
		t.Discounts = append(t.Discounts, ledger.Discount{Days: *s.Days1, Percent: *s.Pct1})
	}
	if s.Days2 != nil {
		t.Discounts = append(t.Discounts, ledger.Discount{Days: *s.Days2, Percent: *s.Pct2})
	}
	return t
}

// invoiceTerms are the payment terms an invoice posts with (T36b), as SAP
// determines them in invoice verification: a correction naming an invoice
// has that invoice's (nil for one posted before terms were kept); a credit
// memo or subsequent credit naming none has none, as it is due at once
// and earns no discount; any other document has its order's term, else
// its supplier's in the legal entity, counted from its invoice date. A
// term that is not configured is refused (422
// payables.unknown_payment_term).
func (e *Endpoints) invoiceTerms(ctx context.Context, tx pgx.Tx, v Invoice, sup partner.Supplier) (*ledger.ItemTerms, error) {
	if v.ReferenceInvoiceID != nil {
		ref, err := load(ctx, tx, *v.ReferenceInvoiceID, false) // held FOR SHARE by requireReference
		return ref.Terms, err
	}
	if credits(v.Kind) {
		return nil, nil
	}
	code := sup.PaymentTermCode
	if v.PurchaseOrderID != nil {
		order, _, err := e.Orders.Order(ctx, *v.PurchaseOrderID) // an order not found has no term, refused below
		if err != nil {
			return nil, err
		}
		code = order.PaymentTermCode
	}
	term, ok, err := e.Suppliers.PaymentTerm(ctx, code)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, problem.Unprocessable("payables.unknown_payment_term", "The invoice's payment term "+code+" is not configured; add it through a change request.")
	}
	t := &ledger.ItemTerms{PaymentTermCode: code, BaselineDate: v.InvoiceDate, NetDays: term.NetDays}
	for _, d := range term.Discounts {
		t.Discounts = append(t.Discounts, ledger.Discount{Days: d.Days, Percent: d.Percent})
	}
	return t, nil
}

// keepTerms records the terms an invoice posted with, if any.
func keepTerms(ctx context.Context, tx pgx.Tx, v Invoice, t *ledger.ItemTerms) error {
	if t == nil {
		return nil
	}
	var days [2]*int
	var pct [2]*string
	for i, d := range t.Discounts { // at most two: the ledger refused more when it kept them
		days[i], pct[i] = &d.Days, &d.Percent
	}
	if _, err := tx.Exec(ctx, `INSERT INTO payables.supplier_invoice_terms (tenant_id, supplier_invoice_id, payment_term_code, baseline_date, net_days,
			discount_days, discount_pct, discount2_days, discount2_pct)
		SELECT tenant_id, id, $2, $3, $4, $5, $6::numeric, $7, $8::numeric FROM payables.supplier_invoice WHERE id = $1`,
		v.ID, t.PaymentTermCode, t.BaselineDate, t.NetDays, days[0], pct[0], days[1], pct[1]); err != nil {
		return fmt.Errorf("payables: keep the terms of invoice %s: %w", v.ID, err)
	}
	return nil
}

// termsEvent is payment terms as events carry them, or nil.
func termsEvent(t *ledger.ItemTerms) map[string]any {
	if t == nil {
		return nil
	}
	discounts := []map[string]any{}
	for _, d := range t.Discounts {
		discounts = append(discounts, map[string]any{"until": t.Until(d).Format(time.DateOnly), "percent": d.Percent})
	}
	return map[string]any{"code": t.PaymentTermCode, "baseline_date": t.BaselineDate.Format(time.DateOnly),
		"net_due_date": t.NetDueDate().Format(time.DateOnly), "discounts": discounts}
}
