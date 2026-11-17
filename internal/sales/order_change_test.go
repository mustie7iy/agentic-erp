package sales_test

import (
	"context"
	"fmt"
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

func changing(id string) string { return "/v1/sales-orders/" + id + ":change" }

func cancelling(id string) string { return "/v1/sales-orders/" + id + ":cancel" }

// itemChange is the JSON of a change of an item: "item number, quantity,
// unit" with the unit price in EUR, when one is given, as "amount".
func itemChange(item, price string) string {
	f := strings.Split(item, " ")
	parts := []string{`"item_no": ` + f[0]}
	if len(f) == 3 {
		parts = append(parts, fmt.Sprintf(`"quantity": {"value": %q, "unit": %q}`, f[1], f[2]))
	}
	if price != "" {
		parts = append(parts, fmt.Sprintf(`"unit_price": {"amount": %q, "currency": "EUR"}`, price))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// changeBody is the JSON of a change of the items, led by more fields
// when extra gives them (each followed by a comma).
func changeBody(extra string, items ...string) string {
	return fmt.Sprintf(`{%s"items": [%s]}`, extra, strings.Join(items, ", "))
}

// version is the order's version as an If-Match value.
func (e env) version(t *testing.T, order string) string {
	t.Helper()
	_, tag := e.Must(200, "GET", "/v1/sales-orders/"+order, "", "")
	return tag
}

// TestOrderChanged: a confirmed order's item takes a new quantity and
// price, priced again, the order's net value their sum exactly; the
// change is kept with its values before and after, numbered, audited and
// published; nothing is posted (T78d acceptance).
func TestOrderChanged(t *testing.T) {
	e := setup(t)
	id, today := e.confirmed(t, e.item(line{quantity: "3", code: "A1"}), e.item(line{quantity: "2", unit: "KG", price: "5.00"}))
	later := instant(t, today+"T00:00:00Z").AddDate(0, 0, 7).Format(time.DateOnly)
	entries := e.journalEntries(t)
	got, tag := e.Must(200, "POST", changing(id), changeBody(fmt.Sprintf(`"reason": "More at a lower price", "requested_delivery_date": %q, `, later),
		itemChange("1 5 EA", "9.50")), `"2"`)
	fields(t, got, map[string]any{"status": "confirmed", "changes": float64(1), "version": float64(3), "requested_delivery_date": later})
	if tag != `"3"` {
		t.Errorf("ETag: got %s, want \"3\"", tag)
	}
	if n := amount(t, got, "net_amount"); n != "57.50 EUR" {
		t.Errorf("net value: got %s, want 57.50 EUR, 5 × 9.50 + 2 × 5.00", n)
	}
	first := items(t, got)[0]
	if q, n := quantity(t, first, "quantity"), amount(t, first, "net_amount"); q != "5 EA" || n != "47.50 EUR" {
		t.Errorf("item 1: got %s for %s, want 5 EA for 47.50 EUR", q, n)
	}
	page, _ := e.Must(200, "GET", "/v1/sales-orders/"+id+"/changes", "", "")
	changes := items(t, page)
	if len(changes) != 1 {
		t.Fatalf("changes: got %d, want 1", len(changes))
	}
	fields(t, changes[0], map[string]any{"change_no": float64(1), "version": float64(3), "reason": "More at a lower price",
		"requested_delivery_date_after": later, "credit_block_before": "none", "credit_block_after": "none"})
	if b, a := amount(t, changes[0], "net_amount_before"), amount(t, changes[0], "net_amount_after"); b != "40.00 EUR" || a != "57.50 EUR" {
		t.Errorf("net value before and after: got %s and %s, want 40.00 EUR and 57.50 EUR", b, a)
	}
	changed := items(t, changes[0])
	if len(changed) != 1 {
		t.Fatalf("items changed: got %d, want 1", len(changed))
	}
	if q := quantity(t, changed[0], "quantity_before") + " > " + quantity(t, changed[0], "quantity_after"); q != "3 EA > 5 EA" {
		t.Errorf("quantity: got %s, want 3 EA > 5 EA", q)
	}
	if p := amount(t, changed[0], "unit_price_before") + " > " + amount(t, changed[0], "unit_price_after"); p != "10.00 EUR > 9.50 EUR" {
		t.Errorf("unit price: got %s, want 10.00 EUR > 9.50 EUR", p)
	}
	if n := e.audits(t, "sales.sales_order.change", id); n != 1 {
		t.Errorf("audit records of the change: got %d, want 1", n)
	}
	events := e.events(t, "sales_order_changed", "sales_order_id", id)
	fields(t, events[len(events)-1], map[string]any{"status": "confirmed", "status_before": "confirmed", "changes": float64(1),
		"version": float64(3), "requested_delivery_date": later})
	if n := e.journalEntries(t); n != entries {
		t.Errorf("journal entries: got %d, want %d, nothing posted", n, entries)
	}
	e.In(t).Problem(422, "sales.nothing_changed", "POST", changing(id), fmt.Sprintf(`{"requested_delivery_date": %q}`, later), `"3"`)
	cleared, _ := e.Must(200, "POST", changing(id), `{"requested_delivery_date": null}`, `"3"`)
	if _, ok := cleared["requested_delivery_date"]; ok || cleared["changes"] != float64(2) {
		t.Errorf("cleared: got %v after %v changes, want no requested delivery date after 2", cleared["requested_delivery_date"], cleared["changes"])
	}
	e.In(t).Problem(412, "request.version_mismatch", "POST", changing(id), changeBody("", itemChange("1 6 EA", "")), `"3"`)
}

// TestOrderChangeRefused: what a change cannot do is refused naming the
// field, and leaves the order as it was (T78d acceptance).
func TestOrderChangeRefused(t *testing.T) {
	e := setup(t)
	id, today := e.confirmed(t, e.item(line{quantity: "3"}), e.item(line{quantity: "2", unit: "KG"}))
	earlier := instant(t, today+"T00:00:00Z").AddDate(0, 0, -1).Format(time.DateOnly)
	cases := []struct {
		name, body, code, field string
	}{
		{"nothing", `{}`, "sales.nothing_changed", ""},
		{"the same quantity", changeBody("", itemChange("1 3 EA", "")), "sales.nothing_changed", ""},
		{"an unknown item", changeBody("", itemChange("3 1 EA", "")), "sales.unknown_order_item", "items[0].item_no"},
		{"an item twice", changeBody("", itemChange("1 4 EA", ""), itemChange("1 5 EA", "")), "sales.item_repeated", "items[1].item_no"},
		{"another unit", changeBody("", itemChange("2 1 EA", "")), "sales.unit_mismatch", "items[0].quantity.unit"},
		{"a quantity of zero", changeBody("", itemChange("1 0 EA", "")), "sales.invalid_quantity", "items[0].quantity.value"},
		{"a price in another currency", changeBody("", `{"item_no": 1, "unit_price": {"amount": "1.00", "currency": "USD"}}`), "sales.currency_mismatch",
			"items[0].unit_price.currency"},
		{"a price unit without a price", changeBody("", `{"item_no": 1, "price_unit": 10}`), "request.validation_failed", "items[0].price_unit"},
		{"a requested delivery date before the order's", fmt.Sprintf(`{"requested_delivery_date": %q}`, earlier), "sales.invalid_requested_delivery_date",
			"requested_delivery_date"},
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
	t.Run("below what is delivered", func(t *testing.T) {
		e.Must(201, "POST", deliveries(id), delivered(today, "1 2 EA"), "")
		refused := e.In(t).Problem(422, "sales.quantity_below_delivered", "POST", changing(id), changeBody("", itemChange("1 1.5 EA", "")), `"3"`)
		if f := field(refused); f != "items[0].quantity.value" {
			t.Errorf("field: got %v, want items[0].quantity.value", f)
		}
		e.Must(200, "POST", changing(id), changeBody("", itemChange("1 2 EA", "")), `"3"`)
	})
	t.Run("an item billed", func(t *testing.T) {
		order, _, _ := e.billedOrder(t, []string{e.item(line{quantity: "3"}), e.item(line{})}, "1 1 EA")
		tag := e.version(t, order)
		refused := e.In(t).Problem(422, "sales.order_item_billed", "POST", changing(order), changeBody("", itemChange("1", "9.00")), tag)
		if f := field(refused); f != "items[0]" {
			t.Errorf("field: got %v, want items[0]", f)
		}
		e.Must(200, "POST", changing(order), changeBody("", itemChange("2", "9.00")), tag)
	})
}

// TestOrderChangePricedAgain: a draft's item priced from the price
// records is priced from them again when its quantity changes; an entered
// price stays entered; a new price is entered (T78d, decided by default).
func TestOrderChangePricedAgain(t *testing.T) {
	e := setup(t)
	e.priced(t, rec{amount: "4.00"})
	got, _ := e.Must(201, "POST", "/v1/sales-orders", e.direct(e.customer, "", e.unpriced(line{quantity: "2"}), e.item(line{quantity: "1"})), "")
	id := got["id"].(string)
	changed, _ := e.Must(200, "POST", changing(id), changeBody("", itemChange("1 3 EA", ""), itemChange("2 4 EA", "")), `"1"`)
	its := items(t, changed)
	fields(t, its[0], map[string]any{"price_source": "price_list"})
	fields(t, its[1], map[string]any{"price_source": "manual"})
	if n := amount(t, its[0], "net_amount") + ", " + amount(t, its[1], "net_amount"); n != "12.00 EUR, 40.00 EUR" {
		t.Errorf("net values: got %s, want 12.00 EUR, 40.00 EUR", n)
	}
	repriced, _ := e.Must(200, "POST", changing(id), changeBody("", `{"item_no": 1, "unit_price": {"amount": "30.00", "currency": "EUR"}, "price_unit": 10}`),
		`"2"`)
	first := items(t, repriced)[0]
	fields(t, first, map[string]any{"price_source": "manual", "price_unit": float64(10)})
	if n := amount(t, first, "net_amount"); n != "9.00 EUR" {
		t.Errorf("net value: got %s, want 9.00 EUR, 3 × 30.00 ÷ 10", n)
	}
	fields(t, repriced, map[string]any{"status": "draft", "changes": float64(2)})
}

// TestOrderChangeCreditCheck: a change that raises a confirmed order's
// value beyond the customer's limit blocks it again, even after a
// release; a lower one leaves the block as it is (T78d acceptance).
func TestOrderChangeCreditCheck(t *testing.T) {
	e := setup(t)
	buyer := e.buyer(t, "BP70000050", "100.00")
	placed := e.placed(t, buyer, e.item(line{quantity: "5"}))
	id := placed["id"].(string)
	within, _ := e.Must(200, "POST", changing(id), changeBody("", itemChange("1 10 EA", "")), `"2"`)
	fields(t, within, map[string]any{"credit_block": "none"})
	over, _ := e.Must(200, "POST", changing(id), changeBody("", itemChange("1 11 EA", "")), `"3"`)
	fields(t, over, map[string]any{"credit_block": "blocked"})
	if ev := e.events(t, "sales_order_credit_blocked", "sales_order_id", id); len(ev) != 1 {
		t.Errorf("SalesOrderCreditBlocked: got %d, want 1", len(ev))
	}
	lower, _ := e.Must(200, "POST", changing(id), changeBody("", itemChange("1 9 EA", "")), `"4"`)
	fields(t, lower, map[string]any{"credit_block": "blocked"})
	e.Must(200, "POST", "/v1/sales-orders/"+id+":releaseCreditBlock", "", `"5"`)
	again, _ := e.Must(200, "POST", changing(id), changeBody("", itemChange("1 12 EA", "")), `"6"`)
	fields(t, again, map[string]any{"credit_block": "blocked"})
	if _, ok := again["credit_released_by"]; ok {
		t.Errorf("credit_released_by: got %v, want none once blocked again", again["credit_released_by"])
	}
	page, _ := e.Must(200, "GET", "/v1/sales-orders/"+id+"/changes?order_by=change_no%20desc&page_size=1", "", "")
	fields(t, items(t, page)[0], map[string]any{"change_no": float64(4), "credit_block_before": "released", "credit_block_after": "blocked"})
	next, _ := e.Must(200, "GET", "/v1/sales-orders/"+id+"/changes?order_by=change_no%20desc&page_size=1&page_token="+page["next_page_token"].(string), "", "")
	fields(t, items(t, next)[0], map[string]any{"change_no": float64(3)})
}

// TestOrderChangeTooLarge: a change whose items add up to more than an
// amount holds is refused, as is one whose value with its tax does,
// which the credit check of a confirmed order's increase calculates.
func TestOrderChangeTooLarge(t *testing.T) {
	e := setup(t)
	id, _ := e.confirmed(t, e.item(line{code: "A1"}), e.item(line{}))
	e.In(t).Problem(422, "sales.amount_too_large", "POST", changing(id),
		changeBody("", itemChange("1 50000000000 EA", "1000000.00"), itemChange("2 50000000000 EA", "1000000.00")), `"2"`)
	e.In(t).Problem(422, "sales.amount_too_large", "POST", changing(id), changeBody("", itemChange("1 80000000000 EA", "1000000.00")), `"2"`)
}

// TestOrderCancelled: an order of which nothing is delivered is
// cancelled once with a reason, takes nothing more and leaves the
// customer's open orders; one with a delivery is refused (T78d
// acceptance).
func TestOrderCancelled(t *testing.T) {
	e := setup(t)
	buyer := e.buyer(t, "BP70000051", "1000.00")
	id := e.placed(t, buyer, e.item(line{}))["id"].(string)
	if x := e.exposed(t, buyer); !strings.Contains(x, "1 order(s)") {
		t.Errorf("exposure before: got %s, want 1 order", x)
	}
	e.In(t).Problem(422, "request.validation_failed", "POST", cancelling(id), `{"reason": "  "}`, `"2"`)
	got, tag := e.Must(200, "POST", cancelling(id), `{"reason": "The customer withdrew"}`, `"2"`)
	fields(t, got, map[string]any{"status": "cancelled", "cancellation_reason": "The customer withdrew", "cancelled_by": e.Tenant.Human.ID.String()})
	if _, ok := got["cancelled_at"]; !ok {
		t.Error("cancelled_at: got none, want the time")
	}
	if name, _ := got["cancelled_by_name"].(string); name == "" {
		t.Errorf("cancelled_by_name: got %v, want who cancelled it", got["cancelled_by_name"])
	}
	if tag != `"3"` {
		t.Errorf("ETag: got %s, want \"3\"", tag)
	}
	if x := e.exposed(t, buyer); !strings.Contains(x, "0 order(s)") {
		t.Errorf("exposure after: got %s, want no order", x)
	}
	e.In(t).Problem(409, "sales.invalid_status", "POST", cancelling(id), `{"reason": "Again"}`, `"3"`)
	e.In(t).Problem(409, "sales.invalid_status", "POST", changing(id), changeBody("", itemChange("1 2 EA", "")), `"3"`)
	e.In(t).Problem(409, "sales.invalid_status", "POST", "/v1/sales-orders/"+id+":confirm", "", `"3"`)
	if n := e.audits(t, "sales.sales_order.cancel", id); n != 1 {
		t.Errorf("audit records of the cancellation: got %d, want 1", n)
	}
	events := e.events(t, "sales_order_changed", "sales_order_id", id)
	fields(t, events[len(events)-1], map[string]any{"status": "cancelled", "status_before": "confirmed", "version": float64(3)})
	draft := e.ordered(t)
	cancelled, _ := e.Must(200, "POST", cancelling(draft), `{"reason": "Entered twice"}`, `"1"`)
	fields(t, cancelled, map[string]any{"status": "cancelled"})
	shipped, today := e.confirmed(t, e.item(line{quantity: "2"}))
	e.Must(201, "POST", deliveries(shipped), delivered(today, "1 1 EA"), "")
	e.In(t).Problem(409, "sales.order_delivered", "POST", cancelling(shipped), `{"reason": "Too late"}`, `"3"`)
	listed, _ := e.Must(200, "GET", "/v1/sales-orders?status=cancelled", "", "")
	if n := len(items(t, listed)); n != 2 {
		t.Errorf("cancelled orders listed: got %d, want 2", n)
	}
}

// TestOrderRequestedDeliveryDate: an order is created with a requested
// delivery date on its date or later, never before.
func TestOrderRequestedDeliveryDate(t *testing.T) {
	e := setup(t)
	got, _ := e.Must(201, "POST", "/v1/sales-orders", e.direct(e.customer, "", e.item(line{})), "")
	today := got["order_date"].(string)
	asked, _ := e.Must(201, "POST", "/v1/sales-orders", e.direct(e.customer, fmt.Sprintf(`, "requested_delivery_date": %q`, today), e.item(line{})), "")
	fields(t, asked, map[string]any{"requested_delivery_date": today, "changes": float64(0)})
	earlier := instant(t, today+"T00:00:00Z").AddDate(0, 0, -1).Format(time.DateOnly)
	refused := e.In(t).Problem(422, "sales.invalid_requested_delivery_date", "POST", "/v1/sales-orders",
		e.direct(e.customer, fmt.Sprintf(`, "requested_delivery_date": %q`, earlier), e.item(line{})), "")
	if f := field(refused); f != "requested_delivery_date" {
		t.Errorf("field: got %v, want requested_delivery_date", f)
	}
}

// TestOrderChangeRacingDelivery: a change lowering an item's quantity and
// a delivery of it wait for one another on the order's lock: whichever
// is second meets the first's work, and what is delivered never exceeds
// what is ordered.
func TestOrderChangeRacingDelivery(t *testing.T) {
	e := setup(t)
	id, today := e.confirmed(t, e.item(line{quantity: "3"}))
	codes := make([]int, 2)
	var wg sync.WaitGroup
	wg.Go(func() {
		codes[0] = e.API.Do(apitest.Req{Method: "POST", Path: changing(id), Body: changeBody("", itemChange("1 1 EA", "")), IfMatch: `"2"`,
			Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey()}).Code
	})
	wg.Go(func() {
		codes[1] = e.API.Do(apitest.Req{Method: "POST", Path: deliveries(id), Body: delivered(today, "1 2 EA"), Tenant: e.Tenant.ID,
			As: e.Tenant.Human, Key: apitest.NewKey()}).Code
	})
	wg.Wait()
	// Change first: the delivery finds 1 open (422). Delivery first: the change meets another version (412).
	sorted := slices.Clone(codes)
	slices.Sort(sorted)
	if !slices.Equal(sorted, []int{200, 422}) && !slices.Equal(sorted, []int{201, 412}) {
		t.Errorf("statuses: got %v, want the change and a refused delivery, or the delivery and a refused change", codes)
	}
	var over bool
	pgtest.Scan(t, `SELECT delivered_quantity > quantity FROM sales.sales_order_item WHERE tenant_id = $1 AND sales_order_id = $2`,
		[]any{e.Tenant.ID, id}, &over)
	if over {
		t.Error("delivered above ordered: got true, want false")
	}
}

// TestOrderWithRevenueContract: a confirmed order with a revenue contract
// keeps its items' quantities and prices and is not cancelled; its
// requested delivery date still changes (T78d, decided by default).
func TestOrderWithRevenueContract(t *testing.T) {
	e := setup(t)
	id, today := e.confirmed(t, e.item(line{quantity: "3"}))
	pgtest.Exec(t, `INSERT INTO revenue.contract (tenant_id, id, legal_entity_id, customer_id, sales_order_id, sales_order_number, currency,
			transaction_price, status, created_by, updated_by)
		SELECT tenant_id, gen_random_uuid(), legal_entity_id, customer_id, id, document_number, currency, net_amount, 'draft', created_by, created_by
		FROM sales.sales_order WHERE tenant_id = $1 AND id = $2`, e.Tenant.ID, id)
	e.Problem(409, "sales.revenue_contract_exists", "POST", changing(id), changeBody("", itemChange("1 4 EA", "")), `"2"`)
	e.Problem(409, "sales.revenue_contract_exists", "POST", cancelling(id), `{"reason": "Withdrawn"}`, `"2"`)
	e.Must(200, "POST", changing(id), fmt.Sprintf(`{"requested_delivery_date": %q}`, today), `"2"`)
}

// TestOrderChangeKeptByItemNumber: a change naming items out of order
// keeps them by item number.
func TestOrderChangeKeptByItemNumber(t *testing.T) {
	e := setup(t)
	id := e.ordered(t)
	e.Must(200, "POST", changing(id), changeBody("", itemChange("1 2 EA", "")), `"1"`)
	got, _ := e.Must(201, "POST", "/v1/sales-orders", e.direct(e.customer, "", e.item(line{}), e.item(line{})), "")
	two := got["id"].(string)
	e.Must(200, "POST", changing(two), changeBody("", itemChange("2 5 EA", ""), itemChange("1 4 EA", "")), `"1"`)
	page, _ := e.Must(200, "GET", "/v1/sales-orders/"+two+"/changes", "", "")
	var nos []any
	for _, it := range items(t, items(t, page)[0]) {
		nos = append(nos, it["item_no"])
	}
	if fmt.Sprint(nos) != "[1 2]" {
		t.Errorf("items changed: got %v, want [1 2]", nos)
	}
}

// TestCancelledOrderNotReleased: a cancelled order keeps the credit block
// it had, and its release is refused naming its status.
func TestCancelledOrderNotReleased(t *testing.T) {
	e := setup(t)
	buyer := e.buyer(t, "BP70000052", "1.00")
	placed := e.placed(t, buyer, e.item(line{}))
	fields(t, placed, map[string]any{"credit_block": "blocked"})
	id := placed["id"].(string)
	e.Must(200, "POST", cancelling(id), `{"reason": "Over the limit"}`, `"2"`)
	e.Problem(409, "sales.invalid_status", "POST", "/v1/sales-orders/"+id+":releaseCreditBlock", "", `"3"`)
}

// TestOrderChangeRacingContract: a change of a confirmed order's item and
// the creation of its revenue contract wait for one another on the
// order's row: either the change is refused for the contract, or the
// contract is made from the changed order. A contract never holds a value
// the order no longer has (T78d, review round 2).
func TestOrderChangeRacingContract(t *testing.T) {
	e := setup(t)
	id, _ := e.confirmed(t, e.item(line{quantity: "3"}))
	codes := make([]int, 2)
	var wg sync.WaitGroup
	wg.Go(func() {
		codes[0] = e.API.Do(apitest.Req{Method: "POST", Path: changing(id), Body: changeBody("", itemChange("1 5 EA", "")), IfMatch: `"2"`,
			Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey()}).Code
	})
	wg.Go(func() {
		codes[1] = e.API.Do(apitest.Req{Method: "POST", Path: "/v1/revenue-contracts", Body: fmt.Sprintf(`{"sales_order_id": %q}`, id),
			Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey()}).Code
	})
	wg.Wait()
	// The contract may already be there if a consumer made it (409 revenue.contract_exists); the price check below is what matters.
	if (codes[1] != 201 && codes[1] != 409) || (codes[0] != 200 && codes[0] != 409) {
		t.Fatalf("statuses: got change %d, contract %d, want the contract made and the change made or refused", codes[0], codes[1])
	}
	var same bool
	pgtest.Scan(t, `SELECT k.transaction_price = o.net_amount FROM revenue.contract k JOIN sales.sales_order o ON o.tenant_id = k.tenant_id AND o.id = k.sales_order_id
		WHERE k.tenant_id = $1 AND k.sales_order_id = $2`, []any{e.Tenant.ID, id}, &same)
	if !same {
		t.Errorf("the contract's transaction price: got another than the order's net value (change %d), want the order's", codes[0])
	}
}

// TestSharedOrderOutsideARequest: the share-locked read is made in a
// request's transaction alone.
func TestSharedOrderOutsideARequest(t *testing.T) {
	if _, _, err := (sales.Orders{}).Shared(context.Background(), uuid.New()); err == nil {
		t.Error("outside a unit of work: got nil error, want an error")
	}
}
