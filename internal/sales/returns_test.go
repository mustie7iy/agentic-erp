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

const returnsOrders = "/v1/returns-orders"

// postedOrder is billedOrder with the billing document posted as a
// customer invoice: what a returns order is for.
func (e env) postedOrder(t *testing.T, items []string, shipped ...string) (order, doc, today string) {
	t.Helper()
	order, doc, today = e.billedOrder(t, items, shipped...)
	e.Must(200, "POST", posting(doc), "", `"1"`)
	return order, doc, today
}

// returning is the JSON of a returns order for the billing document with
// the reason; each item is "the billing item's number, the quantity, the
// unit".
func returning(doc, reason string, items ...string) string {
	parts := make([]string, len(items))
	for i, it := range items {
		f := strings.Split(it, " ")
		parts[i] = fmt.Sprintf(`{"billing_item_no": %s, "quantity": {"value": %q, "unit": %q}}`, f[0], f[1], f[2])
	}
	return fmt.Sprintf(`{"billing_document_id": %q, "reason": %q, "items": [%s]}`, doc, reason, strings.Join(parts, ", "))
}

// returnedOrder creates a returns order for the items of the billing
// document and confirms it; it returns its id.
func (e env) returnedOrder(t *testing.T, doc string, items ...string) string {
	t.Helper()
	got, _ := e.Must(201, "POST", returnsOrders, returning(doc, "defective", items...), "")
	id := got["id"].(string)
	e.Must(200, "POST", returnsOrders+"/"+id+":confirm", "", `"1"`)
	return id
}

// returnsCount counts the tenant's returns orders.
func (e env) returnsCount(t *testing.T) int {
	t.Helper()
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM sales.returns_order WHERE tenant_id = $1`, []any{e.Tenant.ID}, &n)
	return n
}

// TestReturnsOrderCreatedAndConfirmed: a returns order for part of a
// posted billing document's quantities is created a draft with its
// number, its reason and its items, each showing what was billed and
// what is returned; the sales order shows what is returned at a new
// version, its delivered and billed quantities as they were; it is
// confirmed once; both changes are audited and published; nothing is
// posted (T85a acceptance).
func TestReturnsOrderCreatedAndConfirmed(t *testing.T) {
	e := setup(t)
	order, doc, today := e.postedOrder(t, []string{e.item(line{quantity: "2.5", unit: "KG", price: "10.01", key: "BRACKET-M8"}), e.item(line{quantity: "3"})},
		"1 2.5 KG", "2 3 EA")
	billing, _ := e.Must(200, "GET", billings+"/"+doc, "", "")
	sold, _ := e.Must(200, "GET", "/v1/sales-orders/"+order, "", "")
	entries := e.journalEntries(t)
	body := strings.Replace(returning(doc, "wrong_item", "2 1 EA", "1 0.5 KG"), `"reason"`, `"reason_text": "M10 was ordered", "reason"`, 1)
	rec := e.API.Do(apitest.Req{Method: "POST", Path: returnsOrders, Body: body, Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey()})
	if rec.Code != 201 {
		t.Fatalf("status: got %d, want 201: %s", rec.Code, rec.Body)
	}
	got := apitest.JSON(t, rec)
	id := got["id"].(string)
	if loc := rec.Header().Get("Location"); loc != returnsOrders+"/"+id {
		t.Errorf("Location: got %q, want %s/%s", loc, returnsOrders, id)
	}
	if tag := rec.Header().Get("ETag"); tag != `"1"` {
		t.Errorf("ETag: got %s, want \"1\"", tag)
	}
	fields(t, got, map[string]any{"document_number": "RO100000", "status": "draft", "version": float64(1), "legal_entity_id": e.DE.String(),
		"company_code": "2000", "customer_id": e.customer.String(), "customer_number": "BP70000001", "customer_name": "Northwind Traders",
		"billing_document_id": doc, "billing_document_number": billing["document_number"], "sales_order_id": order,
		"sales_order_number": sold["document_number"], "returns_date": today, "reason": "wrong_item", "reason_text": "M10 was ordered",
		"created_by": e.Tenant.Human.ID.String()})
	its := items(t, got)
	if len(its) != 2 {
		t.Fatalf("items: got %d, want 2", len(its))
	}
	for i, want := range []struct {
		billing, order   float64
		key              any
		billed, returned string
	}{{2, 2, nil, "3 EA", "1 EA"}, {1, 1, "BRACKET-M8", "2.5 KG", "0.5 KG"}} {
		t.Run(fmt.Sprintf("item %d", i+1), func(t *testing.T) {
			fields(t, its[i], map[string]any{"item_no": float64(i + 1), "billing_item_no": want.billing, "order_item_no": want.order,
				"item_key": want.key, "description": "Machined bracket"})
			if q := quantity(t, its[i], "billed_quantity"); q != want.billed {
				t.Errorf("billed: got %s, want %s", q, want.billed)
			}
			if q := quantity(t, its[i], "quantity"); q != want.returned {
				t.Errorf("returned: got %s, want %s", q, want.returned)
			}
		})
	}
	t.Run("the sales order shows it", func(t *testing.T) {
		read, tag := e.In(t).Must(200, "GET", "/v1/sales-orders/"+order, "", "")
		if want := fmt.Sprintf(`"%v"`, sold["version"].(float64)+1); tag != want {
			t.Errorf("the order's ETag: got %s, want %s, one more than before the returns order", tag, want)
		}
		fields(t, read, map[string]any{"status": "confirmed"})
		if a, b := amount(t, read, "net_amount"), amount(t, sold, "net_amount"); a != b {
			t.Errorf("the order's net value: got %s, want %s as before the returns order", a, b)
		}
		for f, want := range map[string]string{"returned_quantity": "0.5 KG, 1 EA", "delivered_quantity": "2.5 KG, 3 EA", "billed_quantity": "2.5 KG, 3 EA"} {
			if q := e.quantities(t, order, f); q != want {
				t.Errorf("%s: got %s, want %s", f, q, want)
			}
		}
	})
	event := func(status string, before any, version float64) map[string]any {
		return map[string]any{"returns_order_id": id, "document_number": "RO100000", "legal_entity_id": e.DE.String(), "legal_entity_code": "2000",
			"customer_id": e.customer.String(), "customer_number": "BP70000001", "billing_document_id": doc,
			"billing_document_number": billing["document_number"], "sales_order_id": order, "sales_order_number": sold["document_number"],
			"returns_date": today, "reason": "wrong_item", "status": status, "status_before": before, "version": version}
	}
	t.Run("read back", func(t *testing.T) {
		read, tag := e.In(t).Must(200, "GET", returnsOrders+"/"+id, "", "")
		if a, b := mustJSON(t, read), mustJSON(t, got); a != b {
			t.Errorf("read back: got %s, want %s", a, b)
		}
		if tag != `"1"` {
			t.Errorf("ETag: got %s, want \"1\"", tag)
		}
	})
	t.Run("confirmed once", func(t *testing.T) {
		confirmed, tag := e.In(t).Must(200, "POST", returnsOrders+"/"+id+":confirm", "", `"1"`)
		fields(t, confirmed, map[string]any{"status": "confirmed", "version": float64(2), "document_number": "RO100000"})
		if tag != `"2"` {
			t.Errorf("ETag: got %s, want \"2\"", tag)
		}
		if a, b := mustJSON(t, confirmed["items"]), mustJSON(t, got["items"]); a != b {
			t.Errorf("the confirmed order's items: got %s, want them as created %s", a, b)
		}
		e.In(t).Problem(412, "request.version_mismatch", "POST", returnsOrders+"/"+id+":confirm", "", `"1"`)
		e.In(t).Problem(409, "sales.invalid_status", "POST", returnsOrders+"/"+id+":confirm", "", `"2"`)
		if q := e.quantities(t, order, "returned_quantity"); q != "0.5 KG, 1 EA" {
			t.Errorf("returned after the confirmation: got %s, want 0.5 KG, 1 EA as before", q)
		}
	})
	t.Run("audited and published", func(t *testing.T) {
		for _, action := range []string{"sales.returns_order.create", "sales.returns_order.confirm"} {
			if n := e.audits(t, action, id); n != 1 {
				t.Errorf("audit records of %s: got %d, want 1", action, n)
			}
		}
		want := []map[string]any{event("draft", nil, 1), event("confirmed", "draft", 2)}
		if a, b := mustJSON(t, e.events(t, "returns_order_changed", "returns_order_id", id)), mustJSON(t, want); a != b {
			t.Errorf("ReturnsOrderChanged: got %s, want %s", a, b)
		}
		apitest.NeverPublished(t, e.Tenant, "sales", "Northwind Traders")
	})
	if n := e.journalEntries(t); n != entries {
		t.Errorf("journal entries: got %d, want %d as before the returns order (nothing is posted)", n, entries)
	}
}

// TestReturnsOrderIdempotent: a replay answers the same returns order and
// takes nothing more; a replayed confirmation answers the confirmed
// order.
func TestReturnsOrderIdempotent(t *testing.T) {
	e := setup(t)
	order, doc, _ := e.postedOrder(t, []string{e.item(line{quantity: "3"})}, "1 3 EA")
	req := apitest.Req{Method: "POST", Path: returnsOrders, Body: returning(doc, "defective", "1 1 EA"), Tenant: e.Tenant.ID, As: e.Tenant.Human,
		Key: apitest.NewKey()}
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
	if q := e.quantities(t, order, "returned_quantity"); q != "1 EA" {
		t.Errorf("returned after the replay: got %s, want 1 EA", q)
	}
	if n := e.returnsCount(t); n != 1 {
		t.Errorf("returns orders after the replay: got %d, want 1", n)
	}
	id := apitest.JSON(t, first)["id"].(string)
	confirm := apitest.Req{Method: "POST", Path: returnsOrders + "/" + id + ":confirm", Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey(),
		IfMatch: `"1"`}
	once, again := e.API.Do(confirm), e.API.Do(confirm)
	for i, rec := range []*httptest.ResponseRecorder{once, again} {
		if rec.Code != 200 {
			t.Fatalf("status of confirmation %d: got %d, want 200: %s", i+1, rec.Code, rec.Body)
		}
	}
	if a, b := mustJSON(t, apitest.JSON(t, once)), mustJSON(t, apitest.JSON(t, again)); a != b {
		t.Errorf("replayed confirmation: got %s, want the first answer %s", b, a)
	}
	if ev := e.events(t, "returns_order_changed", "returns_order_id", id); len(ev) != 2 {
		t.Errorf("ReturnsOrderChanged events: got %d, want 2 (created, confirmed)", len(ev))
	}
	req.Key = ""
	if rec := e.API.Do(req); rec.Code != 428 {
		t.Errorf("without an Idempotency-Key: got %d, want 428: %s", rec.Code, rec.Body)
	}
}

// TestReturnsOrderRefusals: only a posted billing document takes a
// returns order, for a known reason, of its own items, each once, in its
// unit, up to what was billed; every refusal names the request's field
// and leaves nothing returned.
func TestReturnsOrderRefusals(t *testing.T) {
	e := setup(t)
	order, doc, _ := e.postedOrder(t, []string{e.item(line{quantity: "2.5", unit: "KG"}), e.item(line{quantity: "3"})}, "1 2.5 KG", "2 3 EA")
	_, created, _ := e.billedOrder(t, []string{e.item(line{quantity: "3"})}, "1 3 EA")
	text := func(s string) string {
		return strings.Replace(returning(doc, "other", "1 1 KG"), `"reason"`, fmt.Sprintf(`"reason_text": %q, "reason"`, s), 1)
	}
	cases := []struct {
		name, body string
		status     int
		code       string
		field      any
	}{
		{"a billing document that is not posted", returning(created, "defective", "1 1 EA"), 409, "sales.billing_document_not_posted", nil},
		{"an unknown billing document", returning(uuid.NewString(), "defective", "1 1 EA"), 422, "request.reference_not_found", "billing_document_id"},
		{"an unknown reason", returning(doc, "bored", "1 1 KG"), 422, "sales.unknown_return_reason", "reason"},
		{"a reason text of white space", text("  "), 422, "request.validation_failed", "reason_text"},
		{"an item the billing document does not have", returning(doc, "defective", "1 1 KG", "3 1 EA"), 422, "sales.unknown_billing_item", "items[1].billing_item_no"},
		{"an item named twice", returning(doc, "defective", "2 1 EA", "2 1 EA"), 422, "sales.item_repeated", "items[1].billing_item_no"},
		{"a quantity of zero", returning(doc, "defective", "1 0 KG"), 422, "sales.invalid_quantity", "items[0].quantity.value"},
		{"a seventh decimal", returning(doc, "defective", "1 0.0000001 KG"), 400, "request.invalid_body", nil},
		{"another unit", returning(doc, "defective", "1 1 EA"), 422, "sales.unit_mismatch", "items[0].quantity.unit"},
		{"more than was billed", returning(doc, "defective", "1 2.5 KG", "2 3.000001 EA"), 422, "sales.exceeds_returnable_quantity", "items[1].quantity.value"},
		{"no items", returning(doc, "defective"), 400, "request.invalid_body", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := e.In(t).Problem(c.status, c.code, "POST", returnsOrders, c.body, "")
			if f := field(got); f != c.field {
				t.Errorf("field: got %v, want %v", f, c.field)
			}
		})
	}
	if q := e.quantities(t, order, "returned_quantity"); q != "0 KG, 0 EA" {
		t.Errorf("returned after the refusals: got %s, want nothing", q)
	}
	if n := e.returnsCount(t); n != 0 {
		t.Errorf("returns orders after the refusals: got %d, want none", n)
	}
	t.Run("no items at the port", func(t *testing.T) {
		// The contract refuses an empty list before the handler; the port refuses it too.
		err := e.inRequest(t, func(ctx context.Context) error {
			_, err := endpoints(t, time.Now()).MakeReturns(ctx, sales.ReturnsInput{BillingDocumentID: uuid.MustParse(doc), Reason: "defective"})
			return err
		})
		refusal(t, err, 422, "sales.items_required")
	})
	t.Run("another company's billing document and returns order", func(t *testing.T) {
		id := e.returnedOrder(t, doc, "1 1 KG")
		reader := e.As(e.Reader(t, "US sales", e.US))
		got := reader.In(t).Problem(422, "request.reference_not_found", "POST", returnsOrders, returning(doc, "defective", "1 1 KG"), "")
		if f := field(got); f != "billing_document_id" {
			t.Errorf("field: got %v, want billing_document_id", f)
		}
		reader.In(t).Problem(404, "request.not_found", "GET", returnsOrders+"/"+id, "", "")
		reader.In(t).Problem(404, "request.not_found", "POST", returnsOrders+"/"+id+":confirm", "", `"2"`)
		listed, _ := reader.In(t).Must(200, "GET", returnsOrders, "", "")
		if n := len(items(t, listed)); n != 0 {
			t.Errorf("returns orders listed for another company's reader: got %d, want none", n)
		}
	})
	t.Run("an unknown returns order", func(t *testing.T) {
		e.In(t).Problem(404, "request.not_found", "GET", returnsOrders+"/"+uuid.NewString(), "", "")
		e.In(t).Problem(404, "request.not_found", "POST", returnsOrders+"/"+uuid.NewString()+":confirm", "", `"1"`)
	})
	t.Run("without the permissions", func(t *testing.T) {
		draft, _ := e.In(t).Must(201, "POST", returnsOrders, returning(doc, "defective", "2 1 EA"), "")
		for _, c := range []struct{ permission, method, path, body, tag string }{
			{"sales.return.confirm", "POST", returnsOrders + "/" + draft["id"].(string) + ":confirm", "", `"1"`},
			{"sales.return.write", "POST", returnsOrders, returning(doc, "defective", "2 1 EA"), ""},
			{"sales.return.read", "GET", returnsOrders, "", ""},
		} {
			pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = $2`, e.Tenant.ID, c.permission)
			e.In(t).Problem(403, "identity.permission_missing", c.method, c.path, c.body, c.tag)
		}
	})
}

// TestReturnsOrdersTakeWhatWasBilled: the returns orders of a billing
// document, drafts included, never take more of an item than it billed;
// the refusal names the item with what was billed and taken; another
// billing document of the same sales order item has its own quantity,
// and the order's item shows the sum.
func TestReturnsOrdersTakeWhatWasBilled(t *testing.T) {
	e := setup(t)
	order, today := e.deliveredOrder(t, []string{e.item(line{quantity: "10"})}, "1 4 EA")
	first, _ := e.Must(201, "POST", billings, bill(order, ""), "")
	e.Must(200, "POST", posting(first["id"].(string)), "", `"1"`)
	e.Must(201, "POST", deliveries(order), delivered(today, "1 6 EA"), "")
	second, _ := e.Must(201, "POST", billings, bill(order, ""), "")
	e.Must(200, "POST", posting(second["id"].(string)), "", `"1"`)
	doc, number := first["id"].(string), first["document_number"].(string)
	e.Must(201, "POST", returnsOrders, returning(doc, "defective", "1 3 EA"), "") // a draft takes its quantity too
	refused := e.Problem(422, "sales.exceeds_returnable_quantity", "POST", returnsOrders, returning(doc, "defective", "1 1.5 EA"), "")
	if f := field(refused); f != "items[0].quantity.value" {
		t.Errorf("field: got %v, want items[0].quantity.value", f)
	}
	want := "Item 1 of billing document " + number + " has 1 EA that can still be returned (4 billed, 3 taken by its returns orders); 1.5 is more."
	if d, _ := refused["detail"].(string); d != want {
		t.Errorf("detail: got %q, want %q", d, want)
	}
	e.Must(201, "POST", returnsOrders, returning(doc, "not_wanted", "1 1 EA"), "")
	e.Problem(422, "sales.exceeds_returnable_quantity", "POST", returnsOrders, returning(doc, "defective", "1 0.000001 EA"), "")
	t.Run("the order's second billing document has its own quantity", func(t *testing.T) {
		e.In(t).Problem(422, "sales.exceeds_returnable_quantity", "POST", returnsOrders, returning(second["id"].(string), "defective", "1 6.5 EA"), "")
		e.In(t).Must(201, "POST", returnsOrders, returning(second["id"].(string), "defective", "1 6 EA"), "")
		if q := e.quantities(t, order, "returned_quantity"); q != "10 EA" {
			t.Errorf("returned of the order's item: got %s, want 10 EA, what its two billing documents' returns orders took", q)
		}
	})
}

// TestReturnsOrdersRacing: two returns orders racing for an item's last
// quantity wait for one another on the sales order's row lock and take
// it once (ADR-0008; T85a acceptance).
func TestReturnsOrdersRacing(t *testing.T) {
	e := setup(t)
	order, doc, _ := e.postedOrder(t, []string{e.item(line{quantity: "3"})}, "1 3 EA")
	e.Must(201, "POST", returnsOrders, returning(doc, "defective", "1 1 EA"), "")
	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Go(func() {
			codes[i] = e.API.Do(apitest.Req{Method: "POST", Path: returnsOrders, Body: returning(doc, "defective", "1 2 EA"), Tenant: e.Tenant.ID,
				As: e.Tenant.Human, Key: apitest.NewKey()}).Code
		})
	}
	wg.Wait()
	// The second waits for the first's lock on the sales order and then finds nothing left to return.
	slices.Sort(codes)
	if !slices.Equal(codes, []int{201, 422}) {
		t.Errorf("statuses: got %v, want one 201 and one 422", codes)
	}
	if q := e.quantities(t, order, "returned_quantity"); q != "3 EA" {
		t.Errorf("returned: got %s, want 3 EA, the last two returned once", q)
	}
	if n := e.returnsCount(t); n != 2 {
		t.Errorf("returns orders: got %d, want 2", n)
	}
}

// TestReturnsOrderConfirmationsRacing: two confirmations of one returns
// order wait for one another on its row lock and confirm it once.
func TestReturnsOrderConfirmationsRacing(t *testing.T) {
	e := setup(t)
	_, doc, _ := e.postedOrder(t, []string{e.item(line{quantity: "3"})}, "1 3 EA")
	got, _ := e.Must(201, "POST", returnsOrders, returning(doc, "defective", "1 1 EA"), "")
	id := got["id"].(string)
	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Go(func() {
			codes[i] = e.API.Do(apitest.Req{Method: "POST", Path: returnsOrders + "/" + id + ":confirm", Tenant: e.Tenant.ID, As: e.Tenant.Human,
				Key: apitest.NewKey(), IfMatch: `"1"`}).Code
		})
	}
	wg.Wait()
	slices.Sort(codes)
	if !slices.Equal(codes, []int{200, 412}) {
		t.Errorf("statuses: got %v, want one 200 and one 412", codes)
	}
	if ev := e.events(t, "returns_order_changed", "returns_order_id", id); len(ev) != 2 {
		t.Errorf("ReturnsOrderChanged events: got %d, want 2 (created, confirmed once)", len(ev))
	}
}

// TestReturnsOrderLeavesCreditAndRevenueAlone: a confirmed returns order
// counts nowhere in the customer's credit exposure and publishes no
// SalesOrderChanged, the event the revenue contract consumer reads, so
// it gets no revenue contract and changes none (ADR-0067, As built
// T85a): the credit for a return is a credit memo's, in receivables.
func TestReturnsOrderLeavesCreditAndRevenueAlone(t *testing.T) {
	e := setup(t)
	customer := e.buyer(t, "BP70000071", "1000.00")
	placed := e.placed(t, customer, e.item(line{quantity: "20", code: "A1"}))
	order, today := placed["id"].(string), placed["order_date"].(string)
	e.Must(201, "POST", deliveries(order), delivered(today, "1 5 EA"), "")
	doc, _ := e.Must(201, "POST", billings, bill(order, ""), "")
	e.Must(200, "POST", posting(doc["id"].(string)), "", `"1"`)
	// 5 of 20 EA are billed and posted: 59.50 gross is the open item, 178.50 stays open on the order.
	const want = "59.50 EUR + 1 order(s) 178.50 EUR = 238.00 EUR, within true"
	if x := e.exposed(t, customer); x != want {
		t.Fatalf("exposure before the returns order: got %s, want %s", x, want)
	}
	changed := len(e.events(t, "sales_order_changed", "sales_order_id", order))
	draft, _ := e.Must(201, "POST", returnsOrders, returning(doc["id"].(string), "defective", "1 5 EA"), "")
	if x := e.exposed(t, customer); x != want {
		t.Errorf("exposure with a draft returns order: got %s, want %s", x, want)
	}
	e.Must(200, "POST", returnsOrders+"/"+draft["id"].(string)+":confirm", "", `"1"`)
	if x := e.exposed(t, customer); x != want {
		t.Errorf("exposure with a confirmed returns order: got %s, want %s", x, want)
	}
	if n := len(e.events(t, "sales_order_changed", "sales_order_id", order)); n != changed {
		t.Errorf("SalesOrderChanged events of the sales order: got %d, want %d as before the returns order", n, changed)
	}
	var others int
	pgtest.Scan(t, `SELECT count(*) FROM sales.outbox WHERE tenant_id = $1 AND aggregate_id = $2 AND event_type <> 'com.example.erp.sales.returns_order_changed.v1'`,
		[]any{e.Tenant.ID, draft["id"]}, &others)
	if others != 0 {
		t.Errorf("events of the returns order other than ReturnsOrderChanged: got %d, want none", others)
	}
}

// TestReturnsOrderDatedInTheCompanysZone: a returns order is dated the
// company's date in its time zone, not UTC's.
func TestReturnsOrderDatedInTheCompanysZone(t *testing.T) {
	e := setup(t)
	_, doc, _ := e.postedOrder(t, []string{e.item(line{quantity: "3"})}, "1 3 EA")
	// 03:00 UTC on 10 March is still 9 March in Toronto.
	h := endpoints(t, instant(t, "2026-03-10T03:00:00Z"))
	err := e.inRequest(t, func(ctx context.Context) error {
		r, err := h.MakeReturns(ctx, sales.ReturnsInput{BillingDocumentID: uuid.MustParse(doc), Reason: "other",
			Items: []sales.ItemQuantity{{ItemNo: 1, Quantity: "1", Unit: "EA"}}})
		if err != nil {
			return err
		}
		if d := r.ReturnsDate.Format(time.DateOnly); d != "2026-03-09" {
			t.Errorf("returns date: got %s, want 2026-03-09, Toronto's date", d)
		}
		if r.ReasonText != nil {
			t.Errorf("reason text: got %q, want none", *r.ReasonText)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestReturnsOrdersListed: returns orders are listed by date and by
// number, in pages, filtered by company, customer, billing document,
// sales order, status and number, each as it reads.
func TestReturnsOrdersListed(t *testing.T) {
	e := setup(t)
	order, doc, _ := e.postedOrder(t, []string{e.item(line{quantity: "5"})}, "1 5 EA")
	other := e.buyer(t, "BP70000072", "")
	placed := e.placed(t, other, e.item(line{quantity: "5"}))
	e.Must(201, "POST", deliveries(placed["id"].(string)), delivered(placed["order_date"].(string), "1 5 EA"), "")
	theirs, _ := e.Must(201, "POST", billings, bill(placed["id"].(string), ""), "")
	e.Must(200, "POST", posting(theirs["id"].(string)), "", `"1"`)
	first := e.returnedOrder(t, doc, "1 1 EA")
	second, _ := e.Must(201, "POST", returnsOrders, returning(doc, "not_wanted", "1 2 EA"), "")
	third := e.returnedOrder(t, theirs["id"].(string), "1 5 EA")
	numbers := func(t *testing.T, query string) (string, string) {
		t.Helper()
		got, _ := e.In(t).Must(200, "GET", returnsOrders+"?"+query, "", "")
		var out []string
		for _, it := range items(t, got) {
			out = append(out, fmt.Sprint(it["document_number"]))
		}
		token, _ := got["next_page_token"].(string)
		return strings.Join(out, " "), token
	}
	for _, c := range []struct{ name, query, want string }{
		{"by date, then in the order they were created", "", "RO100000 RO100001 RO100002"},
		{"by number, the latest first", "order_by=document_number%20desc", "RO100002 RO100001 RO100000"},
		{"a company's", "legal_entity_id=" + e.DE.String(), "RO100000 RO100001 RO100002"},
		{"another company's", "legal_entity_id=" + e.US.String(), ""},
		{"a customer's", "customer_id=" + other.String(), "RO100002"},
		{"a billing document's", "billing_document_id=" + doc, "RO100000 RO100001"},
		{"a sales order's", "sales_order_id=" + order, "RO100000 RO100001"},
		{"the drafts", "status=draft", "RO100001"},
		{"the confirmed", "status=confirmed", "RO100000 RO100002"},
		{"by its number", "document_number=RO100001", "RO100001"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got, _ := numbers(t, c.query); got != c.want {
				t.Errorf("returns orders: got %s, want %s", got, c.want)
			}
		})
	}
	t.Run("in pages", func(t *testing.T) {
		// The three are dated the same day: the id breaks the tie, so the pages hold each once in either order.
		for _, order := range []string{"", "&order_by=document_number"} {
			page, token := numbers(t, "page_size=2"+order)
			if token == "" {
				t.Fatalf("first page (%s): got no next_page_token, want one", order)
			}
			rest, last := numbers(t, "page_size=2"+order+"&page_token="+token)
			if got := page + " " + rest; got != "RO100000 RO100001 RO100002" {
				t.Errorf("the two pages (%s): got %s, want RO100000 RO100001 RO100002", order, got)
			}
			if last != "" {
				t.Errorf("the last page's next_page_token (%s): got %q, want none", order, last)
			}
		}
	})
	t.Run("each as it reads", func(t *testing.T) {
		got, _ := e.In(t).Must(200, "GET", returnsOrders, "", "")
		var want []any
		for _, id := range []string{first, second["id"].(string), third} {
			read, _ := e.In(t).Must(200, "GET", returnsOrders+"/"+id, "", "")
			want = append(want, read)
		}
		if a, b := mustJSON(t, got["items"]), mustJSON(t, want); a != b {
			t.Errorf("listed: got %s, want %s", a, b)
		}
	})
	t.Run("an unknown sort field", func(t *testing.T) {
		e.In(t).Problem(400, "request.invalid_order_by", "GET", returnsOrders+"?order_by=reason", "", "")
	})
}
