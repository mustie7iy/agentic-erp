package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func editPath(id uuid.UUID) string { return invoicePath(id) + "/edit" }

// editForm is an invoice's change form as a browser sends it, with the
// fields changed as given (a name's values in the lines' order).
func editForm(t *testing.T, b *browser, id uuid.UUID, changes url.Values, header ...string) url.Values {
	t.Helper()
	v := formValues(b.get(editPath(id), header...).page(t).form(editPath(id)))
	for k, vs := range changes {
		v[k] = vs
	}
	return v
}

// invoiceRow is what the database holds of an invoice's header and first
// line.
func invoiceRow(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var got string
	pgtest.Scan(t, `SELECT i.supplier_reference || ' ' || i.version || ' ' || coalesce(i.posting_date::text, '-') || ' ' || coalesce(i.tax_date::text, '-')
			|| ' ' || coalesce(it.description, '-') || ' ' || coalesce(it.tax_code, '-')
		FROM payables.supplier_invoice i JOIN payables.supplier_invoice_item it ON it.supplier_invoice_id = i.id AND NOT it.is_removed WHERE i.id = $1
		ORDER BY it.item_no LIMIT 1`,
		[]any{id}, &got)
	return got
}

// TestChangeInvoice: a parked invoice's page links to its change; the
// form holds the invoice as it is with its ETag and a fresh key; what
// changed is sent and nothing else, a form unchanged sends nothing, an
// optional date is cleared; an invoice on an order keeps its lines' order
// items; an invoice no longer parked is not offered the form.
func TestChangeInvoice(t *testing.T) {
	s, d := invoicesStack(t)
	b := s.browser()
	b.signIn()
	page := b.get(invoicePath(d.account)).page(t)
	edit := ""
	for _, a := range page.all("a", "button") {
		if strings.HasSuffix(attrOf(a, "href"), "/edit") {
			edit = attrOf(a, "href")
		}
	}
	if edit != editPath(d.account) {
		t.Errorf("the change's link: got %q, want %q", edit, editPath(d.account))
	}
	r := b.get(editPath(d.account))
	status(t, "the form", r, http.StatusOK)
	p := r.page(t)
	holds(t, "the form", p.mainText(), number(t, d.account), "Change the invoice", "Line 1", "Unit EA", "In EUR")
	form := p.form(editPath(d.account))
	for name, want := range map[string]string{"supplier_reference": "W-1", "invoice_date": "2026-03-10", "posting_date": "", "tax_date": "",
		"gross_amount": "119.00", "tax_amount": "19.00", "item_description": "Fees", "item_quantity": "1", "item_amount": "100.00", "item_tax_code": "V1",
		"if_match": `"1"`} {
		if got := field(form, name); got != want {
			t.Errorf("%s: got %q, want %q", name, got, want)
		}
	}
	if _, err := uuid.Parse(field(form, "idempotency_key")); err != nil {
		t.Errorf("idempotency key: %v", err)
	}

	same := b.do(http.MethodPost, editPath(d.account), editForm(t, b, d.account, nil))
	if same.status != http.StatusSeeOther || same.location() != invoicePath(d.account)+"?done=unchanged" {
		t.Fatalf("unchanged: got %d to %q %s", same.status, same.location(), same.body)
	}
	if got, want := invoiceRow(t, d.account), "W-1 1 - - Fees V1"; got != want {
		t.Errorf("unchanged: got %q, want %q", got, want)
	}
	holds(t, "unchanged", b.get(same.location()).page(t).mainText(), "Nothing was changed")

	changed := b.do(http.MethodPost, editPath(d.account), editForm(t, b, d.account, url.Values{"supplier_reference": {" W-1A "},
		"posting_date": {"2026-03-15"}, "tax_date": {"2026-03-09"}, "item_description": {"Fees, March"}, "item_tax_code": {"v1"}}))
	if changed.status != http.StatusSeeOther || changed.location() != invoicePath(d.account)+"?done=changed" {
		t.Fatalf("changed: got %d to %q %s", changed.status, changed.location(), changed.body)
	}
	if got, want := invoiceRow(t, d.account), "W-1A 2 2026-03-15 2026-03-09 Fees, March V1"; got != want {
		t.Errorf("changed: got %q, want %q", got, want)
	}
	holds(t, "changed", b.get(changed.location()).page(t).mainText(), "The invoice is changed.", "Supplier's reference W-1A")

	cleared := b.do(http.MethodPost, editPath(d.account), editForm(t, b, d.account, url.Values{"posting_date": {""}, "tax_date": {""},
		"item_description": {""}, "item_tax_code": {""}}))
	status(t, "cleared", cleared, http.StatusSeeOther)
	if got, want := invoiceRow(t, d.account), "W-1A 3 - - - -"; got != want {
		t.Errorf("cleared: got %q, want %q", got, want)
	}

	onOrder := b.do(http.MethodPost, editPath(d.clean), editForm(t, b, d.clean, url.Values{"item_description": {"Paper", "Toner"}}))
	status(t, "an invoice on an order", onOrder, http.StatusSeeOther)
	var orderItems string
	pgtest.Scan(t, `SELECT string_agg(po_item_no || ' ' || description, ', ' ORDER BY item_no) FROM payables.supplier_invoice_item WHERE supplier_invoice_id = $1 AND NOT is_removed`,
		[]any{d.clean}, &orderItems)
	if orderItems != "1 Paper, 2 Toner" {
		t.Errorf("an invoice on an order: got %q", orderItems)
	}

	held := b.get(editPath(d.variance))
	if held.status != http.StatusSeeOther || held.location() != invoicePath(d.variance) {
		t.Errorf("an invoice on hold: got %d to %q", held.status, held.location())
	}
	for _, a := range b.get(invoicePath(d.variance)).page(t).all("a", "button") {
		if strings.HasSuffix(attrOf(a, "href"), "/edit") {
			t.Error("an invoice on hold offers its change")
		}
	}
	status(t, "no such invoice", b.get(editPath(uuid.New())), http.StatusNotFound)
	status(t, "no invoice id", b.get("/invoices/x/edit"), http.StatusNotFound)
	status(t, "no invoice id, sent", b.do(http.MethodPost, "/invoices/x/edit", url.Values{"csrf_token": {b.csrf()}}), http.StatusNotFound)
}

// TestChangeInvoiceInFrench: the form shows amounts and quantities as a
// French speaker types them, and reads them back.
func TestChangeInvoiceInFrench(t *testing.T) {
	s, d := invoicesStack(t)
	b := s.browser()
	b.signIn()
	form := b.get(editPath(d.account), "Accept-Language", "fr-CA").page(t).form(editPath(d.account))
	if got := field(form, "gross_amount") + " " + field(form, "item_amount"); got != "119,00 100,00" {
		t.Errorf("amounts: got %q", got)
	}
	r := b.do(http.MethodPost, editPath(d.account), editForm(t, b, d.account, url.Values{"item_quantity": {"1,5"}, "item_amount": {"150,00"},
		"gross_amount": {"178,50"}, "tax_amount": {"28,50"}}, "Accept-Language", "fr-CA"), "Accept-Language", "fr-CA")
	status(t, "French", r, http.StatusSeeOther)
	var got string
	pgtest.Scan(t, `SELECT i.gross_amount || ' ' || i.tax_amount || ' ' || trim_scale(it.quantity) || ' ' || it.amount FROM payables.supplier_invoice i
		JOIN payables.supplier_invoice_item it ON it.supplier_invoice_id = i.id AND NOT it.is_removed WHERE i.id = $1`, []any{d.account}, &got)
	if got != "17850 2850 1.5 15000" {
		t.Errorf("French: got %q, want 17850 2850 1.5 15000", got)
	}
}

// TestChangeInvoiceRefusals: what is wrong is said on its field before
// the API is called, with what was typed kept; a form opened before
// another change is refused (412) and, sent again, applies at the version
// it now carries; a line's field the API refuses is said on its line; a
// form whose lines are not the invoice's is shown again as the invoice is;
// a person who may not change invoices is refused by the API.
func TestChangeInvoiceRefusals(t *testing.T) {
	s, d := invoicesStack(t)
	b := s.browser()
	b.signIn()
	wrong := b.do(http.MethodPost, editPath(d.account), editForm(t, b, d.account, url.Values{"supplier_reference": {" "},
		"invoice_date": {"10.03.2026"}, "posting_date": {"15.03.2026"}, "gross_amount": {"1,119.00"}, "tax_amount": {""},
		"item_quantity": {"-1"}, "item_amount": {"x"}, "item_tax_code": {"TOOLONG"}, "item_description": {strings.Repeat("d", 201)}}))
	status(t, "wrong", wrong, http.StatusUnprocessableEntity)
	p := wrong.page(t)
	holds(t, "wrong", p.mainText(), "Enter the supplier's reference.", "Enter a date as year-month-day", "Enter the amount.",
		"Enter the amount as digits", "Enter the quantity as digits", "A tax code has 1 to 4 letters or digits.", "A text has at most 200 characters.")
	if got := field(p.form(editPath(d.account)), "gross_amount"); got != "1,119.00" {
		t.Errorf("wrong: the gross amount kept is %q", got)
	}
	noQuantity := b.do(http.MethodPost, editPath(d.account), editForm(t, b, d.account, url.Values{"item_quantity": {" "},
		"supplier_reference": {strings.Repeat("r", 41)}}))
	holds(t, "no quantity", noQuantity.page(t).mainText(), "Enter the quantity.", "A reference has at most 40 characters.")

	old := editForm(t, b, d.account, url.Values{"supplier_reference": {"W-1-OLD"}})
	status(t, "a change meanwhile", b.do(http.MethodPost, editPath(d.account), editForm(t, b, d.account, url.Values{"supplier_reference": {"W-1B"}})),
		http.StatusSeeOther)
	stale := b.do(http.MethodPost, editPath(d.account), old)
	status(t, "stale", stale, http.StatusPreconditionFailed)
	sp := stale.page(t)
	holds(t, "stale", sp.mainText(), "The invoice changed since you opened it.", "request.version_mismatch")
	again := formValues(sp.form(editPath(d.account)))
	if got := again.Get("supplier_reference") + " " + again.Get("if_match"); got != `W-1-OLD "2"` {
		t.Errorf("stale: kept %q, want the reference typed at the version now", got)
	}
	status(t, "sent again", b.do(http.MethodPost, editPath(d.account), again), http.StatusSeeOther)
	if got, want := invoiceRow(t, d.account), "W-1-OLD 3 - - Fees V1"; got != want {
		t.Errorf("sent again: got %q, want %q", got, want)
	}

	unknown := b.do(http.MethodPost, editPath(d.account), editForm(t, b, d.account, url.Values{"item_tax_code": {"ZZ"}}))
	status(t, "an unknown code", unknown, http.StatusUnprocessableEntity)
	holds(t, "an unknown code", unknown.page(t).mainText(), "payables.unknown_tax_code", "Tax code: refused by the ERP")

	lines := editForm(t, b, d.account, url.Values{"item_amount": {"100.00", "5.00"}})
	changedLines := b.do(http.MethodPost, editPath(d.account), lines)
	status(t, "lines not the invoice's", changedLines, http.StatusConflict)
	cp := changedLines.page(t)
	holds(t, "lines not the invoice's", cp.mainText(), "The invoice's lines changed since you opened this form.")
	if got := formValues(cp.form(editPath(d.account)))["item_amount"]; fmt.Sprint(got) != "[100.00]" {
		t.Errorf("lines not the invoice's: the form shows %v, want the invoice's line", got)
	}

	viewer := s.as(d.viewer)
	form := viewer.get(editPath(d.account)).page(t).form(editPath(d.account))
	v := formValues(form)
	v.Set("supplier_reference", "W-9")
	refused := viewer.do(http.MethodPost, editPath(d.account), v)
	status(t, "a reader", refused, http.StatusForbidden)
	holds(t, "a reader", refused.page(t).mainText(), "The invoice was not changed", "identity.permission_missing")
	for _, a := range viewer.get(invoicePath(d.account)).page(t).all("a", "button") {
		if strings.HasSuffix(attrOf(a, "href"), "/edit") {
			t.Error("a reader is offered the change")
		}
	}
	status(t, "an outsider", s.as(d.outsider).get(editPath(d.account)), http.StatusForbidden)
	token := b.csrf()
	status(t, "no If-Match", b.do(http.MethodPost, editPath(d.account), url.Values{"csrf_token": {token}, "idempotency_key": {uuid.NewString()}}),
		http.StatusBadRequest)
}

// TestTaxCorrected: a post the API refuses for its tax shows the tax the
// invoice states, the tax its codes' rates give and each code's, in the
// person's words; the clerk corrects the invoice's tax and posts it.
func TestTaxCorrected(t *testing.T) {
	s, d := invoicesStack(t)
	b := s.browser()
	b.signIn()
	r := act(b, d.taxWrong, "post", "", "")
	status(t, "refused", r, http.StatusUnprocessableEntity)
	holds(t, "refused", r.page(t).mainText(), "payables.tax_mismatch", "Tax the invoice states EUR 20.00", "Tax at its codes' rates EUR 19.00",
		"Tax at each code's rates Tax code Calculated tax V1 EUR 19.00")
	frPage := b.do(http.MethodPost, invoicePath(d.taxWrong)+"/post", values(b.get(invoicePath(d.taxWrong)).page(t).form(invoicePath(d.taxWrong)+"/post"), ""),
		"Accept-Language", "fr-CA")
	holds(t, "French", frPage.page(t).mainText(), "Taxe indiquée sur la facture 20,00 EUR", "Taxe aux taux de ses codes 19,00 EUR", "V1 19,00 EUR")
	fixed := b.do(http.MethodPost, editPath(d.taxWrong), editForm(t, b, d.taxWrong, url.Values{"gross_amount": {"119.00"}, "tax_amount": {"19.00"}}))
	status(t, "corrected", fixed, http.StatusSeeOther)
	if posted := act(b, d.taxWrong, "post", "", ""); posted.status != http.StatusSeeOther {
		t.Fatalf("post: got %d %s", posted.status, posted.body)
	}
	if got := invoiceStatus(t, d.taxWrong); got != "posted" {
		t.Errorf("status: got %s, want posted", got)
	}
}
