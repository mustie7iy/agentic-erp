package web

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/net/html"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
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
	holds(t, "the form", p.mainText(), number(t, d.account), "Change the invoice", "Line 1", "In EUR")
	form := p.form(editPath(d.account))
	for name, want := range map[string]string{"supplier_reference": "W-1", "invoice_date": "2026-03-10", "posting_date": "", "tax_date": "",
		"gross_amount": "119.00", "tax_amount": "19.00", "item_description": "Fees", "item_quantity": "1", "item_unit": "EA", "item_amount": "100.00",
		"item_tax_code": "V1", "item_account": "520000", "item_from": "1", "was_item_account": "520000", "if_match": `"1"`} {
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
		"invoice_date": {"2026-03-11"}, "posting_date": {"2026-03-15"}, "tax_date": {"2026-03-09"}, "item_description": {"Fees, March"},
		"item_tax_code": {"v1"}}))
	if changed.status != http.StatusSeeOther || changed.location() != invoicePath(d.account)+"?done=changed" {
		t.Fatalf("changed: got %d to %q %s", changed.status, changed.location(), changed.body)
	}
	if got, want := invoiceRow(t, d.account), "W-1A 2 2026-03-15 2026-03-09 Fees, March V1"; got != want {
		t.Errorf("changed: got %q, want %q", got, want)
	}
	holds(t, "changed", b.get(changed.location()).page(t).mainText(), "The invoice is changed.", "Supplier's reference W-1A", "Invoice date 2026-03-11")

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
	// A line of an order item left without a tax code takes its order
	// item's, as the form's hint says.
	status(t, "an order item's code emptied", b.do(http.MethodPost, editPath(d.clean), editForm(t, b, d.clean, url.Values{"item_tax_code": {"", ""}})),
		http.StatusSeeOther)
	if got := invoiceRow(t, d.clean); !strings.HasSuffix(got, " V1") {
		t.Errorf("an order item's code emptied: got %q, want the order item's V1", got)
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
	// Someone else changes the line's description meanwhile.
	status(t, "a change meanwhile", b.do(http.MethodPost, editPath(d.account), editForm(t, b, d.account, url.Values{"item_description": {"Fees B"}})),
		http.StatusSeeOther)
	stale := b.do(http.MethodPost, editPath(d.account), old)
	status(t, "stale", stale, http.StatusPreconditionFailed)
	sp := stale.page(t)
	holds(t, "stale", sp.mainText(), "The invoice changed since you opened it.", "request.version_mismatch")
	again := formValues(sp.form(editPath(d.account)))
	if got := again.Get("supplier_reference") + " " + again.Get("item_description") + " " + again.Get("if_match"); got != `W-1-OLD Fees B "2"` {
		t.Errorf("stale: got %q, want the reference typed on the invoice as it is now, at its version", got)
	}
	status(t, "sent again", b.do(http.MethodPost, editPath(d.account), again), http.StatusSeeOther)
	// The other person's description stays: only the reference was sent.
	if got, want := invoiceRow(t, d.account), "W-1-OLD 3 - - Fees B V1"; got != want {
		t.Errorf("sent again: got %q, want %q", got, want)
	}

	// A form opened before another change and refused by the checks keeps
	// its version: sent again corrected, the API says the invoice changed.
	stale2 := editForm(t, b, d.account, url.Values{"tax_amount": {"18.00"}, "gross_amount": {"118.00"}, "posting_date": {"x"}})
	status(t, "another change", b.do(http.MethodPost, editPath(d.account), editForm(t, b, d.account, url.Values{"item_description": {"Fees C"}})),
		http.StatusSeeOther)
	checked := b.do(http.MethodPost, editPath(d.account), stale2)
	status(t, "stale and wrong", checked, http.StatusUnprocessableEntity)
	fixed := formValues(checked.page(t).form(editPath(d.account)))
	if got := fixed.Get("if_match") + " " + fixed.Get("item_description") + " " + fixed.Get("tax_amount"); got != `"3" Fees B 18.00` {
		t.Errorf("stale and wrong: got %q, want the form as sent, at its version", got)
	}
	fixed.Set("posting_date", "")
	status(t, "stale and corrected", b.do(http.MethodPost, editPath(d.account), fixed), http.StatusPreconditionFailed)

	unknown := b.do(http.MethodPost, editPath(d.account), editForm(t, b, d.account, url.Values{"item_tax_code": {"ZZ"}}))
	status(t, "an unknown code", unknown, http.StatusUnprocessableEntity)
	holds(t, "an unknown code", unknown.page(t).mainText(), "payables.unknown_tax_code", "Tax code: refused by the ERP")

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

// itemsOf are an invoice's live lines as the database holds them: order
// item, description, quantity, unit, amount, tax code, account's number,
// cost center's code.
func itemsOf(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var got string
	pgtest.Scan(t, `SELECT string_agg(coalesce(it.po_item_no::text, '-') || ' ' || coalesce(it.description, '-') || ' ' || trim_scale(it.quantity) || ' '
			|| it.uom_code || ' ' || it.amount || ' ' || coalesce(it.tax_code, '-') || ' ' || coalesce(a.number, '-') || ' ' || coalesce(cc.code, '-'), '; '
			ORDER BY it.item_no)
		FROM payables.supplier_invoice_item it LEFT JOIN ledger.gl_account a ON a.id = it.account_id LEFT JOIN ledger.cost_center cc ON cc.id = it.cost_center_id
		WHERE it.supplier_invoice_id = $1 AND NOT it.is_removed`, []any{id}, &got)
	return got
}

// autofocused is the id of the form's field that has the focus.
func autofocused(p page) string {
	got := ""
	walk(p.doc, func(n *html.Node) {
		if n.Data == "input" && hasAttr(n, "autofocus") {
			got = attrOf(n, "id")
		}
	})
	return got
}

// TestChangeInvoiceLines: a line is added (without htmx the form again,
// at the version it was opened with, the new line focused; with htmx its
// lines' section alone), filled with its account by number and sent with
// the invoice's other lines; a line left out is not sent; an invoice left
// without lines, a code the company does not know and a line's field
// typed wrong are said on their fields before any call.
func TestChangeInvoiceLines(t *testing.T) {
	s, d := invoicesStack(t)
	b := s.browser()
	b.signIn()
	added := b.do(http.MethodPost, editPath(d.account), editForm(t, b, d.account, url.Values{"op": {"add"}}))
	status(t, "a line added", added, http.StatusOK)
	ap := added.page(t)
	form := formValues(ap.form(editPath(d.account)))
	if got := fmt.Sprint(form["item_from"]) + " " + form.Get("if_match"); got != `[1 ] "1"` {
		t.Errorf("a line added: got %s, want the invoice's line and a new one at the version opened", got)
	}
	if got := autofocused(ap); got != "item-2-item_description" {
		t.Errorf("a line added: the focus is on %q", got)
	}
	for name, values := range map[string][]string{"item_description": {"Fees", "Travel"}, "item_quantity": {"1", "2"}, "item_unit": {"EA", "ea"},
		"item_amount": {"100.00", "50.00"}, "item_tax_code": {"V1", "v1"}, "item_account": {"520000", "530000"}} {
		form[name] = values
	}
	form.Set("gross_amount", "178.50")
	form.Set("tax_amount", "28.50")
	sent := b.do(http.MethodPost, editPath(d.account), form)
	if sent.status != http.StatusSeeOther || sent.location() != invoicePath(d.account)+"?done=changed" {
		t.Fatalf("two lines: got %d to %q %s", sent.status, sent.location(), sent.body)
	}
	if got, want := itemsOf(t, d.account), "- Fees 1 EA 10000 V1 520000 -; - Travel 2 EA 5000 V1 530000 -"; got != want {
		t.Errorf("two lines:\ngot  %s\nwant %s", got, want)
	}
	// A line added and left empty is not sent: nothing changed.
	blank := formValues(b.do(http.MethodPost, editPath(d.account), editForm(t, b, d.account, url.Values{"op": {"add"}})).page(t).form(editPath(d.account)))
	unchanged := b.do(http.MethodPost, editPath(d.account), blank)
	if got := unchanged.location(); got != invoicePath(d.account)+"?done=unchanged" {
		t.Errorf("a blank line: got %d to %q, want nothing changed", unchanged.status, got)
	}
	// A form opened when the company's accounts could not be read shows
	// the account by its id: the invoice is not changed by someone else
	// for that, and lines added on it are kept.
	byID := formValues(b.do(http.MethodPost, editPath(d.account), editForm(t, b, d.account, url.Values{"op": {"add"}})).page(t).form(editPath(d.account)))
	byID.Set("was_item_account", byID.Get("was_item_account_id"))
	for name, values := range map[string][]string{"item_quantity": {"1", "2", "1"}, "item_unit": {"EA", "EA", "EA"}, "item_amount": {"100.00", "50.00", "5.00"},
		"item_account": {"520000", "530000", "530000"}, "item_description": {"Fees", "Travel", "Taxi"}} {
		byID[name] = values
	}
	byID.Set("gross_amount", "183.50")
	status(t, "opened by id", b.do(http.MethodPost, editPath(d.account), byID), http.StatusSeeOther)
	if got := strings.Count(itemsOf(t, d.account), ";"); got != 2 {
		t.Errorf("opened by id: got %d lines, want 3", got+1)
	}
	left := b.do(http.MethodPost, editPath(d.account), editForm(t, b, d.account, url.Values{"item_remove": {"1"}, "gross_amount": {"64.50"},
		"tax_amount": {"9.50"}}))
	status(t, "a line left out", left, http.StatusSeeOther)
	if got, want := itemsOf(t, d.account), "- Travel 2 EA 5000 V1 530000 -; - Taxi 1 EA 500 - 530000 -"; got != want {
		t.Errorf("a line left out: got %s, want %s", got, want)
	}

	swapped := b.do(http.MethodPost, editPath(d.account), editForm(t, b, d.account, url.Values{"op": {"add"}}), "HX-Request", "true")
	status(t, "a line added with htmx", swapped, http.StatusOK)
	holds(t, "a line added with htmx", swapped.body, `id="invoice-items"`, `hx-swap-oob="innerHTML:#invoice-buttons"`, "Line 2")
	if strings.Contains(swapped.body, "<html") {
		t.Error("a line added with htmx: the whole page was answered")
	}
	emptied := b.do(http.MethodPost, editPath(d.account), editForm(t, b, d.account, url.Values{"item_remove": {"1", "2"}}), "HX-Request", "true")
	holds(t, "every line left out with htmx", emptied.body, "Line 1")
	if strings.Contains(emptied.body, `value="Taxi"`) {
		t.Error("every line left out with htmx: the line is still there")
	}
	none := b.do(http.MethodPost, editPath(d.account), editForm(t, b, d.account, url.Values{"item_remove": {"1", "2"}}))
	status(t, "no line", none, http.StatusUnprocessableEntity)
	holds(t, "no line", none.page(t).mainText(), "An invoice has at least one line.", "Line 1")

	for name, tc := range map[string]struct {
		change url.Values
		want   string
	}{
		"an account not known":    {url.Values{"item_account": {"999999"}}, "No account of the company's chart has this number."},
		"a cost center not known": {url.Values{"item_cost_center": {"9999"}}, "The company has no cost center with this code."},
		"a center's code wrong":   {url.Values{"item_profit_center": {"P C"}}, "A code has at most 10 letters"},
		"no unit":                 {url.Values{"item_unit": {" "}}, "A quantity needs its unit."},
		"a unit wrong":            {url.Values{"item_unit": {"PIECES7"}}, "A unit has 1 to 6 capital letters"},
		"a unit not known":        {url.Values{"item_unit": {"PIECES", "EA"}}, "No unit of measure has this code."},
	} {
		t.Run(name, func(t *testing.T) {
			r := b.do(http.MethodPost, editPath(d.account), editForm(t, b, d.account, tc.change))
			status(t, name, r, http.StatusUnprocessableEntity)
			holds(t, name, r.page(t).mainText(), tc.want)
		})
	}
}

// TestChangeInvoiceLinesMeanwhile: lines another person changed meanwhile
// are kept. The person's changes to a line's fields are put on it; lines
// the person did not change are taken as they are now; when both changed
// the lines and lines were added or left out, the form shows the lines as
// they are now, without the person's changes to them, and says so,
// keeping the person's other changes, which sent again apply alone.
func TestChangeInvoiceLinesMeanwhile(t *testing.T) {
	s, d := invoicesStack(t)
	b := s.browser()
	b.signIn()
	// The person changes the line's amount; someone else its description.
	amount := editForm(t, b, d.account, url.Values{"item_amount": {"90.00"}, "gross_amount": {"109.00"}, "tax_amount": {"19.00"}})
	status(t, "a description meanwhile", b.do(http.MethodPost, editPath(d.account), editForm(t, b, d.account, url.Values{"item_description": {"Fees B"}})),
		http.StatusSeeOther)
	merged := b.do(http.MethodPost, editPath(d.account), amount)
	status(t, "both on one line", merged, http.StatusPreconditionFailed)
	status(t, "both on one line, sent again", b.do(http.MethodPost, editPath(d.account), formValues(merged.page(t).form(editPath(d.account)))),
		http.StatusSeeOther)
	if got, want := itemsOf(t, d.account), "- Fees B 1 EA 9000 V1 520000 -"; got != want {
		t.Errorf("both on one line: got %s, want %s", got, want)
	}

	// The person changes the reference alone; someone else adds a line.
	reference := editForm(t, b, d.account, url.Values{"supplier_reference": {"W-1R"}})
	other := formValues(b.do(http.MethodPost, editPath(d.account), editForm(t, b, d.account, url.Values{"op": {"add"}})).page(t).form(editPath(d.account)))
	for name, values := range map[string][]string{"item_quantity": {"1", "1"}, "item_unit": {"EA", "EA"}, "item_amount": {"90.00", "10.00"},
		"item_account": {"520000", "520000"}} {
		other[name] = values
	}
	status(t, "a line added meanwhile", b.do(http.MethodPost, editPath(d.account), other), http.StatusSeeOther)
	kept := b.do(http.MethodPost, editPath(d.account), reference)
	status(t, "lines changed by another", kept, http.StatusPreconditionFailed)
	status(t, "lines changed by another, sent again", b.do(http.MethodPost, editPath(d.account), formValues(kept.page(t).form(editPath(d.account)))),
		http.StatusSeeOther)
	if got := invoiceRow(t, d.account) + " | " + itemsOf(t, d.account); !strings.HasPrefix(got, "W-1R ") || !strings.HasSuffix(got, "; - - 1 EA 1000 - 520000 -") {
		t.Errorf("lines changed by another: got %s, want the reference on the invoice's two lines", got)
	}

	// The person leaves the second line out and changes the reference;
	// someone else changes the first line meanwhile.
	both := editForm(t, b, d.account, url.Values{"supplier_reference": {"W-1S"}, "item_remove": {"2"}})
	status(t, "a line changed meanwhile", b.do(http.MethodPost, editPath(d.account), editForm(t, b, d.account, url.Values{"item_description": {"Fees C", ""}})),
		http.StatusSeeOther)
	lost := b.do(http.MethodPost, editPath(d.account), both)
	status(t, "both changed the lines", lost, http.StatusConflict)
	lp := lost.page(t)
	holds(t, "both changed the lines", lp.mainText(), "Someone else changed the invoice's lines since you opened this form, and so did you.")
	again := formValues(lp.form(editPath(d.account)))
	if got := again.Get("supplier_reference") + " " + fmt.Sprint(again["item_description"]); got != "W-1S [Fees C ]" {
		t.Errorf("both changed the lines: got %s, want the reference typed on the lines as they are", got)
	}
	status(t, "both changed the lines, sent again", b.do(http.MethodPost, editPath(d.account), again), http.StatusSeeOther)
	if got := invoiceRow(t, d.account); !strings.HasPrefix(got, "W-1S ") || !strings.Contains(got, " Fees C ") {
		t.Errorf("both changed the lines, sent again: got %s", got)
	}
	if got := strings.Count(itemsOf(t, d.account), ";"); got != 1 {
		t.Errorf("both changed the lines, sent again: got %d lines, want the two kept", got+1)
	}
}

// TestChangeInvoiceOrderItems: a line of an invoice on an order names its
// order item, which a line added names too and takes its account from; an
// order item typed wrong is said on its field, one the order has not is
// refused by the API on its line.
func TestChangeInvoiceOrderItems(t *testing.T) {
	s, d := invoicesStack(t)
	b := s.browser()
	b.signIn()
	form := formValues(b.get(editPath(d.clean)).page(t).form(editPath(d.clean)))
	if got := fmt.Sprint(form["item_order_item"]); got != "[1 2]" {
		t.Errorf("order items: got %s, want [1 2]", got)
	}
	wrong := b.do(http.MethodPost, editPath(d.clean), editForm(t, b, d.clean, url.Values{"item_order_item": {"1", "x"}}))
	status(t, "an order item typed wrong", wrong, http.StatusUnprocessableEntity)
	holds(t, "an order item typed wrong", wrong.page(t).mainText(), "Enter the order item's number as digits")
	unknown := b.do(http.MethodPost, editPath(d.clean), editForm(t, b, d.clean, url.Values{"item_order_item": {"1", "3"}}))
	status(t, "an order item the order has not", unknown, http.StatusUnprocessableEntity)
	holds(t, "an order item the order has not", unknown.page(t).mainText(), "payables.unknown_po_item", "Order item: refused by the ERP")

	more := formValues(b.do(http.MethodPost, editPath(d.clean), editForm(t, b, d.clean, url.Values{"op": {"add"}})).page(t).form(editPath(d.clean)))
	for name, values := range map[string][]string{"item_order_item": {"1", "2", "2"}, "item_description": {"Paper", "Toner", "Toner, more"},
		"item_quantity": {"10", "5", "1"}, "item_unit": {"EA", "KG", "KG"}, "item_amount": {"120.00", "15.00", "3.00"}} {
		more[name] = values
	}
	more.Set("gross_amount", "160.80")
	status(t, "a line of an order item added", b.do(http.MethodPost, editPath(d.clean), more), http.StatusSeeOther)
	if got, want := itemsOf(t, d.clean), "1 Paper 10 EA 12000 V1 520000 -; 2 Toner 5 KG 1500 - 520000 -; 2 Toner, more 1 KG 300 - 520000 -"; got != want {
		t.Errorf("a line of an order item added:\ngot  %s\nwant %s", got, want)
	}
}

// TestChangeInvoiceCenters: a line's cost center is shown and typed by its
// code; one the API refuses (locked for actual costs) is said on its line.
func TestChangeInvoiceCenters(t *testing.T) {
	var inv uuid.UUID
	// CostCenters gives the tenant's person ledger.cost_center.read itself.
	permissions := slices.DeleteFunc(slices.Clone(fixture.InvoicePermissions), func(p string) bool { return p == "ledger.cost_center.read" })
	s := newStack(t, stackOptions{permissions: permissions, seed: func(f fixture.Env) {
		f.CostCenters(t, nil)
		inv = f.SupplierInvoice(t, f.Vendor, "W-C", "119.00", "19.00", f.AccountItem(t, "Fees", "100.00"))
	}})
	b := s.browser()
	b.signIn()
	status(t, "a cost center", b.do(http.MethodPost, editPath(inv), editForm(t, b, inv, url.Values{"item_cost_center": {"2000"}})), http.StatusSeeOther)
	if got := itemsOf(t, inv); !strings.HasSuffix(got, " 520000 2000") {
		t.Errorf("a cost center: got %s, want 2000", got)
	}
	status(t, "a profit center", b.do(http.MethodPost, editPath(inv), editForm(t, b, inv, url.Values{"item_profit_center": {"pc1"}})), http.StatusSeeOther)
	var profit string
	pgtest.Scan(t, `SELECT coalesce(p.code, '-') FROM payables.supplier_invoice_item it LEFT JOIN ledger.profit_center p ON p.id = it.profit_center_id
		WHERE it.supplier_invoice_id = $1 AND NOT it.is_removed`, []any{inv}, &profit)
	if profit != "PC1" {
		t.Errorf("a profit center: got %s, want PC1", profit)
	}
	form := b.get(editPath(inv)).page(t).form(editPath(inv))
	if got := field(form, "item_cost_center"); got != "2000" {
		t.Errorf("the cost center shown: got %q, want its code", got)
	}
	refused := b.do(http.MethodPost, editPath(inv), editForm(t, b, inv, url.Values{"item_cost_center": {"3000"}}))
	status(t, "a cost center locked", refused, http.StatusUnprocessableEntity)
	holds(t, "a cost center locked", refused.page(t).mainText(), "ledger.cost_center_invalid", "Cost center: refused by the ERP")
}

// TestTaxAndIssuesShown: an invoice's page shows its tax per code; a post
// refused because the match no longer holds names its issues in the
// person's words, the line each concerns, and the API's explanation as
// data (decision D3).
func TestTaxAndIssuesShown(t *testing.T) {
	var first, second uuid.UUID
	s := newStack(t, stackOptions{permissions: fixture.InvoicePermissions, seed: func(f fixture.Env) {
		po := f.Order(t, f.Supplier, "PO-1")
		f.Receive(t, po, 1, "10", "GR-1")
		first = f.SupplierInvoice(t, f.Supplier, "INV-1", "142.80", "22.80", fixture.POItem(po, 1, "10", "EA", "120.00"))
		second = f.SupplierInvoice(t, f.Supplier, "INV-2", "142.80", "22.80", fixture.POItem(po, 1, "10", "EA", "120.00"))
	}})
	b := s.browser()
	b.signIn()
	holds(t, "the tax per code", b.get(invoicePath(first)).page(t).mainText(), "Tax per code Tax the invoice states EUR 22.80",
		"Tax at its codes' rates EUR 22.80", "V1 EUR 22.80")
	// Each matched clean before either posts; the first posts.
	for _, id := range []uuid.UUID{first, second} {
		status(t, "matched", act(b, id, "match", "", ""), http.StatusSeeOther)
	}
	status(t, "the first posted", act(b, first, "post", "", ""), http.StatusSeeOther)
	r := act(b, second, "post", "", "")
	status(t, "the match outdated", r, http.StatusConflict)
	holds(t, "the match outdated", r.page(t).mainText(), "payables.match_outdated", "More is invoiced than was received Line 1")
}

// TestOrderItemChanged: a line whose order item changed takes the new
// order item's account, cost center and tax code unless the person
// changed them too; a line leaving its order item keeps what it shows.
func TestOrderItemChanged(t *testing.T) {
	po, account, other, center := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	inv := gen.SupplierInvoice{Items: []gen.SupplierInvoiceItem{{ItemNo: gen.NewOptInt32(1), PurchaseOrderID: gen.NewOptUUID(po), PoItemNo: gen.NewOptInt32(2),
		AccountID: gen.NewOptUUID(account), CostCenterID: gen.NewOptUUID(center), TaxCode: gen.NewOptString("V1")}}}
	for name, tc := range map[string]struct {
		sent gen.SupplierInvoiceItem
		want gen.SupplierInvoiceItem
	}{
		"another order item": {gen.SupplierInvoiceItem{PurchaseOrderID: gen.NewOptUUID(po), PoItemNo: gen.NewOptInt32(1), AccountID: gen.NewOptUUID(account),
			CostCenterID: gen.NewOptUUID(center), TaxCode: gen.NewOptString("V1")}, gen.SupplierInvoiceItem{PurchaseOrderID: gen.NewOptUUID(po), PoItemNo: gen.NewOptInt32(1)}},
		"another order item, account and code": {gen.SupplierInvoiceItem{PurchaseOrderID: gen.NewOptUUID(po), PoItemNo: gen.NewOptInt32(1), AccountID: gen.NewOptUUID(other),
			TaxCode: gen.NewOptString("V7")}, gen.SupplierInvoiceItem{PurchaseOrderID: gen.NewOptUUID(po), PoItemNo: gen.NewOptInt32(1), AccountID: gen.NewOptUUID(other),
			TaxCode: gen.NewOptString("V7")}},
		"no order item": {gen.SupplierInvoiceItem{AccountID: gen.NewOptUUID(account), CostCenterID: gen.NewOptUUID(center)},
			gen.SupplierInvoiceItem{AccountID: gen.NewOptUUID(account), CostCenterID: gen.NewOptUUID(center)}},
	} {
		t.Run(name, func(t *testing.T) {
			typed := invoiceTyped{items: []itemTyped{{from: 1, sent: tc.sent}}}
			got := typed.itemsChange(inv)
			if len(got) != 1 {
				t.Fatalf("lines: got %d, want 1", len(got))
			}
			if got[0] != tc.want {
				t.Errorf("got %+v, want %+v", got[0], tc.want)
			}
		})
	}
}

// TestRebaseItems: the lines a change sends after another person's, by
// who changed them and how.
func TestRebaseItems(t *testing.T) {
	line := func(from int, amount string) itemTyped { return itemTyped{from: from, amount: amount, unit: "EA"} }
	opened := []itemTyped{line(1, "1.00"), line(2, "2.00")}
	for name, tc := range map[string]struct {
		typed, now []itemTyped
		want       linesTaken
		amounts    string
	}{
		"nobody else":                      {[]itemTyped{line(1, "1.00")}, opened, linesTyped, "1.00"},
		"the person changed none":          {opened, []itemTyped{line(1, "1.00")}, linesNow, "1.00"},
		"fields of both":                   {[]itemTyped{line(1, "1.50"), line(2, "2.00")}, []itemTyped{line(1, "1.00"), line(2, "2.50")}, linesMerged, "1.50 2.50"},
		"both, one left out":               {[]itemTyped{line(1, "1.50")}, []itemTyped{line(1, "1.00"), line(2, "2.50")}, linesLost, "1.00 2.50"},
		"both, one added":                  {[]itemTyped{line(1, "1.50"), line(2, "2.00"), line(0, "3.00")}, []itemTyped{line(1, "1.00"), line(2, "2.50")}, linesLost, "1.00 2.50"},
		"both, one left out and one added": {[]itemTyped{line(1, "1.50"), line(0, "3.00")}, []itemTyped{line(1, "1.00"), line(2, "2.50")}, linesLost, "1.00 2.50"},
		"both, the other added":            {[]itemTyped{line(1, "1.50"), line(2, "2.00")}, []itemTyped{line(1, "1.00"), line(2, "2.00"), line(3, "3.00")}, linesLost, "1.00 2.00 3.00"},
	} {
		t.Run(name, func(t *testing.T) {
			got, taken := rebaseItems(tc.typed, opened, tc.now)
			var amounts []string
			for _, it := range got {
				amounts = append(amounts, it.amount)
			}
			if taken != tc.want {
				t.Errorf("taken: got %d, want %d", taken, tc.want)
			}
			if got := strings.Join(amounts, " "); got != tc.amounts {
				t.Errorf("amounts: got %s, want %s", got, tc.amounts)
			}
		})
	}
}
