package web

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// downPaymentData are a supplier's two paid down payments (EUR 119.00 and
// 238.00, tax code V1), another supplier's, the supplier's posted final
// invoice of EUR 238.00 and a parked one, and a person who may read
// invoices but neither clear nor read the ledger's items.
type downPaymentData struct {
	small, large, other ledger.Entry
	posted, parked      uuid.UUID
	viewer              pgtest.Actor
}

// downPaymentsStack is a workspace whose company 2000 has down payments.
func downPaymentsStack(t *testing.T) (*stack, *downPaymentData) {
	t.Helper()
	d := &downPaymentData{}
	s := newStack(t, stackOptions{permissions: append([]string{"payables.down_payment.request"}, fixture.InvoicePermissions...), seed: func(f fixture.Env) {
		f.DownPayments(t)
		d.small, d.large = f.PaidDownPayment(t, f.Vendor, "119.00", "AB-1"), f.PaidDownPayment(t, f.Vendor, "238.00", "AB-2")
		d.other = f.PaidDownPayment(t, f.Supplier, "59.50", "AB-9")
		d.posted = f.SupplierInvoice(t, f.Vendor, "FIN-1", "238.00", "38.00", f.AccountItem(t, "Fees", "200.00"))
		f.Must(200, "POST", "/v1/supplier-invoices/"+d.posted.String()+":post", `{"posting_date": "2026-03-10"}`, `"1"`)
		d.parked = f.SupplierInvoice(t, f.Vendor, "FIN-2", "119.00", "19.00", f.AccountItem(t, "Fees", "100.00"))
		d.viewer = roleHolder(t, f, "viewer", "payables.invoice.read", "tenancy.org.read")
	}})
	return s, d
}

// downPaymentOf is a down payment's value in the form.
func downPaymentOf(e ledger.Entry) string {
	return strconv.Itoa(int(e.FiscalYear)) + ":" + e.ID.String() + ":1"
}

// requestStatus is the status of the down payment request its entry paid.
func requestStatus(t *testing.T, e ledger.Entry) string {
	t.Helper()
	var got string
	pgtest.Scan(t, `SELECT status FROM payables.down_payment_request WHERE journal_entry_id = $1`, []any{e.ID}, &got)
	return got
}

// boxes are the down payments a page offers, and those checked.
func boxes(p page) (offered, checked []string) {
	for _, in := range p.all("input", "") {
		if attrOf(in, "name") == "down_payment" {
			offered = append(offered, attrOf(in, "value"))
			if hasAttr(in, "checked") {
				checked = append(checked, attrOf(in, "value"))
			}
		}
	}
	slices.Sort(offered)
	slices.Sort(checked)
	return offered, checked
}

// TestClearDownPayments: a posted invoice's page links to the clearing;
// its screen lists the supplier's open down payments alone, the date the
// company's today; what is wrong is said before any call with the choice
// kept; the API's refusal is said with the choice kept at the invoice's
// version now; the down payment chosen is cleared and the screen lists
// only the one left; an invoice not posted is not offered it.
func TestClearDownPayments(t *testing.T) {
	s, d := downPaymentsStack(t)
	b := s.browser()
	b.signIn()
	path := downPaymentsPath(d.posted)
	link := ""
	for _, a := range b.get(invoicePath(d.posted)).page(t).all("a", "button") {
		if attrOf(a, "href") == path {
			link = text(a)
		}
	}
	if link != "Clear down payments" {
		t.Errorf("the clearing's link: got %q, want Clear down payments", link)
	}
	r := b.get(path)
	status(t, "the screen", r, http.StatusOK)
	p := r.page(t)
	holds(t, "the screen", p.mainText(), number(t, d.posted), "Clear down payments", "Gross amount", "238.00", "AB-1", "AB-2", "2026-03-05",
		"Paid, tax included", "EUR 119.00", "EUR 19.00", "EUR 238.00", "EUR 38.00")
	offered, _ := boxes(p)
	want := []string{downPaymentOf(d.small), downPaymentOf(d.large)}
	slices.Sort(want)
	if !slices.Equal(offered, want) {
		t.Errorf("down payments offered: got %v, want the supplier's two %v", offered, want)
	}
	form := p.form(path)
	if got, want := field(form, "date"), s.clock.Now().UTC().Format("2006-01-02"); got != want { // company 2000 keeps no time zone: UTC's
		t.Errorf("the date: got %q, want the company's today %q", got, want)
	}
	if got := field(form, "if_match"); got != `"2"` {
		t.Errorf("the ETag: got %q, want the posted invoice's", got)
	}
	send := func(changes url.Values) response {
		v := formValues(b.get(path).page(t).form(path))
		v.Set("date", "2026-03-20")
		for k, vs := range changes {
			v[k] = vs
		}
		return b.do(http.MethodPost, path, v)
	}
	none := send(nil)
	status(t, "none chosen", none, http.StatusUnprocessableEntity)
	holds(t, "none chosen", none.page(t).mainText(), "Choose at least one down payment.")
	for name, c := range map[string]struct{ date, want string }{
		"no date":      {"", "Enter the posting date."},
		"not a date":   {"20.03.2026", "Enter a date as year-month-day"},
		"a date wrong": {"2026-02-30", "Enter a date as year-month-day"},
	} {
		t.Run(name, func(t *testing.T) {
			r := send(url.Values{"date": {c.date}, "down_payment": {downPaymentOf(d.small)}})
			status(t, name, r, http.StatusUnprocessableEntity)
			p := r.page(t)
			holds(t, name, p.mainText(), c.want)
			if _, checked := boxes(p); !slices.Equal(checked, []string{downPaymentOf(d.small)}) {
				t.Errorf("%s: got %v checked, want the choice kept", name, checked)
			}
			if got := field(p.form(path), "date"); got != c.date {
				t.Errorf("%s: date got %q, want %q kept", name, got, c.date)
			}
		})
	}
	status(t, "a down payment not offered", send(url.Values{"down_payment": {"2026:x:1"}}), http.StatusBadRequest)

	both := send(url.Values{"down_payment": {downPaymentOf(d.small), downPaymentOf(d.large)}})
	status(t, "both, above the gross", both, http.StatusUnprocessableEntity)
	bp := both.page(t)
	holds(t, "both, above the gross", bp.mainText(), "The down payments were not cleared",
		"The down payments chosen, with those cleared before, exceed the invoice's gross amount.")
	if _, checked := boxes(bp); !slices.Equal(checked, want) {
		t.Errorf("both, above the gross: got %v checked, want the choice kept", checked)
	}
	stale := send(url.Values{"down_payment": {downPaymentOf(d.large)}, "if_match": {`"1"`}})
	status(t, "a stale version", stale, http.StatusPreconditionFailed)
	if got := field(stale.page(t).form(path), "if_match"); got != `"2"` {
		t.Errorf("a stale version: the form's ETag got %q, want the invoice's now", got)
	}

	done := send(url.Values{"down_payment": {downPaymentOf(d.large)}})
	if done.status != http.StatusSeeOther || done.location() != invoicePath(d.posted)+"?done=cleared" {
		t.Fatalf("cleared: got %d to %q %s", done.status, done.location(), done.body)
	}
	holds(t, "cleared", b.get(done.location()).page(t).mainText(), "The down payments are cleared against the invoice.")
	for name, c := range map[string]struct {
		entry ledger.Entry
		want  string
	}{"the one chosen": {d.large, "cleared"}, "the one left": {d.small, "paid"}, "another supplier's": {d.other, "paid"}} {
		if got := requestStatus(t, c.entry); got != c.want {
			t.Errorf("%s: got %s, want %s", name, got, c.want)
		}
	}
	if offered, _ := boxes(b.get(path).page(t)); !slices.Equal(offered, []string{downPaymentOf(d.small)}) {
		t.Errorf("after the clearing: got %v offered, want the one left", offered)
	}

	parked := b.get(downPaymentsPath(d.parked))
	if parked.status != http.StatusSeeOther || parked.location() != invoicePath(d.parked) {
		t.Errorf("a parked invoice: got %d to %q", parked.status, parked.location())
	}
	for _, a := range b.get(invoicePath(d.parked)).page(t).all("a", "button") {
		if attrOf(a, "href") == downPaymentsPath(d.parked) {
			t.Error("a parked invoice offers the clearing")
		}
	}
	status(t, "no such invoice", b.get(downPaymentsPath(uuid.New())), http.StatusNotFound)
	status(t, "no invoice id", b.get("/invoices/x/down-payments"), http.StatusNotFound)
	status(t, "no invoice id, sent", b.do(http.MethodPost, "/invoices/x/down-payments", url.Values{"csrf_token": {b.csrf()}}), http.StatusNotFound)
	status(t, "not the page's form", b.do(http.MethodPost, path, url.Values{"csrf_token": {b.csrf()}}), http.StatusBadRequest)
}

// TestClearDownPaymentsInFrench: the screen, its refusal and its notice
// in French.
func TestClearDownPaymentsInFrench(t *testing.T) {
	s, d := downPaymentsStack(t)
	b := s.browser()
	b.signIn()
	path := downPaymentsPath(d.posted)
	fr := []string{"Accept-Language", "fr-CA"}
	holds(t, "the screen", b.get(path, fr...).page(t).mainText(), "Lettrer des acomptes", "Acomptes ouverts du fournisseur", "119,00")
	v := formValues(b.get(path, fr...).page(t).form(path))
	v.Set("date", "2026-03-20")
	v["down_payment"] = []string{downPaymentOf(d.small), downPaymentOf(d.large)}
	holds(t, "refused", b.do(http.MethodPost, path, v, fr...).page(t).mainText(), "Les acomptes n'ont pas été lettrés",
		"dépassent le montant brut de la facture")
}

// TestClearDownPaymentsAsOthers: a person who may read invoices but not
// clear down payments nor read the ledger's items is not offered the
// clearing, and the screen, reached by its address, shows the API's 403
// where the down payments would be.
func TestClearDownPaymentsAsOthers(t *testing.T) {
	s, d := downPaymentsStack(t)
	b := s.as(d.viewer)
	for _, a := range b.get(invoicePath(d.posted)).page(t).all("a", "button") {
		if attrOf(a, "href") == downPaymentsPath(d.posted) {
			t.Error("a viewer is offered the clearing")
		}
	}
	r := b.get(downPaymentsPath(d.posted))
	status(t, "the screen", r, http.StatusForbidden)
	holds(t, "the screen", r.page(t).mainText(), "The down payments were not cleared", "You do not have the permission this needs.")
}

// TestDownPaymentsCallTheirInventory: the clearing's screen, shown and
// sent and refused, calls only operations its routes' inventory lists.
func TestDownPaymentsCallTheirInventory(t *testing.T) {
	s, d := downPaymentsStack(t)
	b := s.browser()
	b.signIn()
	rec := recordCalls(t, s)
	path := downPaymentsPath(d.posted)
	form := formValues(b.get(path).page(t).form(path))
	inventoried(t, "GET /invoices/{id}/down-payments", rec.take())
	form["down_payment"] = []string{downPaymentOf(d.small), downPaymentOf(d.large)}
	form.Set("date", "2026-03-20")
	status(t, "above the gross", b.do(http.MethodPost, path, form), http.StatusUnprocessableEntity)
	inventoried(t, "POST /invoices/{id}/down-payments", rec.take())
}
