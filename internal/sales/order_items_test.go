package sales_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestOrderItemsAddedRemoved: in one change a confirmed order takes a new
// item, priced as a new order's, numbered after its highest, loses an
// item of which nothing is delivered, which stays marked with the change
// and out of the order's value, and changes its payment term; the change
// reads back with each item's kind and the term before and after; the
// removed item is not changed or delivered again (T78e acceptance).
func TestOrderItemsAddedRemoved(t *testing.T) {
	e := setup(t)
	id, today := e.confirmed(t, e.item(line{quantity: "3"}), e.item(line{quantity: "2", unit: "KG", price: "5.00"}))
	body := fmt.Sprintf(`{"reason": "Freight instead of the second item", "payment_term_code": "NT14", "removed_items": [2], "new_items": [%s]}`,
		e.item(line{quantity: "4", price: "2.50"}))
	got, tag := e.Must(200, "POST", changing(id), body, `"2"`)
	fields(t, got, map[string]any{"status": "confirmed", "changes": float64(1), "version": float64(3), "payment_term_code": "NT14"})
	if n := amount(t, got, "net_amount"); n != "40.00 EUR" {
		t.Errorf("net value: got %s, want 40.00 EUR, 3 × 10.00 and 4 × 2.50 without the removed 2 × 5.00", n)
	}
	its := items(t, got)
	if len(its) != 3 {
		t.Fatalf("items: got %d, want 3, the removed one kept", len(its))
	}
	fields(t, its[1], map[string]any{"item_no": float64(2), "removed_in_change": float64(1)})
	fields(t, its[2], map[string]any{"item_no": float64(3), "price_source": "manual"})
	if _, ok := its[0]["removed_in_change"]; ok {
		t.Errorf("item 1's removed_in_change: got %v, want none", its[0]["removed_in_change"])
	}
	page, _ := e.Must(200, "GET", "/v1/sales-orders/"+id+"/changes", "", "")
	change := items(t, page)[0]
	fields(t, change, map[string]any{"payment_term_code_before": "NT30", "payment_term_code_after": "NT14"})
	var kinds []string
	for _, it := range items(t, change) {
		kinds = append(kinds, fmt.Sprintf("%v %v %s>%s %s>%s", it["item_no"], it["change"], quantity(t, it, "quantity_before"), quantity(t, it, "quantity_after"),
			amount(t, it, "net_amount_before"), amount(t, it, "net_amount_after")))
	}
	if want := []string{"2 removed 2 KG>0 KG 10.00 EUR>0.00 EUR", "3 added 0 EA>4 EA 0.00 EUR>10.00 EUR"}; !slices.Equal(kinds, want) {
		t.Errorf("items of the change: got %v, want %v", kinds, want)
	}
	for _, c := range []struct{ name, method, path, body, code, field string }{
		{"the removed item changed", "POST", changing(id), changeBody("", itemChange("2 1 KG", "")), "sales.order_item_removed", "items[0].item_no"},
		{"the removed item removed again", "POST", changing(id), `{"removed_items": [2]}`, "sales.order_item_removed", "removed_items[0]"},
		{"the removed item delivered", "POST", deliveries(id), delivered(today, "1 1 EA", "2 1 KG"), "sales.order_item_removed", "items[1].item_no"},
	} {
		t.Run(c.name, func(t *testing.T) {
			refused := e.In(t).Problem(422, c.code, c.method, c.path, c.body, tag)
			if f := field(refused); f != c.field {
				t.Errorf("field: got %v, want %s", f, c.field)
			}
		})
	}
	e.Must(201, "POST", deliveries(id), delivered(today, "1 3 EA", "3 4 EA"), "")
	doc, _ := e.Must(201, "POST", billings, bill(id, ""), "")
	if n := len(items(t, doc)); n != 2 {
		t.Errorf("billed items: got %d, want 2, the removed one not billed", n)
	}
	if n := amount(t, doc, "net_amount"); n != "40.00 EUR" {
		t.Errorf("billed: got %s, want 40.00 EUR", n)
	}
	t.Run("the payment term once billed", func(t *testing.T) {
		refused := e.In(t).Problem(422, "sales.order_billed", "POST", changing(id), `{"payment_term_code": "NT30"}`, e.version(t, id))
		if f := field(refused); f != "payment_term_code" {
			t.Errorf("field: got %v, want payment_term_code", f)
		}
	})
	var audited int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'sales.sales_order.change' AND object_id = $2
		AND outcome <> 'refused' AND detail->>'items_changed' = '2'`, []any{e.Tenant.ID, id}, &audited)
	if audited != 1 {
		t.Errorf("audit records of the change: got %d, want 1", audited)
	}
}

// TestOrderItemsRefused: what adding and removing items and changing the
// payment term cannot do is refused naming the field, and leaves the
// order as it was (T78e acceptance).
func TestOrderItemsRefused(t *testing.T) {
	e := setup(t)
	id, today := e.confirmed(t, e.item(line{quantity: "3"}), e.item(line{quantity: "2", unit: "KG"}))
	many := make([]string, 999)
	for i := range many {
		many[i] = e.item(line{})
	}
	cases := []struct {
		name, body, code, field string
	}{
		{"an unknown item removed", `{"removed_items": [9]}`, "sales.unknown_order_item", "removed_items[0]"},
		{"an item changed and removed", `{"items": [{"item_no": 1, "quantity": {"value": "4", "unit": "EA"}}], "removed_items": [1]}`, "sales.item_repeated",
			"removed_items[0]"},
		{"every item removed", `{"removed_items": [1, 2]}`, "sales.order_needs_item", "removed_items"},
		{"an item added with a quantity of zero", fmt.Sprintf(`{"new_items": [%s]}`, e.item(line{quantity: "0"})), "sales.invalid_quantity",
			"new_items[0].quantity.value"},
		{"an item added on an account not postable", fmt.Sprintf(`{"new_items": [%s, %s]}`, e.item(line{}), e.item(line{account: e.Tenant.ID})),
			"sales.account_not_postable", "new_items[1].account_id"},
		{"an item added in an unknown unit", fmt.Sprintf(`{"new_items": [%s]}`, e.item(line{unit: "XX"})), "sales.unknown_unit", "new_items[0].quantity.unit"},
		{"more items than an order holds", fmt.Sprintf(`{"new_items": [%s]}`, strings.Join(many, ", ")), "sales.too_many_items", "new_items"},
		{"an unknown payment term", `{"payment_term_code": "NT99"}`, "sales.unknown_payment_term", "payment_term_code"},
		{"the payment term it has", `{"payment_term_code": "NT30"}`, "sales.nothing_changed", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			refused := e.In(t).Problem(422, c.code, "POST", changing(id), c.body, `"2"`)
			if c.field != "" && field(refused) != c.field {
				t.Errorf("field: got %v, want %s", field(refused), c.field)
			}
		})
	}
	if v := e.version(t, id); v != `"2"` {
		t.Errorf("version: got %s, want \"2\", nothing changed", v)
	}
	t.Run("a delivered item removed", func(t *testing.T) {
		e.Must(201, "POST", deliveries(id), delivered(today, "1 1 EA"), "")
		refused := e.In(t).Problem(422, "sales.order_item_delivered", "POST", changing(id), `{"removed_items": [1]}`, `"3"`)
		if f := field(refused); f != "removed_items[0]" {
			t.Errorf("field: got %v, want removed_items[0]", f)
		}
	})
}

// TestOrderItemAddedCreditCheck: an item added to a confirmed order that
// raises it beyond the customer's limit blocks it again; removing an item
// takes it out of the customer's open orders (T78e, as T78d's change).
func TestOrderItemAddedCreditCheck(t *testing.T) {
	e := setup(t)
	buyer := e.buyer(t, "BP70000053", "100.00")
	placed := e.placed(t, buyer, e.item(line{quantity: "5"}), e.item(line{quantity: "2"}))
	id := placed["id"].(string)
	removed, _ := e.Must(200, "POST", changing(id), `{"removed_items": [2]}`, `"2"`)
	fields(t, removed, map[string]any{"credit_block": "none"})
	if x := e.exposed(t, buyer); !strings.Contains(x, "1 order(s) 50.00 EUR") {
		t.Errorf("exposure: got %s, want the order's 50.00 EUR without the removed item", x)
	}
	over, _ := e.Must(200, "POST", changing(id), fmt.Sprintf(`{"new_items": [%s]}`, e.item(line{quantity: "6"})), `"3"`)
	fields(t, over, map[string]any{"credit_block": "blocked"})
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM sales.sales_order_item WHERE tenant_id = $1 AND sales_order_id = $2`, []any{e.Tenant.ID, id}, &n)
	if n != 3 {
		t.Errorf("items: got %d, want 3", n)
	}
}

// TestRemovedItemConstraint: the database's refusal of a delivery of a
// removed item answers as the handlers do (T78e).
func TestRemovedItemConstraint(t *testing.T) {
	p := problem.From(&pgconn.PgError{Code: "23514", ConstraintName: "sales_order_item_removed_undelivered"})
	if p.Code != "sales.order_item_removed" {
		t.Errorf("code: got %s, want sales.order_item_removed", p.Code)
	}
	if p.Status != 422 {
		t.Errorf("status: got %d, want 422", p.Status)
	}
}

// TestChangeKeptBeforeT78e: an item of a change kept before T78e, which
// names no kind, reads as changed.
func TestChangeKeptBeforeT78e(t *testing.T) {
	e := setup(t)
	id := e.ordered(t)
	pgtest.Exec(t, `INSERT INTO sales.sales_order_change (tenant_id, id, sales_order_id, change_no, order_version, net_amount_before, net_amount_after,
			credit_block_before, credit_block_after, items, created_by)
		SELECT tenant_id, gen_random_uuid(), id, 1, 2, net_amount, net_amount, 'none', 'none',
			'[{"item_no": 1, "unit": "EA", "quantity_before": "1", "quantity_after": "2", "unit_price_before": 1000, "unit_price_after": 1000,
			   "price_unit_before": 1, "price_unit_after": 1, "net_amount_before": 1000, "net_amount_after": 2000}]', created_by
		FROM sales.sales_order WHERE tenant_id = $1 AND id = $2`, e.Tenant.ID, id)
	page, _ := e.Must(200, "GET", "/v1/sales-orders/"+id+"/changes", "", "")
	fields(t, items(t, items(t, page)[0])[0], map[string]any{"change": "changed"})
}
