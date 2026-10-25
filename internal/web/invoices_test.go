package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/net/html"

	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
)

// pdf is a captured invoice's document.
const pdf = "%PDF-1.7\n1 0 obj << /Type /Catalog >> endobj\ntrailer\n%%EOF"

// invoiceData are the invoices of invoicesStack's tenant.
type invoiceData struct {
	po, poVariance uuid.UUID // released orders of the supplier, received in full (PO-1) and item 1 alone (PO-2)
	clean          uuid.UUID // INV-1 on PO-1, matching it: parked
	variance       uuid.UUID // INV-X on PO-2 at 13.00 for 12.00: matched by the tenant's human, on hold with its exception request
	account        uuid.UUID // W-1, on an expense account, no order: parked
	taxWrong       uuid.UUID // W-2, on an account, stating 20.00 tax where V1 gives 19.00
	captured       uuid.UUID // C-1, entered by the agent from a document (pdf)
	rec            uuid.UUID // the agent's recommendation on the variance, its text planted
	viewer         pgtest.Actor
	outsider       pgtest.Actor
}

// invoicesStack is the workspace in front of a configured tenant whose
// approver works supplier invoices: two orders with their receipts, a
// clean invoice, one held at matching with a price variance and the AP
// exceptions agent's recommendation on it, two on accounts, one an agent
// captured from a document; a person who may only read invoices and one
// who may not.
func invoicesStack(t *testing.T) (*stack, *invoiceData) {
	t.Helper()
	d := &invoiceData{}
	objects := blobstore.Dir{Root: t.TempDir()}
	s := newStack(t, stackOptions{permissions: fixture.InvoicePermissions, objects: objects, seed: func(f fixture.Env) {
		d.po, d.poVariance = f.Order(t, f.Supplier, "PO-1"), f.Order(t, f.Supplier, "PO-2")
		f.Receive(t, d.po, 1, "10", "GR-1")
		f.Receive(t, d.po, 2, "5", "GR-2")
		f.Receive(t, d.poVariance, 1, "10", "GR-3")
		d.clean = f.SupplierInvoice(t, f.Supplier, "INV-1", "157.80", "22.80",
			fixture.POItem(d.po, 1, "10", "EA", "120.00"), fixture.POItem(d.po, 2, "5", "KG", "15.00"))
		d.variance = f.SupplierInvoice(t, f.Supplier, "INV-X", "154.70", "24.70", fixture.POItem(d.poVariance, 1, "10", "EA", "130.00"))
		_, tag := f.Must(200, "GET", "/v1/supplier-invoices/"+d.variance.String(), "", "")
		f.Must(200, "POST", "/v1/supplier-invoices/"+d.variance.String()+":match", "", tag)
		d.account = f.SupplierInvoice(t, f.Vendor, "W-1", "119.00", "19.00", f.AccountItem(t, "Fees", "100.00"))
		d.taxWrong = f.SupplierInvoice(t, f.Vendor, "W-2", "120.00", "20.00", f.AccountItem(t, "Fees", "100.00"))
		d.captured = f.SupplierInvoice(t, f.Vendor, "C-1", "119.00", "19.00", f.AccountItem(t, "Consulting", "100.00"))
		sum := sha256.Sum256([]byte(pdf))
		key := blobstore.Key(f.Tenant.ID, blobstore.KindDocuments, hex.EncodeToString(sum[:]))
		if err := objects.Put(context.Background(), key, []byte(pdf), blobstore.PutOptions{ContentType: "application/pdf"}); err != nil {
			t.Fatal(err)
		}
		f.Captured(t, d.captured, sum[:], key)
		d.rec = f.Recommend(t, d.variance, "approve_with_variance", planted)
		d.viewer = roleHolder(t, f, "viewer", "payables.invoice.read", "tenancy.org.read")
		d.outsider = roleHolder(t, f, "outsider")
	}})
	return s, d
}

// roleHolder is a human of the tenant in every company holding a role of the
// permissions alone.
func roleHolder(t *testing.T, f fixture.Env, name string, permissions ...string) pgtest.Actor {
	t.Helper()
	a := pgtest.Actor{ID: uuid.Must(uuid.NewV7()), Kind: "human"}
	a.Subject = "human|" + a.ID.String()
	role := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO identity.principal (tenant_id, id, kind, external_issuer, external_subject, display_name, all_companies, status, created_by, updated_by)
		VALUES ($1, $2, 'human', $3, $4, $5, true, 'active', $2, $2)`, f.Tenant.ID, a.ID, pgtest.Issuer(t), a.Subject, name)
	pgtest.Exec(t, `INSERT INTO identity.role (tenant_id, id, code, name, status, created_by, updated_by) VALUES ($1, $2, $3, $3, 'active', $4, $4)`,
		f.Tenant.ID, role, "role_"+name, f.Tenant.Human.ID)
	for _, p := range permissions {
		pgtest.Exec(t, `INSERT INTO identity.role_permission (tenant_id, id, role_id, permission_code, valid_from, created_by) VALUES ($1, gen_random_uuid(), $2, $3, current_date - 1, $4)`,
			f.Tenant.ID, role, p, f.Tenant.Human.ID)
	}
	pgtest.Exec(t, `INSERT INTO identity.principal_role (tenant_id, id, principal_id, role_id, valid_from, created_by) VALUES ($1, gen_random_uuid(), $2, $3, current_date - 1, $2)`,
		f.Tenant.ID, a.ID, role)
	return a
}

// cards are the queue's invoices by number: each card's text.
func cards(t *testing.T, r response) map[string]string {
	t.Helper()
	out := map[string]string{}
	p := r.page(t)
	for _, c := range p.all("li", "card") {
		out[text(p.cardLink(c))] = text(c)
	}
	return out
}

// number is an invoice's document number.
func number(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var n string
	pgtest.Scan(t, `SELECT document_number FROM payables.supplier_invoice WHERE id = $1`, []any{id}, &n)
	return n
}

// TestInvoiceQueue: the queue lists the invoices with their status,
// supplier, reference, company, date and amount, an agent's capture
// badged; the status tabs and the API's filters narrow it, each keeping
// the others; a date that is not one is said so, an id that is not one
// refuses the filters.
func TestInvoiceQueue(t *testing.T) {
	s, d := invoicesStack(t)
	b := s.browser()
	b.signIn()
	r := b.get("/invoices")
	status(t, "queue", r, http.StatusOK)
	all := cards(t, r)
	if len(all) != 5 {
		t.Fatalf("cards: got %d, want 5: %v", len(all), all)
	}
	for id, want := range map[uuid.UUID][]string{
		d.clean:    {"Status Parked", "Supplier BP90000001 (BP90000001)", "INV-1", "Company Acme GmbH", "Invoice date 2026-03-10", "EUR 157.80"},
		d.variance: {"On hold", "Why it is on hold Three-way match: 1 issue"},
		d.captured: {"Entered by agent Agent", "C-1"},
	} {
		holds(t, "card "+number(t, id), all[number(t, id)], want...)
	}
	p := r.page(t)
	checkInert(t, p)
	current := ""
	for _, a := range p.all("a", "navigation__link") {
		if attrOf(a, "aria-current") == "page" {
			current = attrOf(a, "href")
		}
	}
	if current != "/invoices" {
		t.Errorf("navigation: the current entry is %q, want /invoices", current)
	}
	for name, tc := range map[string]struct {
		query string
		want  []uuid.UUID
	}{
		"exceptions":      {"status=on_hold", []uuid.UUID{d.variance}},
		"a supplier":      {"supplier_id=" + s.f.Supplier.String(), []uuid.UUID{d.clean, d.variance}},
		"a company":       {"legal_entity_id=" + s.f.US.String(), nil},
		"a number":        {"document_number=" + url.QueryEscape(number(t, d.account)), []uuid.UUID{d.account}},
		"dates":           {"invoice_date_from=2026-03-10&invoice_date_to=2026-03-10&status=parked", []uuid.UUID{d.clean, d.account, d.taxWrong, d.captured}},
		"after the dates": {"invoice_date_from=2026-03-11", nil},
	} {
		t.Run(name, func(t *testing.T) {
			got := cards(t, b.get("/invoices?"+tc.query))
			if len(got) != len(tc.want) {
				t.Errorf("got %d invoices %v, want %d", len(got), got, len(tc.want))
			}
			for _, id := range tc.want {
				if _, ok := got[number(t, id)]; !ok {
					t.Errorf("invoice %s: not listed", number(t, id))
				}
			}
		})
	}
	held := b.get("/invoices?status=on_hold").page(t)
	tabs := map[string]string{}
	for _, a := range held.all("a", "tabs__link") {
		tabs[text(a)] = attrOf(a, "href") + " " + attrOf(a, "aria-current")
	}
	if got := tabs["On hold"]; got != "/invoices?status=on_hold page" {
		t.Errorf("the current tab: got %q", got)
	}
	if got := tabs["All"]; got != "/invoices " {
		t.Errorf("every status: got %q", got)
	}
	supplier := b.get("/invoices?supplier_id=" + s.f.Supplier.String()).page(t)
	holds(t, "a supplier's queue", supplier.mainText(), "Only the invoices of one supplier: BP90000001 (BP90000001) Show every supplier")
	bad := b.get("/invoices?invoice_date_from=10.03.2026")
	status(t, "a date that is no date", bad, http.StatusUnprocessableEntity)
	holds(t, "a date that is no date", bad.page(t).mainText(), "Enter a date as year-month-day")
	status(t, "an id that is no id", b.get("/invoices?supplier_id=x"), http.StatusBadRequest)
	holds(t, "an id that is no id", b.get("/invoices?legal_entity_id=x").body, "A filter in the address is not valid.")
	fr := b.get("/invoices", "Accept-Language", "fr-CA").page(t).mainText()
	holds(t, "French", fr, "157,80 EUR", "Préenregistrée")
}

// TestInvoicePage: an invoice's page shows its summary, its lines with
// their tax codes, the latest match and its issues, the link to its
// order's section, the exception's request, the agent's recommendation as
// inert text, and the actions its status offers with the invoice's ETag
// and a fresh idempotency key each.
func TestInvoicePage(t *testing.T) {
	s, d := invoicesStack(t)
	b := s.browser()
	b.signIn()
	r := b.get(invoicePath(d.clean))
	status(t, "a clean invoice", r, http.StatusOK)
	p := r.page(t)
	holds(t, "a clean invoice", p.mainText(), number(t, d.clean), "Status Parked", "Supplier's reference INV-1", "Company Acme GmbH",
		"Gross amount EUR 157.80", "Tax amount EUR 22.80", "How it was entered Entered by hand", "Entered by human",
		"1 1 10 EA EUR 120.00 V1", "2 2 5 KG EUR 15.00 None", "Not matched yet.", "The invoice was not captured")
	forms := map[string]*html.Node{}
	for _, verb := range []string{"match", "post", "hold", "cancel"} {
		forms[verb] = p.form(invoicePath(d.clean) + "/" + verb)
	}
	var version int32
	pgtest.Scan(t, `SELECT version FROM payables.supplier_invoice WHERE id = $1`, []any{d.clean}, &version)
	keys := map[string]bool{}
	for verb, f := range forms {
		if got, want := field(f, "if_match"), fmt.Sprintf(`"%d"`, version); got != want {
			t.Errorf("%s: If-Match %s, want %s", verb, got, want)
		}
		keys[field(f, "idempotency_key")] = true
	}
	if len(keys) != 4 {
		t.Errorf("idempotency keys: got %v, want four fresh ones", keys)
	}
	for _, f := range p.all("form", "") {
		if strings.HasSuffix(attrOf(f, "action"), "/release") || strings.HasSuffix(attrOf(f, "action"), "/reverse") {
			t.Errorf("a parked invoice offers %s", attrOf(f, "action"))
		}
	}
	order := ""
	for _, div := range p.all("div", "order") {
		order = attrOf(div, "hx-get") + " " + attrOf(div, "hx-trigger")
	}
	if want := invoicePath(d.clean) + "/order load"; order != want {
		t.Errorf("the order's section: got %q, want %q", order, want)
	}

	variance := b.get(invoicePath(d.variance)).page(t)
	checkInert(t, variance)
	var request uuid.UUID
	pgtest.Scan(t, `SELECT workflow_request_id FROM payables.supplier_invoice WHERE id = $1`, []any{d.variance}, &request)
	holds(t, "the variance", variance.mainText(), "Status On hold", "The latest match found 1 issue:",
		"The price differs from the order beyond the tolerance Line 1", "order item 1: invoiced 130.00 but the order price gives 120.00", "Matched by human",
		"Agents' recommendations", "Approve with the variance", "An agent wrote this", "Recommended by agent Agent", "Decision Open", "Pay now <script>",
		"An exception's approval request is linked to the invoice. While it waits", "withdraws it")
	exception := ""
	for _, a := range variance.all("a", "button") {
		exception = attrOf(a, "href")
	}
	if exception != "/approvals/"+request.String() {
		t.Errorf("the exception's request: got %q", exception)
	}
	variance.form(invoicePath(d.variance) + "/release")
	variance.form(invoicePath(d.variance) + "/recommendations/" + d.rec.String() + "/decide")

	account := b.get(invoicePath(d.account)).page(t)
	holds(t, "an invoice on an account", account.mainText(), "Purchase order None: the invoice is posted to accounts", "1 None Fees 1 EA EUR 100.00 V1",
		"The invoice is not on a purchase order, so it is not matched.")
	for _, f := range account.all("form", "") {
		if strings.HasSuffix(attrOf(f, "action"), "/match") {
			t.Error("an invoice on no order offers matching")
		}
	}
	if len(account.all("div", "order")) != 0 {
		t.Error("an invoice on no order links to an order")
	}

	captured := b.get(invoicePath(d.captured)).page(t)
	holds(t, "a captured invoice", captured.mainText(), "An agent entered this invoice from a document.", "Captured from a document", "Entered by agent Agent")
	frames := captured.all("iframe", "document")
	if len(frames) != 1 || attrOf(frames[0], "src") != invoicePath(d.captured)+"/document" || !hasAttr(frames[0], "sandbox") {
		t.Errorf("the document's frame: got %v", frames)
	}
	status(t, "no such invoice", b.get(invoicePath(uuid.New())), http.StatusNotFound)
	status(t, "no invoice id", b.get("/invoices/x"), http.StatusNotFound)
}

// hasAttr reports whether the element has the attribute.
func hasAttr(n *html.Node, key string) bool {
	_, ok := attr(n, key)
	return ok
}

// TestOrderSection: an invoice's order and receipts, as the section htmx
// loads and as a page of its own.
func TestOrderSection(t *testing.T) {
	s, d := invoicesStack(t)
	b := s.browser()
	b.signIn()
	path := invoicePath(d.clean) + "/order"
	frag := b.get(path, "HX-Request", "true")
	status(t, "the section", frag, http.StatusOK)
	if strings.Contains(frag.body, "<html") || !strings.Contains(frag.body, `<section class="order"`) {
		t.Errorf("the section: got %s, want the section alone", frag.body)
	}
	if got := strings.Join(frag.header.Values("Vary"), ", "); !strings.Contains(got, "HX-Request") {
		t.Errorf("Vary: got %q, want HX-Request in it", got)
	}
	full := b.get(path)
	status(t, "the page", full, http.StatusOK)
	holds(t, "the page", full.page(t).mainText(), "Back to the invoice", "Order PO-1", "Order status released", "Order total EUR 135.00",
		"1 Paper 10 EA EUR 12.00 10 EA V1", "2 Toner 5 KG EUR 3.00 5 KG None", "GR-1 2026-03-05 1 10 EA", "GR-2 2026-03-05 2 5 KG")
	status(t, "an invoice on no order", b.get(invoicePath(d.account)+"/order"), http.StatusNotFound)
	status(t, "no such invoice", b.get(invoicePath(uuid.New())+"/order"), http.StatusNotFound)
	status(t, "no invoice id", b.get("/invoices/x/order"), http.StatusNotFound)
	viewer := s.as(d.viewer)
	refused := viewer.get(path)
	status(t, "the order refused", refused, http.StatusOK)
	holds(t, "the order refused", refused.page(t).mainText(), "The purchase order could not be shown", "identity.permission_missing")
}

// TestInvoiceDocument: the document is served as the bytes the API holds,
// sandboxed, framed by the workspace alone; an invoice without one is not
// found.
func TestInvoiceDocument(t *testing.T) {
	s, d := invoicesStack(t)
	b := s.browser()
	b.signIn()
	r := b.get(invoicePath(d.captured) + "/document")
	status(t, "the document", r, http.StatusOK)
	if r.body != pdf {
		t.Errorf("body: got %q, want the document", r.body)
	}
	for k, want := range map[string]string{"Content-Type": "application/pdf", "Content-Security-Policy": documentCSP, "X-Frame-Options": "SAMEORIGIN",
		"X-Content-Type-Options": "nosniff", "Cache-Control": "no-store", "Content-Disposition": `attachment; filename="` + d.captured.String() + `"`} {
		if got := r.header.Get(k); got != want {
			t.Errorf("%s: got %q, want %q", k, got, want)
		}
	}
	// A PDF is downloaded (D4): the frame that asks for it says so.
	framed := b.get(invoicePath(d.captured)+"/document", "Sec-Fetch-Dest", "iframe")
	status(t, "the document in its frame", framed, http.StatusOK)
	if got, want := framed.header.Get("Content-Type"), "text/plain; charset=utf-8"; got != want {
		t.Errorf("in its frame: type %q, want %q", got, want)
	}
	holds(t, "in its frame", framed.body, "This document is not shown here")
	if got := framed.header.Get("Content-Disposition"); got != "" {
		t.Errorf("in its frame: disposition %q, want none", got)
	}
	none := b.get(invoicePath(d.account) + "/document")
	status(t, "no document", none, http.StatusNotFound)
	if got := none.header.Get("Content-Security-Policy"); got != documentCSP {
		t.Errorf("no document: CSP %q, want the sandbox", got)
	}
	status(t, "no invoice id", b.get("/invoices/x/document"), http.StatusNotFound)
	refused := s.as(d.outsider).get(invoicePath(d.captured) + "/document")
	status(t, "refused", refused, http.StatusForbidden)
	holds(t, "refused", refused.body, "You do not have the permission this needs.")
}

// TestDocumentType: only images are shown as themselves, XML as plain
// text, anything else is downloaded, a PDF too (D4).
func TestDocumentType(t *testing.T) {
	for name, tc := range map[string]struct{ head, ctype, disposition string }{
		"PDF":   {pdf, "application/pdf", "attachment"},
		"PNG":   {"\x89PNG\r\n\x1a\n", "image/png", "inline"},
		"JPEG":  {"\xff\xd8\xff", "image/jpeg", "inline"},
		"XML":   {`<?xml version="1.0"?><Invoice/>`, "text/plain; charset=utf-8", "inline"},
		"text":  {"Invoice 1", "text/plain; charset=utf-8", "inline"},
		"HTML":  {"<html><script>alert(1)</script>", "application/octet-stream", "attachment"},
		"other": {"\x00\x01\x02", "application/octet-stream", "attachment"},
	} {
		t.Run(name, func(t *testing.T) {
			ct, disp := documentType([]byte(tc.head))
			if ct != tc.ctype {
				t.Errorf("type: got %q, want %q", ct, tc.ctype)
			}
			if disp != tc.disposition {
				t.Errorf("disposition: got %q, want %q", disp, tc.disposition)
			}
		})
	}
}

// act posts an invoice's form with its fields, text and date as given.
func act(b *browser, id uuid.UUID, verb, text, date string) response {
	b.s.t.Helper()
	form := b.get(invoicePath(id)).page(b.s.t).form(invoicePath(id) + "/" + verb)
	v := values(form, text)
	v.Set("date", date)
	return b.do(http.MethodPost, invoicePath(id)+"/"+verb, v)
}

// invoiceStatus is an invoice's status.
func invoiceStatus(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var st string
	pgtest.Scan(t, `SELECT status FROM payables.supplier_invoice WHERE id = $1`, []any{id}, &st)
	return st
}

// TestCleanPost: matching a clean invoice and posting it, a resubmitted
// form replaying rather than posting twice; reversing it needs a reason.
func TestCleanPost(t *testing.T) {
	s, d := invoicesStack(t)
	b := s.browser()
	b.signIn()
	r := act(b, d.clean, "match", "", "")
	if r.status != http.StatusSeeOther || r.location() != invoicePath(d.clean)+"?done=matched" {
		t.Fatalf("match: got %d to %q %s", r.status, r.location(), r.body)
	}
	holds(t, "matched", b.get(r.location()).page(t).mainText(), "The invoice was matched.", "The latest match is clean: no issue.", "Matched by approver1")
	form := b.get(invoicePath(d.clean)).page(t).form(invoicePath(d.clean) + "/post")
	v := values(form, "")
	v.Set("date", "2026-03-15")
	if r := b.do(http.MethodPost, invoicePath(d.clean)+"/post", v); r.status != http.StatusSeeOther || r.location() != invoicePath(d.clean)+"?done=posted" {
		t.Fatalf("post: got %d to %q %s", r.status, r.location(), r.body)
	}
	if again := b.do(http.MethodPost, invoicePath(d.clean)+"/post", v); again.status != http.StatusSeeOther {
		t.Errorf("resubmitted: got %d %s, want the first answer replayed", again.status, again.body)
	}
	var entries int
	pgtest.Scan(t, `SELECT count(*) FROM ledger.journal_entry WHERE tenant_id = $1 AND source_document_id = $2`, []any{s.ten.ID, d.clean}, &entries)
	if entries != 1 {
		t.Errorf("journal entries: got %d, want 1", entries)
	}
	posted := b.get(invoicePath(d.clean) + "?done=posted").page(t)
	holds(t, "posted", posted.mainText(), "The invoice is posted.", "Status Posted", "Posting date 2026-03-15")
	empty := act(b, d.clean, "reverse", " ", "")
	status(t, "reverse without a reason", empty, http.StatusUnprocessableEntity)
	holds(t, "reverse without a reason", empty.page(t).mainText(), "Enter the reason.")
	if r := act(b, d.clean, "reverse", "Wrong supplier", "2026-03-20"); r.status != http.StatusSeeOther {
		t.Fatalf("reverse: got %d %s", r.status, r.body)
	}
	if got := invoiceStatus(t, d.clean); got != "reversed" {
		t.Errorf("status: got %s, want reversed", got)
	}
	holds(t, "reversed", b.get(invoicePath(d.clean)).page(t).mainText(), "Status Reversed", "Nothing is left to do on this invoice.")
}

// TestExceptionDecidedElsewhere: an invoice held for a price variance is
// parked again once someone else approved its exception, and posts.
func TestExceptionDecidedElsewhere(t *testing.T) {
	s, d := invoicesStack(t)
	b := s.browser()
	b.signIn()
	var request uuid.UUID
	pgtest.Scan(t, `SELECT workflow_request_id FROM payables.supplier_invoice WHERE id = $1`, []any{d.variance}, &request)
	approveElsewhere(t, s, request)
	if got := invoiceStatus(t, d.variance); got != "parked" {
		t.Fatalf("after the exception's approval: got %s, want parked", got)
	}
	holds(t, "approved", b.get(invoicePath(d.variance)).page(t).mainText(), "Status Parked", "Open the approval request")
	if r := act(b, d.variance, "post", "", ""); r.status != http.StatusSeeOther {
		t.Fatalf("post: got %d %s", r.status, r.page(t).mainText())
	}
	if got := invoiceStatus(t, d.variance); got != "posted" {
		t.Errorf("status: got %s, want posted", got)
	}
}

// approveElsewhere approves the exception's request as the second
// approver, in the workspace's inbox.
func approveElsewhere(t *testing.T, s *stack, request uuid.UUID) {
	t.Helper()
	other := s.as(s.f.Approver2)
	form := other.get(requestPath(request)).page(t).form(requestPath(request) + "/approve")
	if r := other.do(http.MethodPost, requestPath(request)+"/approve", values(form, "Price list of March")); r.status != http.StatusSeeOther {
		t.Fatalf("the exception's approval: got %d %s", r.status, r.page(t).mainText())
	}
}

// TestApprovedThenHeld: an invoice whose exception was approved and that
// a person then held by hand keeps the approved request's link; the page
// does not say the request still waits.
func TestApprovedThenHeld(t *testing.T) {
	s, d := invoicesStack(t)
	b := s.browser()
	b.signIn()
	var request uuid.UUID
	pgtest.Scan(t, `SELECT workflow_request_id FROM payables.supplier_invoice WHERE id = $1`, []any{d.variance}, &request)
	approveElsewhere(t, s, request)
	if r := act(b, d.variance, "hold", "Supplier dispute", ""); r.status != http.StatusSeeOther {
		t.Fatalf("hold: got %d %s", r.status, r.page(t).mainText())
	}
	got := b.get(invoicePath(d.variance)).page(t).mainText()
	holds(t, "approved, then held", got, "Status On hold", "Why it is on hold Supplier dispute", "Open the approval request", "While it waits")
	if strings.Contains(got, "waits for its approval request") {
		t.Errorf("approved, then held: the page says the request still waits: %s", got)
	}
}

// TestVarianceReleasedByHand: releasing an invoice held for a price
// variance withdraws its exception's request; posting it is then refused
// until it is matched again, which holds it again.
func TestVarianceReleasedByHand(t *testing.T) {
	s, d := invoicesStack(t)
	b := s.browser()
	b.signIn()
	if r := act(b, d.variance, "release", "Price list checked by phone", ""); r.status != http.StatusSeeOther {
		t.Fatalf("release: got %d %s", r.status, r.page(t).mainText())
	}
	var request string
	pgtest.Scan(t, `SELECT r.status FROM workflow.approval_request r WHERE r.object_id = $1`, []any{d.variance}, &request)
	if request != "cancelled" {
		t.Errorf("the exception's request: got %s, want it withdrawn", request)
	}
	r := act(b, d.variance, "post", "", "2026-03-15")
	status(t, "post after a release by hand", r, http.StatusConflict)
	p := r.page(t)
	holds(t, "post after a release by hand", p.mainText(), "The invoice was not changed", "The invoice needs a clean match, or an approved exception",
		"payables.match_required")
	if got := field(p.form(invoicePath(d.variance)+"/post"), "date"); got != "2026-03-15" {
		t.Errorf("the date kept: got %q", got)
	}
	if r := act(b, d.variance, "match", "", ""); r.status != http.StatusSeeOther {
		t.Fatalf("match: got %d", r.status)
	}
	if got := invoiceStatus(t, d.variance); got != "on_hold" {
		t.Errorf("matched again: status %s, want on_hold", got)
	}
}

// TestHoldReleaseCancel: a person holds a parked invoice with a reason,
// releases it, and cancels it; then nothing is left to do.
func TestHoldReleaseCancel(t *testing.T) {
	s, d := invoicesStack(t)
	b := s.browser()
	b.signIn()
	status(t, "hold without a reason", act(b, d.account, "hold", "", ""), http.StatusUnprocessableEntity)
	for _, step := range []struct{ verb, text, done, status string }{
		{"hold", "Waiting for the supplier's call-back", "held", "on_hold"},
		{"release", "", "released", "parked"},
		{"cancel", "Entered twice", "cancelled", "cancelled"},
	} {
		r := act(b, d.account, step.verb, step.text, "")
		if r.status != http.StatusSeeOther || r.location() != invoicePath(d.account)+"?done="+step.done {
			t.Fatalf("%s: got %d to %q %s", step.verb, r.status, r.location(), r.page(t).mainText())
		}
		if got := invoiceStatus(t, d.account); got != step.status {
			t.Errorf("%s: status %s, want %s", step.verb, got, step.status)
		}
	}
	holds(t, "cancelled", b.get(invoicePath(d.account)).page(t).mainText(), "Status Cancelled", "Nothing is left to do on this invoice.")
}

// TestInvoiceRefusals: the API's refusals are explained by their codes,
// with what was typed kept: a stale version, a tax that does not agree
// with the lines' codes; a date that is no date is said so before any
// call; a form that is not the page's, no CSRF token or no invoice are
// refused before any call.
func TestInvoiceRefusals(t *testing.T) {
	s, d := invoicesStack(t)
	b := s.browser()
	b.signIn()
	form := b.get(invoicePath(d.account)).page(t).form(invoicePath(d.account) + "/post")
	stale := values(form, "")
	stale.Set("if_match", `"999"`)
	stale.Set("date", "2026-03-15")
	r := b.do(http.MethodPost, invoicePath(d.account)+"/post", stale)
	status(t, "stale", r, http.StatusPreconditionFailed)
	p := r.page(t)
	holds(t, "stale", p.mainText(), "The invoice changed since you opened it.", "request.version_mismatch")
	if got := field(p.form(invoicePath(d.account)+"/post"), "date"); got != "2026-03-15" {
		t.Errorf("stale: the date kept is %q", got)
	}
	tax := act(b, d.taxWrong, "post", "", "")
	status(t, "the tax", tax, http.StatusUnprocessableEntity)
	holds(t, "the tax", tax.page(t).mainText(), "The tax amount does not agree with the tax calculated at the lines' tax codes.", "payables.tax_mismatch")
	date := act(b, d.account, "post", "", "15.03.2026")
	status(t, "a date that is no date", date, http.StatusUnprocessableEntity)
	holds(t, "a date that is no date", date.page(t).mainText(), "Enter a date as year-month-day, for example 2026-03-15.")
	if got := invoiceStatus(t, d.account); got != "parked" {
		t.Errorf("the account invoice: status %s, want parked", got)
	}
	token := b.csrf()
	for name, tc := range map[string]struct {
		path   string
		values url.Values
		status int
	}{
		"no idempotency key": {invoicePath(d.account) + "/hold", url.Values{"csrf_token": {token}, "if_match": {`"1"`}}, http.StatusBadRequest},
		"no If-Match":        {invoicePath(d.account) + "/hold", url.Values{"csrf_token": {token}, "idempotency_key": {uuid.NewString()}}, http.StatusBadRequest},
		"no invoice":         {"/invoices/x/post", url.Values{"csrf_token": {token}}, http.StatusNotFound},
		"no CSRF token":      {invoicePath(d.account) + "/post", url.Values{"if_match": {`"1"`}, "idempotency_key": {uuid.NewString()}}, http.StatusForbidden},
		"no recommendation":  {invoicePath(d.variance) + "/recommendations/x/decide", url.Values{"csrf_token": {token}}, http.StatusNotFound},
		"a form not the page's": {invoicePath(d.variance) + "/recommendations/" + d.rec.String() + "/decide",
			url.Values{"csrf_token": {token}, "if_match": {`"1"`}}, http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			if r := b.do(http.MethodPost, tc.path, tc.values); r.status != tc.status {
				t.Errorf("got %d, want %d", r.status, tc.status)
			}
		})
	}
}

// TestDecideRecommendation: the person records what they did with the
// agent's recommendation, once; an outcome must be chosen; a decided
// recommendation offers no form, and an old form is refused by the API.
func TestDecideRecommendation(t *testing.T) {
	s, d := invoicesStack(t)
	b := s.browser()
	b.signIn()
	path := invoicePath(d.variance) + "/recommendations/" + d.rec.String() + "/decide"
	form := b.get(invoicePath(d.variance)).page(t).form(path)
	second := b.get(invoicePath(d.variance)).page(t).form(path) // another tab, opened before the decision
	none := b.do(http.MethodPost, path, values(form, "No choice"))
	status(t, "no outcome", none, http.StatusUnprocessableEntity)
	holds(t, "no outcome", none.page(t).mainText(), "Choose what you did with the recommendation.")
	v := values(form, "Approved the exception's request")
	v.Set("outcome", "accepted")
	if r := b.do(http.MethodPost, path, v); r.status != http.StatusSeeOther || r.location() != invoicePath(d.variance)+"?done=decided" {
		t.Fatalf("decide: got %d to %q %s", r.status, r.location(), r.body)
	}
	if again := b.do(http.MethodPost, path, v); again.status != http.StatusSeeOther {
		t.Errorf("resubmitted: got %d, want the first answer replayed", again.status)
	}
	after := b.get(invoicePath(d.variance) + "?done=decided").page(t)
	holds(t, "decided", after.mainText(), "Your decision on the recommendation is recorded.", "Decision Accepted", "Decided by approver1")
	for _, f := range after.all("form", "") {
		if attrOf(f, "action") == path {
			t.Error("a decided recommendation offers its form")
		}
	}
	old := values(second, "")
	old.Set("outcome", "ignored")
	r := b.do(http.MethodPost, path, old)
	status(t, "an old form", r, http.StatusPreconditionFailed)
	holds(t, "an old form", r.page(t).mainText(), "The decision was not recorded", "request.version_mismatch")
	var outcome string
	pgtest.Scan(t, `SELECT outcome FROM agents.recommendation WHERE id = $1`, []any{d.rec}, &outcome)
	if outcome != "accepted" {
		t.Errorf("outcome: got %s, want accepted", outcome)
	}
}

// TestInvoicesAsOthers: a person who may only read invoices is offered no
// action, no recommendation and no order, and an action sent anyway is
// the API's 403 explained; a person who may not read invoices is offered
// no queue and sees the API's refusal there.
func TestInvoicesAsOthers(t *testing.T) {
	s, d := invoicesStack(t)
	viewer := s.as(d.viewer)
	p := viewer.get(invoicePath(d.variance)).page(t)
	holds(t, "the viewer", p.mainText(), "You may not change this invoice in its status.")
	if strings.Contains(p.mainText(), "Agents' recommendations") || len(p.all("form", "decide")) != 0 || len(p.all("div", "order")) != 0 {
		t.Errorf("the viewer: offered more than reading: %s", p.mainText())
	}
	r := viewer.do(http.MethodPost, invoicePath(d.account)+"/hold", url.Values{"csrf_token": {viewer.csrf()}, "if_match": {`"1"`},
		"idempotency_key": {uuid.NewString()}, "comment": {"Check"}})
	status(t, "the viewer's hold", r, http.StatusForbidden)
	holds(t, "the viewer's hold", r.page(t).mainText(), "You do not have the permission this needs.", "identity.permission_missing")
	outsider := s.as(d.outsider)
	for _, a := range outsider.get("/").page(t).all("a", "navigation__link") {
		if attrOf(a, "href") == "/invoices" {
			t.Error("navigation: the invoices offered to a person who may not read them")
		}
	}
	for _, path := range []string{"/invoices", invoicePath(d.clean)} {
		r := outsider.get(path)
		status(t, path, r, http.StatusForbidden)
		holds(t, path, r.body, "You do not have the permission this needs.")
	}
}

// TestInvoicesInPseudoLocales: in both pseudo-locales every text of the
// queue, an invoice's page and its order's section is the catalog's, but
// data marked translate="no".
func TestInvoicesInPseudoLocales(t *testing.T) {
	s, d := invoicesStack(t)
	b := s.browser()
	b.signIn()
	for _, tc := range []struct{ lang, open, close string }{{"en-XA", i18n.LongOpen, i18n.LongClose}, {"ar-XB", i18n.RTLOpen, i18n.RTLClose}} {
		for _, path := range []string{"/invoices", invoicePath(d.variance), invoicePath(d.captured), invoicePath(d.clean) + "/order"} {
			t.Run(tc.lang+path, func(t *testing.T) {
				r := b.get(path, "Accept-Language", tc.lang)
				if r.status != http.StatusOK {
					t.Fatalf("got %d", r.status)
				}
				var all []string
				texts(r.page(t).doc, &all)
				for _, x := range all {
					if rest := outsideCatalog(x, tc.open, tc.close); rest != "" {
						t.Errorf("text %q: %q is not from the catalog", x, rest)
					}
				}
			})
		}
	}
}
