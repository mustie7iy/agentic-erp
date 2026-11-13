package sales_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/sales"
)

// posting is the path that posts the billing document.
func posting(doc string) string { return billings + "/" + doc + ":post" }

// billedOrder creates an order of the items, confirms it, confirms each
// "item number, quantity, unit" as delivered today and bills it; it
// returns the order's and the billing document's ids and the day.
func (e env) billedOrder(t *testing.T, items []string, shipped ...string) (order, doc, today string) {
	t.Helper()
	order, today = e.deliveredOrder(t, items, shipped...)
	got, _ := e.Must(201, "POST", billings, bill(order, ""), "")
	return order, got["id"].(string), today
}

// entryLines are the lines of the journal entry, by line number: "account
// amount tax code, whether it names a partner".
func (e env) entryLines(t *testing.T, entry any) string {
	t.Helper()
	var out string
	pgtest.Scan(t, `SELECT coalesce(string_agg(format('%s %s %s %s', a.number, l.amount_tc, coalesce(l.tax_code, '-'), l.partner_id IS NOT NULL), '; ' ORDER BY l.line_no), '')
		FROM ledger.journal_line l JOIN ledger.gl_account a ON a.tenant_id = l.tenant_id AND a.id = l.account_id
		WHERE l.tenant_id = $1 AND l.journal_entry_id = $2`, []any{e.Tenant.ID, entry}, &out)
	return out
}

// invoices counts the customer invoices that name the billing document
// as their source.
func (e env) invoices(t *testing.T, doc string) int {
	t.Helper()
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM receivables.customer_invoice WHERE tenant_id = $1 AND source_document_id = $2`, []any{e.Tenant.ID, doc}, &n)
	return n
}

// invoiceItems are the customer invoice's items: "account number net
// amount tax code = tax", by item number.
func invoiceItems(t *testing.T, invoice map[string]any) string {
	t.Helper()
	var out []string
	for _, it := range items(t, invoice) {
		code, _ := it["tax_code"].(string)
		out = append(out, fmt.Sprintf("%v %s %s = %s", it["account_number"], amount(t, it, "net_amount"), code, amount(t, it, "tax_amount")))
	}
	return strings.Join(out, ", ")
}

// TestBillingPosted: posting a billing document enters and posts one
// customer invoice with the document's items, one for one, the billing
// date as its invoice date and the order's payment term; the invoice
// names the document and the document the invoice and its entry; the
// entry debits the customer with the gross and credits revenue and output
// tax by the document's amounts; the audit record and
// BillingDocumentPosted are written (T79b acceptance).
func TestBillingPosted(t *testing.T) {
	e := setup(t)
	order, doc, today := e.billedOrder(t, []string{e.item(line{quantity: "2.5", unit: "KG", price: "10.01", code: "A1", key: "BRACKET-M8"}),
		e.item(line{quantity: "3", price: "99.99", priceUnit: 100, account: e.other})}, "1 2.5 KG", "2 3 EA")
	created, _ := e.Must(200, "GET", billings+"/"+doc, "", "")
	before := e.journalEntries(t)
	got, tag := e.Must(200, "POST", posting(doc), "", `"1"`)
	if tag != `"2"` {
		t.Errorf("ETag: got %s, want \"2\"", tag)
	}
	fields(t, got, map[string]any{"status": "posted", "version": float64(2), "posting_date": today, "billing_date": today,
		"document_number": created["document_number"], "sales_order_id": order, "journal_entry_year": float64(instant(t, today+"T00:00:00Z").Year())})
	if a := amount(t, got, "net_amount"); a != "28.03 EUR" {
		t.Errorf("net value: got %s, want 28.03 EUR, as it was created", a)
	}
	if b := billed(t, got); b != "1: 2.5 KG = 25.03 EUR, 2: 3 EA = 3.00 EUR" {
		t.Errorf("items: got %s, want them as they were created", b)
	}
	invoiceID, _ := got["customer_invoice_id"].(string)
	entry, _ := got["journal_entry_id"].(string)
	invoice, _ := e.Must(200, "GET", "/v1/customer-invoices/"+invoiceID, "", "")
	t.Run("the customer invoice is the document's", func(t *testing.T) {
		due := instant(t, today+"T00:00:00Z").AddDate(0, 0, 30).Format(time.DateOnly)
		fields(t, invoice, map[string]any{"status": "posted", "document_number": got["customer_invoice_number"], "source_document_id": doc,
			"source_document_number": got["document_number"], "legal_entity_id": e.DE.String(), "customer_id": e.customer.String(), "currency": "EUR",
			"invoice_date": today, "posting_date": today, "payment_term_code": "NT30", "net_due_date": due, "journal_entry_id": entry,
			"journal_entry_number": got["journal_entry_number"]})
		if a := amount(t, invoice, "net_amount"); a != amount(t, got, "net_amount") {
			t.Errorf("the invoice's net amount: got %s, want the document's %s", a, amount(t, got, "net_amount"))
		}
		if a := amount(t, invoice, "tax_amount"); a != "4.76 EUR" {
			t.Errorf("the invoice's tax: got %s, want 4.76 EUR, 19%% of 25.03", a)
		}
		if a := amount(t, invoice, "gross_amount"); a != "32.79 EUR" {
			t.Errorf("the invoice's gross amount: got %s, want 32.79 EUR, net and tax", a)
		}
		if its := invoiceItems(t, invoice); its != "400000 25.03 EUR A1 = 4.76 EUR, 530000 3.00 EUR  = 0.00 EUR" {
			t.Errorf("the invoice's items: got %s, want the document's two, one for one", its)
		}
		if d := items(t, invoice)[0]["description"]; d != "Machined bracket" {
			t.Errorf("the first item's description: got %v, want the order item's", d)
		}
	})
	t.Run("the entry moves receivables, revenue and output tax by the document's amounts", func(t *testing.T) {
		if lines, want := e.entryLines(t, entry), "120000 3279 - t; 400000 -2503 A1 f; 530000 -300 - f; 230000 -476 A1 f"; lines != want {
			t.Errorf("lines: got %s, want %s", lines, want)
		}
		var docType, sourceContext, source, taxDate string
		pgtest.Scan(t, `SELECT document_type, source_context, source_document_id::text, tax_date::text FROM ledger.journal_entry WHERE tenant_id = $1 AND id = $2`,
			[]any{e.Tenant.ID, entry}, &docType, &sourceContext, &source, &taxDate)
		if got, want := docType+" "+sourceContext+" "+source+" "+taxDate, "DR receivables "+invoiceID+" "+today; got != want {
			t.Errorf("entry: got %s, want %s", got, want)
		}
		if n := e.journalEntries(t); n == before {
			t.Error("journal entries: got none more, want the invoice's entry")
		}
	})
	t.Run("read back", func(t *testing.T) {
		read, readTag := e.In(t).Must(200, "GET", billings+"/"+doc, "", "")
		if a, b := mustJSON(t, read), mustJSON(t, got); a != b {
			t.Errorf("read back: got %s, want %s", a, b)
		}
		if readTag != tag {
			t.Errorf("ETag read back: got %s, want %s", readTag, tag)
		}
		listed, _ := e.In(t).Must(200, "GET", billings+"?status=posted", "", "")
		if a, b := mustJSON(t, listed["items"]), mustJSON(t, []any{got}); a != b {
			t.Errorf("the posted documents listed: got %s, want %s", a, b)
		}
	})
	t.Run("audited and published", func(t *testing.T) {
		if n := e.audits(t, "sales.billing_document.post", doc); n != 1 {
			t.Errorf("audit records of the posting: got %d, want 1", n)
		}
		money := func(a string) map[string]any { return map[string]any{"amount": a, "currency": "EUR"} }
		want := map[string]any{"billing_document_id": doc, "document_number": got["document_number"], "legal_entity_id": e.DE.String(),
			"legal_entity_code": "2000", "customer_id": e.customer.String(), "customer_number": "BP70000001", "sales_order_id": order,
			"sales_order_number": got["sales_order_number"], "billing_date": today, "posting_date": today, "net_amount": money("28.03"),
			"tax_amount": money("4.76"), "gross_amount": money("32.79"), "customer_invoice_id": invoiceID,
			"customer_invoice_number": got["customer_invoice_number"], "journal_entry_id": entry, "journal_entry_year": got["journal_entry_year"],
			"journal_entry_number": got["journal_entry_number"], "status": "posted", "status_before": "created", "version": float64(2)}
		if a, b := mustJSON(t, e.events(t, "billing_document_posted", "billing_document_id", doc)), mustJSON(t, []map[string]any{want}); a != b {
			t.Errorf("BillingDocumentPosted: got %s, want %s", a, b)
		}
		if ev := e.events(t, "billing_document_changed", "billing_document_id", doc); len(ev) != 1 {
			t.Errorf("BillingDocumentChanged events: got %d, want the creation's alone", len(ev))
		}
		apitest.NeverPublished(t, e.Tenant, "sales", "Northwind Traders")
	})
	t.Run("the order is as it was", func(t *testing.T) {
		_, orderTag := e.In(t).Must(200, "GET", "/v1/sales-orders/"+order, "", "")
		if orderTag != `"4"` {
			t.Errorf("the order's ETag: got %s, want \"4\" (created, confirmed, delivered, billed): posting does not change what it shows", orderTag)
		}
	})
}

// TestBillingPostedTakesTheRemainder: the documents of an item billed in
// three parts post as three customer invoices whose net amounts are the
// documents', the last taking the remainder, so revenue is credited the
// item's net value exactly and each invoice's gross is its net and tax;
// Orders gives the item's posted billings and no document not yet posted
// (T79b acceptance; ADR-0005).
func TestBillingPostedTakesTheRemainder(t *testing.T) {
	e := setup(t)
	// 3 EA at 10.00 per 3 is 10.00, which no third divides.
	id, today := e.confirmed(t, e.item(line{quantity: "3", price: "10.00", priceUnit: 3, code: "A1"}))
	var nets, grosses []string
	var entries []any
	for i, want := range []string{"3.33 EUR + 0.63 EUR = 3.96 EUR", "3.34 EUR + 0.63 EUR = 3.97 EUR", "3.33 EUR + 0.63 EUR = 3.96 EUR"} {
		t.Run(fmt.Sprintf("document %d", i+1), func(t *testing.T) {
			e.In(t).Must(201, "POST", deliveries(id), delivered(today, "1 1 EA"), "")
			created, _ := e.In(t).Must(201, "POST", billings, bill(id, ""), "")
			posted, _ := e.In(t).Must(200, "POST", posting(created["id"].(string)), "", `"1"`)
			invoice, _ := e.In(t).Must(200, "GET", "/v1/customer-invoices/"+posted["customer_invoice_id"].(string), "", "")
			if a, b := amount(t, invoice, "net_amount"), amount(t, posted, "net_amount"); a != b {
				t.Errorf("the invoice's net amount: got %s, want the document's %s", a, b)
			}
			if got := amount(t, invoice, "net_amount") + " + " + amount(t, invoice, "tax_amount") + " = " + amount(t, invoice, "gross_amount"); got != want {
				t.Errorf("the invoice's net, tax and gross: got %s, want %s", got, want)
			}
			nets, grosses = append(nets, amount(t, invoice, "net_amount")), append(grosses, amount(t, invoice, "gross_amount"))
			entries = append(entries, posted["journal_entry_id"])
		})
	}
	var revenue, receivable, tax int64
	pgtest.Scan(t, `SELECT sum(l.amount_tc) FILTER (WHERE a.number = '400000'), sum(l.amount_tc) FILTER (WHERE a.number = '120000'),
			sum(l.amount_tc) FILTER (WHERE a.number = '230000')
		FROM ledger.journal_line l JOIN ledger.gl_account a ON a.tenant_id = l.tenant_id AND a.id = l.account_id
		WHERE l.tenant_id = $1 AND l.journal_entry_id = ANY($2)`, []any{e.Tenant.ID, entries}, &revenue, &receivable, &tax)
	if revenue != -1000 {
		t.Errorf("revenue credited by the three invoices: got %d, want -1000, the item's net value exactly", revenue)
	}
	if receivable != 1189 {
		t.Errorf("receivables debited by the three invoices: got %d, want 1189, their gross amounts", receivable)
	}
	if revenue+tax+receivable != 0 {
		t.Errorf("the three entries: got a sum of %d, want 0", revenue+tax+receivable)
	}
	// A fourth document, created and not posted, is not among the item's billings: nothing more is delivered, so there is none.
	e.Problem(422, "sales.nothing_to_bill", "POST", billings, bill(id, ""), "")
	err := e.inRequest(t, func(ctx context.Context) error {
		o, _, err := sales.Orders{}.Order(ctx, uuid.MustParse(id))
		if err != nil {
			return err
		}
		var told []string
		for _, b := range o.Items[0].Billings {
			told = append(told, fmt.Sprintf("%s %d on %s", b.DocumentNumber[:2], b.Net, b.PostingDate.Format(time.DateOnly)))
		}
		if got, want := strings.Join(told, "; "), "BD 333 on "+today+"; BD 334 on "+today+"; BD 333 on "+today; got != want {
			t.Errorf("the item's posted billings: got %s, want %s", got, want)
		}
		last := o.Items[0].Billings[len(o.Items[0].Billings)-1]
		if last.EntryID != uuid.MustParse(entries[2].(string)) {
			t.Errorf("the last billing's journal entry: got %s, want %v", last.EntryID, entries[2])
		}
		read, err := sales.GetOrder(ctx, uuid.MustParse(id))
		if err == nil && read.Items[0].Billings != nil {
			t.Errorf("the API's read of the order: got billings %v, want none read", read.Items[0].Billings)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(nets, ", "); got != "3.33 EUR, 3.34 EUR, 3.33 EUR" {
		t.Errorf("the invoices' net amounts: got %s, want 3.33 EUR, 3.34 EUR, 3.33 EUR", got)
	}
	if got := strings.Join(grosses, ", "); got != "3.96 EUR, 3.97 EUR, 3.96 EUR" {
		t.Errorf("the invoices' gross amounts: got %s, want 3.96 EUR, 3.97 EUR, 3.96 EUR", got)
	}
}

// TestOrdersPublishedBillings: Orders gives an item's billings once their
// documents are posted, and none of a document only created.
func TestOrdersPublishedBillings(t *testing.T) {
	e := setup(t)
	order, doc, _ := e.billedOrder(t, []string{e.item(line{quantity: "2"})}, "1 1 EA")
	billings := func() []sales.ItemBilling {
		var out []sales.ItemBilling
		err := e.inRequest(t, func(ctx context.Context) error {
			o, _, err := sales.Orders{}.Order(ctx, uuid.MustParse(order))
			if err == nil {
				out = o.Items[0].Billings
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if b := billings(); b == nil || len(b) != 0 {
		t.Errorf("billings of a document not posted: got %v, want an empty list read", b)
	}
	posted, _ := e.Must(200, "POST", posting(doc), `{"posting_date": "2026-03-12"}`, `"1"`)
	b := billings()
	if len(b) != 1 {
		t.Fatalf("billings once posted: got %d, want 1", len(b))
	}
	got := fmt.Sprintf("%s %s %d on %s, invoice %s, entry %s", b[0].BillingDocumentID, b[0].DocumentNumber, b[0].Net, b[0].PostingDate.Format(time.RFC3339),
		b[0].InvoiceID, b[0].EntryID)
	want := fmt.Sprintf("%s %s 1000 on 2026-03-12T00:00:00Z, invoice %s, entry %s", doc, posted["document_number"], posted["customer_invoice_id"],
		posted["journal_entry_id"])
	if got != want {
		t.Errorf("the billing: got %s, want %s", got, want)
	}
}

// TestBillingPostedOnADate: the billing date is the invoice's date and
// its tax date; the posting date is the request's, else the company's
// date today in its time zone; the order's payment term is the invoice's,
// not the customer's (T79b acceptance; ADR-0063 §4).
func TestBillingPostedOnADate(t *testing.T) {
	e := setup(t)
	created, _ := e.Must(201, "POST", "/v1/sales-orders", e.direct(e.customer, `, "payment_term_code": "NT14"`, e.item(line{quantity: "2", code: "A1"})), "")
	order := created["id"].(string)
	e.Must(200, "POST", "/v1/sales-orders/"+order+":confirm", "", `"1"`)
	e.Must(201, "POST", deliveries(order), delivered("2026-03-01", "1 1 EA"), "")
	first, _ := e.Must(201, "POST", billings, bill(order, `, "billing_date": "2026-03-10"`), "")
	t.Run("a posting date given", func(t *testing.T) {
		got, _ := e.In(t).Must(200, "POST", posting(first["id"].(string)), `{"posting_date": "2026-03-12"}`, `"1"`)
		fields(t, got, map[string]any{"billing_date": "2026-03-10", "posting_date": "2026-03-12", "journal_entry_year": float64(2026)})
		invoice, _ := e.In(t).Must(200, "GET", "/v1/customer-invoices/"+got["customer_invoice_id"].(string), "", "")
		fields(t, invoice, map[string]any{"invoice_date": "2026-03-10", "posting_date": "2026-03-12", "payment_term_code": "NT14", "net_due_date": "2026-03-24"})
		var dates string
		pgtest.Scan(t, `SELECT format('%s %s %s', document_date, posting_date, tax_date) FROM ledger.journal_entry WHERE tenant_id = $1 AND id = $2`,
			[]any{e.Tenant.ID, got["journal_entry_id"]}, &dates)
		if dates != "2026-03-10 2026-03-12 2026-03-10" {
			t.Errorf("the entry's document, posting and tax dates: got %s, want 2026-03-10 2026-03-12 2026-03-10", dates)
		}
	})
	t.Run("no posting date", func(t *testing.T) {
		e.In(t).Must(201, "POST", deliveries(order), delivered("2026-03-01", "1 1 EA"), "")
		second, _ := e.In(t).Must(201, "POST", billings, bill(order, `, "billing_date": "2026-03-10"`), "")
		// 03:00 UTC on 14 March is still 13 March in Toronto.
		h := endpoints(t, instant(t, "2026-03-14T03:00:00Z"))
		err := e.inRequest(t, func(ctx context.Context) error {
			b, err := h.PostBilling(ctx, uuid.MustParse(second["id"].(string)), func(int32) error { return nil }, nil)
			if err == nil && b.PostingDate.Format(time.DateOnly) != "2026-03-13" {
				t.Errorf("posting date: got %s, want 2026-03-13, the company's date", b.PostingDate.Format(time.DateOnly))
			}
			return err
		})
		if err != nil {
			t.Errorf("got %v, want the billing document posted", err)
		}
	})
}

// TestBillingPostIdempotent: a replay answers the same document and
// posts nothing more; a second post is refused, at the version read
// (412) and at the new one (409); the request needs its headers.
func TestBillingPostIdempotent(t *testing.T) {
	e := setup(t)
	_, doc, _ := e.billedOrder(t, []string{e.item(line{quantity: "3", code: "A1"})}, "1 2 EA")
	req := apitest.Req{Method: "POST", Path: posting(doc), Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey(), IfMatch: `"1"`}
	first, second := e.API.Do(req), e.API.Do(req)
	for i, rec := range []*httptest.ResponseRecorder{first, second} {
		if rec.Code != 200 {
			t.Fatalf("status of call %d: got %d, want 200: %s", i+1, rec.Code, rec.Body)
		}
	}
	if a, b := mustJSON(t, apitest.JSON(t, first)), mustJSON(t, apitest.JSON(t, second)); a != b {
		t.Errorf("replay: got %s, want the first answer %s", b, a)
	}
	if h := second.Header().Get("Idempotent-Replayed"); h != "true" {
		t.Errorf("Idempotent-Replayed: got %q, want true", h)
	}
	if n := e.invoices(t, doc); n != 1 {
		t.Errorf("customer invoices of the document after the replay: got %d, want 1", n)
	}
	if ev := e.events(t, "billing_document_posted", "billing_document_id", doc); len(ev) != 1 {
		t.Errorf("BillingDocumentPosted events after the replay: got %d, want 1", len(ev))
	}
	entries := e.journalEntries(t)
	e.Problem(412, "request.version_mismatch", "POST", posting(doc), "", `"1"`)
	refused := e.Problem(409, "sales.invalid_status", "POST", posting(doc), "", `"2"`)
	if d, _ := refused["detail"].(string); !strings.Contains(d, "is posted; only a created one is posted") {
		t.Errorf("detail: got %q, want it to say the document is posted", d)
	}
	if n := e.journalEntries(t); n != entries {
		t.Errorf("journal entries after the second post: got %d more, want none", n-entries)
	}
	if n := e.invoices(t, doc); n != 1 {
		t.Errorf("customer invoices of the document after the second post: got %d, want 1", n)
	}
	for name, change := range map[string]func(*apitest.Req){
		"without an Idempotency-Key": func(r *apitest.Req) { r.Key = "" },
		"without If-Match":           func(r *apitest.Req) { r.Key, r.IfMatch = apitest.NewKey(), "" },
	} {
		t.Run(name, func(t *testing.T) {
			r := req
			change(&r)
			if rec := e.API.Do(r); rec.Code != 428 {
				t.Errorf("got %d, want 428: %s", rec.Code, rec.Body)
			}
		})
	}
}

// TestBillingPostsRacing: two requests to post one billing document wait
// for one another on its row lock; one posts it and the other meets the
// new version, so there is one customer invoice and one entry (ADR-0008;
// T79b acceptance).
func TestBillingPostsRacing(t *testing.T) {
	e := setup(t)
	_, doc, _ := e.billedOrder(t, []string{e.item(line{quantity: "3", code: "A1"})}, "1 2 EA")
	before := e.journalEntries(t)
	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Go(func() {
			codes[i] = e.API.Do(apitest.Req{Method: "POST", Path: posting(doc), Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey(),
				IfMatch: `"1"`}).Code
		})
	}
	wg.Wait()
	// The second waits for the first's lock on the document and then reads it at version 2.
	slices.Sort(codes)
	if !slices.Equal(codes, []int{200, 412}) {
		t.Errorf("statuses: got %v, want one 200 and one 412", codes)
	}
	if n := e.invoices(t, doc); n != 1 {
		t.Errorf("customer invoices of the document: got %d, want 1", n)
	}
	var posted int
	pgtest.Scan(t, `SELECT count(*) FROM ledger.journal_entry WHERE tenant_id = $1 AND source_context = 'receivables'`, []any{e.Tenant.ID}, &posted)
	if after := e.journalEntries(t); posted == 0 || after-before != posted {
		t.Errorf("journal entries: got %d more, %d of receivables; want those of one customer invoice alone", after-before, posted)
	}
	if ev := e.events(t, "billing_document_posted", "billing_document_id", doc); len(ev) != 1 {
		t.Errorf("BillingDocumentPosted events: got %d, want 1", len(ev))
	}
	got, _ := e.Must(200, "GET", billings+"/"+doc, "", "")
	fields(t, got, map[string]any{"status": "posted", "version": float64(2)})
}

// TestBillingPostRefusals: what receivables or the ledger refuses leaves
// the billing document created and nothing posted, and is answered with
// the document's number and its own field; once the cause is gone the
// document posts (T79b acceptance; ADR-0067 §9).
func TestBillingPostRefusals(t *testing.T) {
	e := setup(t)
	_, doc, _ := e.billedOrder(t, []string{e.item(line{quantity: "3", code: "A1"}), e.item(line{quantity: "1", account: e.other})}, "1 2 EA", "2 1 EA")
	read, _ := e.Must(200, "GET", billings+"/"+doc, "", "")
	number, _ := read["document_number"].(string)
	block := func(table, column string, id uuid.UUID, blocked bool) {
		pgtest.Exec(t, `UPDATE `+table+` SET is_posting_blocked = $3, version = version + 1 WHERE tenant_id = $1 AND `+column+` = $2`, e.Tenant.ID, id, blocked)
	}
	a1 := func(validFrom string) {
		code := fixture.Document(t)
		outputCodes(code)
		codes := code.Company("2000").TaxCodes
		i := slices.IndexFunc(codes, func(c config.TaxCode) bool { return c.Code == "A1" })
		codes[i].Components[0].Rates = []config.TaxRate{{RatePercent: "19", ValidFrom: validFrom}}
		fixture.TaxCodes(t, e.Tenant, code, codes[i])
	}
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE tenant_id = $1 AND fiscal_year = 2026 AND period = 2`, e.Tenant.ID)
	cases := []struct {
		name        string
		cause, undo func()
		body        string
		status      int
		code        string
		field       any
		detail      string
	}{
		{name: "a customer blocked for posting in the company",
			cause:  func() { block("partner.customer_company", "partner_id", e.customer, true) },
			undo:   func() { block("partner.customer_company", "partner_id", e.customer, false) },
			status: 422, code: "receivables.customer_not_usable", field: "customer_id", detail: "blocked for posting"},
		{name: "an item's account that can no longer be posted",
			cause:  func() { block("ledger.company_account", "account_id", e.other, true) },
			undo:   func() { block("ledger.company_account", "account_id", e.other, false) },
			status: 422, code: "receivables.account_not_postable", field: "items[1].account_id", detail: "cannot be posted in this legal entity"},
		{name: "a tax code that lost its rate on the billing date",
			cause: func() { a1("2099-01-01") }, undo: func() { a1("2026-01-01") },
			status: 422, code: "receivables.unknown_tax_code", field: "items[0].tax_code", detail: "Tax code A1 is not an output tax code"},
		{name: "the customer's reconciliation account blocked for posting",
			cause:  func() { block("ledger.company_account", "account_id", e.Account(t, "2000", "120000"), true) },
			undo:   func() { block("ledger.company_account", "account_id", e.Account(t, "2000", "120000"), false) },
			status: 422, code: "ledger.account_not_postable", field: "customer_id", detail: "The customer's line: the account is blocked for posting"},
		{name: "the output tax account blocked for posting",
			cause:  func() { block("ledger.company_account", "account_id", e.Account(t, "2000", "230000"), true) },
			undo:   func() { block("ledger.company_account", "account_id", e.Account(t, "2000", "230000"), false) },
			status: 422, code: "ledger.account_not_postable", field: "items", detail: "The output tax on the items: the account is blocked for posting"},
		{name: "a posting date in a closed period", cause: func() {}, undo: func() {}, body: `{"posting_date": "2026-02-12"}`,
			status: 409, code: "ledger.period_not_open", detail: "Period 2026/2 is closed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := e.journalEntries(t)
			c.cause()
			got := e.In(t).Problem(c.status, c.code, "POST", posting(doc), c.body, `"1"`)
			c.undo()
			if f := field(got); f != c.field {
				t.Errorf("field: got %v, want %v", f, c.field)
			}
			detail, _ := got["detail"].(string)
			if lead := "Billing document " + number + " cannot be posted as a customer invoice: "; !strings.HasPrefix(detail, lead) {
				t.Errorf("detail: got %q, want it to lead with %q", detail, lead)
			}
			if !strings.Contains(detail, c.detail) {
				t.Errorf("detail: got %q, want it to contain %q", detail, c.detail)
			}
			if n := e.journalEntries(t); n != before {
				t.Errorf("journal entries: got %d more, want none", n-before)
			}
			if n := e.invoices(t, doc); n != 0 {
				t.Errorf("customer invoices of the document: got %d, want none left by the refused post", n)
			}
			left, _ := e.In(t).Must(200, "GET", billings+"/"+doc, "", "")
			fields(t, left, map[string]any{"status": "created", "version": float64(1), "customer_invoice_id": nil, "journal_entry_id": nil, "posting_date": nil})
		})
	}
	t.Run("another company's document", func(t *testing.T) {
		reader := e.As(e.Reader(t, "US sales", e.US))
		reader.In(t).Problem(404, "request.not_found", "POST", posting(doc), "", `"1"`)
	})
	t.Run("a document that does not exist", func(t *testing.T) {
		e.In(t).Problem(404, "request.not_found", "POST", posting(uuid.NewString()), "", `"1"`)
	})
	t.Run("once the causes are gone it posts", func(t *testing.T) {
		got, _ := e.In(t).Must(200, "POST", posting(doc), "", `"1"`)
		fields(t, got, map[string]any{"status": "posted", "version": float64(2)})
		if n := e.invoices(t, doc); n != 1 {
			t.Errorf("customer invoices of the document: got %d, want 1", n)
		}
	})
	t.Run("without the permission to post", func(t *testing.T) {
		_, other, _ := e.billedOrder(t, []string{e.item(line{})}, "1 1 EA")
		pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = 'sales.billing.post'`, e.Tenant.ID)
		e.In(t).Problem(403, "identity.permission_missing", "POST", posting(other), "", `"1"`)
	})
}

// TestBillingPostNeedsNoReceivablesPermission: the billing clerk posts
// with sales.billing.post alone; entering and posting customer invoices
// are receivables' permissions, which the clerk does not hold, and the
// invoice is entered in process.
func TestBillingPostNeedsNoReceivablesPermission(t *testing.T) {
	e := setup(t)
	_, doc, _ := e.billedOrder(t, []string{e.item(line{})}, "1 1 EA")
	e.Problem(403, "identity.permission_missing", "POST", "/v1/customer-invoices", fmt.Sprintf(
		`{"legal_entity_id": %q, "customer_id": %q, "invoice_date": "2026-03-10", "currency": "EUR", "items": [{"account_id": %q, "net_amount": {"amount": "1.00", "currency": "EUR"}}]}`,
		e.DE, e.customer, e.revenue), "")
	got, _ := e.Must(200, "POST", posting(doc), "", `"1"`)
	fields(t, got, map[string]any{"status": "posted"})
}
