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

// confirmed creates an order of the items directly and confirms it; it
// returns the order's id and its date, the company's today.
func (e env) confirmed(t *testing.T, items ...string) (id, date string) {
	t.Helper()
	got, _ := e.Must(201, "POST", "/v1/sales-orders", e.direct(e.customer, "", items...), "")
	id = got["id"].(string)
	e.Must(200, "POST", "/v1/sales-orders/"+id+":confirm", "", `"1"`)
	return id, got["order_date"].(string)
}

// delivered is the JSON of a delivery on the date; each item is "the
// order item's number, the quantity, the unit".
func delivered(date string, items ...string) string {
	parts := make([]string, len(items))
	for i, it := range items {
		f := strings.Split(it, " ")
		parts[i] = fmt.Sprintf(`{"item_no": %s, "quantity": {"value": %q, "unit": %q}}`, f[0], f[1], f[2])
	}
	return fmt.Sprintf(`{"delivery_date": %q, "items": [%s]}`, date, strings.Join(parts, ", "))
}

func deliveries(id string) string { return "/v1/sales-orders/" + id + "/deliveries" }

// quantities are the field of each of the order's items, in item order.
func (e env) quantities(t *testing.T, order, field string) string {
	t.Helper()
	got, _ := e.Must(200, "GET", "/v1/sales-orders/"+order, "", "")
	var out []string
	for _, it := range items(t, got) {
		out = append(out, quantity(t, it, field))
	}
	return strings.Join(out, ", ")
}

func (e env) journalEntries(t *testing.T) int {
	t.Helper()
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM ledger.journal_entry WHERE tenant_id = $1`, []any{e.Tenant.ID}, &n)
	return n
}

// TestDeliveryConfirmed: quantities of a confirmed order's items are
// confirmed as delivered on a date; the delivery reads back with the
// order's items; the order shows what is delivered at a new version; the
// audit record and SalesOrderDelivered are written; no stock moves and
// nothing is posted (T78c acceptance).
func TestDeliveryConfirmed(t *testing.T) {
	e := setup(t)
	id, today := e.confirmed(t, e.item(line{quantity: "2.5", unit: "KG", price: "10.01", key: "BRACKET-M8"}), e.item(line{quantity: "3"}))
	order, _ := e.Must(200, "GET", "/v1/sales-orders/"+id, "", "")
	got, _ := e.Must(201, "POST", deliveries(id), delivered(today, "2 3 EA", "1 1.5 KG"), "")
	fields(t, got, map[string]any{"delivery_no": float64(1), "sales_order_id": id, "sales_order_number": order["document_number"],
		"delivery_date": today, "created_by": e.Tenant.Human.ID.String()})
	its := items(t, got)
	if len(its) != 2 {
		t.Fatalf("items: got %d, want 2", len(its))
	}
	for i, want := range []struct {
		no       float64
		key      any
		quantity string
	}{{1, "BRACKET-M8", "1.5 KG"}, {2, nil, "3 EA"}} {
		t.Run(fmt.Sprintf("item %d", i+1), func(t *testing.T) {
			fields(t, its[i], map[string]any{"item_no": want.no, "item_key": want.key, "description": "Machined bracket"})
			if q := quantity(t, its[i], "quantity"); q != want.quantity {
				t.Errorf("quantity: got %s, want %s", q, want.quantity)
			}
		})
	}
	t.Run("the order shows it", func(t *testing.T) {
		read, tag := e.In(t).Must(200, "GET", "/v1/sales-orders/"+id, "", "")
		if tag != `"3"` {
			t.Errorf("the order's ETag: got %s, want \"3\" (created, confirmed, delivered)", tag)
		}
		fields(t, read, map[string]any{"status": "confirmed", "version": float64(3)})
		if d := e.quantities(t, id, "delivered_quantity"); d != "1.5 KG, 3 EA" {
			t.Errorf("delivered: got %s, want 1.5 KG, 3 EA", d)
		}
		if b := e.quantities(t, id, "billed_quantity"); b != "0 KG, 0 EA" {
			t.Errorf("billed: got %s, want 0 KG, 0 EA", b)
		}
	})
	t.Run("audited and published", func(t *testing.T) {
		if n := e.audits(t, "sales.delivery.confirm", got["id"].(string)); n != 1 {
			t.Errorf("audit records of the delivery: got %d, want 1", n)
		}
		qty := func(v, u string) map[string]any { return map[string]any{"value": v, "unit": u} }
		want := map[string]any{"delivery_id": got["id"], "delivery_no": float64(1), "sales_order_id": id, "document_number": order["document_number"],
			"legal_entity_id": e.DE.String(), "legal_entity_code": "2000", "customer_id": e.customer.String(), "customer_number": "BP70000001",
			"delivery_date": today, "version": float64(3), "items": []any{
				map[string]any{"item_no": float64(1), "quantity": qty("1.5", "KG"), "delivered_quantity": qty("1.5", "KG")},
				map[string]any{"item_no": float64(2), "quantity": qty("3", "EA"), "delivered_quantity": qty("3", "EA")}}}
		if a, b := mustJSON(t, e.events(t, "sales_order_delivered", "sales_order_id", id)), mustJSON(t, []map[string]any{want}); a != b {
			t.Errorf("SalesOrderDelivered: got %s, want %s", a, b)
		}
		apitest.NeverPublished(t, e.Tenant, "sales", "Northwind Traders")
	})
	t.Run("the rest, then nothing is open", func(t *testing.T) {
		second, _ := e.In(t).Must(201, "POST", deliveries(id), delivered(today, "1 1 KG"), "")
		if second["delivery_no"] != float64(2) {
			t.Errorf("the second delivery's number: got %v, want 2", second["delivery_no"])
		}
		if d := e.quantities(t, id, "delivered_quantity"); d != "2.5 KG, 3 EA" {
			t.Errorf("delivered: got %s, want 2.5 KG, 3 EA, all that was ordered", d)
		}
		refused := e.In(t).Problem(422, "sales.exceeds_open_quantity", "POST", deliveries(id), delivered(today, "2 0.000001 EA"), "")
		if f := field(refused); f != "items[0].quantity.value" {
			t.Errorf("field: got %v, want items[0].quantity.value", f)
		}
		if d, _ := refused["detail"].(string); !strings.Contains(d, "Item 2 of sales order "+order["document_number"].(string)+" has 0 EA open") {
			t.Errorf("detail: got %q, want it to name item 2 and its open quantity", d)
		}
	})
	if n := e.journalEntries(t); n != 0 {
		t.Errorf("journal entries: got %d, want none (nothing is posted)", n)
	}
}

// TestDeliveryIdempotent: a replay answers the same delivery and delivers
// nothing more.
func TestDeliveryIdempotent(t *testing.T) {
	e := setup(t)
	id, today := e.confirmed(t, e.item(line{quantity: "3"}))
	req := apitest.Req{Method: "POST", Path: deliveries(id), Body: delivered(today, "1 1 EA"), Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey()}
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
	if d := e.quantities(t, id, "delivered_quantity"); d != "1 EA" {
		t.Errorf("delivered after the replay: got %s, want 1 EA", d)
	}
	req.Key = ""
	if rec := e.API.Do(req); rec.Code != 428 {
		t.Errorf("without an Idempotency-Key: got %d, want 428: %s", rec.Code, rec.Body)
	}
}

// TestDeliveryRefusals: only a confirmed order takes a delivery, dated no
// later than the company's today, of its own items, each once, in its
// unit, up to what is open; every refusal names the request's field and
// leaves nothing delivered.
func TestDeliveryRefusals(t *testing.T) {
	e := setup(t)
	id, today := e.confirmed(t, e.item(line{quantity: "2.5", unit: "KG"}), e.item(line{quantity: "3"}))
	draft := e.ordered(t)
	cases := []struct {
		name, order, body string
		status            int
		code              string
		field             any
	}{
		{"a draft order", draft, delivered(today, "1 1 EA"), 409, "sales.order_not_confirmed", nil},
		{"an unknown order", uuid.NewString(), delivered(today, "1 1 EA"), 404, "request.not_found", nil},
		{"a date after today", id, delivered(far, "1 1 KG"), 422, "sales.delivery_date_in_future", "delivery_date"},
		{"an item the order does not have", id, delivered(today, "1 1 KG", "3 1 EA"), 422, "sales.unknown_order_item", "items[1].item_no"},
		{"an item named twice", id, delivered(today, "2 1 EA", "2 1 EA"), 422, "sales.item_repeated", "items[1].item_no"},
		{"a quantity of zero", id, delivered(today, "1 0 KG"), 422, "sales.invalid_quantity", "items[0].quantity.value"},
		{"a seventh decimal", id, delivered(today, "1 0.0000001 KG"), 400, "request.invalid_body", nil},
		{"another unit", id, delivered(today, "1 1 EA"), 422, "sales.unit_mismatch", "items[0].quantity.unit"},
		{"more than was ordered", id, delivered(today, "1 2.5 KG", "2 3.000001 EA"), 422, "sales.exceeds_open_quantity", "items[1].quantity.value"},
		{"no items", id, delivered(today), 400, "request.invalid_body", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := e.In(t).Problem(c.status, c.code, "POST", deliveries(c.order), c.body, "")
			if f := field(got); f != c.field {
				t.Errorf("field: got %v, want %v", f, c.field)
			}
		})
	}
	if d := e.quantities(t, id, "delivered_quantity"); d != "0 KG, 0 EA" {
		t.Errorf("delivered after the refusals: got %s, want nothing", d)
	}
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM sales.delivery WHERE tenant_id = $1`, []any{e.Tenant.ID}, &n)
	if n != 0 {
		t.Errorf("deliveries after the refusals: got %d, want none", n)
	}
	t.Run("no items at the port", func(t *testing.T) {
		// The contract refuses an empty list before the handler; the port refuses it too.
		err := e.inRequest(t, func(ctx context.Context) error {
			_, err := endpoints(t, time.Now()).ConfirmDelivery(ctx, uuid.MustParse(id), sales.DeliveryInput{Date: instant(t, today+"T00:00:00Z")})
			return err
		})
		refusal(t, err, 422, "sales.items_required")
	})
	t.Run("another company's order", func(t *testing.T) {
		reader := e.As(e.Reader(t, "US sales", e.US))
		reader.In(t).Problem(404, "request.not_found", "POST", deliveries(id), delivered(today, "1 1 KG"), "")
		reader.In(t).Problem(404, "request.not_found", "GET", deliveries(id), "", "")
	})
	t.Run("without the permission to confirm deliveries", func(t *testing.T) {
		pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = 'sales.delivery.confirm'`, e.Tenant.ID)
		e.In(t).Problem(403, "identity.permission_missing", "POST", deliveries(id), delivered(today, "1 1 KG"), "")
	})
}

// TestDeliveriesRacing: two deliveries racing for an item's last quantity
// wait for one another on the order's row lock and deliver it once
// (ADR-0008; T78c acceptance).
func TestDeliveriesRacing(t *testing.T) {
	e := setup(t)
	id, today := e.confirmed(t, e.item(line{quantity: "3"}))
	e.Must(201, "POST", deliveries(id), delivered(today, "1 1 EA"), "")
	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Go(func() {
			codes[i] = e.API.Do(apitest.Req{Method: "POST", Path: deliveries(id), Body: delivered(today, "1 2 EA"), Tenant: e.Tenant.ID,
				As: e.Tenant.Human, Key: apitest.NewKey()}).Code
		})
	}
	wg.Wait()
	// The second waits for the first's lock on the order and then finds nothing open.
	slices.Sort(codes)
	if !slices.Equal(codes, []int{201, 422}) {
		t.Errorf("statuses: got %v, want one 201 and one 422", codes)
	}
	if d := e.quantities(t, id, "delivered_quantity"); d != "3 EA" {
		t.Errorf("delivered: got %s, want 3 EA, the last two delivered once", d)
	}
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM sales.delivery WHERE tenant_id = $1 AND sales_order_id = $2`, []any{e.Tenant.ID, id}, &n)
	if n != 2 {
		t.Errorf("deliveries of the order: got %d, want 2", n)
	}
	if ev := e.events(t, "sales_order_delivered", "sales_order_id", id); len(ev) != 2 {
		t.Errorf("SalesOrderDelivered events: got %d, want 2", len(ev))
	}
}

// TestDeliveryDatedInTheCompanysZone: a delivery's date is judged on the
// company's date in its time zone, not UTC's.
func TestDeliveryDatedInTheCompanysZone(t *testing.T) {
	e := setup(t)
	id, _ := e.confirmed(t, e.item(line{quantity: "3"}))
	// 03:00 UTC on 10 March is still 9 March in Toronto.
	h := endpoints(t, instant(t, "2026-03-10T03:00:00Z"))
	deliver := func(date string) error {
		return e.inRequest(t, func(ctx context.Context) error {
			_, err := h.ConfirmDelivery(ctx, uuid.MustParse(id), sales.DeliveryInput{Date: instant(t, date+"T00:00:00Z"),
				Items: []sales.DeliveryItemInput{{ItemNo: 1, Quantity: "1", Unit: "EA"}}})
			return err
		})
	}
	t.Run("UTC's date, tomorrow in Toronto", func(t *testing.T) {
		err := deliver("2026-03-10")
		refusal(t, err, 422, "sales.delivery_date_in_future")
		if d := fmt.Sprint(err); !strings.Contains(d, "2026-03-09, the company's date in America/Toronto") {
			t.Errorf("detail: got %q, want it to name the company's date and zone", d)
		}
	})
	t.Run("Toronto's date", func(t *testing.T) {
		if err := deliver("2026-03-09"); err != nil {
			t.Errorf("got %v, want the delivery confirmed", err)
		}
	})
	t.Run("an earlier day", func(t *testing.T) {
		if err := deliver("2026-03-01"); err != nil {
			t.Errorf("got %v, want the delivery confirmed", err)
		}
	})
}

// TestDeliveriesListed: an order's deliveries are listed by date, in
// pages, each with its items.
func TestDeliveriesListed(t *testing.T) {
	e := setup(t)
	id, today := e.confirmed(t, e.item(line{quantity: "5"}), e.item(line{quantity: "1", unit: "KG"}))
	other, _ := e.confirmed(t, e.item(line{quantity: "5"}))
	day := instant(t, today+"T00:00:00Z")
	earlier, earliest := day.AddDate(0, 0, -1).Format(time.DateOnly), day.AddDate(0, 0, -2).Format(time.DateOnly)
	first, _ := e.Must(201, "POST", deliveries(id), delivered(today, "1 1 EA"), "")
	second, _ := e.Must(201, "POST", deliveries(id), delivered(earliest, "1 2 EA", "2 1 KG"), "")
	third, _ := e.Must(201, "POST", deliveries(id), delivered(earlier, "1 2 EA"), "")
	e.Must(201, "POST", deliveries(other), delivered(today, "1 1 EA"), "")
	nos := func(t *testing.T, query string) (string, string) {
		t.Helper()
		got, _ := e.In(t).Must(200, "GET", deliveries(id)+"?"+query, "", "")
		var out []string
		for _, it := range items(t, got) {
			out = append(out, fmt.Sprint(it["delivery_no"]))
		}
		token, _ := got["next_page_token"].(string)
		return strings.Join(out, " "), token
	}
	for _, c := range []struct{ name, query, want string }{
		{"by date", "", "2 3 1"},
		{"the latest first", "order_by=delivery_date%20desc", "1 3 2"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got, _ := nos(t, c.query); got != c.want {
				t.Errorf("deliveries: got %s, want %s", got, c.want)
			}
		})
	}
	t.Run("in pages", func(t *testing.T) {
		page, token := nos(t, "page_size=2")
		if token == "" {
			t.Fatal("first page: got no next_page_token, want one")
		}
		rest, last := nos(t, "page_size=2&page_token="+token)
		if got := page + " " + rest; got != "2 3 1" {
			t.Errorf("the two pages: got %s, want 2 3 1", got)
		}
		if last != "" {
			t.Errorf("the last page's next_page_token: got %q, want none", last)
		}
	})
	t.Run("each as it was answered", func(t *testing.T) {
		got, _ := e.In(t).Must(200, "GET", deliveries(id), "", "")
		if a, b := mustJSON(t, got["items"]), mustJSON(t, []any{second, third, first}); a != b {
			t.Errorf("listed: got %s, want %s", a, b)
		}
	})
	t.Run("an unknown sort field", func(t *testing.T) {
		e.In(t).Problem(400, "request.invalid_order_by", "GET", deliveries(id)+"?order_by=delivery_no", "", "")
	})
	t.Run("an unknown order", func(t *testing.T) {
		e.In(t).Problem(404, "request.not_found", "GET", deliveries(uuid.NewString()), "", "")
	})
}

// TestOrdersPublishedDeliveries: sales.Orders gives each item's
// deliveries, by date, with their quantities (what T81c reads); the API's
// own read of an order leaves them out.
func TestOrdersPublishedDeliveries(t *testing.T) {
	e := setup(t)
	id, today := e.confirmed(t, e.item(line{quantity: "5"}), e.item(line{quantity: "1", unit: "KG"}))
	day := instant(t, today+"T00:00:00Z")
	earlier := day.AddDate(0, 0, -1).Format(time.DateOnly)
	e.Must(201, "POST", deliveries(id), delivered(today, "1 1 EA"), "")
	e.Must(201, "POST", deliveries(id), delivered(earlier, "1 2.5 EA"), "")
	told := func(o sales.Order) string {
		var out []string
		for _, it := range o.Items {
			for _, d := range it.Deliveries {
				out = append(out, fmt.Sprintf("item %d: %s on %s", it.ItemNo, d.Quantity, d.Date.Format(time.RFC3339)))
			}
		}
		return strings.Join(out, "; ")
	}
	err := e.inRequest(t, func(ctx context.Context) error {
		o, ok, err := sales.Orders{}.Order(ctx, uuid.MustParse(id))
		if err != nil {
			return err
		}
		if !ok {
			t.Fatal("got no order, want the order delivered")
		}
		if got, want := told(o), "item 1: 2.5 on "+earlier+"T00:00:00Z; item 1: 1 on "+today+"T00:00:00Z"; got != want {
			t.Errorf("deliveries: got %s, want %s", got, want)
		}
		if d := o.Items[0].Delivered; d != "3.5" {
			t.Errorf("item 1's delivered quantity: got %s, want 3.5", d)
		}
		if d := o.Items[1].Deliveries; d == nil {
			t.Error("item 2's deliveries: got nil, want an empty list read")
		}
		if n := len(o.Items[1].Deliveries); n != 0 {
			t.Errorf("item 2's deliveries: got %d, want none", n)
		}
		read, err := sales.GetOrder(ctx, uuid.MustParse(id))
		if err != nil {
			return err
		}
		if got := told(read); got != "" {
			t.Errorf("the API's read of the order: got deliveries %s, want none read", got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
