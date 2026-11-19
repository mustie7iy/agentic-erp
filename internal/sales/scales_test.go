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
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// qty and eur are a scale level's start as the request gives it.
func qty(value, unit string) map[string]any { return map[string]any{"value": value, "unit": unit} }
func eur(value string) map[string]any       { return map[string]any{"amount": value, "currency": "EUR"} }

// boltPrice is the request fields of BOLT-M10's general price, 0.80 EUR
// each, 0.70 from 1000 EA and 0.60 from 5000.
func boltPrice() map[string]any {
	return map[string]any{"item_key": "BOLT-M10", "unit": "EA", "unit_price": eur("0.80"), "scale_basis": "quantity", "scale": []any{
		map[string]any{"from_quantity": qty("1000", "EA"), "unit_price": eur("0.70")},
		map[string]any{"from_quantity": qty("5000", "EA"), "unit_price": eur("0.60")}}}
}

// boltDiscount is the request fields of a K007 discount on BOLT-M10 for
// the customer, one percent, two from an item value of 1000.00 EUR and
// three from 5000.00.
func (e env) boltDiscount() map[string]any {
	return map[string]any{"item_key": "BOLT-M10", "customer_id": e.customer, "condition_type": "K007", "percent": "1", "scale_basis": "value",
		"scale": []any{map[string]any{"from_amount": eur("1000.00"), "percent": "2"}, map[string]any{"from_amount": eur("5000.00"), "percent": "3"}}}
}

// scaleOf is a record's scale as "basis: level from rate; ...".
func scaleOf(t *testing.T, p map[string]any) string {
	t.Helper()
	raw, _ := p["scale"].([]any)
	out := make([]string, len(raw))
	for i, r := range raw {
		l, _ := r.(map[string]any)
		from := fmt.Sprintf("%v", l["from_quantity"])
		if q, ok := l["from_quantity"].(map[string]any); ok {
			from = fmt.Sprintf("%v %v", q["value"], q["unit"])
		} else if _, ok := l["from_amount"]; ok {
			from = amount(t, l, "from_amount")
		}
		rate := fmt.Sprintf("%v%%", l["percent"])
		if _, ok := l["unit_price"]; ok {
			rate = amount(t, l, "unit_price")
		}
		out[i] = fmt.Sprintf("%v from %s at %s", l["level"], from, rate)
	}
	return fmt.Sprintf("%v: %s", p["scale_basis"], strings.Join(out, "; "))
}

// TestScaleRecordedAndReadBack: a price with a quantity scale and a
// discount with a value scale are recorded, read back and listed with
// their levels, and published with them (T80e).
func TestScaleRecordedAndReadBack(t *testing.T) {
	e := setupPricing(t, "PR00", "K007")
	price, _ := e.Must(201, "POST", "/v1/sales-prices", e.record(t, boltPrice()), "")
	discount, _ := e.Must(201, "POST", "/v1/sales-prices", e.record(t, e.boltDiscount()), "")
	plain, _ := e.Must(201, "POST", "/v1/sales-prices", e.priceBody(rec{}), "")
	cases := []struct {
		name string
		made map[string]any
		want string
	}{
		{"a price by quantity", price, "quantity: 1 from 1000 EA at 0.70 EUR; 2 from 5000 EA at 0.60 EUR"},
		{"a discount by value", discount, "value: 1 from 1000.00 EUR at 2%; 2 from 5000.00 EUR at 3%"},
		{"a record without a scale", plain, "<nil>: "},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := scaleOf(t, c.made); got != c.want {
				t.Errorf("scale recorded: got %s, want %s", got, c.want)
			}
			id, _ := c.made["id"].(string)
			read, _ := e.In(t).Must(200, "GET", "/v1/sales-prices/"+id, "", "")
			if a, b := mustJSON(t, read), mustJSON(t, c.made); a != b {
				t.Errorf("read back: got %s, want %s", a, b)
			}
		})
	}
	listed, _ := e.Must(200, "GET", "/v1/sales-prices?item_key=BOLT-M10&order_by=valid_from", "", "")
	var scales []string
	for _, it := range items(t, listed) {
		scales = append(scales, scaleOf(t, it))
	}
	if got, want := strings.Join(scales, " | "), cases[0].want+" | "+cases[1].want; got != want {
		t.Errorf("listed: got %s, want %s", got, want)
	}
	wantPrice := []any{map[string]any{"level": float64(1), "from_quantity": map[string]any{"value": "1000", "unit": "EA"}, "from_amount": nil,
		"unit_price": eur("0.70"), "percent": nil}, map[string]any{"level": float64(2), "from_quantity": map[string]any{"value": "5000", "unit": "EA"},
		"from_amount": nil, "unit_price": eur("0.60"), "percent": nil}}
	wantDiscount := []any{map[string]any{"level": float64(1), "from_quantity": nil, "from_amount": eur("1000.00"), "unit_price": nil, "percent": "2"},
		map[string]any{"level": float64(2), "from_quantity": nil, "from_amount": eur("5000.00"), "unit_price": nil, "percent": "3"}}
	for name, c := range map[string]struct {
		id, basis string
		levels    []any
	}{"the price": {price["id"].(string), "quantity", wantPrice}, "the discount": {discount["id"].(string), "value", wantDiscount}} {
		t.Run("SalesPriceChanged of "+name, func(t *testing.T) {
			events := e.events(t, "sales_price_changed", "price_id", c.id)
			if len(events) != 1 {
				t.Fatalf("events: got %d, want 1", len(events))
			}
			if events[0]["scale_basis"] != c.basis {
				t.Errorf("scale_basis: got %v, want %s", events[0]["scale_basis"], c.basis)
			}
			if a, b := mustJSON(t, events[0]["scale"]), mustJSON(t, c.levels); a != b {
				t.Errorf("scale: got %s, want %s", a, b)
			}
		})
	}
}

// TestScaleRefusals: a scale is refused naming the field when its basis
// or its levels are missing, its basis is unknown or a price's by value,
// a level starts at the other basis's field, at nothing, at zero, not
// above the one before or in another unit or currency than the first,
// or gives the other kind's rate or one out of range; nothing is recorded
// (T80e acceptance).
func TestScaleRefusals(t *testing.T) {
	e := setupPricing(t, "PR00", "K007")
	levels := func(l ...map[string]any) []any {
		out := make([]any, len(l))
		for i := range l {
			out[i] = l[i]
		}
		return out
	}
	price := func(basis any, l ...map[string]any) map[string]any {
		return map[string]any{"unit": "EA", "unit_price": eur("0.80"), "scale_basis": basis, "scale": levels(l...)}
	}
	discount := func(basis any, l ...map[string]any) map[string]any {
		return map[string]any{"condition_type": "K007", "percent": "1", "scale_basis": basis, "scale": levels(l...)}
	}
	at := func(from map[string]any, rate string, value any) map[string]any {
		l := map[string]any{rate: value}
		for k, v := range from {
			l[k] = v
		}
		return l
	}
	q := func(v, u string) map[string]any { return map[string]any{"from_quantity": qty(v, u)} }
	a := func(v, c string) map[string]any {
		return map[string]any{"from_amount": map[string]any{"amount": v, "currency": c}}
	}
	cases := []struct {
		name   string
		fields map[string]any
		status int
		code   string
		field  string
	}{
		{"levels without a basis", map[string]any{"unit": "EA", "unit_price": eur("0.80"), "scale": levels(at(q("10", "EA"), "unit_price", eur("0.70")))},
			422, "sales.invalid_scale", "scale_basis"},
		{"a basis without levels", map[string]any{"unit": "EA", "unit_price": eur("0.80"), "scale_basis": "quantity"}, 422, "sales.invalid_scale", "scale"},
		{"an unknown basis", price("weight", at(q("10", "EA"), "unit_price", eur("0.70"))), 422, "sales.invalid_scale", "scale_basis"},
		{"a price by value", price("value", at(a("100.00", "EUR"), "unit_price", eur("0.70"))), 422, "sales.invalid_scale", "scale_basis"},
		{"a quantity level from a value", price("quantity", at(a("100.00", "EUR"), "unit_price", eur("0.70"))), 422, "sales.invalid_scale",
			"scale[0].from_amount"},
		{"a quantity level from nowhere", price("quantity", map[string]any{"unit_price": eur("0.70")}), 422, "sales.invalid_scale", "scale[0].from_quantity"},
		{"a quantity level from zero", price("quantity", at(q("0", "EA"), "unit_price", eur("0.70"))), 422, "sales.invalid_scale",
			"scale[0].from_quantity.value"},
		{"a price's level in another unit", price("quantity", at(q("10", "KG"), "unit_price", eur("0.70"))), 422, "sales.invalid_scale",
			"scale[0].from_quantity.unit"},
		{"a discount's level in an unknown unit", discount("quantity", at(q("10", "XX"), "percent", "2")), 422, "sales.unknown_unit",
			"scale[0].from_quantity.unit"},
		{"a discount's levels in two units", discount("quantity", at(q("10", "EA"), "percent", "2"), at(q("20", "KG"), "percent", "3")), 422,
			"sales.invalid_scale", "scale[1].from_quantity.unit"},
		{"a level from the quantity before it", price("quantity", at(q("10", "EA"), "unit_price", eur("0.70")), at(q("10.0", "EA"), "unit_price", eur("0.60"))),
			422, "sales.invalid_scale", "scale[1].from_quantity.value"},
		{"a level below the one before", price("quantity", at(q("10", "EA"), "unit_price", eur("0.70")), at(q("9.999999", "EA"), "unit_price", eur("0.60"))),
			422, "sales.invalid_scale", "scale[1].from_quantity.value"},
		{"a value level from a quantity", discount("value", at(q("10", "EA"), "percent", "2")), 422, "sales.invalid_scale", "scale[0].from_quantity"},
		{"a value level from nowhere", discount("value", map[string]any{"percent": "2"}), 422, "sales.invalid_scale", "scale[0].from_amount"},
		{"a value level in an unknown currency", discount("value", at(a("100.00", "XXA"), "percent", "2")), 422, "sales.unknown_currency",
			"scale[0].from_amount.currency"},
		{"value levels in two currencies", discount("value", at(a("100.00", "EUR"), "percent", "2"), at(a("200.00", "USD"), "percent", "3")), 422,
			"sales.invalid_scale", "scale[1].from_amount.currency"},
		{"a value level from zero", discount("value", at(a("0.00", "EUR"), "percent", "2")), 422, "sales.invalid_scale", "scale[0].from_amount.amount"},
		{"a value level of too many decimals", discount("value", at(a("1.001", "EUR"), "percent", "2")), 400, "request.invalid_amount",
			"scale[0].from_amount.amount"},
		{"a value level from the value before it", discount("value", at(a("100.00", "EUR"), "percent", "2"), at(a("100.00", "EUR"), "percent", "3")),
			422, "sales.invalid_scale", "scale[1].from_amount.amount"},
		{"a price's level with a percent", price("quantity", at(q("10", "EA"), "percent", "2")), 422, "sales.invalid_scale", "scale[0].percent"},
		{"a price's level without a price", price("quantity", q("10", "EA")), 422, "sales.invalid_scale", "scale[0].unit_price"},
		{"a price's level in another currency", price("quantity", at(q("10", "EA"), "unit_price", map[string]any{"amount": "0.70", "currency": "USD"})),
			422, "sales.currency_mismatch", "scale[0].unit_price.currency"},
		{"a price's level of nothing", price("quantity", at(q("10", "EA"), "unit_price", eur("0.00"))), 422, "sales.amount_not_positive",
			"scale[0].unit_price.amount"},
		{"a price's level of too many decimals", price("quantity", at(q("10", "EA"), "unit_price", eur("0.701"))), 400, "request.invalid_amount",
			"scale[0].unit_price.amount"},
		{"a discount's level with a price", discount("quantity", at(q("10", "EA"), "unit_price", eur("0.70"))), 422, "sales.invalid_scale",
			"scale[0].unit_price"},
		{"a discount's level without a percent", discount("quantity", q("10", "EA")), 422, "sales.invalid_scale", "scale[0].percent"},
		{"a discount's level of everything", discount("quantity", at(q("10", "EA"), "percent", "100")), 422, "sales.invalid_percent", "scale[0].percent"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := e.In(t).Problem(c.status, c.code, "POST", "/v1/sales-prices", e.record(t, c.fields), "")
			if f := field(got); f != c.field {
				t.Errorf("field: got %v, want %v", f, c.field)
			}
		})
	}
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM sales.price WHERE tenant_id = $1`, []any{e.Tenant.ID}, &n)
	if n != 0 {
		t.Errorf("price records after the refusals: got %d, want none", n)
	}
}

// scaled are an item's conditions, each as "type rate [basis level from]
// = amount", the scale part only for a scaled record's.
func scaled(t *testing.T, it map[string]any) string {
	t.Helper()
	raw, _ := it["conditions"].([]any)
	out := make([]string, len(raw))
	for i, r := range raw {
		c, _ := r.(map[string]any)
		rate := fmt.Sprintf("%v%%", c["percent"])
		if c["kind"] == "price" {
			rate = amount(t, c, "unit_price")
		}
		s := ""
		if sc, ok := c["scale"].(map[string]any); ok {
			from := ""
			if q, ok := sc["from_quantity"].(map[string]any); ok {
				from = fmt.Sprintf(" from %v %v", q["value"], q["unit"])
			} else if _, ok := sc["from_amount"]; ok {
				from = " from " + amount(t, sc, "from_amount")
			}
			s = fmt.Sprintf(" [%v %v%s]", sc["basis"], sc["level"], from)
		}
		out[i] = fmt.Sprintf("%v %s%s = %s", c["condition_type"], rate, s, amount(t, c, "amount"))
	}
	return strings.Join(out, "; ")
}

// TestScaleDetermination: an item priced from a scaled record takes the
// rate of the highest level its quantity, or its value before a
// discount, reaches, and the record's own below the first, exact in
// minor units; a discount scaled in another unit does not count; an
// entered price has no scale and no discount; without a procedure the
// price's scale still applies (T80e acceptance).
func TestScaleDetermination(t *testing.T) {
	e := setupPricing(t, "PR00", "K007", "KA00")
	other := e.partner(t, "BP70000003", "Contoso", "active", "customer")
	e.Must(201, "POST", "/v1/sales-prices", e.record(t, boltPrice()), "")
	e.Must(201, "POST", "/v1/sales-prices", e.record(t, e.boltDiscount()), "")
	promotion := func(from, unit, percent string) map[string]any {
		return map[string]any{"item_key": "BOLT-M10", "condition_type": "KA00", "percent": "0.5", "valid_from": from, "scale_basis": "quantity",
			"scale": []any{map[string]any{"from_quantity": qty("2000", unit), "percent": percent}}}
	}
	e.Must(201, "POST", "/v1/sales-prices", e.record(t, promotion("2020-01-01", "EA", "1.5")), "")
	e.Must(201, "POST", "/v1/sales-prices", e.record(t, promotion("2021-01-01", "KG", "10")), "") // later, but in another unit
	bolts := func(quantity string) string { return e.unpriced(line{key: "BOLT-M10", quantity: quantity}) }
	cases := []struct {
		name     string
		customer uuid.UUID
		item     string
		want     string
	}{
		// 999.999999 × 0.80 is 799.9999992: 800.00. 1% of it is 8.00,
		// leaving 792.00; 0.5% of that is 3.96, leaving 788.04.
		{"just below every first level", e.customer, bolts("999.999999"),
			"PR00 0.80 EUR [quantity 0] = 800.00 EUR; K007 1% [value 0] = -8.00 EUR; KA00 0.5% [quantity 0] = -3.96 EUR"},
		// 1000 × 0.70 is 700.00; 1% is 7.00, leaving 693.00; 0.5% of
		// that is 3.465: 3.47 half away from zero.
		{"the first price level", e.customer, bolts("1000"),
			"PR00 0.70 EUR [quantity 1 from 1000 EA] = 700.00 EUR; K007 1% [value 0] = -7.00 EUR; KA00 0.5% [quantity 0] = -3.47 EUR"},
		// 6000 × 0.60 is 3600.00; 2% is 72.00, leaving 3528.00; 1.5% of
		// that is 52.92.
		{"the second price level and the first value level", e.customer, bolts("6000"),
			"PR00 0.60 EUR [quantity 2 from 5000 EA] = 3600.00 EUR; K007 2% [value 1 from 1000.00 EUR] = -72.00 EUR; " +
				"KA00 1.5% [quantity 1 from 2000 EA] = -52.92 EUR"},
		// 8333.333333 × 0.60 is 4999.9999998: 5000.00, the second value
		// level's start exactly. 3% is 150.00, leaving 4850.00; 1.5% of
		// that is 72.75.
		{"a value level reached exactly by the value before it", e.customer, bolts("8333.333333"),
			"PR00 0.60 EUR [quantity 2 from 5000 EA] = 5000.00 EUR; K007 3% [value 2 from 5000.00 EUR] = -150.00 EUR; " +
				"KA00 1.5% [quantity 1 from 2000 EA] = -72.75 EUR"},
		{"a customer without the discount", other, bolts("6000"),
			"PR00 0.60 EUR [quantity 2 from 5000 EA] = 3600.00 EUR; KA00 1.5% [quantity 1 from 2000 EA] = -54.00 EUR"},
		{"an entered price", e.customer, e.item(line{key: "BOLT-M10", quantity: "6000", price: "0.65"}), "PR00 0.65 EUR = 3900.00 EUR"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := e.In(t).Must(201, "POST", "/v1/quotations", e.body(c.customer, far, c.item), "")
			if s := scaled(t, items(t, got)[0]); s != c.want {
				t.Errorf("conditions: got %s, want %s", s, c.want)
			}
		})
	}
	t.Run("a sales order's item, and the order's net value", func(t *testing.T) {
		got, _ := e.In(t).Must(201, "POST", "/v1/sales-orders", e.direct(e.customer, "", bolts("1000")), "")
		if s := scaled(t, items(t, got)[0]); s != cases[1].want {
			t.Errorf("conditions: got %s, want %s", s, cases[1].want)
		}
		if a := amount(t, got, "net_amount"); a != "689.53 EUR" {
			t.Errorf("net value: got %s, want 689.53 EUR", a)
		}
	})
}

// TestScaleWithoutAProcedure: a tenant without a pricing procedure takes
// a scaled price's level and lists no conditions (T80e, ADR-0067 §12).
func TestScaleWithoutAProcedure(t *testing.T) {
	e := setup(t)
	e.Must(201, "POST", "/v1/sales-prices", e.record(t, boltPrice()), "")
	it := e.quotedItem(t, e.customer, e.unpriced(line{key: "BOLT-M10", quantity: "5000"}))
	if p := pricing(t, it); p != "0.60 EUR per 1, price_list, net 3000.00 EUR" {
		t.Errorf("pricing: got %s, want 0.60 EUR per 1, price_list, net 3000.00 EUR", p)
	}
	if s := scaled(t, it); s != "" {
		t.Errorf("conditions: got %s, want none", s)
	}
}

// TestScaleDatabaseFailures: every statement a scaled record's recording,
// reading, listing and determination runs fails into an error (T80e).
func TestScaleDatabaseFailures(t *testing.T) {
	e := setupPricing(t, "PR00", "K007")
	h := endpoints(t, time.Now())
	price, _ := e.Must(201, "POST", "/v1/sales-prices", e.record(t, boltPrice()), "")
	e.Must(201, "POST", "/v1/sales-prices", e.record(t, e.boltDiscount()), "")
	from := instant(t, "2026-01-01T00:00:00Z")
	calls := map[string]func(ctx context.Context) error{
		"record a price by quantity": func(ctx context.Context) error {
			_, err := h.CreateSalesPrice(ctx, &gen.SalesPriceCreate{LegalEntityID: e.DE, ItemKey: "BOLT-M10", Unit: gen.NewOptString("EA"),
				UnitPrice: gen.NewOptMoney(gen.Money{Amount: "0.80", Currency: "EUR"}), ValidFrom: from, ScaleBasis: gen.NewOptString("quantity"),
				Scale: []gen.SalesPriceScaleLevelCreate{{FromQuantity: gen.NewOptQuantity(gen.Quantity{Value: "10", Unit: "EA"}),
					UnitPrice: gen.NewOptMoney(gen.Money{Amount: "0.70", Currency: "EUR"})}}}, gen.CreateSalesPriceParams{})
			return err
		},
		"record a discount by value": func(ctx context.Context) error {
			_, err := h.CreateSalesPrice(ctx, &gen.SalesPriceCreate{LegalEntityID: e.DE, ItemKey: "BOLT-M10", ConditionType: gen.NewOptString("K007"),
				Percent: gen.NewOptString("1"), ValidFrom: from, ScaleBasis: gen.NewOptString("value"),
				Scale: []gen.SalesPriceScaleLevelCreate{{FromAmount: gen.NewOptMoney(gen.Money{Amount: "100.00", Currency: "EUR"}),
					Percent: gen.NewOptString("2")}}}, gen.CreateSalesPriceParams{})
			return err
		},
		"record a discount by quantity": func(ctx context.Context) error {
			_, err := h.CreateSalesPrice(ctx, &gen.SalesPriceCreate{LegalEntityID: e.DE, ItemKey: "BOLT-M10", ConditionType: gen.NewOptString("K007"),
				Percent: gen.NewOptString("1"), ValidFrom: from, ScaleBasis: gen.NewOptString("quantity"),
				Scale: []gen.SalesPriceScaleLevelCreate{{FromQuantity: gen.NewOptQuantity(gen.Quantity{Value: "10", Unit: "EA"}),
					Percent: gen.NewOptString("2")}}}, gen.CreateSalesPriceParams{})
			return err
		},
		"get a scaled price": func(ctx context.Context) error {
			_, err := h.GetSalesPrice(ctx, gen.GetSalesPriceParams{ID: uuid.MustParse(price["id"].(string))})
			return err
		},
		"list scaled prices": func(ctx context.Context) error {
			_, err := h.ListSalesPrices(ctx, gen.ListSalesPricesParams{ItemKey: gen.NewOptString("BOLT-M10")})
			return err
		},
		"a quotation priced from scales": func(ctx context.Context) error {
			_, err := h.CreateQuotation(ctx, &gen.QuotationCreate{LegalEntityID: e.DE, CustomerID: e.customer, ValidUntil: instant(t, far+"T00:00:00Z"),
				Currency: "EUR", Items: []gen.SalesItemCreate{{ItemKey: gen.NewOptString("BOLT-M10"), Description: gen.NewOptString("Bolt"),
					Quantity: gen.Quantity{Value: "6000", Unit: "EA"}, AccountID: e.revenue}}}, gen.CreateQuotationParams{})
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
