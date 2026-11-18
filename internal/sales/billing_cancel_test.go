package sales_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/sales"
)

func cancelBilling(doc string) string { return billings + "/" + doc + ":cancel" }

// TestPostedBillingCancelled: a posted billing document is cancelled by a
// cancellation numbered from the billing range, whose credit memo names
// the invoice and the cancellation and gives back its net, tax and gross
// to the minor unit; the order's items are billable again, and a new
// document bills them at the cumulative amounts; audited and published,
// once (T79c acceptance).
func TestPostedBillingCancelled(t *testing.T) {
	e := setup(t)
	order, doc, today := e.postedOrder(t, []string{e.item(line{quantity: "3", price: "10.00", priceUnit: 3, code: "A1"}),
		e.item(line{price: "5.00", account: e.other})}, "1 3 EA", "2 1 EA")
	posted, _ := e.Must(200, "GET", billings+"/"+doc, "", "")
	invoice, _ := e.Must(200, "GET", "/v1/customer-invoices/"+posted["customer_invoice_id"].(string), "", "")
	before := e.openItems(t)
	got, tag := e.Must(200, "POST", cancelBilling(doc), `{"reason": "Billed at the wrong price"}`, `"2"`)
	if tag != `"3"` {
		t.Errorf("ETag: got %s, want \"3\"", tag)
	}
	fields(t, got, map[string]any{"status": "cancelled", "version": float64(3), "customer_invoice_id": posted["customer_invoice_id"],
		"posting_date": posted["posting_date"]})
	c, _ := got["cancellation"].(map[string]any)
	fields(t, c, map[string]any{"cancellation_date": today, "reason": "Billed at the wrong price"})
	if n, _ := c["document_number"].(string); n == "" || n == got["document_number"] {
		t.Errorf("the cancellation's number: got %q, want one of its own", n)
	}
	memo, _ := e.Must(200, "GET", "/v1/customer-invoices/"+c["customer_invoice_id"].(string), "", "")
	fields(t, memo, map[string]any{"kind": "credit_memo", "status": "posted", "reference_invoice_id": posted["customer_invoice_id"],
		"source_document_id": c["id"], "source_document_number": c["document_number"], "invoice_date": today})
	for _, f := range []string{"net_amount", "tax_amount", "gross_amount"} {
		if a, b := amount(t, memo, f), amount(t, invoice, f); a != b {
			t.Errorf("the credit memo's %s: got %s, want the invoice's %s", f, a, b)
		}
	}
	if lines := e.entryLines(t, c["journal_entry_id"]); lines != "120000 -1690 - t; 400000 1000 A1 f; 530000 500 - f; 230000 190 A1 f" {
		t.Errorf("the credit memo's entry: got %s, want the invoice's lines reversed", lines)
	}
	want := append(slices.Clone(before), "-16.90 EUR")
	slices.Sort(want)
	if after := e.openItems(t); !slices.Equal(after, want) {
		t.Errorf("open items: got %v, want %v, the credit beside the invoice", after, want)
	}
	if q := e.quantities(t, order, "billed_quantity"); q != "0 EA, 0 EA" {
		t.Errorf("billed: got %s, want 0 EA, 0 EA", q)
	}
	t.Run("billed again", func(t *testing.T) {
		again, _ := e.In(t).Must(201, "POST", billings, bill(order, ""), "")
		if b := billed(t, again); b != billed(t, posted) {
			t.Errorf("the new document: got %s, want the cancelled one's %s", b, billed(t, posted))
		}
	})
	t.Run("audited and published", func(t *testing.T) {
		if n := e.audits(t, "sales.billing_document.cancel", doc); n != 1 {
			t.Errorf("audit records: got %d, want 1", n)
		}
		ev := e.events(t, "billing_document_cancelled", "billing_document_id", doc)
		if len(ev) != 1 {
			t.Fatalf("BillingDocumentCancelled: got %d, want 1", len(ev))
		}
		fields(t, ev[0], map[string]any{"status": "cancelled", "status_before": "posted", "version": float64(3), "cancellation_id": c["id"],
			"customer_invoice_id": c["customer_invoice_id"], "reference_invoice_id": posted["customer_invoice_id"], "cancellation_date": today})
		if g := mustJSON(t, ev[0]["gross_amount"]); g != `{"amount":"16.90","currency":"EUR"}` {
			t.Errorf("the event's gross: got %s", g)
		}
		apitest.NeverPublished(t, e.Tenant, "sales", "Northwind Traders")
	})
	t.Run("read back and listed", func(t *testing.T) {
		read, _ := e.In(t).Must(200, "GET", billings+"/"+doc, "", "")
		if a, b := mustJSON(t, read), mustJSON(t, got); a != b {
			t.Errorf("read back: got %s, want %s", a, b)
		}
		listed, _ := e.In(t).Must(200, "GET", billings+"?status=cancelled&sales_order_id="+order, "", "")
		if n := len(items(t, listed)); n != 1 {
			t.Errorf("cancelled documents of the order: got %d, want 1", n)
		}
	})
	t.Run("cancelled once", func(t *testing.T) {
		e.In(t).Problem(412, "request.version_mismatch", "POST", cancelBilling(doc), `{"reason": "Again"}`, `"2"`)
		e.In(t).Problem(409, "sales.invalid_status", "POST", cancelBilling(doc), `{"reason": "Again"}`, `"3"`)
	})
}

// TestCreatedBillingCancelled: a created document is cancelled without a
// credit memo, nothing posted, and its quantities are billable again.
func TestCreatedBillingCancelled(t *testing.T) {
	e := setup(t)
	order, doc, _ := e.billedOrder(t, []string{e.item(line{quantity: "2"})}, "1 2 EA")
	entries := e.journalEntries(t)
	got, _ := e.Must(200, "POST", cancelBilling(doc), `{"reason": "Entered too early"}`, `"1"`)
	fields(t, got, map[string]any{"status": "cancelled", "version": float64(2)})
	c, _ := got["cancellation"].(map[string]any)
	if _, ok := c["customer_invoice_id"]; ok {
		t.Errorf("the cancellation's credit memo: got %v, want none", c["customer_invoice_id"])
	}
	if n := e.journalEntries(t); n != entries {
		t.Errorf("journal entries: got %d, want %d, nothing posted", n, entries)
	}
	if q := e.quantities(t, order, "billed_quantity"); q != "0 EA" {
		t.Errorf("billed: got %s, want 0 EA", q)
	}
	e.In(t).Problem(409, "sales.invalid_status", "POST", posting(doc), "", `"2"`)
	ev := e.events(t, "billing_document_cancelled", "billing_document_id", doc)
	fields(t, ev[0], map[string]any{"status_before": "created", "customer_invoice_id": nil, "tax_amount": nil})
}

// TestBillingCancelRefused: a document a returns order or a credit memo
// request names, a date before its posting and a blank reason are
// refused, the document left as it was.
func TestBillingCancelRefused(t *testing.T) {
	e := setup(t)
	_, returned, _ := e.postedOrder(t, []string{e.item(line{quantity: "2"})}, "1 2 EA")
	e.Must(201, "POST", returnsOrders, returning(returned, "defective", "1 1 EA"), "")
	e.Problem(409, "sales.billing_document_referenced", "POST", cancelBilling(returned), `{"reason": "Wrong"}`, `"2"`)
	_, credited, _ := e.postedOrder(t, []string{e.item(line{})}, "1 1 EA")
	request, _ := e.Must(201, "POST", creditRequests, byAmount(credited, "Goodwill", "1 1.00"), "")
	e.Problem(409, "sales.billing_document_referenced", "POST", cancelBilling(credited), `{"reason": "Wrong"}`, `"2"`)
	e.Must(200, "POST", creditRequests+"/"+request["id"].(string)+":decline", `{"reason": "No"}`, `"1"`)
	e.Must(200, "POST", cancelBilling(credited), `{"reason": "Wrong"}`, `"2"`)
	_, doc, _ := e.postedOrder(t, []string{e.item(line{})}, "1 1 EA")
	cases := []struct{ name, body, code, field string }{
		{"a reason of white space", `{"reason": " "}`, "request.validation_failed", "reason"},
		{"a date before the posting", `{"reason": "Wrong", "cancellation_date": "2026-01-01"}`, "sales.invalid_cancellation_date", "cancellation_date"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := e.In(t).Problem(422, c.code, "POST", cancelBilling(doc), c.body, `"2"`)
			if f := field(got); f != c.field {
				t.Errorf("field: got %v, want %s", f, c.field)
			}
		})
	}
	t.Run("a date before the posting, after the billing", func(t *testing.T) {
		order, _ := e.deliveredOrder(t, []string{e.item(line{})}, "1 1 EA")
		early, _ := e.In(t).Must(201, "POST", billings, bill(order, `, "billing_date": "2026-01-02"`), "")
		e.In(t).Must(200, "POST", posting(early["id"].(string)), "", `"1"`)
		got := e.In(t).Problem(422, "sales.invalid_cancellation_date", "POST", cancelBilling(early["id"].(string)),
			`{"reason": "Wrong", "cancellation_date": "2026-01-02"}`, `"2"`)
		if f := field(got); f != "cancellation_date" {
			t.Errorf("field: got %v, want cancellation_date", f)
		}
	})
	t.Run("a customer blocked for posting", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE partner.customer_company SET is_posting_blocked = true, version = version + 1 WHERE tenant_id = $1 AND partner_id = $2`, e.Tenant.ID, e.customer)
		got := e.In(t).Problem(422, "receivables.customer_not_usable", "POST", cancelBilling(doc), `{"reason": "Wrong"}`, `"2"`)
		pgtest.Exec(t, `UPDATE partner.customer_company SET is_posting_blocked = false, version = version + 1 WHERE tenant_id = $1 AND partner_id = $2`, e.Tenant.ID, e.customer)
		if d, _ := got["detail"].(string); !strings.HasPrefix(d, "Billing document ") {
			t.Errorf("detail: got %q, want it to lead with the document", d)
		}
	})
	t.Run("a credit memo refused by the ledger", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE tenant_id = $1`, e.Tenant.ID)
		e.In(t).Problem(409, "ledger.period_not_open", "POST", cancelBilling(doc), `{"reason": "Wrong"}`, `"2"`)
		pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'open', version = version + 1 WHERE tenant_id = $1`, e.Tenant.ID)
	})
	read, _ := e.Must(200, "GET", billings+"/"+doc, "", "")
	fields(t, read, map[string]any{"status": "posted", "version": float64(2)})
}

// TestBillingCancelsRacing: two requests to cancel one document wait for
// one another: one cancels it, the other meets another version.
func TestBillingCancelsRacing(t *testing.T) {
	e := setup(t)
	_, doc, _ := e.postedOrder(t, []string{e.item(line{code: "A1"})}, "1 1 EA")
	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Go(func() {
			codes[i] = e.API.Do(apitest.Req{Method: "POST", Path: cancelBilling(doc), Body: `{"reason": "Twice"}`, IfMatch: `"2"`, Tenant: e.Tenant.ID,
				As: e.Tenant.Human, Key: apitest.NewKey()}).Code
		})
	}
	wg.Wait()
	slices.Sort(codes)
	if !slices.Equal(codes, []int{200, 412}) {
		t.Errorf("statuses: got %v, want one 200 and one 412", codes)
	}
	var memos int
	pgtest.Scan(t, `SELECT count(*) FROM sales.billing_cancellation WHERE tenant_id = $1 AND billing_document_id = $2`, []any{e.Tenant.ID, doc}, &memos)
	if memos != 1 {
		t.Errorf("cancellations: got %d, want 1", memos)
	}
}

// TestOrdersPublishedCancellations: a cancelled posted billing reads in
// the order's published billings as it posted and, on the cancellation's
// date, as its negative, so the billings to a day add up to what is
// billed then; net revenue by segment counts both.
func TestOrdersPublishedCancellations(t *testing.T) {
	e := setup(t)
	order, doc, today := e.postedOrder(t, []string{e.item(line{quantity: "2"})}, "1 2 EA")
	cancelled, _ := e.Must(200, "POST", cancelBilling(doc), `{"reason": "Wrong"}`, `"2"`)
	c := cancelled["cancellation"].(map[string]any)
	if err := e.inRequest(t, func(ctx context.Context) error {
		o, _, err := sales.Orders{}.Order(ctx, uuid.MustParse(order))
		if err != nil {
			return err
		}
		var got []string
		for _, b := range o.Items[0].Billings {
			got = append(got, b.DocumentNumber+" "+b.Net.Plain(2))
		}
		if want := []string{cancelled["document_number"].(string) + " 20.00", c["document_number"].(string) + " -20.00"}; !slices.Equal(got, want) {
			t.Errorf("billings: got %v, want %v", got, want)
		}
		day := instant(t, today+"T00:00:00Z")
		segments, _, err := sales.Orders{}.Segments(ctx, sales.SegmentQuery{LegalEntityID: e.DE, From: day, To: day, Currency: "EUR"})
		if err != nil {
			return err
		}
		for _, s := range segments {
			if s.Net != 0 || s.Gross != 0 {
				t.Errorf("segment %v: got gross %d, net %d, want 0, billed and cancelled on the day", s.CustomerID, s.Gross, s.Net)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestCancelledBillingKeepsItsPrice: the path a wrong price takes. The
// billing is cancelled, the order's price corrected and the item billed
// again at it; the cancelled document still reads at the price it was
// billed at.
func TestCancelledBillingKeepsItsPrice(t *testing.T) {
	e := setup(t)
	order, doc, _ := e.billedOrder(t, []string{e.item(line{quantity: "2"})}, "1 2 EA")
	e.Must(200, "POST", cancelBilling(doc), `{"reason": "Wrong price"}`, `"1"`)
	e.Must(200, "POST", changing(order), changeBody("", itemChange("1", "9.00")), e.version(t, order))
	again, _ := e.Must(201, "POST", billings, bill(order, ""), "")
	if b := billed(t, again); !strings.Contains(b, "18.00") {
		t.Errorf("billed again: got %s, want 18.00 at the corrected price", b)
	}
	read, _ := e.Must(200, "GET", billings+"/"+doc, "", "")
	if p := amount(t, items(t, read)[0], "unit_price"); p != "10.00 EUR" {
		t.Errorf("the cancelled document's price: got %s, want 10.00 EUR, as billed", p)
	}
}
