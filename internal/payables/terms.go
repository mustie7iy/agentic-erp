package payables

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
)

// termsColumns are an invoice's payment terms as storedTerms scans them,
// from payables.supplier_invoice_terms joined as st (null without them).
const termsColumns = `st.payment_term_code, st.baseline_date, st.net_days, st.discount_days, trim_scale(st.discount_pct)::text, st.discount2_days,
	trim_scale(st.discount2_pct)::text`

// storedTerms are the terms columns of an invoice.
type storedTerms struct {
	code         *string
	baseline     *time.Time
	netDays      *int
	days1, days2 *int
	pct1, pct2   *string
}

func (s *storedTerms) fields() []any {
	return []any{&s.code, &s.baseline, &s.netDays, &s.days1, &s.pct1, &s.days2, &s.pct2}
}

// terms are the stored terms, or nil without them.
func (s storedTerms) terms() *ledger.ItemTerms {
	if s.code == nil {
		return nil
	}
	t := &ledger.ItemTerms{PaymentTermCode: *s.code, BaselineDate: *s.baseline, NetDays: *s.netDays}
	if s.days1 != nil { // a tier's days and percent are null together (supplier_invoice_terms_pairs)
		t.Discounts = append(t.Discounts, ledger.Discount{Days: *s.days1, Percent: *s.pct1})
	}
	if s.days2 != nil {
		t.Discounts = append(t.Discounts, ledger.Discount{Days: *s.days2, Percent: *s.pct2})
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
func (e *Endpoints) invoiceTerms(ctx context.Context, tx pgx.Tx, v Invoice, sup partner.Supplier, order *procurement.Order) (*ledger.ItemTerms, error) {
	if v.ReferenceInvoiceID != nil {
		ref, err := load(ctx, tx, *v.ReferenceInvoiceID, false) // held FOR SHARE by requireReference
		return ref.Terms, err
	}
	if credits(v.Kind) {
		return nil, nil
	}
	code := sup.PaymentTermCode
	if order != nil {
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
func keepTerms(ctx context.Context, tx pgx.Tx, p reqctx.Principal, v Invoice, t *ledger.ItemTerms) error {
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
		VALUES ($9, $1, $2, $3, $4, $5, $6::numeric, $7, $8::numeric)`,
		v.ID, t.PaymentTermCode, t.BaselineDate, t.NetDays, days[0], pct[0], days[1], pct[1], p.TenantID); err != nil {
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
