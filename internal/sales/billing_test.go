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

const billings = "/v1/billing-documents"

// bill is the JSON of a billing document of the order; extra is further
// fields, each led by a comma.
func bill(order, extra string) string { return fmt.Sprintf(`{"sales_order_id": %q%s}`, order, extra) }

// deliveredOrder creates an order of the items, confirms it and confirms
// each "item number, quantity, unit" as delivered today; it returns the
// order's id and the day.
func (e env) deliveredOrder(t *testing.T, items []string, shipped ...string) (id, today string) {
	t.Helper()
	id, today = e.confirmed(t, items...)
	e.Must(201, "POST", deliveries(id), delivered(today, shipped...), "")
	return id, today
}

// billed is what a billing document's items bill: "order item: quantity
// unit = amount", by item number.
func billed(t *testing.T, doc map[string]any) string {
	t.Helper()
	var out []string
	for i, it := range items(t, doc) {
		if it["item_no"] != float64(i+1) {
			t.Errorf("item %d's number: got %v, want %d", i+1, it["item_no"], i+1)
		}
		out = append(out, fmt.Sprintf("%v: %s = %s", it["order_item_no"], quantity(t, it, "quantity"), amount(t, it, "net_amount")))
	}
	return strings.Join(out, ", ")
}

// TestBillingCreated: a billing document bills what is delivered of an
// order's items at the order's prices and reads back with its net value;
// the order shows what is billed at a new version; the audit record and
// BillingDocumentChanged are written; nothing is posted (T79a
// acceptance).
func TestBillingCreated(t *testing.T) {
	e := setup(t)
	id, today := e.deliveredOrder(t, []string{e.item(line{quantity: "2.5", unit: "KG", price: "10.01", code: "A1", key: "BRACKET-M8"}),
		e.item(line{quantity: "3", price: "99.99", priceUnit: 100, account: e.other})}, "1 2.5 KG", "2 3 EA")
	order, _ := e.Must(200, "GET", "/v1/sales-orders/"+id, "", "")
	got, tag := e.Must(201, "POST", billings, bill(id, ""), "")
	if tag != `"1"` {
		t.Errorf("ETag: got %s, want \"1\"", tag)
	}
	fields(t, got, map[string]any{"status": "created", "version": float64(1), "company_code": "2000", "company_name": "Acme GmbH",
		"customer_id": e.customer.String(), "customer_number": "BP70000001", "customer_name": "Northwind Traders", "legal_entity_id": e.DE.String(),
		"sales_order_id": id, "sales_order_number": order["document_number"], "payment_term_code": "NT30", "billing_date": today, "currency": "EUR"})
	if n, _ := got["document_number"].(string); !strings.HasPrefix(n, "BD") {
		t.Errorf("document number: got %v, want one of the billing_document range (BD…)", got["document_number"])
	}
	if a := amount(t, got, "net_amount"); a != "28.03 EUR" {
		t.Errorf("net value: got %s, want 28.03 EUR, the order's", a)
	}
	if b := billed(t, got); b != "1: 2.5 KG = 25.03 EUR, 2: 3 EA = 3.00 EUR" {
		t.Errorf("items: got %s, want 1: 2.5 KG = 25.03 EUR, 2: 3 EA = 3.00 EUR", b)
	}
	t.Run("an item is the order's", func(t *testing.T) {
		its, ordered := items(t, got), items(t, order)
		for i := range its {
			for _, f := range []string{"item_key", "description", "price_unit", "account_id", "account_number", "account_name", "tax_code"} {
				if its[i][f] != ordered[i][f] {
					t.Errorf("item %d's %s: got %v, want the order item's %v", i+1, f, its[i][f], ordered[i][f])
				}
			}
			if a, b := amount(t, its[i], "unit_price"), amount(t, ordered[i], "unit_price"); a != b {
				t.Errorf("item %d's unit price: got %s, want the order item's %s", i+1, a, b)
			}
		}
	})
	doc, _ := got["id"].(string)
	t.Run("read back", func(t *testing.T) {
		read, readTag := e.In(t).Must(200, "GET", billings+"/"+doc, "", "")
		if a, b := mustJSON(t, read), mustJSON(t, got); a != b {
			t.Errorf("read back: got %s, want %s", a, b)
		}
		if readTag != tag {
			t.Errorf("ETag read back: got %s, want %s", readTag, tag)
		}
	})
	t.Run("the order shows it", func(t *testing.T) {
		read, orderTag := e.In(t).Must(200, "GET", "/v1/sales-orders/"+id, "", "")
		if orderTag != `"4"` {
			t.Errorf("the order's ETag: got %s, want \"4\" (created, confirmed, delivered, billed)", orderTag)
		}
		fields(t, read, map[string]any{"status": "confirmed", "version": float64(4)})
		if b := e.quantities(t, id, "billed_quantity"); b != "2.5 KG, 3 EA" {
			t.Errorf("billed: got %s, want 2.5 KG, 3 EA", b)
		}
	})
	t.Run("audited and published", func(t *testing.T) {
		if n := e.audits(t, "sales.billing_document.create", doc); n != 1 {
			t.Errorf("audit records of the creation: got %d, want 1", n)
		}
		want := map[string]any{"billing_document_id": doc, "document_number": got["document_number"], "legal_entity_id": e.DE.String(),
			"legal_entity_code": "2000", "customer_id": e.customer.String(), "customer_number": "BP70000001", "sales_order_id": id,
			"sales_order_number": order["document_number"], "billing_date": today, "net_amount": map[string]any{"amount": "28.03", "currency": "EUR"},
			"status": "created", "status_before": nil, "version": float64(1)}
		if a, b := mustJSON(t, e.events(t, "billing_document_changed", "billing_document_id", doc)), mustJSON(t, []map[string]any{want}); a != b {
			t.Errorf("BillingDocumentChanged: got %s, want %s", a, b)
		}
		apitest.NeverPublished(t, e.Tenant, "sales", "Northwind Traders")
	})
	t.Run("nothing left to bill", func(t *testing.T) {
		refused := e.In(t).Problem(422, "sales.nothing_to_bill", "POST", billings, bill(id, ""), "")
		if f := field(refused); f != "sales_order_id" {
			t.Errorf("field: got %v, want sales_order_id", f)
		}
	})
	if n := e.journalEntries(t); n != 0 {
		t.Errorf("journal entries: got %d, want none (nothing is posted)", n)
	}
}

// TestBillingTakesTheRemainder: each document bills the delivered
// quantity less what earlier documents took, and its amount is the item's
// net value for the quantity billed so far less what earlier documents
// billed, so the documents of a fully billed item add up to its net value
// to the minor unit, the last taking the remainder (T79a acceptance).
func TestBillingTakesTheRemainder(t *testing.T) {
	e := setup(t)
	// 3 EA at 10.00 per 3 is 10.00, which no third divides; 4 KG at 2.50 is 10.00.
	id, today := e.confirmed(t, e.item(line{quantity: "3", price: "10.00", priceUnit: 3}), e.item(line{quantity: "4", unit: "KG", price: "2.50"}))
	for i, c := range []struct {
		shipped      []string
		want, billed string
		net          string
	}{
		{[]string{"1 1 EA", "2 1 KG"}, "1: 1 EA = 3.33 EUR, 2: 1 KG = 2.50 EUR", "1 EA, 1 KG", "5.83 EUR"},
		{[]string{"1 1 EA"}, "1: 1 EA = 3.34 EUR", "2 EA, 1 KG", "3.34 EUR"},
		{[]string{"2 3 KG"}, "2: 3 KG = 7.50 EUR", "2 EA, 4 KG", "7.50 EUR"},
		{[]string{"1 1 EA"}, "1: 1 EA = 3.33 EUR", "3 EA, 4 KG", "3.33 EUR"},
	} {
		t.Run(fmt.Sprintf("document %d", i+1), func(t *testing.T) {
			e.In(t).Must(201, "POST", deliveries(id), delivered(today, c.shipped...), "")
			got, _ := e.In(t).Must(201, "POST", billings, bill(id, ""), "")
			if b := billed(t, got); b != c.want {
				t.Errorf("items: got %s, want %s", b, c.want)
			}
			if a := amount(t, got, "net_amount"); a != c.net {
				t.Errorf("net value: got %s, want %s", a, c.net)
			}
			if b := e.quantities(t, id, "billed_quantity"); b != c.billed {
				t.Errorf("billed of the order: got %s, want %s", b, c.billed)
			}
		})
	}
	var total string
	pgtest.Scan(t, `SELECT string_agg(amount::text, ' ' ORDER BY order_item_no) FROM (SELECT order_item_no, sum(net_amount) AS amount
		FROM sales.billing_document_item WHERE tenant_id = $1 AND sales_order_id = $2 GROUP BY order_item_no) x`, []any{e.Tenant.ID, id}, &total)
	if total != "1000 1000" {
		t.Errorf("billed of each item in all: got %s, want 1000 1000, the items' net values", total)
	}
}

// TestBillingLeavesAnAmountOfZero: a quantity whose amount rounds to zero
// is left for a later document, so no document has an item without an
// amount.
func TestBillingLeavesAnAmountOfZero(t *testing.T) {
	e := setup(t)
	// 3 EA at 0.01 per 3 is 0.01 in all: a third of it rounds to nothing, two thirds to 0.01.
	id, today := e.deliveredOrder(t, []string{e.item(line{quantity: "3", price: "0.01", priceUnit: 3})}, "1 1 EA")
	e.Problem(422, "sales.nothing_to_bill", "POST", billings, bill(id, ""), "")
	e.Must(201, "POST", deliveries(id), delivered(today, "1 1 EA"), "")
	got, _ := e.Must(201, "POST", billings, bill(id, ""), "")
	if b := billed(t, got); b != "1: 2 EA = 0.01 EUR" {
		t.Errorf("items: got %s, want 1: 2 EA = 0.01 EUR", b)
	}
	e.Must(201, "POST", deliveries(id), delivered(today, "1 1 EA"), "")
	e.Problem(422, "sales.nothing_to_bill", "POST", billings, bill(id, ""), "")
	if b := e.quantities(t, id, "billed_quantity"); b != "2 EA" {
		t.Errorf("billed of the order: got %s, want 2 EA, the last worth nothing more", b)
	}
}

// TestBillingIdempotent: a replay answers the same document and bills
// nothing more.
func TestBillingIdempotent(t *testing.T) {
	e := setup(t)
	id, _ := e.deliveredOrder(t, []string{e.item(line{quantity: "3"})}, "1 2 EA")
	req := apitest.Req{Method: "POST", Path: billings, Body: bill(id, ""), Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey()}
	first, second := e.API.Do(req), e.API.Do(req)
	for i, rec := range []*httptest.ResponseRecorder{first, second} {
		if rec.Code != 201 {
			t.Fatalf("status of call %d: got %d, want 201: %s", i+1, rec.Code, rec.Body)
		}
	}
	if a, b := mustJSON(t, apitest.JSON(t, first)), mustJSON(t, apitest.JSON(t, second)); a != b {
		t.Errorf("replay: got %s, want the first answer %s", b, a)
	}
	if h := second.Header().Get("Idempotent-Replayed"); h != "true" {
		t.Errorf("Idempotent-Replayed: got %q, want true", h)
	}
	if loc := first.Header().Get("Location"); loc != billings+"/"+apitest.JSON(t, first)["id"].(string) {
		t.Errorf("Location: got %q, want the document's path", loc)
	}
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM sales.billing_document WHERE tenant_id = $1 AND sales_order_id = $2`, []any{e.Tenant.ID, id}, &n)
	if n != 1 {
		t.Errorf("billing documents after the replay: got %d, want 1", n)
	}
	req.Key = ""
	if rec := e.API.Do(req); rec.Code != 428 {
		t.Errorf("without an Idempotency-Key: got %d, want 428: %s", rec.Code, rec.Body)
	}
}

// TestBillingRefusals: an order the caller cannot read is a wrong
// reference; one with nothing delivered that is not billed, and one whose
// item can no longer be billed on the billing date, are refused naming
// sales_order_id; nothing is billed.
func TestBillingRefusals(t *testing.T) {
	e := setup(t)
	id, _ := e.deliveredOrder(t, []string{e.item(line{quantity: "3", code: "A1"}), e.item(line{quantity: "1", account: e.other})}, "1 2 EA")
	undelivered, _ := e.confirmed(t, e.item(line{}))
	draft := e.ordered(t)
	cases := []struct {
		name, body string
		status     int
		code       string
		field      any
		detail     string
	}{
		{"an unknown order", bill(uuid.NewString(), ""), 422, "request.reference_not_found", "sales_order_id", ""},
		{"a draft order", bill(draft, ""), 422, "sales.nothing_to_bill", "sales_order_id", "has nothing to bill"},
		{"an order with nothing delivered", bill(undelivered, ""), 422, "sales.nothing_to_bill", "sales_order_id", "has nothing to bill"},
		{"a date its tax code has no rate on", bill(id, `, "billing_date": "2025-12-31"`), 422, "sales.order_item_not_billable", "sales_order_id",
			"Item 1 of sales order SO"},
		{"a date without a fiscal year", bill(id, `, "billing_date": "2099-12-31"`), 422, "ledger.no_fiscal_period", nil, ""},
		{"no order", `{}`, 400, "request.invalid_body", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := e.In(t).Problem(c.status, c.code, "POST", billings, c.body, "")
			if f := field(got); f != c.field {
				t.Errorf("field: got %v, want %v", f, c.field)
			}
			if d, _ := got["detail"].(string); !strings.Contains(d, c.detail) {
				t.Errorf("detail: got %q, want it to contain %q", d, c.detail)
			}
		})
	}
	t.Run("an account that can no longer be posted", func(t *testing.T) {
		other, today := e.deliveredOrder(t, []string{e.item(line{}), e.item(line{account: e.other})}, "1 1 EA", "2 1 EA")
		pgtest.Exec(t, `UPDATE ledger.company_account SET is_posting_blocked = true, version = version + 1 WHERE tenant_id = $1 AND account_id = $2`, e.Tenant.ID, e.other)
		got := e.In(t).Problem(422, "sales.order_item_not_billable", "POST", billings, bill(other, ""), "")
		if f := field(got); f != "sales_order_id" {
			t.Errorf("field: got %v, want sales_order_id", f)
		}
		if d, _ := got["detail"].(string); !strings.Contains(d, "Item 2 of sales order SO") || !strings.Contains(d, "cannot be billed on "+today+": its account") {
			t.Errorf("detail: got %q, want it to name item 2, the date and the account", d)
		}
	})
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM sales.billing_document WHERE tenant_id = $1`, []any{e.Tenant.ID}, &n)
	if n != 0 {
		t.Errorf("billing documents after the refusals: got %d, want none", n)
	}
	if b := e.quantities(t, id, "billed_quantity"); b != "0 EA, 0 EA" {
		t.Errorf("billed after the refusals: got %s, want nothing", b)
	}
	t.Run("another company's order", func(t *testing.T) {
		reader := e.As(e.Reader(t, "US sales", e.US))
		if f := field(reader.In(t).Problem(422, "request.reference_not_found", "POST", billings, bill(id, ""), "")); f != "sales_order_id" {
			t.Errorf("field: got %v, want sales_order_id", f)
		}
	})
	t.Run("without the permission to bill", func(t *testing.T) {
		pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = 'sales.billing.write'`, e.Tenant.ID)
		e.In(t).Problem(403, "identity.permission_missing", "POST", billings, bill(id, ""), "")
	})
}

// TestBillingsRacing: two requests to bill one order wait for one another
// on the order's row lock and never bill a quantity twice (ADR-0008; T79a
// acceptance).
func TestBillingsRacing(t *testing.T) {
	e := setup(t)
	id, _ := e.deliveredOrder(t, []string{e.item(line{quantity: "3"})}, "1 2 EA")
	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Go(func() {
			codes[i] = e.API.Do(apitest.Req{Method: "POST", Path: billings, Body: bill(id, ""), Tenant: e.Tenant.ID, As: e.Tenant.Human,
				Key: apitest.NewKey()}).Code
		})
	}
	wg.Wait()
	// The second waits for the first's lock on the order and then finds nothing left to bill.
	slices.Sort(codes)
	if !slices.Equal(codes, []int{201, 422}) {
		t.Errorf("statuses: got %v, want one 201 and one 422", codes)
	}
	if b := e.quantities(t, id, "billed_quantity"); b != "2 EA" {
		t.Errorf("billed: got %s, want 2 EA, what was delivered, once", b)
	}
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM sales.billing_document WHERE tenant_id = $1 AND sales_order_id = $2`, []any{e.Tenant.ID, id}, &n)
	if n != 1 {
		t.Errorf("billing documents of the order: got %d, want 1", n)
	}
	if ev := e.events(t, "billing_document_changed", "sales_order_id", id); len(ev) != 1 {
		t.Errorf("BillingDocumentChanged events: got %d, want 1", len(ev))
	}
}

// TestBillingDatedInTheCompanysZone: a billing document without a date is
// dated the company's date in its time zone; one with a date takes it.
func TestBillingDatedInTheCompanysZone(t *testing.T) {
	e := setup(t)
	id, _ := e.deliveredOrder(t, []string{e.item(line{quantity: "3"})}, "1 2 EA")
	// 03:00 UTC on 10 March is still 9 March in Toronto.
	h := endpoints(t, instant(t, "2026-03-10T03:00:00Z"))
	given := instant(t, "2026-02-01T00:00:00Z")
	for _, c := range []struct {
		name string
		date *time.Time
		want string
	}{{"no date", nil, "2026-03-09"}, {"a date given", &given, "2026-02-01"}} {
		t.Run(c.name, func(t *testing.T) {
			err := e.inRequest(t, func(ctx context.Context) error {
				b, err := h.CreateBilling(ctx, sales.BillingInput{OrderID: uuid.MustParse(id), Date: c.date})
				if err == nil && b.BillingDate.Format(time.DateOnly) != c.want {
					t.Errorf("billing date: got %s, want %s", b.BillingDate.Format(time.DateOnly), c.want)
				}
				return err
			})
			if err != nil {
				t.Errorf("got %v, want the billing document created", err)
			}
		})
	}
}

// TestBillingsListed: billing documents are listed by company, customer,
// order, status and number, in pages, and only those of the companies the
// caller may act in.
func TestBillingsListed(t *testing.T) {
	e := setup(t)
	other := e.partner(t, "BP70000003", "Contoso", "active", "customer")
	e.companyData(t, other, e.DE, "NT14")
	order, today := e.deliveredOrder(t, []string{e.item(line{quantity: "3"})}, "1 1 EA")
	first, _ := e.Must(201, "POST", billings, bill(order, ""), "")
	e.Must(201, "POST", deliveries(order), delivered(today, "1 1 EA"), "")
	second, _ := e.Must(201, "POST", billings, bill(order, ""), "")
	created, _ := e.Must(201, "POST", "/v1/sales-orders", e.direct(other, "", e.item(line{})), "")
	theirOrder := created["id"].(string)
	e.Must(200, "POST", "/v1/sales-orders/"+theirOrder+":confirm", "", `"1"`)
	e.Must(201, "POST", deliveries(theirOrder), delivered(today, "1 1 EA"), "")
	theirs, _ := e.Must(201, "POST", billings, bill(theirOrder, ""), "")
	ids := func(t *testing.T, c apitest.Client, query string) string {
		t.Helper()
		got, _ := c.In(t).Must(200, "GET", billings+"?"+query, "", "")
		var out []string
		for _, it := range items(t, got) {
			out = append(out, it["id"].(string))
		}
		return strings.Join(out, " ")
	}
	fid, sid, tid := first["id"].(string), second["id"].(string), theirs["id"].(string)
	for _, c := range []struct{ name, query, want string }{
		{"a customer's, by number", "customer_id=" + e.customer.String() + "&order_by=document_number", fid + " " + sid},
		{"an order's, newest number first", "sales_order_id=" + order + "&order_by=document_number%20desc", sid + " " + fid},
		{"another customer's", "customer_id=" + other.String(), tid},
		{"the created, newest number first", "status=created&order_by=document_number%20desc&page_size=2", tid + " " + sid},
		{"in no such status", "status=posted", ""},
		{"a company's, by number", "legal_entity_id=" + e.DE.String() + "&order_by=document_number", fid + " " + sid + " " + tid},
		{"another company's", "legal_entity_id=" + e.US.String(), ""},
		{"by number", "document_number=" + second["document_number"].(string), sid},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := ids(t, e.Client, c.query); got != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
	t.Run("in pages", func(t *testing.T) {
		// The three are dated the same day: the id breaks the tie, so the pages hold each once.
		for _, order := range []string{"", "&order_by=document_number"} {
			query := "page_size=2" + order
			page, _ := e.In(t).Must(200, "GET", billings+"?"+query, "", "")
			token, _ := page["next_page_token"].(string)
			if token == "" {
				t.Fatalf("first page (%s): got no next_page_token, want one", query)
			}
			all := strings.Fields(ids(t, e.Client, query) + " " + ids(t, e.Client, query+"&page_token="+token))
			slices.Sort(all)
			want := []string{fid, sid, tid}
			slices.Sort(want)
			if !slices.Equal(all, want) {
				t.Errorf("the two pages (%s): got %v, want %v", query, all, want)
			}
		}
	})
	t.Run("only the caller's companies", func(t *testing.T) {
		reader := e.As(e.Reader(t, "US sales", e.US))
		if got := ids(t, reader, ""); got != "" {
			t.Errorf("listed: got %s, want none", got)
		}
		reader.In(t).Problem(404, "request.not_found", "GET", billings+"/"+fid, "", "")
	})
	t.Run("an unknown document", func(t *testing.T) {
		e.In(t).Problem(404, "request.not_found", "GET", billings+"/"+uuid.NewString(), "", "")
	})
}
