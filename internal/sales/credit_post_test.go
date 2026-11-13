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
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/sales"
)

// creditPosting is the path that posts the credit memo request.
func creditPosting(id string) string { return creditRequests + "/" + id + ":post" }

// approvedRequest creates the request from the body and approves it; it
// returns the approved request.
func (e env) approvedRequest(t *testing.T, body string) map[string]any {
	t.Helper()
	created, _ := e.Must(201, "POST", creditRequests, body, "")
	approved, _ := e.Must(200, "POST", creditRequests+"/"+created["id"].(string)+":release", "", `"1"`)
	return approved
}

// creditMemoOf reads the customer credit memo a posted request names.
func (e env) creditMemoOf(t *testing.T, posted map[string]any) map[string]any {
	t.Helper()
	memo, _ := e.Must(200, "GET", "/v1/customer-invoices/"+posted["customer_invoice_id"].(string), "", "")
	return memo
}

// openItems are the customer's open items in company 2000, "amount
// currency" each, sorted: items due on one date have no defined order.
func (e env) openItems(t *testing.T) []string {
	t.Helper()
	got, _ := e.Must(200, "GET", fmt.Sprintf("/v1/open-items?legal_entity_id=%s&partner_id=%s", e.DE, e.customer), "", "")
	var out []string
	for _, it := range items(t, got) {
		out = append(out, amount(t, it, "amount"))
	}
	slices.Sort(out)
	return out
}

// TestCreditMemoRequestPosted: an approved request posts once as a
// customer credit memo naming the billing document's invoice and the
// request, its items the request's one for one on the billing items'
// accounts and tax codes; its net is the request's and its tax the share
// of the tax the invoice's item bore; the entry credits the customer and
// debits revenue and output tax; the customer's open items fall by its
// gross; the billing item shows the amount credited; audited and
// published (T85d acceptance).
func TestCreditMemoRequestPosted(t *testing.T) {
	e := setup(t)
	_, doc, today := e.postedOrder(t, []string{e.item(line{quantity: "3", price: "10.00", priceUnit: 3, code: "A1"}), e.item(line{price: "5.00", account: e.other})},
		"1 3 EA", "2 1 EA")
	billing, _ := e.Must(200, "GET", billings+"/"+doc, "", "")
	invoice := billing["customer_invoice_id"].(string)
	returns := e.returnedOrder(t, doc, "1 3 EA")
	e.receive(t, returns, today, "1 1 EA")
	approved := e.approvedRequest(t, forReturns(doc, returns))
	id := approved["id"].(string)
	before := e.openItems(t)
	rec := e.API.Do(apitest.Req{Method: "POST", Path: creditPosting(id), Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey(), IfMatch: `"2"`})
	if rec.Code != 200 {
		t.Fatalf("status: got %d, want 200: %s", rec.Code, rec.Body)
	}
	if tag := rec.Header().Get("ETag"); tag != `"3"` {
		t.Errorf("ETag: got %s, want \"3\"", tag)
	}
	posted := apitest.JSON(t, rec)
	fields(t, posted, map[string]any{"status": "posted", "version": float64(3), "posting_date": today})
	memo := e.creditMemoOf(t, posted)
	fields(t, memo, map[string]any{"kind": "credit_memo", "status": "posted", "reference_invoice_id": invoice, "source_document_id": id,
		"source_document_number": posted["document_number"], "document_number": posted["customer_invoice_number"], "invoice_date": today})
	for f, want := range map[string]string{"net_amount": "3.33 EUR", "tax_amount": "0.63 EUR", "gross_amount": "3.96 EUR"} {
		if a := amount(t, memo, f); a != want {
			t.Errorf("the credit memo's %s: got %s, want %s", f, a, want)
		}
	}
	if its := invoiceItems(t, memo); its != "400000 3.33 EUR A1 = 0.63 EUR" {
		t.Errorf("the credit memo's items: got %s, want 400000 3.33 EUR A1 = 0.63 EUR", its)
	}
	t.Run("the entry and the open items", func(t *testing.T) {
		if lines := e.entryLines(t, posted["journal_entry_id"]); lines != "120000 -396 - t; 400000 333 A1 f; 230000 63 A1 f" {
			t.Errorf("the entry's lines: got %s, want the customer credited and revenue and tax debited", lines)
		}
		want := append(slices.Clone(before), "-3.96 EUR")
		slices.Sort(want)
		if after := e.openItems(t); !slices.Equal(after, want) {
			t.Errorf("the customer's open items: got %v, want %v, the credit of -3.96 EUR beside what was open", after, want)
		}
	})
	t.Run("the billing item shows what is credited", func(t *testing.T) {
		read, _ := e.In(t).Must(200, "GET", billings+"/"+doc, "", "")
		var credited []string
		for _, it := range items(t, read) {
			credited = append(credited, amount(t, it, "credited_amount"))
		}
		if got := fmt.Sprint(credited); got != "[3.33 EUR 0.00 EUR]" {
			t.Errorf("credited: got %s, want [3.33 EUR 0.00 EUR]", got)
		}
	})
	t.Run("read back", func(t *testing.T) {
		read, _ := e.In(t).Must(200, "GET", creditRequests+"/"+id, "", "")
		if a, b := mustJSON(t, read), mustJSON(t, posted); a != b {
			t.Errorf("read back: got %s, want %s", a, b)
		}
		if read["journal_entry_number"] == nil || read["journal_entry_year"] == nil {
			t.Errorf("the entry's number and year: got %v and %v, want both", read["journal_entry_number"], read["journal_entry_year"])
		}
	})
	t.Run("audited and published", func(t *testing.T) {
		if n := e.audits(t, "sales.credit_memo_request.post", id); n != 1 {
			t.Errorf("audit records: got %d, want 1", n)
		}
		ev := e.events(t, "credit_memo_request_posted", "credit_memo_request_id", id)
		if len(ev) != 1 {
			t.Fatalf("CreditMemoRequestPosted: got %d, want 1", len(ev))
		}
		fields(t, ev[0], map[string]any{"status": "posted", "status_before": "approved", "version": float64(3), "reference_invoice_id": invoice,
			"customer_invoice_id": posted["customer_invoice_id"], "returns_order_id": returns, "posting_date": today})
		if got := mustJSON(t, []any{ev[0]["net_amount"], ev[0]["tax_amount"], ev[0]["gross_amount"]}); got != `[{"amount":"3.33","currency":"EUR"},{"amount":"0.63","currency":"EUR"},{"amount":"3.96","currency":"EUR"}]` {
			t.Errorf("the event's amounts: got %s", got)
		}
		apitest.NeverPublished(t, e.Tenant, "sales", "Northwind Traders")
	})
	t.Run("posted once", func(t *testing.T) {
		e.In(t).Problem(412, "request.version_mismatch", "POST", creditPosting(id), "", `"2"`)
		e.In(t).Problem(409, "sales.invalid_status", "POST", creditPosting(id), "", `"3"`)
	})
}

// TestCreditsGiveBackWhatWasBilled: the money test. The credits of a
// billing item's whole quantity, in one request or in parts, give back
// its net amount and the tax its invoice item bore, to the minor unit.
func TestCreditsGiveBackWhatWasBilled(t *testing.T) {
	e := setup(t)
	// 3 EA at 10.00 per 3 is 10.00 with 1.90 of A1; a third is 3.33 and 0.63, which three times is not what was billed.
	lines := []string{e.item(line{quantity: "3", price: "10.00", priceUnit: 3, code: "A1"})}
	t.Run("in parts", func(t *testing.T) {
		_, doc, today := e.postedOrder(t, lines, "1 3 EA")
		returns := e.returnedOrder(t, doc, "1 3 EA")
		var memos []string
		for range 3 {
			e.receive(t, returns, today, "1 1 EA")
			approved := e.approvedRequest(t, forReturns(doc, returns))
			posted, _ := e.Must(200, "POST", creditPosting(approved["id"].(string)), "", `"2"`)
			memo := e.creditMemoOf(t, posted)
			memos = append(memos, amount(t, memo, "net_amount")+" + "+amount(t, memo, "tax_amount"))
		}
		if got := fmt.Sprint(memos); got != "[3.33 EUR + 0.63 EUR 3.34 EUR + 0.64 EUR 3.33 EUR + 0.63 EUR]" {
			t.Errorf("the credit memos: got %s, want 10.00 and 1.90 in all", got)
		}
	})
	t.Run("at once", func(t *testing.T) {
		_, doc, _ := e.postedOrder(t, lines, "1 3 EA")
		approved := e.approvedRequest(t, byAmount(doc, "Goodwill", "1 10.00"))
		posted, _ := e.Must(200, "POST", creditPosting(approved["id"].(string)), "", `"2"`)
		memo := e.creditMemoOf(t, posted)
		if got := amount(t, memo, "net_amount") + " + " + amount(t, memo, "tax_amount"); got != "10.00 EUR + 1.90 EUR" {
			t.Errorf("the credit memo: got %s, want 10.00 EUR + 1.90 EUR, what was billed", got)
		}
	})
}

// TestCreditMemoRequestPostRefusals: only an approved request posts; what
// receivables or the ledger refuses leaves the request approved and
// nothing posted, naming the request's own fields.
func TestCreditMemoRequestPostRefusals(t *testing.T) {
	e := setup(t)
	_, doc, _ := e.postedOrder(t, []string{e.item(line{quantity: "3", code: "A1"})}, "1 3 EA")
	billing, _ := e.Must(200, "GET", billings+"/"+doc, "", "")
	open, _ := e.Must(201, "POST", creditRequests, byAmount(doc, "Goodwill", "1 1.00"), "")
	e.Problem(409, "sales.invalid_status", "POST", creditPosting(open["id"].(string)), "", `"1"`)
	approved := e.approvedRequest(t, byAmount(doc, "Goodwill", "1 20.00"))
	id := approved["id"].(string)
	t.Run("a customer blocked for posting", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE partner.customer_company SET is_posting_blocked = true, version = version + 1 WHERE tenant_id = $1 AND partner_id = $2`, e.Tenant.ID, e.customer)
		got := e.In(t).Problem(422, "receivables.customer_not_usable", "POST", creditPosting(id), "", `"2"`)
		pgtest.Exec(t, `UPDATE partner.customer_company SET is_posting_blocked = false, version = version + 1 WHERE tenant_id = $1 AND partner_id = $2`, e.Tenant.ID, e.customer)
		if f := field(got); f != "customer_id" {
			t.Errorf("field: got %v, want customer_id", f)
		}
		if d, _ := got["detail"].(string); !strings.HasPrefix(d, "Credit memo request "+approved["document_number"].(string)) {
			t.Errorf("detail: got %q, want it to lead with the request", d)
		}
	})
	t.Run("a posting date in a closed period", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE tenant_id = $1 AND fiscal_year = 2026 AND period = 2`, e.Tenant.ID)
		e.In(t).Problem(409, "ledger.period_not_open", "POST", creditPosting(id), `{"posting_date": "2026-02-12"}`, `"2"`)
	})
	t.Run("more than is left of the invoice's gross", func(t *testing.T) {
		// A credit memo a person enters for the invoice leaves 0.01 of its gross; entering it takes receivables' permissions.
		pgtest.Exec(t, `INSERT INTO identity.role_permission (tenant_id, id, role_id, permission_code, valid_from, created_by)
			SELECT tenant_id, gen_random_uuid(), role_id, x, current_date - 1, created_by FROM identity.role_permission,
				unnest(ARRAY['receivables.invoice.write', 'receivables.invoice.post']) AS x
			WHERE tenant_id = $1 AND permission_code = 'sales.billing.post'`, e.Tenant.ID)
		memo, _ := e.In(t).Must(201, "POST", "/v1/customer-invoices", fmt.Sprintf(`{"legal_entity_id": %q, "customer_id": %q, "kind": "credit_memo",
			"reference_invoice_id": %q, "invoice_date": %q, "currency": "EUR", "items": [{"account_id": %q, "net_amount": {"amount": "35.69", "currency": "EUR"}}]}`,
			e.DE, e.customer, billing["customer_invoice_id"], billing["billing_date"], e.revenue), "")
		e.In(t).Must(200, "POST", "/v1/customer-invoices/"+memo["id"].(string)+":post", "", `"1"`)
		got := e.In(t).Problem(422, "receivables.credit_exceeds_invoice", "POST", creditPosting(id), "", `"2"`)
		if f := field(got); f != "net_amount" {
			t.Errorf("field: got %v, want net_amount", f)
		}
	})
	read, _ := e.Must(200, "GET", creditRequests+"/"+id, "", "")
	fields(t, read, map[string]any{"status": "approved", "version": float64(2), "customer_invoice_id": nil})
	var memos int
	pgtest.Scan(t, `SELECT count(*) FROM receivables.customer_invoice WHERE tenant_id = $1 AND source_document_id = $2`, []any{e.Tenant.ID, id}, &memos)
	if memos != 0 {
		t.Errorf("credit memos of the request: got %d, want none", memos)
	}
	t.Run("without the permission to post", func(t *testing.T) {
		pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = 'sales.billing.post'`, e.Tenant.ID)
		e.In(t).Problem(403, "identity.permission_missing", "POST", creditPosting(id), "", `"2"`)
	})
}

// TestCreditMemoRequestPostIdempotent: a replay answers the same posted
// request and posts nothing more.
func TestCreditMemoRequestPostIdempotent(t *testing.T) {
	e := setup(t)
	_, doc, _ := e.postedOrder(t, []string{e.item(line{quantity: "3", code: "A1"})}, "1 3 EA")
	id := e.approvedRequest(t, byAmount(doc, "Goodwill", "1 1.00"))["id"].(string)
	req := apitest.Req{Method: "POST", Path: creditPosting(id), Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey(), IfMatch: `"2"`}
	first, second := e.API.Do(req), e.API.Do(req)
	for i, rec := range []*httptest.ResponseRecorder{first, second} {
		if rec.Code != 200 {
			t.Fatalf("status of call %d: got %d, want 200: %s", i+1, rec.Code, rec.Body)
		}
	}
	if a, b := mustJSON(t, apitest.JSON(t, first)), mustJSON(t, apitest.JSON(t, second)); a != b {
		t.Errorf("replay: got %s, want the first answer %s", b, a)
	}
	var memos int
	pgtest.Scan(t, `SELECT count(*) FROM receivables.customer_invoice WHERE tenant_id = $1 AND source_document_id = $2`, []any{e.Tenant.ID, id}, &memos)
	if memos != 1 {
		t.Errorf("credit memos after the replay: got %d, want 1", memos)
	}
}

// TestCreditMemoRequestPostsRacing: two requests of one billing item
// posted at once wait for one another on the sales order's row lock, so
// the second's tax counts the first's credit.
func TestCreditMemoRequestPostsRacing(t *testing.T) {
	e := setup(t)
	_, doc, _ := e.postedOrder(t, []string{e.item(line{quantity: "3", price: "10.00", priceUnit: 3, code: "A1"})}, "1 3 EA")
	ids := []string{e.approvedRequest(t, byAmount(doc, "Goodwill", "1 3.33"))["id"].(string), e.approvedRequest(t, byAmount(doc, "Goodwill", "1 6.67"))["id"].(string)}
	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Go(func() {
			codes[i] = e.API.Do(apitest.Req{Method: "POST", Path: creditPosting(ids[i]), Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey(), IfMatch: `"2"`}).Code
		})
	}
	wg.Wait()
	if !slices.Equal(codes, []int{200, 200}) {
		t.Fatalf("statuses: got %v, want 200 twice", codes)
	}
	var tax int64
	pgtest.Scan(t, `SELECT sum(tax_amount)::bigint FROM receivables.customer_invoice WHERE tenant_id = $1 AND source_document_id = ANY($2::uuid[])`,
		[]any{e.Tenant.ID, ids}, &tax)
	if tax != 190 {
		t.Errorf("the credit memos' tax in all: got %d, want 190, what the invoice bore", tax)
	}
}

// TestOrdersPublishedCredits: sales.Orders gives each order item its
// posted credits; a request not posted is not among them.
func TestOrdersPublishedCredits(t *testing.T) {
	e := setup(t)
	order, doc, _ := e.postedOrder(t, []string{e.item(line{quantity: "2"})}, "1 2 EA")
	approved := e.approvedRequest(t, byAmount(doc, "Goodwill", "1 5.00"))
	credits := func() []sales.ItemCredit {
		var out []sales.ItemCredit
		err := e.inRequest(t, func(ctx context.Context) error {
			o, _, err := sales.Orders{}.Order(ctx, uuid.MustParse(order))
			if err == nil {
				out = o.Items[0].Credits
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if c := credits(); c == nil || len(c) != 0 {
		t.Errorf("credits of a request not posted: got %v, want an empty list read", c)
	}
	posted, _ := e.Must(200, "POST", creditPosting(approved["id"].(string)), `{"posting_date": "2026-03-12"}`, `"2"`)
	c := credits()
	if len(c) != 1 {
		t.Fatalf("credits once posted: got %d, want 1", len(c))
	}
	got := fmt.Sprintf("%s %s %s %d on %s, credit memo %s, entry %s", c[0].RequestID, c[0].DocumentNumber, c[0].BillingDocumentID, c[0].Net,
		c[0].PostingDate.Format(time.RFC3339), c[0].InvoiceID, c[0].EntryID)
	want := fmt.Sprintf("%s %s %s 500 on 2026-03-12T00:00:00Z, credit memo %s, entry %s", posted["id"], posted["document_number"], doc,
		posted["customer_invoice_id"], posted["journal_entry_id"])
	if got != want {
		t.Errorf("the credit: got %s, want %s", got, want)
	}
}
