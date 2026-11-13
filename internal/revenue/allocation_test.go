package revenue_test

import (
	"context"
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

// contract creates the revenue contract of a confirmed order of the
// items, on request, and returns its id and its order's.
func (e env) contract(t *testing.T, items ...string) (id, order string) {
	t.Helper()
	order, _ = e.confirmed(t, items...)
	got, _ := e.Must(201, "POST", contracts, forOrder(order), "")
	return got["id"].(string), order
}

// set is the JSON of a change of the contract's obligations.
func set(obligations ...string) string {
	return fmt.Sprintf(`{"obligations": [%s]}`, strings.Join(obligations, ", "))
}

// overTime is the JSON that sets the obligation to over time for the
// period at the stand-alone selling price in EUR.
func overTime(no int, from, to, price string) string {
	return fmt.Sprintf(`{"obligation_no": %d, "timing": "over_time", "service_from": %q, "service_to": %q, "standalone_selling_price": {"amount": %q, "currency": "EUR"}}`,
		no, from, to, price)
}

// allocated are the obligations' allocated amounts, "-" for one without.
func allocated(t *testing.T, c map[string]any) []string {
	t.Helper()
	var out []string
	for _, o := range obligations(t, c) {
		if _, ok := o["allocated_amount"]; !ok {
			out = append(out, "-")
			continue
		}
		out = append(out, amount(t, o, "allocated_amount"))
	}
	return out
}

// TestObligationsSet: on a draft the revenue accountant sets an
// obligation to over time with its service period and a stand-alone
// selling price; what the request does not name stays; the version
// moves; it is audited and published; nothing is allocated yet (T81b
// acceptance).
func TestObligationsSet(t *testing.T) {
	e := setup(t)
	id, order := e.contract(t, e.goods(), e.support())
	path := contracts + "/" + id
	got, tag := e.Must(200, "PATCH", path, set(overTime(2, "2026-01-01", "2026-12-31", "1300.00")), `"1"`)
	if tag != `"2"` {
		t.Errorf("ETag: got %s, want \"2\"", tag)
	}
	fields(t, got, map[string]any{"status": "draft", "version": float64(2)})
	obs := obligations(t, got)
	fields(t, obs[1], map[string]any{"timing": "over_time", "service_from": "2026-01-01", "service_to": "2026-12-31"})
	if a := amount(t, obs[1], "standalone_selling_price"); a != "1300.00 EUR" {
		t.Errorf("the service's stand-alone selling price: got %s, want 1300.00 EUR", a)
	}
	if a := amount(t, obs[1], "net_amount"); a != "1200.00 EUR" {
		t.Errorf("the service's order value: got %s, want 1200.00 EUR, unchanged", a)
	}
	fields(t, obs[0], map[string]any{"timing": "point_in_time", "service_from": nil, "service_to": nil})
	if a := amount(t, obs[0], "standalone_selling_price"); a != "1500.00 EUR" {
		t.Errorf("the goods' stand-alone selling price: got %s, want 1500.00 EUR, unchanged", a)
	}
	if a := amount(t, got, "transaction_price"); a != "2700.00 EUR" {
		t.Errorf("transaction price: got %s, want 2700.00 EUR, unchanged", a)
	}
	if got := allocated(t, got); !slices.Equal(got, []string{"-", "-"}) {
		t.Errorf("allocated amounts of a draft: got %v, want none", got)
	}
	read, _ := e.Must(200, "GET", path, "", "")
	if a, b := mustJSON(t, read), mustJSON(t, got); a != b {
		t.Errorf("read back: got %s, want %s", a, b)
	}
	if n := e.count(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'revenue.contract.update' AND object_id = $2`, id); n != 1 {
		t.Errorf("audit records of the change: got %d, want 1", n)
	}
	events := e.events(t, order)
	if len(events) != 2 {
		t.Fatalf("RevenueContractChanged events: got %d, want 2 (created, changed)", len(events))
	}
	fields(t, events[1], map[string]any{"status": "draft", "status_before": "draft", "version": float64(2), "customer_number": "BP70000001"})
	t.Run("a service period of one day and another price", func(t *testing.T) {
		got, _ := e.In(t).Must(200, "PATCH", path, `{"obligations": [{"obligation_no": 2, "service_from": "2026-12-31"},
			{"obligation_no": 1, "standalone_selling_price": {"amount": "1400.00", "currency": "EUR"}}]}`, `"2"`)
		obs := obligations(t, got)
		fields(t, obs[1], map[string]any{"timing": "over_time", "service_from": "2026-12-31", "service_to": "2026-12-31"})
		if a := amount(t, obs[1], "standalone_selling_price"); a != "1300.00 EUR" {
			t.Errorf("the service's stand-alone selling price: got %s, want 1300.00 EUR, unchanged", a)
		}
		if a := amount(t, obs[0], "standalone_selling_price"); a != "1400.00 EUR" {
			t.Errorf("the goods' stand-alone selling price: got %s, want 1400.00 EUR", a)
		}
	})
	t.Run("back to a point in time", func(t *testing.T) {
		got, _ := e.In(t).Must(200, "PATCH", path, `{"obligations": [{"obligation_no": 2, "timing": "point_in_time", "service_from": null, "service_to": null}]}`, `"3"`)
		fields(t, obligations(t, got)[1], map[string]any{"timing": "point_in_time", "service_from": nil, "service_to": nil})
	})
	t.Run("a replay answers the same", func(t *testing.T) {
		req := apitest.Req{Method: "PATCH", Path: path, Body: set(overTime(2, "2026-01-01", "2026-06-30", "1250.00")), Tenant: e.Tenant.ID, As: e.Tenant.Human,
			Key: apitest.NewKey(), IfMatch: `"4"`, Header: map[string][]string{"Content-Type": {"application/merge-patch+json"}}}
		first, second := e.API.Do(req), e.API.Do(req)
		if first.Code != 200 {
			t.Fatalf("status: got %d, want 200: %s", first.Code, first.Body)
		}
		if a, b := mustJSON(t, apitest.JSON(t, second)), mustJSON(t, apitest.JSON(t, first)); a != b {
			t.Errorf("replay: got %s, want the first answer %s", a, b)
		}
		if h := second.Header().Get("Idempotent-Replayed"); h != "true" {
			t.Errorf("Idempotent-Replayed: got %q, want true", h)
		}
	})
}

// TestObligationsRefused: a change naming an obligation the contract does
// not have or one twice, an over-time obligation without its service
// period or with one that ends before it starts, a point-in-time
// obligation with one, and a stand-alone selling price that is not
// positive or in another currency are refused naming the request's own
// field, and leave the contract as it was (T81b acceptance).
func TestObligationsRefused(t *testing.T) {
	e := setup(t)
	id, _ := e.contract(t, e.goods(), e.support())
	path := contracts + "/" + id
	price := func(amount, currency string) string {
		return fmt.Sprintf(`{"obligation_no": 1, "standalone_selling_price": {"amount": %q, "currency": %q}}`, amount, currency)
	}
	for _, c := range []struct {
		name   string
		body   string
		status int
		code   string
		field  any
	}{
		{"an obligation the contract does not have", set(overTime(3, "2026-01-01", "2026-12-31", "1.00")), 422, "revenue.unknown_obligation", "obligations[0].obligation_no"},
		{"an obligation named twice", set(price("1.00", "EUR"), price("2.00", "EUR")), 422, "revenue.obligation_repeated", "obligations[1].obligation_no"},
		{"over time without a service period", set(`{"obligation_no": 1, "timing": "over_time"}`), 422, "revenue.service_period_required", "obligations[0].service_from"},
		{"over time without the period's end", set(`{"obligation_no": 2, "timing": "over_time", "service_from": "2026-01-01"}`), 422,
			"revenue.service_period_required", "obligations[0].service_to"},
		{"a period that ends before it starts", set(price("1.00", "EUR"), overTime(2, "2026-02-01", "2026-01-31", "1.00")), 422,
			"revenue.invalid_service_period", "obligations[1].service_to"},
		{"a service period at a point in time", set(`{"obligation_no": 1, "service_from": "2026-01-01", "service_to": "2026-12-31"}`), 422,
			"revenue.service_period_not_allowed", "obligations[0].service_from"},
		{"a period's end at a point in time", set(`{"obligation_no": 1, "timing": "point_in_time", "service_to": "2026-12-31"}`), 422,
			"revenue.service_period_not_allowed", "obligations[0].service_to"},
		{"a price of zero", set(price("0.00", "EUR")), 422, "revenue.price_not_positive", "obligations[0].standalone_selling_price.amount"},
		{"a price below zero", set(price("-5.00", "EUR")), 422, "revenue.price_not_positive", "obligations[0].standalone_selling_price.amount"},
		{"a price in another currency", set(price("5.00", "USD")), 422, "revenue.currency_mismatch", "obligations[0].standalone_selling_price.currency"},
		{"a price with three decimals", set(price("5.001", "EUR")), 400, "request.invalid_amount", "obligations[0].standalone_selling_price.amount"},
		{"an unknown timing", set(`{"obligation_no": 1, "timing": "on_delivery"}`), 422, "revenue.unknown_timing", "obligations[0].timing"},
		{"no obligation", set(), 400, "request.invalid_body", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			if f := field(e.In(t).Problem(c.status, c.code, "PATCH", path, c.body, `"1"`)); f != c.field {
				t.Errorf("field: got %v, want %v", f, c.field)
			}
		})
	}
	ok := set(overTime(2, "2026-01-01", "2026-12-31", "1300.00"))
	t.Run("without the version read", func(t *testing.T) {
		e.In(t).Problem(428, "request.precondition_required", "PATCH", path, ok, "")
	})
	t.Run("at another version", func(t *testing.T) {
		e.In(t).Problem(412, "request.version_mismatch", "PATCH", path, ok, `"7"`)
	})
	t.Run("an unknown contract", func(t *testing.T) {
		e.In(t).Problem(404, "request.not_found", "PATCH", contracts+"/"+uuid.NewString(), ok, `"1"`)
	})
	t.Run("another company's contract", func(t *testing.T) {
		e.As(e.Reader(t, "US accountant", e.US)).In(t).Problem(404, "request.not_found", "PATCH", path, ok, `"1"`)
	})
	t.Run("the contract is as it was", func(t *testing.T) {
		got, tag := e.In(t).Must(200, "GET", path, "", "")
		if tag != `"1"` {
			t.Errorf("ETag: got %s, want \"1\"", tag)
		}
		for i, o := range obligations(t, got) {
			if o["timing"] != "point_in_time" {
				t.Errorf("obligation %d's timing: got %v, want point_in_time", i+1, o["timing"])
			}
		}
	})
	t.Run("without the permission to change", func(t *testing.T) {
		pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = 'revenue.contract.write'`, e.Tenant.ID)
		e.In(t).Problem(403, "identity.permission_missing", "PATCH", path, ok, `"1"`)
	})
}

// TestContractActivated: activating a draft allocates its transaction
// price to the obligations in proportion to their stand-alone selling
// prices, the shares adding up to the price to the minor unit, moves it
// to active once and fixes it: a later change and a second activation
// are refused; a replay answers the same; it is audited and published;
// nothing is posted (T81b acceptance).
func TestContractActivated(t *testing.T) {
	e := setup(t)
	id, order := e.contract(t, e.goods(), e.support())
	path := contracts + "/" + id
	e.Must(200, "PATCH", path, set(overTime(2, "2026-01-01", "2026-12-31", "1300.00")), `"1"`)
	e.Problem(428, "request.precondition_required", "POST", path+":activate", "", "")
	e.Problem(412, "request.version_mismatch", "POST", path+":activate", "", `"1"`)
	req := apitest.Req{Method: "POST", Path: path + ":activate", Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey(), IfMatch: `"2"`}
	first, second := e.API.Do(req), e.API.Do(req)
	for i, rec := range []*httptest.ResponseRecorder{first, second} {
		if rec.Code != 200 {
			t.Fatalf("status of call %d: got %d, want 200: %s", i+1, rec.Code, rec.Body)
		}
	}
	got := apitest.JSON(t, first)
	fields(t, got, map[string]any{"status": "active", "version": float64(3)})
	if tag := first.Header().Get("ETag"); tag != `"3"` {
		t.Errorf("ETag: got %s, want \"3\"", tag)
	}
	// 2700.00 × 1500 ÷ 2800 is 1446.428…, × 1300 is 1253.571…: 1446.42 + 1253.57 leaves a cent, which goes to the larger remainder.
	if got := allocated(t, got); !slices.Equal(got, []string{"1446.43 EUR", "1253.57 EUR"}) {
		t.Errorf("allocated amounts: got %v, want 1446.43 EUR and 1253.57 EUR", got)
	}
	if n := e.count(t, `SELECT count(*) FROM revenue.contract k WHERE k.tenant_id = $1 AND k.id = $2 AND k.transaction_price =
		(SELECT sum(o.allocated_amount) FROM revenue.performance_obligation o WHERE o.tenant_id = k.tenant_id AND o.contract_id = k.id)`, id); n != 1 {
		t.Errorf("the allocated amounts do not add up to the transaction price in minor units")
	}
	if a, b := mustJSON(t, apitest.JSON(t, second)), mustJSON(t, apitest.JSON(t, first)); a != b {
		t.Errorf("replay: got %s, want the first answer %s", a, b)
	}
	if h := second.Header().Get("Idempotent-Replayed"); h != "true" {
		t.Errorf("Idempotent-Replayed: got %q, want true", h)
	}
	read, _ := e.Must(200, "GET", path, "", "")
	if a, b := mustJSON(t, read), mustJSON(t, got); a != b {
		t.Errorf("read back: got %s, want %s", a, b)
	}
	if n := e.count(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'revenue.contract.activate' AND object_id = $2`, id); n != 1 {
		t.Errorf("audit records of the activation: got %d, want 1", n)
	}
	events := e.events(t, order)
	if len(events) != 3 {
		t.Fatalf("RevenueContractChanged events: got %d, want 3 (created, changed, activated)", len(events))
	}
	fields(t, events[2], map[string]any{"status": "active", "status_before": "draft", "version": float64(3)})
	apitest.NeverPublished(t, e.Tenant, "revenue", "Northwind Traders")
	if n := e.count(t, `SELECT count(*) FROM ledger.journal_entry WHERE tenant_id = $1`); n != 0 {
		t.Errorf("journal entries: got %d, want none (nothing is posted)", n)
	}
	t.Run("a later change", func(t *testing.T) {
		e.In(t).Problem(409, "revenue.invalid_status", "PATCH", path, set(overTime(2, "2026-01-01", "2026-12-31", "1200.00")), `"3"`)
	})
	t.Run("activated once", func(t *testing.T) {
		e.In(t).Problem(409, "revenue.invalid_status", "POST", path+":activate", "", `"3"`)
	})
	t.Run("listed among the active contracts", func(t *testing.T) {
		page, _ := e.In(t).Must(200, "GET", contracts+"?status=active", "", "")
		items := apitest.List(t, page, "items")
		if len(items) != 1 {
			t.Fatalf("active contracts: got %d, want 1", len(items))
		}
		if a, b := mustJSON(t, items[0]), mustJSON(t, got); a != b {
			t.Errorf("listed: got %s, want %s", a, b)
		}
	})
	t.Run("an unknown contract", func(t *testing.T) {
		e.In(t).Problem(404, "request.not_found", "POST", contracts+"/"+uuid.NewString()+":activate", "", `"1"`)
	})
	t.Run("another company's contract", func(t *testing.T) {
		other, _ := e.contract(t, e.goods())
		e.As(e.Reader(t, "US accountant", e.US)).In(t).Problem(404, "request.not_found", "POST", contracts+"/"+other+":activate", "", `"1"`)
	})
	t.Run("without the permission to activate", func(t *testing.T) {
		other, _ := e.contract(t, e.goods())
		pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = 'revenue.contract.activate'`, e.Tenant.ID)
		e.In(t).Problem(403, "identity.permission_missing", "POST", contracts+"/"+other+":activate", "", `"1"`)
	})
}

// TestAllocationWithTheStartingPrices: with the prices a contract starts
// with, the order items' net values, each obligation's allocated amount
// is its order item's value (T81b acceptance); so the contract the
// order's confirmation created, activated as it is, allocates the order's
// values.
func TestAllocationWithTheStartingPrices(t *testing.T) {
	e := setup(t)
	order, _ := e.confirmed(t, e.goods(), e.support(), e.item("Installation", "", "2.5", "10.01", "EUR"))
	var log pgtest.SyncBuffer
	if err := consumer(t, &log).Handle(context.Background(), e.busEvent(t, order)); err != nil {
		t.Fatalf("the order's event: got %v, want nil", err)
	}
	id := e.contractOf(t, order)["id"].(string)
	got, _ := e.Must(200, "POST", contracts+"/"+id+":activate", "", `"1"`)
	if got := allocated(t, got); !slices.Equal(got, []string{"1500.00 EUR", "1200.00 EUR", "25.03 EUR"}) {
		t.Errorf("allocated amounts: got %v, want the order's item values 1500.00, 1200.00 and 25.03 EUR", got)
	}
	fields(t, got, map[string]any{"status": "active", "version": float64(2), "updated_by": e.Tenant.Human.ID.String()})
}

// TestActivationAtOnce: of two activations of one contract at once, and
// of an activation and a change at once, one is done and the other, which
// waited for the contract's row lock, finds it at another version.
func TestActivationAtOnce(t *testing.T) {
	e := setup(t)
	for name, other := range map[string]apitest.Req{
		"two activations":            {Method: "POST", Path: ":activate"},
		"an activation and a change": {Method: "PATCH", Body: set(overTime(1, "2026-01-01", "2026-12-31", "900.00")), Header: map[string][]string{"Content-Type": {"application/merge-patch+json"}}},
	} {
		t.Run(name, func(t *testing.T) {
			id, order := e.contract(t, e.goods(), e.support())
			calls := []apitest.Req{{Method: "POST", Path: ":activate"}, other}
			codes := make([]int, len(calls))
			var wg sync.WaitGroup
			for i, c := range calls {
				c.Path, c.Tenant, c.As, c.Key, c.IfMatch = contracts+"/"+id+c.Path, e.Tenant.ID, e.Tenant.Human, apitest.NewKey(), `"1"`
				wg.Go(func() { codes[i] = e.API.Do(c).Code })
			}
			wg.Wait()
			slices.Sort(codes)
			if !slices.Equal(codes, []int{200, 412}) {
				t.Errorf("statuses: got %v, want one 200 and one 412", codes)
			}
			if n := len(e.events(t, order)); n != 2 {
				t.Errorf("RevenueContractChanged events: got %d, want 2 (created and the one change done)", n)
			}
			if n := e.count(t, `SELECT count(*) FROM revenue.contract k WHERE k.tenant_id = $1 AND k.id = $2 AND k.status = 'active' AND k.transaction_price IS DISTINCT FROM
				(SELECT sum(o.allocated_amount) FROM revenue.performance_obligation o WHERE o.tenant_id = k.tenant_id AND o.contract_id = k.id)`, id); n != 0 {
				t.Errorf("an active contract whose allocated amounts are not its transaction price")
			}
		})
	}
}
