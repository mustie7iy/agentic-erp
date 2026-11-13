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

func receipts(id string) string { return returnsOrders + "/" + id + "/receipts" }

// received is the JSON of a receipt on the date; each item is "the
// returns order item's number, the quantity, the unit".
func received(date string, items ...string) string {
	parts := make([]string, len(items))
	for i, it := range items {
		f := strings.Split(it, " ")
		parts[i] = fmt.Sprintf(`{"item_no": %s, "quantity": {"value": %q, "unit": %q}}`, f[0], f[1], f[2])
	}
	return fmt.Sprintf(`{"receipt_date": %q, "items": [%s]}`, date, strings.Join(parts, ", "))
}

// receivedQuantities are the field of each of the returns order's items,
// in item order.
func (e env) receivedQuantities(t *testing.T, returns, field string) string {
	t.Helper()
	got, _ := e.Must(200, "GET", returnsOrders+"/"+returns, "", "")
	var out []string
	for _, it := range items(t, got) {
		out = append(out, quantity(t, it, field))
	}
	return strings.Join(out, ", ")
}

// receiptsCount counts the tenant's return receipts.
func (e env) receiptsCount(t *testing.T) int {
	t.Helper()
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM sales.return_receipt WHERE tenant_id = $1`, []any{e.Tenant.ID}, &n)
	return n
}

// TestReturnReceiptConfirmed: quantities of a confirmed returns order's
// items are confirmed as received on a date; the receipt reads back with
// the returns order's items; the returns order shows what is received
// and what is still expected at a new version; the sales order's
// delivered, billed and returned quantities and its version do not
// change; the audit record and ReturnReceived are written; nothing is
// posted (T85b acceptance).
func TestReturnReceiptConfirmed(t *testing.T) {
	e := setup(t)
	order, doc, today := e.postedOrder(t, []string{e.item(line{quantity: "2.5", unit: "KG", price: "10.01", key: "BRACKET-M8"}), e.item(line{quantity: "3"})},
		"1 2.5 KG", "2 3 EA")
	id := e.returnedOrder(t, doc, "2 3 EA", "1 1.5 KG")
	returns, _ := e.Must(200, "GET", returnsOrders+"/"+id, "", "")
	sold, soldTag := e.Must(200, "GET", "/v1/sales-orders/"+order, "", "")
	entries := e.journalEntries(t)
	for f, want := range map[string]string{"received_quantity": "0 EA, 0 KG", "open_quantity": "3 EA, 1.5 KG"} {
		if q := e.receivedQuantities(t, id, f); q != want {
			t.Errorf("%s before a receipt: got %s, want %s", f, q, want)
		}
	}
	got, _ := e.Must(201, "POST", receipts(id), received(today, "2 0.5 KG", "1 3 EA"), "")
	fields(t, got, map[string]any{"receipt_no": float64(1), "returns_order_id": id, "returns_order_number": returns["document_number"],
		"receipt_date": today, "created_by": e.Tenant.Human.ID.String()})
	its := items(t, got)
	if len(its) != 2 {
		t.Fatalf("items: got %d, want 2", len(its))
	}
	for i, want := range []struct {
		billing  float64
		key      any
		quantity string
	}{{2, nil, "3 EA"}, {1, "BRACKET-M8", "0.5 KG"}} {
		t.Run(fmt.Sprintf("item %d", i+1), func(t *testing.T) {
			fields(t, its[i], map[string]any{"item_no": float64(i + 1), "billing_item_no": want.billing, "item_key": want.key, "description": "Machined bracket"})
			if q := quantity(t, its[i], "quantity"); q != want.quantity {
				t.Errorf("quantity: got %s, want %s", q, want.quantity)
			}
		})
	}
	t.Run("the returns order shows it", func(t *testing.T) {
		read, tag := e.In(t).Must(200, "GET", returnsOrders+"/"+id, "", "")
		if tag != `"3"` {
			t.Errorf("the returns order's ETag: got %s, want \"3\" (created, confirmed, received)", tag)
		}
		fields(t, read, map[string]any{"status": "confirmed", "version": float64(3)})
		for f, want := range map[string]string{"received_quantity": "3 EA, 0.5 KG", "open_quantity": "0 EA, 1 KG", "quantity": "3 EA, 1.5 KG",
			"billed_quantity": "3 EA, 2.5 KG"} {
			if q := e.receivedQuantities(t, id, f); q != want {
				t.Errorf("%s: got %s, want %s", f, q, want)
			}
		}
	})
	t.Run("the sales order is as it was", func(t *testing.T) {
		read, tag := e.In(t).Must(200, "GET", "/v1/sales-orders/"+order, "", "")
		if tag != soldTag {
			t.Errorf("the sales order's ETag: got %s, want %s as before the receipt", tag, soldTag)
		}
		if a, b := mustJSON(t, read["items"]), mustJSON(t, sold["items"]); a != b {
			t.Errorf("the sales order's items: got %s, want %s as before the receipt", a, b)
		}
	})
	t.Run("audited and published", func(t *testing.T) {
		if n := e.audits(t, "sales.return_receipt.confirm", got["id"].(string)); n != 1 {
			t.Errorf("audit records of the receipt: got %d, want 1", n)
		}
		qty := func(v, u string) map[string]any { return map[string]any{"value": v, "unit": u} }
		want := map[string]any{"receipt_id": got["id"], "receipt_no": float64(1), "returns_order_id": id, "document_number": returns["document_number"],
			"legal_entity_id": e.DE.String(), "legal_entity_code": "2000", "customer_id": e.customer.String(), "customer_number": "BP70000001",
			"billing_document_id": doc, "billing_document_number": returns["billing_document_number"], "sales_order_id": order,
			"sales_order_number": returns["sales_order_number"], "receipt_date": today, "version": float64(3), "items": []any{
				map[string]any{"item_no": float64(1), "billing_item_no": float64(2), "quantity": qty("3", "EA"), "received_quantity": qty("3", "EA")},
				map[string]any{"item_no": float64(2), "billing_item_no": float64(1), "quantity": qty("0.5", "KG"), "received_quantity": qty("0.5", "KG")}}}
		if a, b := mustJSON(t, e.events(t, "return_received", "returns_order_id", id)), mustJSON(t, []map[string]any{want}); a != b {
			t.Errorf("ReturnReceived: got %s, want %s", a, b)
		}
		if n := len(e.events(t, "returns_order_changed", "returns_order_id", id)); n != 2 {
			t.Errorf("ReturnsOrderChanged events: got %d, want 2 (created, confirmed; a receipt publishes none)", n)
		}
		apitest.NeverPublished(t, e.Tenant, "sales", "Northwind Traders")
	})
	t.Run("the rest, then nothing is expected", func(t *testing.T) {
		second, _ := e.In(t).Must(201, "POST", receipts(id), received(today, "2 1 KG"), "")
		if second["receipt_no"] != float64(2) {
			t.Errorf("the second receipt's number: got %v, want 2", second["receipt_no"])
		}
		for f, want := range map[string]string{"received_quantity": "3 EA, 1.5 KG", "open_quantity": "0 EA, 0 KG"} {
			if q := e.receivedQuantities(t, id, f); q != want {
				t.Errorf("%s: got %s, want %s, all that is returned", f, q, want)
			}
		}
		refused := e.In(t).Problem(422, "sales.exceeds_open_quantity", "POST", receipts(id), received(today, "2 0.000001 KG"), "")
		if f := field(refused); f != "items[0].quantity.value" {
			t.Errorf("field: got %v, want items[0].quantity.value", f)
		}
		want := "Item 2 of returns order " + returns["document_number"].(string) + " has 0 KG still expected (1.5 returned, 1.5 received); 0.000001 is more."
		if d, _ := refused["detail"].(string); d != want {
			t.Errorf("detail: got %q, want %q", d, want)
		}
	})
	if n := e.journalEntries(t); n != entries {
		t.Errorf("journal entries: got %d, want %d as before the receipts (nothing is posted)", n, entries)
	}
}

// TestReturnReceiptIdempotent: a replay answers the same receipt and
// receives nothing more.
func TestReturnReceiptIdempotent(t *testing.T) {
	e := setup(t)
	_, doc, today := e.postedOrder(t, []string{e.item(line{quantity: "3"})}, "1 3 EA")
	id := e.returnedOrder(t, doc, "1 3 EA")
	req := apitest.Req{Method: "POST", Path: receipts(id), Body: received(today, "1 1 EA"), Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey()}
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
	if q := e.receivedQuantities(t, id, "received_quantity"); q != "1 EA" {
		t.Errorf("received after the replay: got %s, want 1 EA", q)
	}
	if n := e.receiptsCount(t); n != 1 {
		t.Errorf("receipts after the replay: got %d, want 1", n)
	}
	req.Key = ""
	if rec := e.API.Do(req); rec.Code != 428 {
		t.Errorf("without an Idempotency-Key: got %d, want 428: %s", rec.Code, rec.Body)
	}
}

// TestReturnReceiptRefusals: only a confirmed returns order takes a
// receipt, dated no later than the company's today, of its own items,
// each once, in its unit, up to what is open; every refusal names the
// request's field and leaves nothing received.
func TestReturnReceiptRefusals(t *testing.T) {
	e := setup(t)
	_, doc, today := e.postedOrder(t, []string{e.item(line{quantity: "2.5", unit: "KG"}), e.item(line{quantity: "3"})}, "1 2.5 KG", "2 3 EA")
	id := e.returnedOrder(t, doc, "1 2 KG", "2 2 EA")
	created, _ := e.Must(201, "POST", returnsOrders, returning(doc, "defective", "2 1 EA"), "")
	draft := created["id"].(string)
	cases := []struct {
		name, returns, body string
		status              int
		code                string
		field               any
	}{
		{"a draft returns order", draft, received(today, "1 1 EA"), 409, "sales.returns_order_not_confirmed", nil},
		{"an unknown returns order", uuid.NewString(), received(today, "1 1 EA"), 404, "request.not_found", nil},
		{"a date after today", id, received(far, "1 1 KG"), 422, "sales.receipt_date_in_future", "receipt_date"},
		{"an item the returns order does not have", id, received(today, "1 1 KG", "3 1 EA"), 422, "sales.unknown_returns_item", "items[1].item_no"},
		{"an item named twice", id, received(today, "2 1 EA", "2 1 EA"), 422, "sales.item_repeated", "items[1].item_no"},
		{"a quantity of zero", id, received(today, "1 0 KG"), 422, "sales.invalid_quantity", "items[0].quantity.value"},
		{"a seventh decimal", id, received(today, "1 0.0000001 KG"), 400, "request.invalid_body", nil},
		{"another unit", id, received(today, "1 1 EA"), 422, "sales.unit_mismatch", "items[0].quantity.unit"},
		{"more than is returned", id, received(today, "1 2 KG", "2 2.000001 EA"), 422, "sales.exceeds_open_quantity", "items[1].quantity.value"},
		{"no items", id, received(today), 400, "request.invalid_body", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := e.In(t).Problem(c.status, c.code, "POST", receipts(c.returns), c.body, "")
			if f := field(got); f != c.field {
				t.Errorf("field: got %v, want %v", f, c.field)
			}
		})
	}
	if q := e.receivedQuantities(t, id, "received_quantity"); q != "0 KG, 0 EA" {
		t.Errorf("received after the refusals: got %s, want nothing", q)
	}
	if n := e.receiptsCount(t); n != 0 {
		t.Errorf("receipts after the refusals: got %d, want none", n)
	}
	t.Run("no items at the port", func(t *testing.T) {
		// The contract refuses an empty list before the handler; the port refuses it too.
		err := e.inRequest(t, func(ctx context.Context) error {
			_, err := endpoints(t, time.Now()).ConfirmReceipt(ctx, uuid.MustParse(id), sales.ReceiptInput{Date: instant(t, today+"T00:00:00Z")})
			return err
		})
		refusal(t, err, 422, "sales.items_required")
	})
	t.Run("another company's returns order", func(t *testing.T) {
		reader := e.As(e.Reader(t, "US sales", e.US))
		reader.In(t).Problem(404, "request.not_found", "POST", receipts(id), received(today, "1 1 KG"), "")
		reader.In(t).Problem(404, "request.not_found", "GET", receipts(id), "", "")
	})
	t.Run("without the permission to confirm receipts", func(t *testing.T) {
		pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = 'sales.return.receive'`, e.Tenant.ID)
		e.In(t).Problem(403, "identity.permission_missing", "POST", receipts(id), received(today, "1 1 KG"), "")
	})
}

// TestReturnReceiptsRacing: two receipts racing for an item's last
// quantity wait for one another on the returns order's row lock and
// receive it once (ADR-0008; T85b acceptance).
func TestReturnReceiptsRacing(t *testing.T) {
	e := setup(t)
	_, doc, today := e.postedOrder(t, []string{e.item(line{quantity: "3"})}, "1 3 EA")
	id := e.returnedOrder(t, doc, "1 3 EA")
	e.Must(201, "POST", receipts(id), received(today, "1 1 EA"), "")
	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Go(func() {
			codes[i] = e.API.Do(apitest.Req{Method: "POST", Path: receipts(id), Body: received(today, "1 2 EA"), Tenant: e.Tenant.ID,
				As: e.Tenant.Human, Key: apitest.NewKey()}).Code
		})
	}
	wg.Wait()
	// The second waits for the first's lock on the returns order and then finds nothing open.
	slices.Sort(codes)
	if !slices.Equal(codes, []int{201, 422}) {
		t.Errorf("statuses: got %v, want one 201 and one 422", codes)
	}
	if q := e.receivedQuantities(t, id, "received_quantity"); q != "3 EA" {
		t.Errorf("received: got %s, want 3 EA, the last two received once", q)
	}
	if n := e.receiptsCount(t); n != 2 {
		t.Errorf("receipts of the returns order: got %d, want 2", n)
	}
	if ev := e.events(t, "return_received", "returns_order_id", id); len(ev) != 2 {
		t.Errorf("ReturnReceived events: got %d, want 2", len(ev))
	}
}

// TestReturnReceiptDatedInTheCompanysZone: a receipt's date is judged on
// the company's date in its time zone, not UTC's.
func TestReturnReceiptDatedInTheCompanysZone(t *testing.T) {
	e := setup(t)
	_, doc, _ := e.postedOrder(t, []string{e.item(line{quantity: "3"})}, "1 3 EA")
	id := e.returnedOrder(t, doc, "1 3 EA")
	// 03:00 UTC on 10 March is still 9 March in Toronto.
	h := endpoints(t, instant(t, "2026-03-10T03:00:00Z"))
	receive := func(date string) error {
		return e.inRequest(t, func(ctx context.Context) error {
			_, err := h.ConfirmReceipt(ctx, uuid.MustParse(id), sales.ReceiptInput{Date: instant(t, date+"T00:00:00Z"),
				Items: []sales.ItemQuantity{{ItemNo: 1, Quantity: "1", Unit: "EA"}}})
			return err
		})
	}
	t.Run("UTC's date, tomorrow in Toronto", func(t *testing.T) {
		err := receive("2026-03-10")
		refusal(t, err, 422, "sales.receipt_date_in_future")
		if d := fmt.Sprint(err); !strings.Contains(d, "2026-03-09, the company's date in America/Toronto") {
			t.Errorf("detail: got %q, want it to name the company's date and zone", d)
		}
	})
	t.Run("Toronto's date", func(t *testing.T) {
		if err := receive("2026-03-09"); err != nil {
			t.Errorf("got %v, want the receipt confirmed", err)
		}
	})
	t.Run("an earlier day", func(t *testing.T) {
		if err := receive("2026-03-01"); err != nil {
			t.Errorf("got %v, want the receipt confirmed", err)
		}
	})
}

// TestReturnReceiptsListed: a returns order's receipts are listed by
// date, in pages, each with its items.
func TestReturnReceiptsListed(t *testing.T) {
	e := setup(t)
	_, doc, today := e.postedOrder(t, []string{e.item(line{quantity: "5"}), e.item(line{quantity: "1", unit: "KG"})}, "1 5 EA", "2 1 KG")
	id := e.returnedOrder(t, doc, "1 5 EA", "2 1 KG")
	_, otherDoc, _ := e.postedOrder(t, []string{e.item(line{quantity: "5"})}, "1 5 EA")
	other := e.returnedOrder(t, otherDoc, "1 5 EA")
	day := instant(t, today+"T00:00:00Z")
	earlier, earliest := day.AddDate(0, 0, -1).Format(time.DateOnly), day.AddDate(0, 0, -2).Format(time.DateOnly)
	first, _ := e.Must(201, "POST", receipts(id), received(today, "1 1 EA"), "")
	second, _ := e.Must(201, "POST", receipts(id), received(earliest, "1 2 EA", "2 1 KG"), "")
	third, _ := e.Must(201, "POST", receipts(id), received(earlier, "1 2 EA"), "")
	e.Must(201, "POST", receipts(other), received(today, "1 1 EA"), "")
	nos := func(t *testing.T, query string) (string, string) {
		t.Helper()
		got, _ := e.In(t).Must(200, "GET", receipts(id)+"?"+query, "", "")
		var out []string
		for _, it := range items(t, got) {
			out = append(out, fmt.Sprint(it["receipt_no"]))
		}
		token, _ := got["next_page_token"].(string)
		return strings.Join(out, " "), token
	}
	for _, c := range []struct{ name, query, want string }{
		{"by date", "", "2 3 1"},
		{"the latest first", "order_by=receipt_date%20desc", "1 3 2"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got, _ := nos(t, c.query); got != c.want {
				t.Errorf("receipts: got %s, want %s", got, c.want)
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
		got, _ := e.In(t).Must(200, "GET", receipts(id), "", "")
		if a, b := mustJSON(t, got["items"]), mustJSON(t, []any{second, third, first}); a != b {
			t.Errorf("listed: got %s, want %s", a, b)
		}
	})
	t.Run("an unknown sort field", func(t *testing.T) {
		e.In(t).Problem(400, "request.invalid_order_by", "GET", receipts(id)+"?order_by=receipt_no", "", "")
	})
	t.Run("an unknown returns order", func(t *testing.T) {
		e.In(t).Problem(404, "request.not_found", "GET", receipts(uuid.NewString()), "", "")
	})
}
