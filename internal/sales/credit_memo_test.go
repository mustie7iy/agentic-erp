package sales_test

import (
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

const creditRequests = "/v1/credit-memo-requests"

// forReturns is the JSON of a credit memo request for the returns order's
// received quantities.
func forReturns(doc, returns string) string {
	return fmt.Sprintf(`{"billing_document_id": %q, "returns_order_id": %q}`, doc, returns)
}

// byAmount is the JSON of a credit memo request by amount with the reason;
// each item is "the billing item's number, the amount in EUR".
func byAmount(doc, reason string, items ...string) string {
	parts := make([]string, len(items))
	for i, it := range items {
		f := strings.Split(it, " ")
		parts[i] = fmt.Sprintf(`{"billing_item_no": %s, "net_amount": {"amount": %q, "currency": "EUR"}}`, f[0], f[1])
	}
	return fmt.Sprintf(`{"billing_document_id": %q, "reason": %q, "items": [%s]}`, doc, reason, strings.Join(parts, ", "))
}

// receive confirms "item number, quantity, unit" of the returns order as
// received today.
func (e env) receive(t *testing.T, returns, today string, items ...string) {
	t.Helper()
	e.Must(201, "POST", receipts(returns), received(today, items...), "")
}

// credits are the request's items: "billing item: quantity = amount", the
// quantity "-" on a request by amount.
func credits(t *testing.T, r map[string]any) string {
	t.Helper()
	var out []string
	for _, it := range items(t, r) {
		q := "-"
		if _, ok := it["quantity"]; ok {
			q = quantity(t, it, "quantity")
		}
		out = append(out, fmt.Sprintf("%v: %s = %s", it["billing_item_no"], q, amount(t, it, "net_amount")))
	}
	return strings.Join(out, ", ")
}

// requestsCount counts the tenant's credit memo requests.
func (e env) requestsCount(t *testing.T) int {
	t.Helper()
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM sales.credit_memo_request WHERE tenant_id = $1`, []any{e.Tenant.ID}, &n)
	return n
}

// TestCreditMemoRequestForReturns: a request for a returns order takes
// what its items received and no request took, at the billed amount of
// that quantity; it reads back naming what it credits; the requests for
// an item's whole billed quantity add up to the billed amount to the
// minor unit, the last taking the remainder; nothing else changes and
// nothing is posted (T85c acceptance, the money test).
func TestCreditMemoRequestForReturns(t *testing.T) {
	e := setup(t)
	// 3 EA at 10.00 per 3 is 10.00, which no third divides; 2 KG at 2.50 is 5.00.
	order, doc, today := e.postedOrder(t, []string{e.item(line{quantity: "3", price: "10.00", priceUnit: 3, code: "A1", key: "BRACKET-M8"}),
		e.item(line{quantity: "2", unit: "KG", price: "2.50", account: e.other})}, "1 3 EA", "2 2 KG")
	returns := e.returnedOrder(t, doc, "1 3 EA", "2 1 KG")
	e.receive(t, returns, today, "1 1 EA", "2 1 KG")
	sold, soldTag := e.Must(200, "GET", "/v1/sales-orders/"+order, "", "")
	_, returnsTag := e.Must(200, "GET", returnsOrders+"/"+returns, "", "")
	billing, _ := e.Must(200, "GET", billings+"/"+doc, "", "")
	entries := e.journalEntries(t)
	rec := e.API.Do(apitest.Req{Method: "POST", Path: creditRequests, Body: forReturns(doc, returns), Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey()})
	if rec.Code != 201 {
		t.Fatalf("status: got %d, want 201: %s", rec.Code, rec.Body)
	}
	got := apitest.JSON(t, rec)
	id := got["id"].(string)
	if loc := rec.Header().Get("Location"); loc != creditRequests+"/"+id {
		t.Errorf("Location: got %q, want %s/%s", loc, creditRequests, id)
	}
	fields(t, got, map[string]any{"document_number": "CR100000", "status": "open", "version": float64(1), "legal_entity_id": e.DE.String(),
		"company_code": "2000", "customer_id": e.customer.String(), "customer_number": "BP70000001", "customer_name": "Northwind Traders",
		"billing_document_id": doc, "billing_document_number": billing["document_number"], "sales_order_id": order,
		"sales_order_number": sold["document_number"], "returns_order_id": returns, "returns_order_number": "RO100000", "request_date": today,
		"reason": nil, "currency": "EUR", "created_by": e.Tenant.Human.ID.String()})
	if c := credits(t, got); c != "1: 1 EA = 3.33 EUR, 2: 1 KG = 2.50 EUR" {
		t.Errorf("items: got %s, want 1: 1 EA = 3.33 EUR, 2: 1 KG = 2.50 EUR", c)
	}
	if a := amount(t, got, "net_amount"); a != "5.83 EUR" {
		t.Errorf("net value: got %s, want 5.83 EUR", a)
	}
	its := items(t, got)
	fields(t, its[0], map[string]any{"item_no": float64(1), "order_item_no": float64(1), "item_key": "BRACKET-M8", "description": "Machined bracket",
		"account_id": e.revenue.String(), "account_number": "400000", "tax_code": "A1"})
	fields(t, its[1], map[string]any{"item_no": float64(2), "account_id": e.other.String(), "account_number": "530000", "tax_code": nil})
	if a, q := amount(t, its[0], "billed_amount"), quantity(t, its[0], "billed_quantity"); a+" "+q != "10.00 EUR 3 EA" {
		t.Errorf("what the first billing item billed: got %s %s, want 10.00 EUR 3 EA", a, q)
	}
	t.Run("read back", func(t *testing.T) {
		read, tag := e.In(t).Must(200, "GET", creditRequests+"/"+id, "", "")
		if a, b := mustJSON(t, read), mustJSON(t, got); a != b {
			t.Errorf("read back: got %s, want %s", a, b)
		}
		if tag != `"1"` {
			t.Errorf("ETag: got %s, want \"1\"", tag)
		}
	})
	t.Run("audited and published", func(t *testing.T) {
		if n := e.audits(t, "sales.credit_memo_request.create", id); n != 1 {
			t.Errorf("audit records: got %d, want 1", n)
		}
		want := map[string]any{"credit_memo_request_id": id, "document_number": "CR100000", "legal_entity_id": e.DE.String(), "legal_entity_code": "2000",
			"customer_id": e.customer.String(), "customer_number": "BP70000001", "billing_document_id": doc,
			"billing_document_number": billing["document_number"], "sales_order_id": order, "sales_order_number": sold["document_number"],
			"returns_order_id": returns, "returns_order_number": "RO100000", "request_date": today,
			"net_amount": map[string]any{"amount": "5.83", "currency": "EUR"}, "status": "open", "status_before": nil, "version": float64(1)}
		if a, b := mustJSON(t, e.events(t, "credit_memo_request_changed", "credit_memo_request_id", id)), mustJSON(t, []map[string]any{want}); a != b {
			t.Errorf("CreditMemoRequestChanged: got %s, want %s", a, b)
		}
		apitest.NeverPublished(t, e.Tenant, "sales", "Northwind Traders")
	})
	t.Run("nothing else changes", func(t *testing.T) {
		if _, tag := e.In(t).Must(200, "GET", "/v1/sales-orders/"+order, "", ""); tag != soldTag {
			t.Errorf("the sales order's ETag: got %s, want %s", tag, soldTag)
		}
		if _, tag := e.In(t).Must(200, "GET", returnsOrders+"/"+returns, "", ""); tag != returnsTag {
			t.Errorf("the returns order's ETag: got %s, want %s", tag, returnsTag)
		}
	})
	t.Run("nothing more received, nothing to credit", func(t *testing.T) {
		refused := e.In(t).Problem(422, "sales.nothing_to_credit", "POST", creditRequests, forReturns(doc, returns), "")
		if f := field(refused); f != "returns_order_id" {
			t.Errorf("field: got %v, want returns_order_id", f)
		}
	})
	t.Run("the rest as it arrives adds up to what was billed", func(t *testing.T) {
		for i, want := range []string{"1: 1 EA = 3.34 EUR", "1: 1 EA = 3.33 EUR"} {
			e.receive(t, returns, today, "1 1 EA")
			next, _ := e.In(t).Must(201, "POST", creditRequests, forReturns(doc, returns), "")
			if c := credits(t, next); c != want {
				t.Errorf("request %d: got %s, want %s", i+2, c, want)
			}
		}
		var total string
		pgtest.Scan(t, `SELECT string_agg(amount::text, ' ' ORDER BY billing_item_no) FROM (SELECT billing_item_no, sum(net_amount) AS amount
			FROM sales.credit_memo_request_item WHERE tenant_id = $1 AND billing_document_id = $2 GROUP BY billing_item_no) x`, []any{e.Tenant.ID, doc}, &total)
		if total != "1000 250" {
			t.Errorf("credited of each item in all: got %s, want 1000 250, the first item's billed amount in full and the half returned of the second", total)
		}
	})
	if n := e.journalEntries(t); n != entries {
		t.Errorf("journal entries: got %d, want %d as before the requests (nothing is posted)", n, entries)
	}
}

// TestCreditMemoRequestLeavesAnAmountOfZero: a received quantity whose
// billed amount rounds to zero is left for a later request, as billing
// leaves one.
func TestCreditMemoRequestLeavesAnAmountOfZero(t *testing.T) {
	e := setup(t)
	// 3 EA at 0.01 per 3 is 0.01 in all: a third of it rounds to nothing, two thirds to 0.01.
	_, doc, today := e.postedOrder(t, []string{e.item(line{quantity: "3", price: "0.01", priceUnit: 3})}, "1 3 EA")
	returns := e.returnedOrder(t, doc, "1 3 EA")
	e.receive(t, returns, today, "1 1 EA")
	e.Problem(422, "sales.nothing_to_credit", "POST", creditRequests, forReturns(doc, returns), "")
	e.receive(t, returns, today, "1 1 EA")
	got, _ := e.Must(201, "POST", creditRequests, forReturns(doc, returns), "")
	if c := credits(t, got); c != "1: 2 EA = 0.01 EUR" {
		t.Errorf("items: got %s, want 1: 2 EA = 0.01 EUR", c)
	}
	e.receive(t, returns, today, "1 1 EA")
	e.Problem(422, "sales.nothing_to_credit", "POST", creditRequests, forReturns(doc, returns), "")
}

// TestCreditMemoRequestByAmount: a request by amount names billing items
// with positive net amounts and a reason; per billing item the requests
// not rejected never exceed its billed amount, a request for a returns
// order included; a rejected request frees what it asked for.
func TestCreditMemoRequestByAmount(t *testing.T) {
	e := setup(t)
	_, doc, today := e.postedOrder(t, []string{e.item(line{quantity: "3", code: "A1"}), e.item(line{quantity: "1", price: "5.00"})}, "1 3 EA", "2 1 EA")
	got, _ := e.Must(201, "POST", creditRequests, byAmount(doc, "Price agreed after delivery", "2 1.50", "1 20.00"), "")
	fields(t, got, map[string]any{"reason": "Price agreed after delivery", "returns_order_id": nil, "status": "open"})
	if c := credits(t, got); c != "2: - = 1.50 EUR, 1: - = 20.00 EUR" {
		t.Errorf("items: got %s, want 2: - = 1.50 EUR, 1: - = 20.00 EUR", c)
	}
	refused := e.Problem(422, "sales.exceeds_billed_amount", "POST", creditRequests, byAmount(doc, "More", "2 3.51"), "")
	if f := field(refused); f != "items[0].net_amount.amount" {
		t.Errorf("field: got %v, want items[0].net_amount.amount", f)
	}
	want := "Item 2 of billing document " + got["billing_document_number"].(string) + " billed 5.00 EUR, of which its credit memo requests ask for 1.50; 3.51 more is above what it billed."
	if d, _ := refused["detail"].(string); d != want {
		t.Errorf("detail: got %q, want %q", d, want)
	}
	e.Must(201, "POST", creditRequests, byAmount(doc, "The rest", "2 3.50"), "")
	t.Run("a request for a returns order counts the requests by amount", func(t *testing.T) {
		returns := e.returnedOrder(t, doc, "1 3 EA")
		e.receive(t, returns, today, "1 3 EA")
		refused := e.In(t).Problem(422, "sales.exceeds_billed_amount", "POST", creditRequests, forReturns(doc, returns), "")
		if f := field(refused); f != "returns_order_id" {
			t.Errorf("field: got %v, want returns_order_id", f)
		}
		e.In(t).Must(200, "POST", creditRequests+"/"+got["id"].(string)+":decline", `{"reason": "Agreed as a return instead"}`, `"1"`)
		freed, _ := e.In(t).Must(201, "POST", creditRequests, forReturns(doc, returns), "")
		if c := credits(t, freed); c != "1: 3 EA = 30.00 EUR" {
			t.Errorf("after the rejection: got %s, want 1: 3 EA = 30.00 EUR", c)
		}
	})
}

// TestCreditMemoRequestRefusals: every refusal names the request's field
// and creates nothing.
func TestCreditMemoRequestRefusals(t *testing.T) {
	e := setup(t)
	_, doc, _ := e.postedOrder(t, []string{e.item(line{quantity: "3"})}, "1 3 EA")
	_, created, _ := e.billedOrder(t, []string{e.item(line{})}, "1 1 EA")
	_, other, _ := e.postedOrder(t, []string{e.item(line{})}, "1 1 EA")
	otherReturns := e.returnedOrder(t, other, "1 1 EA")
	both := strings.Replace(forReturns(doc, otherReturns), "}", `, "items": [{"billing_item_no": 1, "net_amount": {"amount": "1.00", "currency": "EUR"}}]}`, 1)
	cases := []struct {
		name, body string
		status     int
		code       string
		field      any
	}{
		{"an unknown billing document", byAmount(uuid.NewString(), "x", "1 1.00"), 422, "request.reference_not_found", "billing_document_id"},
		{"a billing document not posted", byAmount(created, "x", "1 1.00"), 409, "sales.billing_document_not_posted", nil},
		{"neither a returns order nor items", fmt.Sprintf(`{"billing_document_id": %q}`, doc), 422, "sales.credit_memo_request_basis", "returns_order_id"},
		{"both", both, 422, "sales.credit_memo_request_basis", "returns_order_id"},
		{"items without a reason", strings.Replace(byAmount(doc, "x", "1 1.00"), `"reason": "x", `, "", 1), 422, "request.validation_failed", "reason"},
		{"a reason of white space", byAmount(doc, "  ", "1 1.00"), 422, "request.validation_failed", "reason"},
		{"an unknown returns order", forReturns(doc, uuid.NewString()), 422, "request.reference_not_found", "returns_order_id"},
		{"another document's returns order", forReturns(doc, otherReturns), 422, "sales.returns_order_of_other_document", "returns_order_id"},
		{"an unknown billing item", byAmount(doc, "x", "2 1.00"), 422, "sales.unknown_billing_item", "items[0].billing_item_no"},
		{"an item named twice", byAmount(doc, "x", "1 1.00", "1 1.00"), 422, "sales.item_repeated", "items[1].billing_item_no"},
		{"another currency", strings.Replace(byAmount(doc, "x", "1 1.00"), "EUR", "USD", 1), 422, "sales.currency_mismatch", "items[0].net_amount.currency"},
		{"three decimals", byAmount(doc, "x", "1 1.001"), 400, "request.invalid_amount", "items[0].net_amount.amount"},
		{"zero", byAmount(doc, "x", "1 0.00"), 422, "sales.amount_not_positive", "items[0].net_amount.amount"},
		{"more than was billed", byAmount(doc, "x", "1 30.01"), 422, "sales.exceeds_billed_amount", "items[0].net_amount.amount"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := e.In(t).Problem(c.status, c.code, "POST", creditRequests, c.body, "")
			if f := field(got); f != c.field {
				t.Errorf("field: got %v, want %v", f, c.field)
			}
		})
	}
	if n := e.requestsCount(t); n != 0 {
		t.Errorf("credit memo requests after the refusals: got %d, want none", n)
	}
	t.Run("without the permission to create", func(t *testing.T) {
		pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = 'sales.credit_memo_request.write'`, e.Tenant.ID)
		e.In(t).Problem(403, "identity.permission_missing", "POST", creditRequests, byAmount(doc, "x", "1 1.00"), "")
	})
}

// TestCreditMemoRequestDecided: an open request is approved or rejected
// once, at the version read, by the person who created it too; each
// decision is audited and published; a second is refused.
func TestCreditMemoRequestDecided(t *testing.T) {
	e := setup(t)
	_, doc, _ := e.postedOrder(t, []string{e.item(line{quantity: "3"})}, "1 3 EA")
	first, _ := e.Must(201, "POST", creditRequests, byAmount(doc, "Goodwill", "1 1.00"), "")
	second, _ := e.Must(201, "POST", creditRequests, byAmount(doc, "Goodwill", "1 2.00"), "")
	approve := func(id string) string { return creditRequests + "/" + id + ":release" }
	reject := func(id string) string { return creditRequests + "/" + id + ":decline" }
	a, b := first["id"].(string), second["id"].(string)
	approved, tag := e.Must(200, "POST", approve(a), "", `"1"`)
	fields(t, approved, map[string]any{"status": "approved", "version": float64(2), "updated_by": e.Tenant.Human.ID.String(), "rejection_reason": nil})
	if tag != `"2"` {
		t.Errorf("ETag: got %s, want \"2\"", tag)
	}
	e.Problem(412, "request.version_mismatch", "POST", approve(a), "", `"1"`)
	e.Problem(409, "sales.invalid_status", "POST", approve(a), "", `"2"`)
	e.Problem(409, "sales.invalid_status", "POST", reject(a), `{"reason": "Too late"}`, `"2"`)
	refused := e.Problem(422, "request.validation_failed", "POST", reject(b), `{"reason": " "}`, `"1"`)
	if f := field(refused); f != "reason" {
		t.Errorf("field: got %v, want reason", f)
	}
	rejected, _ := e.Must(200, "POST", reject(b), `{"reason": "Not agreed"}`, `"1"`)
	fields(t, rejected, map[string]any{"status": "rejected", "version": float64(2), "rejection_reason": "Not agreed"})
	for _, c := range []struct{ id, action, status string }{{a, "approve", "approved"}, {b, "reject", "rejected"}} {
		t.Run(c.action, func(t *testing.T) {
			if n := e.audits(t, "sales.credit_memo_request."+c.action, c.id); n != 1 {
				t.Errorf("audit records: got %d, want 1", n)
			}
			ev := e.events(t, "credit_memo_request_changed", "credit_memo_request_id", c.id)
			if len(ev) != 2 {
				t.Fatalf("events: got %d, want 2", len(ev))
			}
			fields(t, ev[1], map[string]any{"status": c.status, "status_before": "open", "version": float64(2)})
		})
	}
	t.Run("another company's request", func(t *testing.T) {
		reader := e.As(e.Reader(t, "US sales", e.US))
		reader.In(t).Problem(404, "request.not_found", "GET", creditRequests+"/"+a, "", "")
	})
	t.Run("an unknown request", func(t *testing.T) {
		e.In(t).Problem(404, "request.not_found", "POST", reject(uuid.NewString()), `{"reason": "x"}`, `"1"`)
	})
	t.Run("without the permission to decide", func(t *testing.T) {
		third, _ := e.In(t).Must(201, "POST", creditRequests, byAmount(doc, "Goodwill", "1 1.00"), "")
		pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = 'sales.credit_memo_request.approve'`, e.Tenant.ID)
		e.In(t).Problem(403, "identity.permission_missing", "POST", approve(third["id"].(string)), "", `"1"`)
	})
}

// TestCreditMemoRequestIdempotent: a replay answers the same request and
// creates nothing more; a replayed decision answers the same decision.
func TestCreditMemoRequestIdempotent(t *testing.T) {
	e := setup(t)
	_, doc, _ := e.postedOrder(t, []string{e.item(line{quantity: "3"})}, "1 3 EA")
	req := apitest.Req{Method: "POST", Path: creditRequests, Body: byAmount(doc, "Goodwill", "1 1.00"), Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey()}
	first, second := e.API.Do(req), e.API.Do(req)
	for i, rec := range []*httptest.ResponseRecorder{first, second} {
		if rec.Code != 201 {
			t.Fatalf("status of call %d: got %d, want 201: %s", i+1, rec.Code, rec.Body)
		}
	}
	if a, b := mustJSON(t, apitest.JSON(t, first)), mustJSON(t, apitest.JSON(t, second)); a != b {
		t.Errorf("replay: got %s, want the first answer %s", b, a)
	}
	if n := e.requestsCount(t); n != 1 {
		t.Errorf("requests after the replay: got %d, want 1", n)
	}
	id := apitest.JSON(t, first)["id"].(string)
	approve := apitest.Req{Method: "POST", Path: creditRequests + "/" + id + ":release", Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey(), IfMatch: `"1"`}
	once, again := e.API.Do(approve), e.API.Do(approve)
	if once.Code != 200 || again.Code != 200 {
		t.Fatalf("statuses of the approvals: got %d and %d, want 200 twice", once.Code, again.Code)
	}
	if ev := e.events(t, "credit_memo_request_changed", "credit_memo_request_id", id); len(ev) != 2 {
		t.Errorf("events: got %d, want 2 (created, approved)", len(ev))
	}
	req.Key = ""
	if rec := e.API.Do(req); rec.Code != 428 {
		t.Errorf("without an Idempotency-Key: got %d, want 428: %s", rec.Code, rec.Body)
	}
}

// TestCreditMemoRequestsRacing: two requests racing for an item's last
// amount wait for one another on the sales order's row lock and ask for
// it once (ADR-0008; T85c acceptance).
func TestCreditMemoRequestsRacing(t *testing.T) {
	e := setup(t)
	_, doc, _ := e.postedOrder(t, []string{e.item(line{quantity: "3"})}, "1 3 EA")
	e.Must(201, "POST", creditRequests, byAmount(doc, "Goodwill", "1 10.00"), "")
	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Go(func() {
			codes[i] = e.API.Do(apitest.Req{Method: "POST", Path: creditRequests, Body: byAmount(doc, "Goodwill", "1 20.00"), Tenant: e.Tenant.ID,
				As: e.Tenant.Human, Key: apitest.NewKey()}).Code
		})
	}
	wg.Wait()
	slices.Sort(codes)
	if !slices.Equal(codes, []int{201, 422}) {
		t.Errorf("statuses: got %v, want one 201 and one 422", codes)
	}
	if n := e.requestsCount(t); n != 2 {
		t.Errorf("requests: got %d, want 2", n)
	}
}

// TestCreditMemoRequestsListed: the requests are listed by date or
// number, filtered and in pages, each as it reads.
func TestCreditMemoRequestsListed(t *testing.T) {
	e := setup(t)
	order, doc, today := e.postedOrder(t, []string{e.item(line{quantity: "5"})}, "1 5 EA")
	other := e.buyer(t, "BP70000073", "")
	placed := e.placed(t, other, e.item(line{quantity: "5"}))
	e.Must(201, "POST", deliveries(placed["id"].(string)), delivered(placed["order_date"].(string), "1 5 EA"), "")
	theirs, _ := e.Must(201, "POST", billings, bill(placed["id"].(string), ""), "")
	e.Must(200, "POST", posting(theirs["id"].(string)), "", `"1"`)
	returns := e.returnedOrder(t, doc, "1 1 EA")
	e.receive(t, returns, today, "1 1 EA")
	first, _ := e.Must(201, "POST", creditRequests, forReturns(doc, returns), "")
	second, _ := e.Must(201, "POST", creditRequests, byAmount(doc, "Goodwill", "1 1.00"), "")
	third, _ := e.Must(201, "POST", creditRequests, byAmount(theirs["id"].(string), "Goodwill", "1 1.00"), "")
	e.Must(200, "POST", creditRequests+"/"+second["id"].(string)+":release", "", `"1"`)
	numbers := func(t *testing.T, query string) (string, string) {
		t.Helper()
		got, _ := e.In(t).Must(200, "GET", creditRequests+"?"+query, "", "")
		var out []string
		for _, it := range items(t, got) {
			out = append(out, fmt.Sprint(it["document_number"]))
		}
		token, _ := got["next_page_token"].(string)
		return strings.Join(out, " "), token
	}
	for _, c := range []struct{ name, query, want string }{
		{"by date", "", "CR100000 CR100001 CR100002"},
		{"by number, the latest first", "order_by=document_number%20desc", "CR100002 CR100001 CR100000"},
		{"another company's", "legal_entity_id=" + e.US.String(), ""},
		{"a customer's", "customer_id=" + other.String(), "CR100002"},
		{"a billing document's", "billing_document_id=" + doc, "CR100000 CR100001"},
		{"a sales order's", "sales_order_id=" + order, "CR100000 CR100001"},
		{"a returns order's", "returns_order_id=" + returns, "CR100000"},
		{"the approved", "status=approved", "CR100001"},
		{"by its number", "document_number=CR100002", "CR100002"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got, _ := numbers(t, c.query); got != c.want {
				t.Errorf("requests: got %s, want %s", got, c.want)
			}
		})
	}
	t.Run("in pages", func(t *testing.T) {
		for _, order := range []string{"", "&order_by=document_number"} {
			page, token := numbers(t, "page_size=2"+order)
			rest, last := numbers(t, "page_size=2"+order+"&page_token="+token)
			if got := page + " " + rest; got != "CR100000 CR100001 CR100002" || last != "" {
				t.Errorf("the two pages (%s): got %s and token %q, want CR100000 CR100001 CR100002 and none", order, got, last)
			}
		}
	})
	t.Run("each as it reads", func(t *testing.T) {
		got, _ := e.In(t).Must(200, "GET", creditRequests, "", "")
		var want []any
		for _, id := range []any{first["id"], second["id"], third["id"]} {
			read, _ := e.In(t).Must(200, "GET", creditRequests+"/"+id.(string), "", "")
			want = append(want, read)
		}
		if a, b := mustJSON(t, got["items"]), mustJSON(t, want); a != b {
			t.Errorf("listed: got %s, want %s", a, b)
		}
	})
	t.Run("an unknown sort field", func(t *testing.T) {
		e.In(t).Problem(400, "request.invalid_order_by", "GET", creditRequests+"?order_by=reason", "", "")
	})
}
