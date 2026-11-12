package sales_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/sales"
)

// conditionTypes are a price, a customer discount and a promotion.
var conditionTypes = []config.ConditionType{{Code: "PR00", Name: "Price", Kind: config.ConditionPrice},
	{Code: "K007", Name: "Customer discount", Kind: config.ConditionDiscountPercent},
	{Code: "KA00", Name: "Promotion", Kind: config.ConditionDiscountPercent}}

// setupPricing is setup with the condition types and a procedure of the
// steps named, in that order; none: the types without a procedure.
func setupPricing(t *testing.T, steps ...string) env {
	t.Helper()
	return setupWith(t, func(doc *config.Config) {
		outputCodes(doc)
		doc.Pricing.ConditionTypes = conditionTypes
		for _, s := range steps {
			doc.Pricing.Procedure = append(doc.Pricing.Procedure, config.ProcedureStep{ConditionType: s})
		}
	})
}

// record is the JSON of a price record of company 2000 for BRACKET-M8
// valid from 2020-01-01 with the further fields given, each as the
// request names it.
func (e env) record(t *testing.T, more map[string]any) string {
	t.Helper()
	body := map[string]any{"legal_entity_id": e.DE, "item_key": "BRACKET-M8", "valid_from": "2020-01-01"}
	for k, v := range more {
		body[k] = v
	}
	return mustJSON(t, body)
}

// applied are an item's conditions, each as "type kind rate amount".
func applied(t *testing.T, it map[string]any) string {
	t.Helper()
	raw, _ := it["conditions"].([]any)
	out := make([]string, len(raw))
	for i, r := range raw {
		c, _ := r.(map[string]any)
		rate := fmt.Sprintf("%v%%", c["percent"])
		if c["kind"] == "price" {
			rate = fmt.Sprintf("%s per %v", amount(t, c, "unit_price"), c["price_unit"])
		}
		out[i] = fmt.Sprintf("%v %v %s = %s", c["condition_type"], c["name"], rate, amount(t, c, "amount"))
	}
	return strings.Join(out, "; ")
}

// TestDiscountRecordedAndReadBack: a customer's discount and a price of
// the procedure's price type are recorded, read back with their kind,
// found by their condition type, audited and published (T80b).
func TestDiscountRecordedAndReadBack(t *testing.T) {
	e := setupPricing(t, "PR00", "K007")
	discount, tag := e.Must(201, "POST", "/v1/sales-prices", e.priceBody(rec{condition: "K007", percent: "5.50", customer: e.customer, to: "2030-12-31"}), "")
	if tag != `"1"` {
		t.Errorf("ETag: got %s, want \"1\"", tag)
	}
	fields(t, discount, map[string]any{"kind": "discount_percent", "condition_type": "K007", "percent": "5.5", "unit": nil, "unit_price": nil,
		"price_unit": nil, "item_key": "BRACKET-M8", "customer_number": "BP70000001", "customer_name": "Northwind Traders", "valid_from": "2020-01-01",
		"valid_to": "2030-12-31", "company_code": "2000"})
	typed, _ := e.Must(201, "POST", "/v1/sales-prices", e.priceBody(rec{condition: "PR00", amount: "12.50", priceUnit: 10}), "")
	fields(t, typed, map[string]any{"kind": "price", "condition_type": "PR00", "percent": nil, "unit": "EA", "price_unit": float64(10)})
	if a := amount(t, typed, "unit_price"); a != "12.50 EUR" {
		t.Errorf("the typed price: got %s, want 12.50 EUR", a)
	}
	untyped, _ := e.Must(201, "POST", "/v1/sales-prices", e.priceBody(rec{}), "")
	fields(t, untyped, map[string]any{"kind": "price", "condition_type": nil, "percent": nil, "price_unit": float64(1)})
	id := discount["id"].(string)
	read, _ := e.Must(200, "GET", "/v1/sales-prices/"+id, "", "")
	if a, b := mustJSON(t, read), mustJSON(t, discount); a != b {
		t.Errorf("read back: got %s, want %s", a, b)
	}
	for _, c := range []struct{ name, query, want string }{
		{"the discounts", "condition_type=K007", id},
		{"the typed prices", "condition_type=PR00", typed["id"].(string)},
		{"a type without records", "condition_type=KA00", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, _ := e.In(t).Must(200, "GET", "/v1/sales-prices?"+c.query, "", "")
			var ids []string
			for _, it := range items(t, got) {
				ids = append(ids, it["id"].(string))
			}
			if s := strings.Join(ids, " "); s != c.want {
				t.Errorf("got %s, want %s", s, c.want)
			}
		})
	}
	if n := e.audits(t, "sales.price.create", id); n != 1 {
		t.Errorf("audit records of the discount: got %d, want 1", n)
	}
	want := map[string]any{"price_id": id, "legal_entity_id": e.DE.String(), "legal_entity_code": "2000", "item_key": "BRACKET-M8",
		"customer_id": e.customer.String(), "customer_number": "BP70000001", "kind": "discount_percent", "condition_type": "K007", "unit": nil,
		"unit_price": nil, "price_unit": nil, "percent": "5.5", "valid_from": "2020-01-01", "valid_to": "2030-12-31"}
	if a, b := mustJSON(t, e.events(t, "sales_price_changed", "price_id", id)), mustJSON(t, []map[string]any{want}); a != b {
		t.Errorf("SalesPriceChanged of the discount: got %s, want %s", a, b)
	}
	apitest.NeverPublished(t, e.Tenant, "sales", "Northwind Traders")
}

// TestConditionRecordRefusals: a record names a condition type of the
// configuration, and gives what its kind asks for and nothing of the
// other kind; a discount is more than 0 and less than 100 percent. Each
// refusal names the request's own field and writes nothing (T80b
// acceptance).
func TestConditionRecordRefusals(t *testing.T) {
	e := setupPricing(t, "PR00", "K007")
	price := map[string]any{"amount": "10.00", "currency": "EUR"}
	cases := []struct {
		name  string
		more  map[string]any
		code  string
		field string
	}{
		{"an unknown condition type", map[string]any{"condition_type": "ZZ00", "percent": "5"}, "sales.unknown_condition_type", "condition_type"},
		{"a discount with a unit", map[string]any{"condition_type": "K007", "percent": "5", "unit": "EA"}, "sales.price_or_percent", "unit"},
		{"a discount with a price", map[string]any{"condition_type": "K007", "percent": "5", "unit_price": price}, "sales.price_or_percent", "unit_price"},
		{"a discount with a price unit", map[string]any{"condition_type": "K007", "percent": "5", "price_unit": 10}, "sales.price_or_percent", "price_unit"},
		{"a discount without a percent", map[string]any{"condition_type": "K007"}, "sales.price_or_percent", "percent"},
		{"a discount type not in the procedure, without a percent", map[string]any{"condition_type": "KA00"}, "sales.price_or_percent", "percent"},
		{"a typed price with a percent", map[string]any{"condition_type": "PR00", "unit": "EA", "unit_price": price, "percent": "5"}, "sales.price_or_percent", "percent"},
		{"an untyped record with a percent", map[string]any{"unit": "EA", "unit_price": price, "percent": "5"}, "sales.price_or_percent", "percent"},
		{"a percent without a type", map[string]any{"percent": "5"}, "sales.price_or_percent", "percent"},
		{"a typed price without a unit", map[string]any{"condition_type": "PR00", "unit_price": price}, "sales.price_or_percent", "unit"},
		{"a price without a price", map[string]any{"unit": "EA"}, "sales.price_or_percent", "unit_price"},
		{"a discount of nothing", map[string]any{"condition_type": "K007", "percent": "0"}, "sales.invalid_percent", "percent"},
		{"a discount of nothing, with decimals", map[string]any{"condition_type": "K007", "percent": "0.000"}, "sales.invalid_percent", "percent"},
		{"a discount of everything", map[string]any{"condition_type": "K007", "percent": "100"}, "sales.invalid_percent", "percent"},
		{"a discount of more than everything", map[string]any{"condition_type": "K007", "percent": "100.5"}, "sales.invalid_percent", "percent"},
		{"a negative discount", map[string]any{"condition_type": "K007", "percent": "-5"}, "sales.invalid_percent", "percent"},
		{"a discount that is no number", map[string]any{"condition_type": "K007", "percent": "five"}, "sales.invalid_percent", "percent"},
		{"a discount of seven decimals", map[string]any{"condition_type": "K007", "percent": "5.1234567"}, "sales.invalid_percent", "percent"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := e.In(t).Problem(422, c.code, "POST", "/v1/sales-prices", e.record(t, c.more), "")
			if f := field(got); f != c.field {
				t.Errorf("field: got %v, want %s", f, c.field)
			}
		})
	}
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM sales.price WHERE tenant_id = $1`, []any{e.Tenant.ID}, &n)
	if n != 0 {
		t.Errorf("price records after the refusals: got %d, want none", n)
	}
	t.Run("the largest discount", func(t *testing.T) {
		got, _ := e.In(t).Must(201, "POST", "/v1/sales-prices", e.record(t, map[string]any{"condition_type": "K007", "percent": "99.999999"}), "")
		fields(t, got, map[string]any{"percent": "99.999999"})
	})
	t.Run("a tenant without condition types", func(t *testing.T) {
		plain := setup(t)
		got := plain.In(t).Problem(422, "sales.unknown_condition_type", "POST", "/v1/sales-prices",
			plain.record(t, map[string]any{"condition_type": "K007", "percent": "5"}), "")
		if f := field(got); f != "condition_type" {
			t.Errorf("field: got %v, want condition_type", f)
		}
	})
}

// TestItemConditions: with a pricing procedure an item shows each
// condition applied, in the procedure's order: the price, then each
// discount recorded for its key, the customer's before the general one,
// each a percentage of the value before it rounded half away from zero;
// the net value is the value less the discounts, exact in minor units
// (T80b acceptance).
func TestItemConditions(t *testing.T) {
	e := setupPricing(t, "PR00", "K007", "KA00")
	other := e.partner(t, "BP70000003", "Contoso", "active", "customer")
	e.priced(t, rec{amount: "10.01"})
	e.priced(t, rec{condition: "K007", percent: "5", customer: e.customer})
	e.priced(t, rec{condition: "KA00", percent: "10"})
	e.priced(t, rec{condition: "KA00", percent: "50", from: "2021-01-01", to: "2021-12-31"}) // over
	e.priced(t, rec{condition: "K007", percent: "7.5", key: "WASHER"})                       // another key's
	cases := []struct {
		name       string
		customer   uuid.UUID
		item       string
		conditions string
		pricing    string
	}{
		// 2.5 × 10.01 is 25.025: 25.03. 5% of 25.03 is 1.2515: 1.25,
		// leaving 23.78. 10% of 23.78 is 2.378: 2.38, leaving 21.40.
		{"the customer's discount, then the promotion on what is left", e.customer, e.unpriced(line{quantity: "2.5"}),
			"PR00 Price 10.01 EUR per 1 = 25.03 EUR; K007 Customer discount 5% = -1.25 EUR; KA00 Promotion 10% = -2.38 EUR",
			"10.01 EUR per 1, price_list, net 21.40 EUR"},
		// 10% of 25.03 is 2.503: 2.50.
		{"a customer without a discount of its own", other, e.unpriced(line{quantity: "2.5"}),
			"PR00 Price 10.01 EUR per 1 = 25.03 EUR; KA00 Promotion 10% = -2.50 EUR", "10.01 EUR per 1, price_list, net 22.53 EUR"},
		// 5% of 0.10 is 0.005: 0.01, half away from zero, leaving 0.09.
		// 10% of 0.09 is 0.009: 0.01, leaving 0.08.
		{"half a minor unit rounds away from zero, on an entered price", e.customer, e.item(line{price: "0.10", key: "BRACKET-M8"}),
			"PR00 Price 0.10 EUR per 1 = 0.10 EUR; K007 Customer discount 5% = -0.01 EUR; KA00 Promotion 10% = -0.01 EUR",
			"0.10 EUR per 1, manual, net 0.08 EUR"},
		// 10% of 0.04 is 0.004: nothing.
		{"a discount that rounds to nothing is still listed", other, e.item(line{price: "0.04", key: "BRACKET-M8"}),
			"PR00 Price 0.04 EUR per 1 = 0.04 EUR; KA00 Promotion 10% = 0.00 EUR", "0.04 EUR per 1, manual, net 0.04 EUR"},
		{"an item without a key has its price alone", e.customer, e.item(line{price: "20.00", quantity: "3", priceUnit: 2}),
			"PR00 Price 20.00 EUR per 2 = 30.00 EUR", "20.00 EUR per 2, manual, net 30.00 EUR"},
		{"a key without discounts", e.customer, e.item(line{price: "20.00", key: "NUT"}), "PR00 Price 20.00 EUR per 1 = 20.00 EUR",
			"20.00 EUR per 1, manual, net 20.00 EUR"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			it := e.quotedItem(t, c.customer, c.item)
			if got := applied(t, it); got != c.conditions {
				t.Errorf("conditions: got %s, want %s", got, c.conditions)
			}
			if got := pricing(t, it); got != c.pricing {
				t.Errorf("pricing: got %s, want %s", got, c.pricing)
			}
		})
	}
	t.Run("the customer's discount wins over the general one, the latest of each", func(t *testing.T) {
		e.priced(t, rec{condition: "K007", percent: "3"})
		e.priced(t, rec{condition: "K007", percent: "4", from: "2019-01-01"})
		e.priced(t, rec{condition: "K007", percent: "6", customer: e.customer, from: "2020-06-01"})
		// 6% of 100.10 is 6.006: 6.01, leaving 94.09; 10% of it 9.409: 9.41.
		if got, want := applied(t, e.quotedItem(t, e.customer, e.unpriced(line{quantity: "10"}))),
			"PR00 Price 10.01 EUR per 1 = 100.10 EUR; K007 Customer discount 6% = -6.01 EUR; KA00 Promotion 10% = -9.41 EUR"; got != want {
			t.Errorf("the customer's: got %s, want %s", got, want)
		}
		// 3% of 100.10 is 3.003: 3.00, leaving 97.10; 10% of it 9.71.
		if got, want := applied(t, e.quotedItem(t, other, e.unpriced(line{quantity: "10"}))),
			"PR00 Price 10.01 EUR per 1 = 100.10 EUR; K007 Customer discount 3% = -3.00 EUR; KA00 Promotion 10% = -9.71 EUR"; got != want {
			t.Errorf("another customer's: got %s, want %s", got, want)
		}
	})
	t.Run("a price of the procedure's type counts as a price", func(t *testing.T) {
		e.priced(t, rec{condition: "PR00", amount: "8.00", key: "NUT", from: "2021-01-01"})
		e.priced(t, rec{amount: "9.00", key: "NUT"})
		if got, want := pricing(t, e.quotedItem(t, other, e.unpriced(line{key: "NUT"}))), "8.00 EUR per 1, price_list, net 8.00 EUR"; got != want {
			t.Errorf("got %s, want %s, the later of the typed and the untyped record", got, want)
		}
	})
	t.Run("a discount is no price", func(t *testing.T) {
		got := e.In(t).Problem(422, "sales.no_price", "POST", "/v1/quotations", e.body(other, far, e.unpriced(line{key: "WASHER"})), "")
		if f := field(got); f != "items[0].unit_price" {
			t.Errorf("field: got %v, want items[0].unit_price", f)
		}
	})
	t.Run("discounts that leave nothing", func(t *testing.T) {
		e.priced(t, rec{condition: "K007", percent: "50", key: "CENT"})
		// 50% of 0.01 is 0.005: 0.01, leaving nothing.
		got := e.In(t).Problem(422, "sales.amount_not_positive", "POST", "/v1/quotations", e.body(other, far, e.item(line{price: "0.01", key: "CENT"})), "")
		if f := field(got); f != "items[0].unit_price.amount" {
			t.Errorf("field: got %v, want items[0].unit_price.amount", f)
		}
	})
}

// TestConditionsOnDocuments: a document's net value is the sum of its
// items' values after their discounts; an order from the quotation
// copies the items with their conditions, an order created directly is
// priced by the procedure too, and the published read gives them (T80b
// acceptance).
func TestConditionsOnDocuments(t *testing.T) {
	e := setupPricing(t, "PR00", "K007")
	e.priced(t, rec{amount: "10.01"})
	e.priced(t, rec{condition: "K007", percent: "5", customer: e.customer})
	const want = "PR00 Price 10.01 EUR per 1 = 25.03 EUR; K007 Customer discount 5% = -1.25 EUR"
	quotation := e.accepted(t, e.customer, e.unpriced(line{quantity: "2.5"}), e.item(line{price: "3.00"}))
	quoted, _ := e.Must(200, "GET", "/v1/quotations/"+quotation, "", "")
	if a := amount(t, quoted, "net_amount"); a != "26.78 EUR" {
		t.Errorf("the quotation's net value: got %s, want 26.78 EUR, 23.78 and 3.00", a)
	}
	e.priced(t, rec{condition: "K007", percent: "9", customer: e.customer, from: "2020-06-01"})
	order, _ := e.Must(201, "POST", "/v1/sales-orders", fromQuotation(quotation), "")
	if got := applied(t, items(t, order)[0]); got != want {
		t.Errorf("the order's conditions from the quotation: got %s, want %s", got, want)
	}
	if a := amount(t, order, "net_amount"); a != "26.78 EUR" {
		t.Errorf("the order's net value: got %s, want 26.78 EUR, the quotation's", a)
	}
	// 9% of 25.03 is 2.2527: 2.25.
	direct, _ := e.Must(201, "POST", "/v1/sales-orders", e.direct(e.customer, "", e.unpriced(line{quantity: "2.5"})), "")
	if got, want := applied(t, items(t, direct)[0]), "PR00 Price 10.01 EUR per 1 = 25.03 EUR; K007 Customer discount 9% = -2.25 EUR"; got != want {
		t.Errorf("a direct order's conditions: got %s, want %s", got, want)
	}
	if a := amount(t, direct, "net_amount"); a != "22.78 EUR" {
		t.Errorf("a direct order's net value: got %s, want 22.78 EUR", a)
	}
	err := e.inRequest(t, func(ctx context.Context) error {
		o, ok, err := sales.Orders{}.Order(ctx, uuid.MustParse(order["id"].(string)))
		if err != nil {
			return err
		}
		if !ok {
			t.Fatal("the published read: got no order, want the one created")
		}
		cs := o.Items[0].Conditions
		if len(cs) != 2 {
			t.Fatalf("the published read's conditions: got %d, want 2", len(cs))
		}
		if got := fmt.Sprintf("%s %d %s %d", cs[0].Type, cs[0].Amount, cs[1].Type, cs[1].Amount); got != "PR00 2503 K007 -125" {
			t.Errorf("the published read's conditions: got %s, want PR00 2503 K007 -125", got)
		}
		if o.Items[0].Net != 2378 {
			t.Errorf("the published read's net value: got %d, want 2378", o.Items[0].Net)
		}
		return nil
	})
	if err != nil {
		t.Error(err)
	}
}

// TestPricingWithoutAProcedure: a tenant without a pricing procedure
// prices as T80a does, though it has condition types: an item has its
// price, entered or recorded without a type, and no conditions; records
// of a type wait for a procedure that names it (T80b acceptance).
func TestPricingWithoutAProcedure(t *testing.T) {
	e := setupPricing(t)
	e.priced(t, rec{amount: "10.01"})
	e.priced(t, rec{condition: "PR00", amount: "8.00", from: "2021-01-01"})
	e.priced(t, rec{condition: "K007", percent: "5"})
	e.priced(t, rec{condition: "PR00", amount: "7.00", key: "NUT"})
	it := e.quotedItem(t, e.customer, e.unpriced(line{quantity: "2.5"}))
	if got, want := pricing(t, it), "10.01 EUR per 1, price_list, net 25.03 EUR"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	if cs, ok := it["conditions"].([]any); !ok || len(cs) != 0 {
		t.Errorf("conditions: got %v, want an empty list", it["conditions"])
	}
	manual := e.quotedItem(t, e.customer, e.item(line{price: "20.00", key: "BRACKET-M8"}))
	if got, want := pricing(t, manual), "20.00 EUR per 1, manual, net 20.00 EUR"; got != want {
		t.Errorf("an entered price: got %s, want %s", got, want)
	}
	e.In(t).Problem(422, "sales.no_price", "POST", "/v1/quotations", e.body(e.customer, far, e.unpriced(line{key: "NUT"})), "")
	t.Run("a tenant without the section", func(t *testing.T) {
		plain := setup(t)
		plain.priced(t, rec{amount: "10.01"})
		it := plain.quotedItem(t, plain.customer, plain.unpriced(line{quantity: "2.5"}))
		if got, want := pricing(t, it), "10.01 EUR per 1, price_list, net 25.03 EUR"; got != want {
			t.Errorf("got %s, want %s", got, want)
		}
		if cs, ok := it["conditions"].([]any); !ok || len(cs) != 0 {
			t.Errorf("conditions: got %v, want an empty list", it["conditions"])
		}
	})
}

// TestConditionDatabaseFailures calls the handlers that read the
// procedure and its discounts outside a unit of work and with each of
// their database statements failing in turn.
func TestConditionDatabaseFailures(t *testing.T) {
	e := setupPricing(t, "PR00", "K007", "KA00")
	h := endpoints(t, time.Now())
	e.priced(t, rec{})
	e.priced(t, rec{condition: "K007", percent: "5", customer: e.customer})
	e.priced(t, rec{condition: "KA00", percent: "10"})
	calls := map[string]func(ctx context.Context) error{
		"record a discount": func(ctx context.Context) error {
			_, err := h.CreateSalesPrice(ctx, &gen.SalesPriceCreate{LegalEntityID: e.DE, ItemKey: "BRACKET-M8", ConditionType: gen.NewOptString("K007"),
				Percent: gen.NewOptString("5"), ValidFrom: instant(t, "2026-01-01T00:00:00Z")}, gen.CreateSalesPriceParams{})
			return err
		},
		"record a typed price": func(ctx context.Context) error {
			_, err := h.CreateSalesPrice(ctx, &gen.SalesPriceCreate{LegalEntityID: e.DE, ItemKey: "BRACKET-M8", ConditionType: gen.NewOptString("PR00"),
				Unit: gen.NewOptString("EA"), UnitPrice: gen.NewOptMoney(gen.Money{Amount: "10.00", Currency: "EUR"}), PriceUnit: gen.NewOptInt32(10),
				ValidFrom: instant(t, "2026-01-01T00:00:00Z")}, gen.CreateSalesPriceParams{})
			return err
		},
		"a quotation with discounts": func(ctx context.Context) error {
			_, err := h.CreateQuotation(ctx, &gen.QuotationCreate{LegalEntityID: e.DE, CustomerID: e.customer, ValidUntil: instant(t, far+"T00:00:00Z"),
				Currency: "EUR", Items: []gen.SalesItemCreate{{ItemKey: gen.NewOptString("BRACKET-M8"), Description: "Bracket",
					Quantity: gen.Quantity{Value: "2", Unit: "EA"}, AccountID: e.revenue}}}, gen.CreateQuotationParams{})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, call)
		})
	}
}
