package revenue_test

import (
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestContractCreatedOnRequest: a person creates the contract of a
// confirmed order that has none; it is a draft with the order's net value
// as its transaction price and one obligation per order item, each
// satisfied at a point in time with the item's net value as its
// stand-alone selling price; it reads back the same; it is audited and
// published; nothing is posted; a second is refused and a replay answers
// the same (T81a acceptance).
func TestContractCreatedOnRequest(t *testing.T) {
	e := setup(t)
	// 2.5 × 10.01 is 25.025: the order's item value is 25.03, which the obligation takes as it is.
	order, number := e.confirmed(t, e.goods(), e.support(), e.item("Installation", "", "2.5", "10.01", "EUR"))
	req := apitest.Req{Method: "POST", Path: contracts, Body: forOrder(order), Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey()}
	first := e.API.Do(req)
	if first.Code != 201 {
		t.Fatalf("status: got %d, want 201: %s", first.Code, first.Body)
	}
	got := apitest.JSON(t, first)
	id, _ := got["id"].(string)
	if tag := first.Header().Get("ETag"); tag != `"1"` {
		t.Errorf("ETag: got %s, want \"1\"", tag)
	}
	if loc := first.Header().Get("Location"); loc != contracts+"/"+id {
		t.Errorf("Location: got %s, want %s", loc, contracts+"/"+id)
	}
	fields(t, got, map[string]any{"status": "draft", "version": float64(1), "company_code": "2000", "company_name": "Acme GmbH",
		"customer_id": e.customer.String(), "customer_number": "BP70000001", "customer_name": "Northwind Traders", "currency": "EUR",
		"legal_entity_id": e.DE.String(), "sales_order_id": order, "sales_order_number": number, "created_by": e.Tenant.Human.ID.String()})
	if a := amount(t, got, "transaction_price"); a != "2725.03 EUR" {
		t.Errorf("transaction price: got %s, want 2725.03 EUR, the order's net value", a)
	}
	obs := obligations(t, got)
	if len(obs) != 3 {
		t.Fatalf("obligations: got %d, want 3, one per order item", len(obs))
	}
	for i, want := range []struct {
		description, net string
		key              any
	}{{"Machined bracket M8", "1500.00 EUR", "BRACKET-M8"}, {"Support for the fiscal year", "1200.00 EUR", nil}, {"Installation", "25.03 EUR", nil}} {
		t.Run(fmt.Sprintf("obligation %d", i+1), func(t *testing.T) {
			fields(t, obs[i], map[string]any{"obligation_no": float64(i + 1), "order_item_no": float64(i + 1), "description": want.description,
				"item_key": want.key, "timing": "point_in_time"})
			if a := amount(t, obs[i], "net_amount"); a != want.net {
				t.Errorf("the order item's net value: got %s, want %s", a, want.net)
			}
			if a := amount(t, obs[i], "standalone_selling_price"); a != want.net {
				t.Errorf("stand-alone selling price: got %s, want the item's net value %s", a, want.net)
			}
		})
	}
	read, tag := e.Must(200, "GET", contracts+"/"+id, "", "")
	if a, b := mustJSON(t, read), mustJSON(t, got); a != b {
		t.Errorf("read back: got %s, want %s", a, b)
	}
	if tag != `"1"` {
		t.Errorf("ETag read back: got %s, want \"1\"", tag)
	}
	if n := e.audits(t, id, "request"); n != 1 {
		t.Errorf("audit records of the creation on request: got %d, want 1", n)
	}
	want := map[string]any{"revenue_contract_id": id, "sales_order_id": order, "sales_order_number": number, "legal_entity_id": e.DE.String(),
		"legal_entity_code": "2000", "customer_id": e.customer.String(), "customer_number": "BP70000001",
		"transaction_price": map[string]any{"amount": "2725.03", "currency": "EUR"}, "obligation_count": float64(3), "status": "draft",
		"status_before": nil, "version": float64(1)}
	if a, b := mustJSON(t, e.events(t, order)), mustJSON(t, []map[string]any{want}); a != b {
		t.Errorf("RevenueContractChanged: got %s, want %s", a, b)
	}
	apitest.NeverPublished(t, e.Tenant, "revenue", "Northwind Traders")
	if n := e.count(t, `SELECT count(*) FROM ledger.journal_entry WHERE tenant_id = $1`); n != 0 {
		t.Errorf("journal entries: got %d, want none (nothing is posted)", n)
	}
	t.Run("a replay answers the same", func(t *testing.T) {
		second := e.API.Do(req)
		if second.Code != 201 {
			t.Fatalf("status: got %d, want 201: %s", second.Code, second.Body)
		}
		if a, b := mustJSON(t, apitest.JSON(t, second)), mustJSON(t, got); a != b {
			t.Errorf("replay: got %s, want the first answer %s", a, b)
		}
		if h := second.Header().Get("Idempotent-Replayed"); h != "true" {
			t.Errorf("Idempotent-Replayed: got %q, want true", h)
		}
	})
	t.Run("one contract per sales order", func(t *testing.T) {
		e.In(t).Problem(409, "revenue.contract_exists", "POST", contracts, forOrder(order), "")
		if n := e.contractsOf(t, order); n != 1 {
			t.Errorf("contracts of the order: got %d, want 1", n)
		}
		if n := len(e.events(t, order)); n != 1 {
			t.Errorf("RevenueContractChanged events: got %d, want 1", n)
		}
	})
}

// TestContractRefused: no contract is created for an order that is not
// confirmed, is in a foreign currency, does not exist or is of a company
// the caller may not act in, each refusal naming sales_order_id; nor
// without the permission (T81a acceptance).
func TestContractRefused(t *testing.T) {
	e := setup(t)
	draft := e.draft(t, "EUR", e.goods())["id"].(string)
	foreign := e.foreign(t)
	order, _ := e.confirmed(t, e.goods())
	stranger := e.As(e.Reader(t, "US accountant", e.US))
	for _, c := range []struct {
		name   string
		client apitest.Client
		order  string
		code   string
	}{
		{"a draft order", e.Client, draft, "revenue.order_not_confirmed"},
		{"an order in a foreign currency", e.Client, foreign, "revenue.foreign_currency"},
		{"an unknown order", e.Client, uuid.NewString(), "request.reference_not_found"},
		{"another company's order", stranger, order, "request.reference_not_found"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if f := field(c.client.In(t).Problem(422, c.code, "POST", contracts, forOrder(c.order), "")); f != "sales_order_id" {
				t.Errorf("field: got %v, want sales_order_id", f)
			}
			if n := e.contractsOf(t, c.order); n != 0 {
				t.Errorf("contracts of the order: got %d, want none", n)
			}
		})
	}
	t.Run("without a sales order", func(t *testing.T) {
		e.In(t).Problem(400, "request.invalid_body", "POST", contracts, `{}`, "")
	})
	t.Run("without the permission to create", func(t *testing.T) {
		pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = 'revenue.contract.write'`, e.Tenant.ID)
		e.In(t).Problem(403, "identity.permission_missing", "POST", contracts, forOrder(order), "")
	})
}

// TestContractsReadByCompany: a contract of a company the caller may not
// act in is not found, as one that does not exist, and is not listed.
func TestContractsReadByCompany(t *testing.T) {
	e := setup(t)
	order, _ := e.confirmed(t, e.goods())
	got, _ := e.Must(201, "POST", contracts, forOrder(order), "")
	id := got["id"].(string)
	stranger := e.As(e.Reader(t, "US accountant", e.US))
	t.Run("another company's contract", func(t *testing.T) {
		stranger.In(t).Problem(404, "request.not_found", "GET", contracts+"/"+id, "", "")
	})
	t.Run("an unknown contract", func(t *testing.T) {
		e.In(t).Problem(404, "request.not_found", "GET", contracts+"/"+uuid.NewString(), "", "")
	})
	t.Run("another company's contracts are not listed", func(t *testing.T) {
		page, _ := stranger.In(t).Must(200, "GET", contracts, "", "")
		if n := len(apitest.List(t, page, "items")); n != 0 {
			t.Errorf("contracts listed: got %d, want none", n)
		}
	})
}

// TestContractsListed: the list filters by company, customer, sales order
// and status, pages by the orders' numbers and gives each contract as the
// single read does.
func TestContractsListed(t *testing.T) {
	e := setup(t)
	var ids, orders, numbers []string
	for range 3 {
		order, number := e.confirmed(t, e.goods(), e.support())
		got, _ := e.Must(201, "POST", contracts, forOrder(order), "")
		ids, orders, numbers = append(ids, got["id"].(string)), append(orders, order), append(numbers, number)
	}
	listed := func(t *testing.T, query string) []string {
		t.Helper()
		page, _ := e.In(t).Must(200, "GET", contracts+query, "", "")
		var out []string
		for _, it := range apitest.List(t, page, "items") {
			out = append(out, it.(map[string]any)["sales_order_number"].(string))
		}
		return out
	}
	for _, c := range []struct {
		name, query string
		want        []string
	}{
		{"all, by the orders' numbers", "", numbers},
		{"latest first", "?order_by=sales_order_number%20desc", []string{numbers[2], numbers[1], numbers[0]}},
		{"of the company", "?legal_entity_id=" + e.DE.String(), numbers},
		{"of another company", "?legal_entity_id=" + e.US.String(), nil},
		{"with the customer", "?customer_id=" + e.customer.String(), numbers},
		{"with another customer", "?customer_id=" + uuid.NewString(), nil},
		{"of a sales order", "?sales_order_id=" + orders[1], numbers[1:2]},
		{"drafts", "?status=draft", numbers},
		{"in another status", "?status=active", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := listed(t, c.query); mustJSON(t, got) != mustJSON(t, c.want) {
				t.Errorf("contracts: got %v, want %v", got, c.want)
			}
		})
	}
	t.Run("page by page", func(t *testing.T) {
		var got []string
		query := "?page_size=2"
		for range 2 {
			page, _ := e.In(t).Must(200, "GET", contracts+query, "", "")
			for _, it := range apitest.List(t, page, "items") {
				got = append(got, it.(map[string]any)["id"].(string))
			}
			token, _ := page["next_page_token"].(string)
			query = "?page_size=2&page_token=" + token
		}
		if mustJSON(t, got) != mustJSON(t, ids) {
			t.Errorf("contracts over two pages: got %v, want %v", got, ids)
		}
	})
	t.Run("as the single read gives it", func(t *testing.T) {
		single, _ := e.In(t).Must(200, "GET", contracts+"/"+ids[0], "", "")
		if a, b := mustJSON(t, e.contractOf(t, orders[0])), mustJSON(t, single); a != b {
			t.Errorf("listed: got %s, want %s", a, b)
		}
	})
	t.Run("an unknown sort field", func(t *testing.T) {
		e.In(t).Problem(400, "request.invalid_order_by", "GET", contracts+"?order_by=status", "", "")
	})
}
