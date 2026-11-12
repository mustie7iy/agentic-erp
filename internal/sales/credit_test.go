package sales_test

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/sales"
)

// buyer is a new customer of company 2000, with a credit limit of the
// amount in EUR there when one is given.
func (e env) buyer(t *testing.T, number, limit string) uuid.UUID {
	t.Helper()
	id := e.partner(t, number, "Customer "+number, "active", "customer")
	e.companyData(t, id, e.DE, "NT30")
	if limit != "" {
		e.limited(t, id, limit)
	}
	return id
}

// limited sets the customer's credit limit in company 2000, in EUR.
func (e env) limited(t *testing.T, customer uuid.UUID, limit string) {
	t.Helper()
	e.Must(201, "POST", "/v1/credit-limits", fmt.Sprintf(`{"legal_entity_id": %q, "customer_id": %q, "limit": {"amount": %q, "currency": "EUR"}}`,
		e.DE, customer, limit), "")
}

// drafted creates an order of the items for the customer in the
// currency and returns its id.
func (e env) drafted(t *testing.T, customer uuid.UUID, currency string, items ...string) string {
	t.Helper()
	body := strings.Replace(e.direct(customer, "", items...), `"currency": "EUR"`, fmt.Sprintf(`"currency": %q`, currency), 1)
	got, _ := e.Must(201, "POST", "/v1/sales-orders", body, "")
	return got["id"].(string)
}

// placed creates an order of the items for the customer in EUR and
// confirms it; it returns the confirmed order.
func (e env) placed(t *testing.T, customer uuid.UUID, items ...string) map[string]any {
	t.Helper()
	got, _ := e.Must(200, "POST", "/v1/sales-orders/"+e.drafted(t, customer, "EUR", items...)+":confirm", "", `"1"`)
	return got
}

// exposed is the customer's exposure in company 2000 as "open items +
// N order(s) open orders = exposure, within".
func (e env) exposed(t *testing.T, customer uuid.UUID) string {
	t.Helper()
	x, _ := e.In(t).Must(200, "GET", "/v1/credit-exposure?legal_entity_id="+e.DE.String()+"&customer_id="+customer.String(), "", "")
	return fmt.Sprintf("%s + %v order(s) %s = %s, within %v", amount(t, x, "open_items"), x["open_order_count"], amount(t, x, "open_orders"),
		amount(t, x, "exposure"), x["is_within_limit"])
}

// creditCheck is the credit check the confirmation's audit record holds.
func (e env) creditCheck(t *testing.T, order string) string {
	t.Helper()
	var detail string
	pgtest.Scan(t, `SELECT (detail -> 'credit_check')::text FROM audit.audit_event WHERE tenant_id = $1 AND action = 'sales.sales_order.confirm'
		AND object_id = $2`, []any{e.Tenant.ID, order}, &detail)
	return detail
}

// usd records the average rate of 0.80 EUR for a US dollar from 2026.
func (e env) usd(t *testing.T) {
	t.Helper()
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-01-01', 0.8, 'ECB', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
}

// TestCreditCheckAtConfirmation: an order is confirmed without a block
// while its gross value and the customer's exposure are within the
// limit, up to the limit itself; a cent more confirms it blocked. The
// exposure shows the confirmed orders beside the open items, a blocked
// one left out; the block is audited with what was checked and
// published (T77b acceptance).
func TestCreditCheckAtConfirmation(t *testing.T) {
	e := setup(t)
	customer := e.buyer(t, "BP70000010", "500.00")
	if x := e.exposed(t, customer); x != "0.00 EUR + 0 order(s) 0.00 EUR = 0.00 EUR, within true" {
		t.Fatalf("before any order: got %s, want nothing owed or ordered", x)
	}
	var blocked string
	for _, c := range []struct {
		name, block, exposure, check string
		item                         line
	}{
		// 100.00 with 19% tax is 119.00.
		{"within the limit", "none", "0.00 EUR + 1 order(s) 119.00 EUR = 119.00 EUR, within true",
			`{"limit": {"amount": "500.00", "currency": "EUR"}, "amount": {"amount": "119.00", "currency": "EUR"}, "exposure": {"amount": "0.00", "currency": "EUR"}, "has_limit": true}`,
			line{quantity: "10", code: "A1"}},
		{"reaching the limit", "none", "0.00 EUR + 2 order(s) 500.00 EUR = 500.00 EUR, within true",
			`{"limit": {"amount": "500.00", "currency": "EUR"}, "amount": {"amount": "381.00", "currency": "EUR"}, "exposure": {"amount": "119.00", "currency": "EUR"}, "has_limit": true}`,
			line{price: "381.00"}},
		{"a cent over the limit", "blocked", "0.00 EUR + 2 order(s) 500.00 EUR = 500.00 EUR, within true",
			`{"limit": {"amount": "500.00", "currency": "EUR"}, "amount": {"amount": "0.01", "currency": "EUR"}, "exposure": {"amount": "500.00", "currency": "EUR"}, "has_limit": true}`,
			line{price: "0.01"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := e.placed(t, customer, e.item(c.item))
			id := got["id"].(string)
			fields(t, got, map[string]any{"status": "confirmed", "credit_block": c.block, "version": float64(2)})
			if x := e.exposed(t, customer); x != c.exposure {
				t.Errorf("exposure after the confirmation: got %s, want %s", x, c.exposure)
			}
			if d := e.creditCheck(t, id); d != c.check {
				t.Errorf("credit check in the audit record: got %s, want %s", d, c.check)
			}
			changed := e.events(t, "sales_order_changed", "sales_order_id", id)
			if len(changed) != 2 {
				t.Fatalf("SalesOrderChanged events: got %d, want 2 (created, confirmed)", len(changed))
			}
			if changed[1]["credit_block"] != c.block {
				t.Errorf("credit_block of SalesOrderChanged: got %v, want %s", changed[1]["credit_block"], c.block)
			}
			if c.block == sales.Blocked {
				blocked = id
			} else if ev := e.events(t, "sales_order_credit_blocked", "sales_order_id", id); len(ev) != 0 {
				t.Errorf("SalesOrderCreditBlocked events of an order within the limit: got %d, want none", len(ev))
			}
		})
	}
	t.Run("the block is published", func(t *testing.T) {
		got, _ := e.In(t).Must(200, "GET", "/v1/sales-orders/"+blocked, "", "")
		eur := func(a string) map[string]any { return map[string]any{"amount": a, "currency": "EUR"} }
		want := map[string]any{"sales_order_id": blocked, "document_number": got["document_number"], "legal_entity_id": e.DE.String(),
			"legal_entity_code": "2000", "customer_id": customer.String(), "customer_number": "BP70000010", "gross_amount": eur("0.01"),
			"checked_amount": eur("0.01"), "limit": eur("500.00"), "exposure": eur("500.00"), "version": float64(2)}
		if a, b := mustJSON(t, e.events(t, "sales_order_credit_blocked", "sales_order_id", blocked)), mustJSON(t, []map[string]any{want}); a != b {
			t.Errorf("SalesOrderCreditBlocked: got %s, want %s", a, b)
		}
		apitest.NeverPublished(t, e.Tenant, "sales", "Customer BP70000010")
	})
	t.Run("listed by credit block", func(t *testing.T) {
		got, _ := e.In(t).Must(200, "GET", "/v1/sales-orders?customer_id="+customer.String()+"&credit_block=blocked", "", "")
		var ids []string
		for _, it := range items(t, got) {
			ids = append(ids, it["id"].(string))
		}
		if s := strings.Join(ids, " "); s != blocked {
			t.Errorf("the customer's blocked orders: got %s, want %s", s, blocked)
		}
		none, _ := e.In(t).Must(200, "GET", "/v1/sales-orders?customer_id="+customer.String()+"&credit_block=none", "", "")
		if n := len(items(t, none)); n != 2 {
			t.Errorf("the customer's orders without a block: got %d, want 2", n)
		}
	})
}

// TestCreditCheckAmount: the amount checked is the order's gross value,
// net plus output tax, never the net value alone; a customer without a
// limit is never blocked, and a limit of zero blocks every order.
func TestCreditCheckAmount(t *testing.T) {
	e := setup(t)
	for i, c := range []struct {
		name, limit, block string
		item               line
	}{
		{"net within the limit, gross over it", "110.00", "blocked", line{quantity: "10", code: "A1"}},
		{"gross at the limit", "119.00", "none", line{quantity: "10", code: "A1"}},
		{"an item without a tax code at its net value", "100.00", "none", line{quantity: "10"}},
		{"a limit of zero", "0.00", "blocked", line{price: "0.01"}},
		{"no limit", "", "none", line{price: "1000000.00", code: "A1"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			customer := e.buyer(t, fmt.Sprintf("BP7000002%d", i), c.limit)
			got := e.placed(t, customer, e.item(c.item))
			if got["credit_block"] != c.block {
				t.Errorf("credit_block: got %v, want %s", got["credit_block"], c.block)
			}
		})
	}
	t.Run("two items of one tax code, taxed together", func(t *testing.T) {
		// 2 x 100.00 with 19% tax is 238.00, the limit.
		customer := e.buyer(t, "BP70000031", "238.00")
		got := e.placed(t, customer, e.item(line{quantity: "10", code: "A1"}), e.item(line{quantity: "10", code: "A1"}))
		if got["credit_block"] != "none" {
			t.Errorf("credit_block: got %v, want none: 238.00 EUR at the limit", got["credit_block"])
		}
	})
	t.Run("an order of a customer without a limit counts in its exposure", func(t *testing.T) {
		customer := e.buyer(t, "BP70000030", "")
		id := e.placed(t, customer, e.item(line{quantity: "10", code: "A1"}))["id"].(string)
		if x := e.exposed(t, customer); x != "0.00 EUR + 1 order(s) 119.00 EUR = 119.00 EUR, within true" {
			t.Errorf("exposure: got %s, want the order's 119.00 EUR", x)
		}
		if d, want := e.creditCheck(t, id), `{"amount": {"amount": "119.00", "currency": "EUR"}, "has_limit": false}`; d != want {
			t.Errorf("credit check in the audit record: got %s, want %s", d, want)
		}
	})
}

// TestCreditBlockReleased: a blocked order takes no delivery and has
// nothing to bill; its block is released once, at the caller's version,
// by a person holding the permission, the one who confirmed it included;
// the release is audited and published; the released order is delivered
// and counts in the exposure (T77b acceptance).
func TestCreditBlockReleased(t *testing.T) {
	e := setup(t)
	customer := e.buyer(t, "BP70000040", "100.00")
	order := e.placed(t, customer, e.item(line{quantity: "20", code: "A1"}))
	id, today := order["id"].(string), order["order_date"].(string)
	path := "/v1/sales-orders/" + id + ":releaseCreditBlock"
	if order["credit_block"] != "blocked" {
		t.Fatalf("credit_block: got %v, want blocked: 238.00 EUR against a limit of 100.00", order["credit_block"])
	}
	if _, ok := order["credit_released_by"]; ok {
		t.Errorf("credit_released_by of a blocked order: got %v, want it absent", order["credit_released_by"])
	}
	t.Run("a blocked order takes no delivery", func(t *testing.T) {
		e.In(t).Problem(409, "sales.order_credit_blocked", "POST", deliveries(id), delivered(today, "1 5 EA"), "")
	})
	t.Run("a blocked order has nothing to bill", func(t *testing.T) {
		e.In(t).Problem(422, "sales.nothing_to_bill", "POST", billings, bill(id, ""), "")
	})
	t.Run("a blocked order does not count in the exposure", func(t *testing.T) {
		if x := e.exposed(t, customer); x != "0.00 EUR + 0 order(s) 0.00 EUR = 0.00 EUR, within true" {
			t.Errorf("exposure: got %s, want nothing ordered", x)
		}
	})
	t.Run("the release takes the caller's version", func(t *testing.T) {
		e.In(t).Problem(428, "request.precondition_required", "POST", path, "", "")
		e.In(t).Problem(412, "request.version_mismatch", "POST", path, "", `"7"`)
	})
	req := apitest.Req{Method: "POST", Path: path, Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey(), IfMatch: `"2"`}
	first, second := e.API.Do(req), e.API.Do(req)
	for i, rec := range []*httptest.ResponseRecorder{first, second} {
		if rec.Code != 200 {
			t.Fatalf("status of call %d: got %d, want 200: %s", i+1, rec.Code, rec.Body)
		}
	}
	a, b := apitest.JSON(t, first), apitest.JSON(t, second)
	t.Run("released by the person who confirmed it", func(t *testing.T) {
		fields(t, a, map[string]any{"status": "confirmed", "credit_block": "released", "version": float64(3),
			"credit_released_by": e.Tenant.Human.ID.String()})
		if at, err := time.Parse(time.RFC3339Nano, fmt.Sprint(a["credit_released_at"])); err != nil || time.Since(at) > time.Hour {
			t.Errorf("credit_released_at: got %v (%v), want the time of the release", a["credit_released_at"], err)
		}
		var name string
		pgtest.Scan(t, `SELECT display_name FROM identity.principal WHERE tenant_id = $1 AND id = $2`, []any{e.Tenant.ID, e.Tenant.Human.ID}, &name)
		if name == "" {
			t.Fatal("the releasing person's display name: got none, want the fixture's")
		}
		if a["credit_released_by_name"] != name {
			t.Errorf("credit_released_by_name: got %v, want the releasing person's name, %q", a["credit_released_by_name"], name)
		}
		if tag := first.Header().Get("ETag"); tag != `"3"` {
			t.Errorf("ETag: got %s, want \"3\"", tag)
		}
	})
	t.Run("a retry answers the released order", func(t *testing.T) {
		if mustJSON(t, a) != mustJSON(t, b) {
			t.Errorf("replay: got %v, want the first answer %v", b, a)
		}
		if h := second.Header().Get("Idempotent-Replayed"); h != "true" {
			t.Errorf("Idempotent-Replayed: got %q, want true", h)
		}
	})
	t.Run("audited and published once", func(t *testing.T) {
		if n := e.audits(t, "sales.sales_order.release_credit_block", id); n != 1 {
			t.Errorf("audit records of the release: got %d, want 1", n)
		}
		ev := e.events(t, "sales_order_credit_released", "sales_order_id", id)
		if len(ev) != 1 {
			t.Fatalf("SalesOrderCreditReleased events: got %d, want 1", len(ev))
		}
		// The event gives the instant to the microsecond, the order to the second.
		at := instant(t, fmt.Sprint(ev[0]["released_at"]))
		if shown := instant(t, fmt.Sprint(a["credit_released_at"])); !at.Truncate(time.Second).Equal(shown) {
			t.Errorf("released_at: got %v, want the order's credit_released_at %v", at, shown)
		}
		want := map[string]any{"sales_order_id": id, "document_number": order["document_number"], "legal_entity_id": e.DE.String(),
			"legal_entity_code": "2000", "customer_id": customer.String(), "customer_number": "BP70000040",
			"released_by": e.Tenant.Human.ID.String(), "released_at": ev[0]["released_at"], "version": float64(3)}
		if got, w := mustJSON(t, ev[0]), mustJSON(t, want); got != w {
			t.Errorf("SalesOrderCreditReleased: got %s, want %s", got, w)
		}
	})
	t.Run("released once", func(t *testing.T) {
		e.In(t).Problem(409, "sales.order_not_credit_blocked", "POST", path, "", `"3"`)
	})
	t.Run("the released order counts in the exposure, over the limit", func(t *testing.T) {
		if x := e.exposed(t, customer); x != "0.00 EUR + 1 order(s) 238.00 EUR = 238.00 EUR, within false" {
			t.Errorf("exposure: got %s, want the order's 238.00 EUR", x)
		}
	})
	t.Run("the released order is delivered", func(t *testing.T) {
		e.In(t).Must(201, "POST", deliveries(id), delivered(today, "1 20 EA"), "")
		if d := e.quantities(t, id, "delivered_quantity"); d != "20 EA" {
			t.Errorf("delivered: got %s, want 20 EA", d)
		}
	})
	t.Run("an order without a block has none to release", func(t *testing.T) {
		free := e.placed(t, e.buyer(t, "BP70000041", ""), e.item(line{}))["id"].(string)
		e.In(t).Problem(409, "sales.order_not_credit_blocked", "POST", "/v1/sales-orders/"+free+":releaseCreditBlock", "", `"2"`)
	})
	t.Run("an unknown order", func(t *testing.T) {
		e.In(t).Problem(404, "request.not_found", "POST", "/v1/sales-orders/"+uuid.NewString()+":releaseCreditBlock", "", `"1"`)
	})
	t.Run("another company's order", func(t *testing.T) {
		other := e.placed(t, customer, e.item(line{}))["id"].(string)
		e.As(e.Reader(t, "US sales", e.US)).In(t).Problem(404, "request.not_found", "POST", "/v1/sales-orders/"+other+":releaseCreditBlock", "", `"2"`)
	})
	t.Run("without the permission to release", func(t *testing.T) {
		other := e.placed(t, customer, e.item(line{}))["id"].(string)
		pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = 'sales.order.credit_release'`, e.Tenant.ID)
		e.In(t).Problem(403, "identity.permission_missing", "POST", "/v1/sales-orders/"+other+":releaseCreditBlock", "", `"2"`)
	})
}

// TestConfirmationsOfOneCustomerSerialize: two orders of one customer,
// each within the limit alone and not together, are confirmed in two
// transactions at once. The second waits for the first's hold on the
// customer's limit and is then checked against an exposure that holds
// the first order: one is confirmed without a block, the other blocked,
// never both without (ADR-0066, As built T77b; ADR-0008).
func TestConfirmationsOfOneCustomerSerialize(t *testing.T) {
	e := setup(t)
	customer := e.buyer(t, "BP70000050", "500.00")
	h := endpoints(t, time.Now())
	confirm := func(order string, block *string) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			o, err := h.ConfirmOrder(ctx, uuid.MustParse(order), etag.IfMatch(`"1"`))
			*block = o.CreditBlock
			return err
		}
	}
	a, b := e.drafted(t, customer, "EUR", e.item(line{price: "300.00"})), e.drafted(t, customer, "EUR", e.item(line{price: "300.00"}))
	var first, second string
	errA, errB := apitest.Serialized(t, e.Tenant, e.Tenant.Human, confirm(a, &first), confirm(b, &second))
	if errA != nil {
		t.Fatalf("the first confirmation: got %v, want it confirmed", errA)
	}
	if errB != nil {
		t.Fatalf("the second confirmation: got %v, want it confirmed", errB)
	}
	if first != sales.NoBlock {
		t.Errorf("the first order's credit block: got %s, want none: 300.00 EUR within a limit of 500.00", first)
	}
	if second != sales.Blocked {
		t.Errorf("the second order's credit block: got %s, want blocked: 600.00 EUR with the first against a limit of 500.00", second)
	}
	if x := e.exposed(t, customer); x != "0.00 EUR + 1 order(s) 300.00 EUR = 300.00 EUR, within true" {
		t.Errorf("exposure: got %s, want the first order's 300.00 EUR alone", x)
	}
}

// TestCreditCheckInAnotherCurrency: an order in another currency than
// the company's is checked, and counts in the exposure, at its gross
// value converted at the average rate valid on its date; without a rate
// it is not confirmed.
func TestCreditCheckInAnotherCurrency(t *testing.T) {
	e := setup(t)
	customer := e.buyer(t, "BP70000060", "100.00")
	dollars := func(price string) string { return e.item(line{price: price, currency: "USD", code: "A1"}) }
	t.Run("without a rate on the order's date", func(t *testing.T) {
		id := e.drafted(t, customer, "USD", dollars("10.00"))
		p := e.In(t).Problem(422, "ledger.no_exchange_rate", "POST", "/v1/sales-orders/"+id+":confirm", "", `"1"`)
		if d := fmt.Sprint(p["detail"]); !strings.Contains(d, "Sales order SO") || !strings.Contains(d, "USD") {
			t.Errorf("detail: got %q, want it to name the order and its currency", d)
		}
	})
	e.usd(t)
	t.Run("within the limit at the rate", func(t *testing.T) {
		// 100.00 USD with 19% tax is 119.00 USD, 95.20 EUR at 0.80.
		id := e.drafted(t, customer, "USD", dollars("100.00"))
		got, _ := e.In(t).Must(200, "POST", "/v1/sales-orders/"+id+":confirm", "", `"1"`)
		if got["credit_block"] != "none" {
			t.Errorf("credit_block: got %v, want none: 95.20 EUR within 100.00", got["credit_block"])
		}
		if x := e.exposed(t, customer); x != "0.00 EUR + 1 order(s) 95.20 EUR = 95.20 EUR, within true" {
			t.Errorf("exposure: got %s, want the order's 95.20 EUR", x)
		}
	})
	t.Run("over the limit at the rate", func(t *testing.T) {
		// 5.05 USD with 19% tax is 6.01 USD (6.0095), 4.81 EUR at 0.80 (4.808): 100.01 with the first.
		id := e.drafted(t, customer, "USD", dollars("5.05"))
		got, _ := e.In(t).Must(200, "POST", "/v1/sales-orders/"+id+":confirm", "", `"1"`)
		if got["credit_block"] != "blocked" {
			t.Errorf("credit_block: got %v, want blocked: 95.20 + 4.81 EUR against 100.00", got["credit_block"])
		}
		ev := e.events(t, "sales_order_credit_blocked", "sales_order_id", id)
		if len(ev) != 1 {
			t.Fatalf("SalesOrderCreditBlocked events: got %d, want 1", len(ev))
		}
		if g, c := amount(t, ev[0], "gross_amount"), amount(t, ev[0], "checked_amount"); g+", "+c != "6.01 USD, 4.81 EUR" {
			t.Errorf("gross and checked amounts: got %s, %s, want 6.01 USD, 4.81 EUR", g, c)
		}
	})
}

// TestOpenOrdersAndBilling: what a confirmed order counts in the
// exposure is its part not yet posted as a customer invoice. Deliveries
// and a created billing document post nothing, so the order counts in
// full. Posting the billing document (T79b) moves its part from the open
// orders to the open items: the open orders fall by the document's gross
// (its items leave sales.openOrders once the document is `posted`), the
// open items rise by the invoice's gross, and the exposure stays as it
// was, the invoice's tax being the order date's. Nothing counts twice.
func TestOpenOrdersAndBilling(t *testing.T) {
	e := setup(t)
	customer := e.buyer(t, "BP70000070", "1000.00")
	order := e.placed(t, customer, e.item(line{quantity: "20", code: "A1"}))
	id, today := order["id"].(string), order["order_date"].(string)
	const want = "0.00 EUR + 1 order(s) 238.00 EUR = 238.00 EUR, within true"
	e.Must(201, "POST", deliveries(id), delivered(today, "1 5 EA"), "")
	if x := e.exposed(t, customer); x != want {
		t.Errorf("exposure after a delivery: got %s, want %s", x, want)
	}
	doc, _ := e.Must(201, "POST", billings, bill(id, ""), "")
	if x := e.exposed(t, customer); x != want {
		t.Errorf("exposure after a billing document that is not posted: got %s, want %s", x, want)
	}
	// 5 of 20 EA at 10.00 are billed: 50.00 net and 19% tax, 59.50, become
	// the customer's open item; 150.00 net with its tax, 178.50, stay open
	// on the order.
	const posted = "59.50 EUR + 1 order(s) 178.50 EUR = 238.00 EUR, within true"
	e.Must(200, "POST", posting(doc["id"].(string)), "", `"1"`)
	if x := e.exposed(t, customer); x != posted {
		t.Errorf("exposure after the billing document is posted: got %s, want %s", x, posted)
	}
	t.Run("a draft does not count", func(t *testing.T) {
		e.drafted(t, customer, "EUR", e.item(line{price: "500.00"}))
		if x := e.exposed(t, customer); x != posted {
			t.Errorf("exposure with a draft order: got %s, want %s", x, posted)
		}
	})
	t.Run("another company's orders do not count", func(t *testing.T) {
		x, _ := e.In(t).Must(200, "GET", "/v1/credit-exposure?legal_entity_id="+e.US.String()+"&customer_id="+customer.String(), "", "")
		if got := fmt.Sprintf("%v %s", x["open_order_count"], amount(t, x, "open_orders")); got != "0 0.00 USD" {
			t.Errorf("open orders in company 1000: got %s, want 0 0.00 USD", got)
		}
	})
	t.Run("an order billed and posted in full no longer counts", func(t *testing.T) {
		const want = "238.00 EUR + 0 order(s) 0.00 EUR = 238.00 EUR, within true"
		e.Must(201, "POST", deliveries(id), delivered(today, "1 15 EA"), "")
		rest, _ := e.Must(201, "POST", billings, bill(id, ""), "")
		e.Must(200, "POST", posting(rest["id"].(string)), "", `"1"`)
		if x := e.exposed(t, customer); x != want {
			t.Errorf("exposure after the whole order is posted: got %s, want %s", x, want)
		}
	})
}

// codes answers the ledger's tax code read with a fixed code, or none.
type codes struct {
	sales.Accounts
	code *ledger.TaxCode
}

func (c codes) TaxCode(context.Context, uuid.UUID, string, time.Time) (ledger.TaxCode, bool, error) {
	if c.code == nil {
		return ledger.TaxCode{}, false, nil
	}
	return *c.code, true, nil
}

// rates converts at a fixed rate between currencies of two decimals.
type rates string

func (r rates) Conversion(_ context.Context, from, to string, _ time.Time) (ledger.Conversion, error) {
	return ledger.Conversion{From: from, To: to, Rate: string(r), FromExp: 2, ToExp: 2}, nil
}

// refusing refuses every credit check.
type refusing struct{}

func (refusing) Check(context.Context, uuid.UUID, uuid.UUID, money.Minor) (sales.CreditCheck, error) {
	return sales.CreditCheck{}, errors.New("credit is not to be asked")
}

// TestOrderValueRefusals: an order whose value with its tax cannot be
// calculated is not confirmed, and refuses the exposure it would count
// in, rather than counting as something else.
func TestOrderValueRefusals(t *testing.T) {
	e := setup(t)
	customer := e.buyer(t, "BP70000080", "")
	confirm := func(h *sales.Endpoints, order string) error {
		return e.inRequest(t, func(ctx context.Context) error {
			_, err := h.ConfirmOrder(ctx, uuid.MustParse(order), etag.IfMatch(`"1"`))
			return err
		})
	}
	taxed := e.drafted(t, customer, "EUR", e.item(line{code: "A1"}))
	t.Run("a tax code that is no longer one on the order's date", func(t *testing.T) {
		h := endpoints(t, time.Now())
		h.Accounts = codes{Accounts: h.Accounts}
		refusal(t, confirm(h, taxed), 422, "sales.unknown_tax_code")
	})
	t.Run("a tax code that became an input tax code", func(t *testing.T) {
		h := endpoints(t, time.Now())
		h.Accounts = codes{Accounts: h.Accounts, code: &ledger.TaxCode{Code: "A1", Direction: "input"}}
		refusal(t, confirm(h, taxed), 422, "sales.unknown_tax_code")
	})
	t.Run("a tax code the calculator cannot calculate", func(t *testing.T) {
		h := endpoints(t, time.Now())
		h.Accounts = codes{Accounts: h.Accounts, code: &ledger.TaxCode{Code: "A1", Direction: "output",
			Components: []ledger.TaxComponent{{Key: "vat", AccountKey: "output", RatePct: "x"}}}}
		err := confirm(h, taxed)
		if p := problem.From(err); err == nil || p.Status != 500 {
			t.Errorf("got %v, want an internal error: the code was read with a rate that is none", err)
		}
	})
	t.Run("credit cannot be asked", func(t *testing.T) {
		h := endpoints(t, time.Now())
		h.Credit = refusing{}
		if err := confirm(h, taxed); err == nil || !strings.Contains(err.Error(), "credit is not to be asked") {
			t.Errorf("got %v, want credit's error", err)
		}
	})
	t.Run("a value with its tax too large", func(t *testing.T) {
		id := e.drafted(t, customer, "EUR", e.item(line{price: "90000000000000000.00", code: "A1"}))
		e.In(t).Problem(422, "sales.amount_too_large", "POST", "/v1/sales-orders/"+id+":confirm", "", `"1"`)
	})
	t.Run("a value too large in the company's currency", func(t *testing.T) {
		id := e.drafted(t, customer, "USD", e.item(line{price: "90000000000000000.00", currency: "USD"}))
		h := endpoints(t, time.Now())
		h.Rates = rates("1000")
		refusal(t, confirm(h, id), 422, "sales.amount_too_large")
	})
	t.Run("open orders that add up to too much", func(t *testing.T) {
		rich := e.buyer(t, "BP70000081", "")
		for range 2 {
			e.placed(t, rich, e.item(line{price: "50000000000000000.00"}))
		}
		e.In(t).Problem(422, "sales.amount_too_large", "GET", "/v1/credit-exposure?legal_entity_id="+e.DE.String()+"&customer_id="+rich.String(), "", "")
		t.Run("and the next confirmation, with a limit", func(t *testing.T) {
			e.limited(t, rich, "10.00")
			id := e.drafted(t, rich, "EUR", e.item(line{}))
			e.In(t).Problem(422, "sales.amount_too_large", "POST", "/v1/sales-orders/"+id+":confirm", "", `"1"`)
		})
	})
	t.Run("an open order whose tax code is no longer one refuses the exposure", func(t *testing.T) {
		other := e.buyer(t, "BP70000082", "")
		e.placed(t, other, e.item(line{code: "A1"}))
		h := endpoints(t, time.Now())
		h.Accounts = codes{Accounts: h.Accounts}
		err := e.inRequest(t, func(ctx context.Context) error {
			_, _, err := h.OpenOrders(ctx, e.DE, other)
			return err
		})
		refusal(t, err, 422, "sales.unknown_tax_code")
	})
}
