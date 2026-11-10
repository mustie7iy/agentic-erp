package payables_test

import (
	"fmt"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// credit enters a credit memo without an order naming the invoice (none
// when invoice is nil), of the net amount at 19% tax.
func (e env) credit(invoice map[string]any, ref, gross, tax, net string) map[string]any {
	e.T.Helper()
	extra := ""
	if invoice != nil {
		extra = fmt.Sprintf(`, "reference_invoice_id": %q`, id(invoice))
	}
	memo, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.document("credit_memo", ref, gross, tax, extra, accountItem(e.expense, net)), "")
	return memo
}

// feeInvoice enters and posts an invoice without an order of 119.00
// (100.00 at 19%).
func (e env) feeInvoice(t *testing.T, ref string) map[string]any {
	t.Helper()
	invoice, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, ref, "119.00", "19.00", accountItem(e.expense, "100.00")), "")
	return e.act(t, 200, id(invoice), "post", `{"posting_date": "2026-03-21"}`)
}

// wantExceeds posts the credit and wants it refused, left parked, with the
// figures of the limit: the invoice, its gross, what is already credited
// and what was attempted, each with its currency.
func (e env) wantExceeds(t *testing.T, memo, invoice map[string]any, gross, credited, attempted string) {
	t.Helper()
	p := e.Problem(422, "payables.credit_exceeds_invoice", "POST", "/v1/supplier-invoices/"+id(memo)+":post", `{"posting_date": "2026-03-21"}`, e.tag(t, id(memo)))
	limit, ok := p["credit_limit"].(map[string]any)
	if !ok {
		t.Fatalf("credit_limit of the refusal: got %v, want the figures", p["credit_limit"])
	}
	money := func(amount string) string { return fmt.Sprint(map[string]any{"amount": amount, "currency": "EUR"}) }
	for field, want := range map[string]string{"invoice_id": id(invoice), "invoice_number": invoice["document_number"].(string),
		"invoice_gross": money(gross), "credited": money(credited), "attempted": money(attempted)} {
		if got := fmt.Sprint(limit[field]); got != want {
			t.Errorf("credit_limit.%s: got %v, want %v", field, got, want)
		}
	}
	if errs, _ := p["errors"].([]any); len(errs) != 1 || errs[0].(map[string]any)["field"] != "gross_amount" {
		t.Errorf("errors of the refusal: got %v, want one on gross_amount", p["errors"])
	}
	e.wantParked(t, memo)
}

// TestCreditsStayWithinTheirInvoice: the posted credit memos and
// subsequent credits naming an invoice, with the one posting, may not
// exceed that invoice's gross amount (product owner 2026-11-10), with or
// without a purchase order; only posted credits count, a reversed one
// frees its amount, a paid invoice is credited up to its gross, and a
// credit naming no invoice has no such limit.
func TestCreditsStayWithinTheirInvoice(t *testing.T) {
	e := setup(t)
	post := func(e env, memo map[string]any) {
		e.T.Helper()
		e.Must(200, "POST", "/v1/supplier-invoices/"+id(memo)+":post", `{"posting_date": "2026-03-21"}`, e.tag(t, id(memo)))
	}
	t.Run("a second credit past a credited invoice", func(t *testing.T) {
		// The gap as lane crmemo's probe showed it: 119.00 and then
		// 1,190.00 credited against an invoice of 119.00.
		e := e.in(t)
		invoice := e.feeInvoice(t, "A-INV")
		post(e, e.credit(invoice, "A-CM1", "119.00", "19.00", "100.00"))
		e.wantExceeds(t, e.credit(invoice, "A-CM2", "1190.00", "190.00", "1000.00"), invoice, "119.00", "119.00", "1190.00")
	})
	t.Run("to the cent, parked credits not counted, a reversal frees", func(t *testing.T) {
		e := e.in(t)
		invoice := e.feeInvoice(t, "B-INV")
		first, second := e.credit(invoice, "B-CM1", "59.50", "9.50", "50.00"), e.credit(invoice, "B-CM2", "59.50", "9.50", "50.00")
		third := e.credit(invoice, "B-CM3", "23.80", "3.80", "20.00")
		// Three parked credits of 142.80 in all name the invoice of
		// 119.00: none counts until it posts.
		post(e, first)
		post(e, second) // 59.50 + 59.50 = 119.00, the gross exactly
		e.wantExceeds(t, third, invoice, "119.00", "119.00", "23.80")
		e.act(t, 200, id(first), "reverse", `{"reason": "wrong"}`)
		post(e, third) // 59.50 + 23.80 = 83.30, leaving 35.70
		e.wantExceeds(t, e.credit(invoice, "B-CM4", "35.71", "5.70", "30.01"), invoice, "119.00", "83.30", "35.71")
		post(e, e.credit(invoice, "B-CM5", "35.70", "5.70", "30.00"))
	})
	t.Run("a subsequent credit counts, a subsequent debit does not", func(t *testing.T) {
		e := e.in(t)
		po := e.order(t, e.Supplier, "PO-C")
		e.receive(t, po, 1, "10")
		invoice, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "C-INV", "142.80", "22.80", poItem(po, 1, "10", "EA", "120.00")), "")
		invoice = e.posted(t, invoice)
		naming := fmt.Sprintf(`, "purchase_order_id": %q, "reference_invoice_id": %q`, po, id(invoice))
		e.posted(t, e.matched(t, e.document("subsequent_credit", "C-SC", "2.38", "0.38", naming, poItem(po, 1, "0", "EA", "2.00"))))
		// A debit naming the invoice neither counts against the limit nor
		// raises it.
		e.posted(t, e.matched(t, e.document("subsequent_debit", "C-SD", "5.95", "0.95", naming, poItem(po, 1, "0", "EA", "5.00"))))
		// 142.80 - 2.38 = 140.42 is left.
		e.wantExceeds(t, e.credit(invoice, "C-CM1", "140.43", "22.42", "118.01"), invoice, "142.80", "2.38", "140.43")
		post(e, e.credit(invoice, "C-CM2", "140.42", "22.42", "118.00"))
		// A subsequent credit is held to the limit as a credit memo is, on
		// its order; a debit still posts against the credited invoice.
		over := e.matched(t, e.document("subsequent_credit", "C-SC2", "1.19", "0.19", naming, poItem(po, 1, "0", "EA", "1.00")))
		e.wantExceeds(t, over, invoice, "142.80", "142.80", "1.19")
		e.posted(t, e.matched(t, e.document("subsequent_debit", "C-SD2", "1.19", "0.19", naming, poItem(po, 1, "0", "EA", "1.00"))))
	})
	t.Run("a paid invoice is credited up to its gross", func(t *testing.T) {
		e := e.in(t)
		invoice := e.feeInvoice(t, "D-INV")
		pay, _ := e.Must(201, "POST", "/v1/journal-entries", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-03-25", "posting_date": "2026-03-25",
			"currency": "EUR", "lines": [{"account_id": %q, "amount": {"amount": "119.00", "currency": "EUR"}, "partner_id": %q},
			{"account_id": %q, "amount": {"amount": "-119.00", "currency": "EUR"}}]}`, e.DE, e.ap, e.Supplier, e.Account(t, "2000", "100000")), "")
		e.Must(201, "POST", "/v1/clearings", fmt.Sprintf(`{"legal_entity_id": %q, "clearing_date": "2026-03-31", "items": [
			{"fiscal_year": 2026, "journal_entry_id": %q, "line_no": 3}, {"fiscal_year": 2026, "journal_entry_id": %q, "line_no": 1}]}`,
			e.DE, invoice["journal_entry_id"], id(pay)), "")
		e.Problem(409, "payables.invoice_cleared", "POST", "/v1/supplier-invoices/"+id(invoice)+":reverse", `{"reason": "r"}`, e.tag(t, id(invoice)))
		post(e, e.credit(invoice, "D-CM1", "119.00", "19.00", "100.00"))
		e.wantExceeds(t, e.credit(invoice, "D-CM2", "1.19", "0.19", "1.00"), invoice, "119.00", "119.00", "1.19")
	})
	t.Run("a credit naming no invoice has no limit", func(t *testing.T) {
		e := e.in(t)
		post(e, e.credit(nil, "E-CM1", "1190.00", "190.00", "1000.00"))
		post(e, e.credit(nil, "E-CM2", "1190.00", "190.00", "1000.00"))
	})
}

// TestConcurrentCreditsSerialize posts two credits naming one invoice in
// two transactions at once (apitest.Serialized), each within its gross
// alone and together past it: the second waits for the first's lock on
// the invoice, then counts what the first committed and is refused, so
// exactly one posts.
func TestConcurrentCreditsSerialize(t *testing.T) {
	e := setup(t)
	invoice := e.feeInvoice(t, "INV-1")
	first, second := e.credit(invoice, "CM-1", "71.40", "11.40", "60.00"), e.credit(invoice, "CM-2", "71.40", "11.40", "60.00")
	firstErr, secondErr := apitest.Serialized(t, e.Tenant, e.Tenant.Human, postIn(first), postIn(second))
	if firstErr != nil {
		t.Fatalf("first credit: got %v, want it posted", firstErr)
	}
	refusedWith(t, secondErr, "payables.credit_exceeds_invoice", "already credit 71.40")
	for memo, want := range map[string]string{id(first): "posted", id(second): "parked"} {
		got, _ := e.Must(200, "GET", "/v1/supplier-invoices/"+memo, "", "")
		if got["status"] != want {
			t.Errorf("%v after the two posts: got %v, want %v", got["supplier_reference"], got["status"], want)
		}
	}
	var credited int64
	pgtest.Scan(t, `SELECT coalesce(sum(gross_amount), 0)::bigint FROM payables.supplier_invoice WHERE reference_invoice_id = $1 AND status = 'posted'`,
		[]any{id(invoice)}, &credited)
	if credited != 7140 {
		t.Errorf("posted credits naming the invoice of 119.00: got %d minor units, want 7140", credited)
	}
}
