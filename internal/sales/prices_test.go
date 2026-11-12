package sales_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/sales"
)

// rec is a price record as a test names it; the zero value's fields take
// the defaults of priceBody. With a percent it is a discount of the
// condition type.
type rec struct {
	key, unit, amount, currency, from, to string
	condition, percent                    string
	priceUnit                             int
	customer, company                     uuid.UUID
}

// priceBody is the JSON of a price record: by default a general price of
// company 2000 for BRACKET-M8 per 1 EA in EUR, valid from 2020-01-01.
func (e env) priceBody(r rec) string {
	def := func(v *string, d string) {
		if *v == "" {
			*v = d
		}
	}
	def(&r.key, "BRACKET-M8")
	def(&r.unit, "EA")
	def(&r.amount, "10.00")
	def(&r.currency, "EUR")
	def(&r.from, "2020-01-01")
	if r.company == uuid.Nil {
		r.company = e.DE
	}
	extra := ""
	if r.customer != uuid.Nil {
		extra += fmt.Sprintf(`, "customer_id": %q`, r.customer)
	}
	if r.priceUnit != 0 {
		extra += fmt.Sprintf(`, "price_unit": %d`, r.priceUnit)
	}
	if r.to != "" {
		extra += fmt.Sprintf(`, "valid_to": %q`, r.to)
	}
	if r.condition != "" {
		extra += fmt.Sprintf(`, "condition_type": %q`, r.condition)
	}
	if r.percent != "" {
		return fmt.Sprintf(`{"legal_entity_id": %q, "item_key": %q, "percent": %q, "valid_from": %q%s}`, r.company, r.key, r.percent, r.from, extra)
	}
	return fmt.Sprintf(`{"legal_entity_id": %q, "item_key": %q, "unit": %q, "unit_price": {"amount": %q, "currency": %q}, "valid_from": %q%s}`,
		r.company, r.key, r.unit, r.amount, r.currency, r.from, extra)
}

// priced records a price and returns its id.
func (e env) priced(t *testing.T, r rec) string {
	t.Helper()
	got, _ := e.In(t).Must(201, "POST", "/v1/sales-prices", e.priceBody(r), "")
	return got["id"].(string)
}

// unpriced is the JSON of an item that gives no unit price: by default 1
// EA of BRACKET-M8 on the revenue account.
func (e env) unpriced(l line) string {
	if l.quantity == "" {
		l.quantity = "1"
	}
	if l.unit == "" {
		l.unit = "EA"
	}
	key := `, "item_key": "BRACKET-M8"`
	switch l.key {
	case "":
	case "-":
		key = ""
	default:
		key = fmt.Sprintf(`, "item_key": %q`, l.key)
	}
	return fmt.Sprintf(`{"description": "Machined bracket", "quantity": {"value": %q, "unit": %q}, "account_id": %q%s}`, l.quantity, l.unit, e.revenue, key)
}

// quotedItem creates a quotation of the one item to the customer and
// returns the item as the quotation shows it.
func (e env) quotedItem(t *testing.T, customer uuid.UUID, item string) map[string]any {
	t.Helper()
	got, _ := e.In(t).Must(201, "POST", "/v1/quotations", e.body(customer, far, item), "")
	return items(t, got)[0]
}

// pricing is an item's unit price, price unit, source and net value.
func pricing(t *testing.T, it map[string]any) string {
	t.Helper()
	return fmt.Sprintf("%s per %v, %v, net %s", amount(t, it, "unit_price"), it["price_unit"], it["price_source"], amount(t, it, "net_amount"))
}

// TestPriceRecordedAndReadBack: a general price and a customer's price
// are recorded with their validity, read back the same with the names of
// what they reference, audited and published without the customer's name
// (T80a).
func TestPriceRecordedAndReadBack(t *testing.T) {
	e := setup(t)
	general, tag := e.Must(201, "POST", "/v1/sales-prices", e.priceBody(rec{amount: "12.50"}), "")
	if tag != `"1"` {
		t.Errorf("ETag: got %s, want \"1\"", tag)
	}
	fields(t, general, map[string]any{"version": float64(1), "legal_entity_id": e.DE.String(), "company_code": "2000", "company_name": "Acme GmbH",
		"item_key": "BRACKET-M8", "customer_id": nil, "customer_number": nil, "customer_name": nil, "kind": "price", "condition_type": nil,
		"unit": "EA", "price_unit": float64(1), "percent": nil, "valid_from": "2020-01-01", "valid_to": nil})
	if a := amount(t, general, "unit_price"); a != "12.50 EUR" {
		t.Errorf("unit price: got %s, want 12.50 EUR", a)
	}
	own, _ := e.Must(201, "POST", "/v1/sales-prices",
		e.priceBody(rec{amount: "1175.00", priceUnit: 100, customer: e.customer, from: "2026-01-01", to: "2026-12-31"}), "")
	fields(t, own, map[string]any{"customer_id": e.customer.String(), "customer_number": "BP70000001", "customer_name": "Northwind Traders",
		"price_unit": float64(100), "valid_from": "2026-01-01", "valid_to": "2026-12-31"})
	if a := amount(t, own, "unit_price"); a != "1175.00 EUR" {
		t.Errorf("the customer's unit price: got %s, want 1175.00 EUR", a)
	}
	for name, made := range map[string]map[string]any{"general": general, "the customer's": own} {
		t.Run(name, func(t *testing.T) {
			id, _ := made["id"].(string)
			read, readTag := e.In(t).Must(200, "GET", "/v1/sales-prices/"+id, "", "")
			if a, b := mustJSON(t, read), mustJSON(t, made); a != b {
				t.Errorf("read back: got %s, want %s", a, b)
			}
			if readTag != `"1"` {
				t.Errorf("ETag read back: got %s, want \"1\"", readTag)
			}
			if n := e.audits(t, "sales.price.create", id); n != 1 {
				t.Errorf("audit records of the record: got %d, want 1", n)
			}
		})
	}
	wantGeneral := map[string]any{"price_id": general["id"], "legal_entity_id": e.DE.String(), "legal_entity_code": "2000", "item_key": "BRACKET-M8",
		"customer_id": nil, "customer_number": nil, "kind": "price", "condition_type": nil, "unit": "EA",
		"unit_price": map[string]any{"amount": "12.50", "currency": "EUR"}, "price_unit": float64(1), "percent": nil, "valid_from": "2020-01-01",
		"valid_to": nil}
	if a, b := mustJSON(t, e.events(t, "sales_price_changed", "price_id", general["id"].(string))), mustJSON(t, []map[string]any{wantGeneral}); a != b {
		t.Errorf("SalesPriceChanged of the general price: got %s, want %s", a, b)
	}
	wantOwn := map[string]any{"price_id": own["id"], "legal_entity_id": e.DE.String(), "legal_entity_code": "2000", "item_key": "BRACKET-M8",
		"customer_id": e.customer.String(), "customer_number": "BP70000001", "kind": "price", "condition_type": nil, "unit": "EA",
		"unit_price": map[string]any{"amount": "1175.00", "currency": "EUR"}, "price_unit": float64(100), "percent": nil,
		"valid_from": "2026-01-01", "valid_to": "2026-12-31"}
	if a, b := mustJSON(t, e.events(t, "sales_price_changed", "price_id", own["id"].(string))), mustJSON(t, []map[string]any{wantOwn}); a != b {
		t.Errorf("SalesPriceChanged of the customer's price: got %s, want %s", a, b)
	}
	apitest.NeverPublished(t, e.Tenant, "sales", "Northwind Traders")
}

// TestPriceIdempotent: the same Idempotency-Key records one price and
// answers it again; records have no uniqueness rule, so the key is what
// keeps a retry from recording the price twice.
func TestPriceIdempotent(t *testing.T) {
	e := setup(t)
	req := apitest.Req{Method: "POST", Path: "/v1/sales-prices", Body: e.priceBody(rec{}), Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey()}
	first, second := e.API.Do(req), e.API.Do(req)
	for i, rec := range []*httptest.ResponseRecorder{first, second} {
		if rec.Code != 201 {
			t.Fatalf("status of call %d: got %d, want 201: %s", i+1, rec.Code, rec.Body)
		}
	}
	a, b := apitest.JSON(t, first), apitest.JSON(t, second)
	fields(t, b, map[string]any{"id": a["id"]})
	if h := second.Header().Get("Idempotent-Replayed"); h != "true" {
		t.Errorf("Idempotent-Replayed: got %q, want true", h)
	}
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM sales.price WHERE tenant_id = $1`, []any{e.Tenant.ID}, &n)
	if n != 1 {
		t.Errorf("price records: got %d, want 1", n)
	}
}

// TestPriceRefusals: each refusal names the request's own field and
// writes nothing (T80a).
func TestPriceRefusals(t *testing.T) {
	e := setup(t)
	supplier := e.partner(t, "BP70000002", "Only a supplier", "active", "supplier")
	cases := []struct {
		name, body string
		status     int
		code       string
		field      any
	}{
		{"an unknown company", e.priceBody(rec{company: uuid.New()}), 422, "request.reference_not_found", "legal_entity_id"},
		{"an item key of white space", e.priceBody(rec{key: "  "}), 422, "request.validation_failed", "item_key"},
		{"an unknown customer", e.priceBody(rec{customer: uuid.New()}), 422, "request.reference_not_found", "customer_id"},
		{"a partner that is no customer", e.priceBody(rec{customer: supplier}), 422, "sales.customer_not_usable", "customer_id"},
		{"an unknown unit", e.priceBody(rec{unit: "XX"}), 422, "sales.unknown_unit", "unit"},
		{"an unknown currency", e.priceBody(rec{currency: "XXA"}), 422, "sales.unknown_currency", "unit_price.currency"},
		{"a price with too many decimals", e.priceBody(rec{amount: "1.234"}), 400, "request.invalid_amount", "unit_price.amount"},
		{"a price of zero", e.priceBody(rec{amount: "0.00"}), 422, "sales.amount_not_positive", "unit_price.amount"},
		{"a negative price", e.priceBody(rec{amount: "-1.00"}), 422, "sales.amount_not_positive", "unit_price.amount"},
		{"valid until the day before it starts", e.priceBody(rec{from: "2026-03-10", to: "2026-03-09"}), 422, "sales.invalid_validity", "valid_to"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := e.In(t).Problem(c.status, c.code, "POST", "/v1/sales-prices", c.body, "")
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
	t.Run("valid on its first day alone", func(t *testing.T) {
		e.In(t).Must(201, "POST", "/v1/sales-prices", e.priceBody(rec{from: "2026-03-10", to: "2026-03-10"}), "")
	})
	t.Run("unknown", func(t *testing.T) {
		e.In(t).Problem(404, "request.not_found", "GET", "/v1/sales-prices/"+uuid.NewString(), "", "")
	})
}

// TestPriceDetermination: an item that names an item key without a price
// takes the price valid on the document's date in its unit and the
// document's currency, the customer's before the general one and among
// several the latest valid_from; a price sent with the item stays and
// reads as manual; an item with neither is refused naming the item
// (T80a acceptance).
func TestPriceDetermination(t *testing.T) {
	e := setup(t)
	other := e.partner(t, "BP70000003", "Contoso", "active", "customer")
	e.priced(t, rec{amount: "10.01"})
	e.priced(t, rec{amount: "9.00", from: "2021-01-01", to: "2021-12-31"}) // over
	e.priced(t, rec{amount: "8.00", from: "2098-01-01"})                   // not yet
	e.priced(t, rec{amount: "7.00", unit: "KG", from: "2025-01-01"})       // another unit's
	e.priced(t, rec{amount: "6.00", currency: "USD", from: "2025-01-01"})  // another currency's
	e.priced(t, rec{amount: "5.00", key: "bracket-m8", from: "2025-01-01"})
	e.priced(t, rec{amount: "4.00", company: e.US, currency: "EUR", from: "2025-01-01"})
	e.priced(t, rec{amount: "99.99", priceUnit: 100, customer: e.customer, from: "2019-01-01"})
	cases := []struct {
		name     string
		customer uuid.UUID
		item     string
		want     string
	}{
		// 2.5 × 10.01 is 25.025: 25.03, half away from zero.
		{"another customer takes the general price", other, e.unpriced(line{quantity: "2.5"}), "10.01 EUR per 1, price_list, net 25.03 EUR"},
		// 3 × 99.99 per 100 is 2.9997: 3.00.
		{"the customer's price wins, though it is the older", e.customer, e.unpriced(line{quantity: "3"}), "99.99 EUR per 100, price_list, net 3.00 EUR"},
		{"the price is the unit's", other, e.unpriced(line{quantity: "2", unit: "KG"}), "7.00 EUR per 1, price_list, net 14.00 EUR"},
		{"the key is compared exactly", other, e.unpriced(line{key: "bracket-m8"}), "5.00 EUR per 1, price_list, net 5.00 EUR"},
		{"a price sent with the item stays", e.customer, e.item(line{price: "20.00", key: "BRACKET-M8", priceUnit: 10, quantity: "5"}),
			"20.00 EUR per 10, manual, net 10.00 EUR"},
		{"a price sent without a key", e.customer, e.item(line{price: "20.00"}), "20.00 EUR per 1, manual, net 20.00 EUR"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pricing(t, e.quotedItem(t, c.customer, c.item)); got != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
	t.Run("among several the latest valid_from", func(t *testing.T) {
		e.priced(t, rec{amount: "11.00", from: "2025-06-01"})
		e.priced(t, rec{amount: "12.00", from: "2024-06-01"})
		if got, want := pricing(t, e.quotedItem(t, other, e.unpriced(line{}))), "11.00 EUR per 1, price_list, net 11.00 EUR"; got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	})
	t.Run("of two with the same valid_from the one recorded last", func(t *testing.T) {
		e.priced(t, rec{amount: "13.00", from: "2025-06-01"})
		if got, want := pricing(t, e.quotedItem(t, other, e.unpriced(line{}))), "13.00 EUR per 1, price_list, net 13.00 EUR"; got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	})
	refusals := []struct {
		name, item, field string
		code              string
	}{
		{"neither a price nor a key", e.unpriced(line{key: "-"}), "items[0].unit_price", "sales.no_price"},
		{"a key without a price record", e.unpriced(line{key: "WASHER"}), "items[0].unit_price", "sales.no_price"},
		{"a record in another unit alone", e.unpriced(line{unit: "H"}), "items[0].unit_price", "sales.no_price"},
		// 0.0001 × 13.00 is 0.0013: nothing in minor units.
		{"a value that rounds to zero", e.unpriced(line{quantity: "0.0001"}), "items[0].quantity.value", "sales.amount_not_positive"},
		{"a value out of range", e.unpriced(line{quantity: "99999999999999", key: "HUGE"}), "items", "sales.amount_too_large"},
	}
	e.priced(t, rec{amount: "10000000000000000.00", key: "HUGE"})
	for _, c := range refusals {
		t.Run(c.name, func(t *testing.T) {
			got := e.In(t).Problem(422, c.code, "POST", "/v1/quotations", e.body(other, far, c.item), "")
			if f := field(got); f != c.field {
				t.Errorf("field: got %v, want %s", f, c.field)
			}
		})
	}
	t.Run("another currency's document", func(t *testing.T) {
		body := strings.Replace(e.body(other, far, e.unpriced(line{})), `"currency": "EUR"`, `"currency": "USD"`, 1)
		got, _ := e.In(t).Must(201, "POST", "/v1/quotations", body, "")
		if p, want := pricing(t, items(t, got)[0]), "6.00 USD per 1, price_list, net 6.00 USD"; p != want {
			t.Errorf("got %s, want %s", p, want)
		}
	})
}

// TestPriceIsFixedOnTheItem: the price is fixed on the item when it is
// created, on a quotation and on an order, and a later price record does
// not change it; an order from the quotation copies the item with its
// price and its source (T80a acceptance).
func TestPriceIsFixedOnTheItem(t *testing.T) {
	e := setup(t)
	e.priced(t, rec{amount: "10.00"})
	quotation := e.accepted(t, e.customer, e.unpriced(line{quantity: "4"}), e.item(line{price: "3.00"}))
	direct, _ := e.Must(201, "POST", "/v1/sales-orders", e.direct(e.customer, "", e.unpriced(line{quantity: "4"})), "")
	e.priced(t, rec{amount: "11.00", from: "2025-01-01"})
	e.priced(t, rec{amount: "9.50", customer: e.customer, from: "2025-01-01"})
	read, _ := e.Must(200, "GET", "/v1/quotations/"+quotation, "", "")
	if got, want := pricing(t, items(t, read)[0]), "10.00 EUR per 1, price_list, net 40.00 EUR"; got != want {
		t.Errorf("the quotation's item after later records: got %s, want %s", got, want)
	}
	if a := amount(t, read, "net_amount"); a != "43.00 EUR" {
		t.Errorf("the quotation's net value: got %s, want 43.00 EUR", a)
	}
	order, _ := e.Must(201, "POST", "/v1/sales-orders", fromQuotation(quotation), "")
	its := items(t, order)
	if got, want := pricing(t, its[0]), "10.00 EUR per 1, price_list, net 40.00 EUR"; got != want {
		t.Errorf("the order's item from the quotation: got %s, want %s", got, want)
	}
	if got, want := pricing(t, its[1]), "3.00 EUR per 1, manual, net 3.00 EUR"; got != want {
		t.Errorf("the order's entered item from the quotation: got %s, want %s", got, want)
	}
	again, _ := e.Must(200, "GET", "/v1/sales-orders/"+direct["id"].(string), "", "")
	if got, want := pricing(t, items(t, again)[0]), "10.00 EUR per 1, price_list, net 40.00 EUR"; got != want {
		t.Errorf("the direct order's item after later records: got %s, want %s", got, want)
	}
	fresh, _ := e.Must(201, "POST", "/v1/sales-orders", e.direct(e.customer, "", e.unpriced(line{quantity: "4"})), "")
	if got, want := pricing(t, items(t, fresh)[0]), "9.50 EUR per 1, price_list, net 38.00 EUR"; got != want {
		t.Errorf("a new order's item: got %s, want %s, the customer's new price", got, want)
	}
}

// TestPriceValidOnTheCompanysDate: the price is the one valid on the
// document's date, the company's in its time zone, not UTC's: a record
// whose last day is that date still counts, one starting on UTC's date
// does not yet (T80a acceptance).
func TestPriceValidOnTheCompanysDate(t *testing.T) {
	e := setup(t)
	e.priced(t, rec{amount: "10.00", from: "2026-01-01", to: "2026-03-09"})
	e.priced(t, rec{amount: "20.00", from: "2026-03-10"})
	key := "BRACKET-M8"
	in := sales.QuotationInput{LegalEntityID: e.DE, CustomerID: e.customer, Currency: "EUR",
		Items: []sales.ItemInput{{ItemKey: &key, Description: "Bracket", Quantity: "1", Unit: "EA", PriceUnit: 1, AccountID: e.revenue}}}
	for _, c := range []struct {
		name, now, until string
		want             int64
	}{
		// 03:00 UTC on 10 March is still 9 March in Toronto.
		{"the last day of the first record", "2026-03-10T03:00:00Z", "2026-03-09T00:00:00Z", 1000},
		{"the first day of the second", "2026-03-10T05:00:00Z", "2026-03-10T00:00:00Z", 2000},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := endpoints(t, instant(t, c.now))
			in := in
			in.ValidUntil = instant(t, c.until)
			err := e.inRequest(t, func(ctx context.Context) error {
				q, err := h.MakeQuotation(ctx, in)
				if err == nil && int64(q.Items[0].UnitPrice) != c.want {
					t.Errorf("unit price: got %d, want %d minor units", q.Items[0].UnitPrice, c.want)
				}
				return err
			})
			if err != nil {
				t.Errorf("got %v, want the quotation made", err)
			}
		})
	}
	t.Run("before any record", func(t *testing.T) {
		h := endpoints(t, instant(t, "2025-12-31T12:00:00Z"))
		in := in
		in.ValidUntil = instant(t, far+"T00:00:00Z")
		err := e.inRequest(t, func(ctx context.Context) error {
			_, err := h.MakeQuotation(ctx, in)
			return err
		})
		refusal(t, err, 422, "sales.no_price")
	})
}

// TestPriceList: price records are listed for the companies the caller
// may act in, filtered by company, item key, customer and a day of
// validity, in a stable order and page by page; another company's record
// is not found (T80a).
func TestPriceList(t *testing.T) {
	e := setup(t)
	a := e.priced(t, rec{key: "A", from: "2026-03-01"})
	b := e.priced(t, rec{key: "B", from: "2026-01-01", to: "2026-01-31", customer: e.customer})
	c := e.priced(t, rec{key: "C", from: "2026-02-01"})
	us := e.priced(t, rec{key: "A", company: e.US, currency: "USD", from: "2026-04-01"})
	ids := func(t *testing.T, client apitest.Client, query string) string {
		t.Helper()
		got, _ := client.In(t).Must(200, "GET", "/v1/sales-prices?"+query, "", "")
		var out []string
		for _, it := range items(t, got) {
			out = append(out, it["id"].(string))
		}
		return strings.Join(out, " ")
	}
	for _, tc := range []struct{ name, query, want string }{
		{"by item key, then id", "", strings.Join([]string{a, us, b, c}, " ")},
		{"by valid_from", "order_by=valid_from", strings.Join([]string{b, c, a, us}, " ")},
		{"the latest first", "order_by=valid_from%20desc", strings.Join([]string{us, a, c, b}, " ")},
		{"a company's", "legal_entity_id=" + e.US.String(), us},
		{"an item key's", "item_key=A", a + " " + us},
		{"a customer's", "customer_id=" + e.customer.String(), b},
		{"valid on a day", "valid_on=2026-01-31", b},
		{"valid on a day after a last day", "valid_on=2026-02-01", c},
		{"valid on a later day", "valid_on=2026-03-01", a + " " + c},
		{"none", "item_key=Z", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ids(t, e.Client, tc.query); got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
	t.Run("page by page", func(t *testing.T) {
		first, _ := e.In(t).Must(200, "GET", "/v1/sales-prices?page_size=3", "", "")
		token, _ := first["next_page_token"].(string)
		if token == "" {
			t.Fatal("next page token: got none, want one after three of four records")
		}
		if got := ids(t, e.Client, "page_size=3&page_token="+token); got != c {
			t.Errorf("the second page: got %s, want %s", got, c)
		}
		e.In(t).Problem(400, "request.invalid_page_token", "GET", "/v1/sales-prices?item_key=A&page_token="+token, "", "")
	})
	for _, tc := range []struct {
		name, order   string
		first, second string
	}{
		{"page by page by valid_from", "valid_from", b + " " + c, a + " " + us},
		{"page by page, the latest first", "valid_from%20desc", us + " " + a, c + " " + b},
	} {
		t.Run(tc.name, func(t *testing.T) {
			query := "order_by=" + tc.order + "&page_size=2"
			first, _ := e.In(t).Must(200, "GET", "/v1/sales-prices?"+query, "", "")
			if got := ids(t, e.Client, query); got != tc.first {
				t.Errorf("the first page: got %s, want %s", got, tc.first)
			}
			token, _ := first["next_page_token"].(string)
			if token == "" {
				t.Fatal("next page token: got none, want one after two of four records")
			}
			if got := ids(t, e.Client, query+"&page_token="+token); got != tc.second {
				t.Errorf("the second page: got %s, want %s", got, tc.second)
			}
		})
	}
	t.Run("another company's", func(t *testing.T) {
		reader := e.As(e.Reader(t, "US pricing", e.US))
		if got := ids(t, reader, ""); got != us {
			t.Errorf("a reader of the US company alone: got %s, want %s", got, us)
		}
		reader.In(t).Problem(404, "request.not_found", "GET", "/v1/sales-prices/"+a, "", "")
		reader.In(t).Problem(422, "request.reference_not_found", "POST", "/v1/sales-prices", e.priceBody(rec{}), "")
	})
	t.Run("without the permission", func(t *testing.T) {
		nobody := e.As(e.Reader(t, "No pricing", e.DE))
		pgtest.Exec(t, `DELETE FROM identity.principal_role WHERE tenant_id = $1 AND principal_id = $2`, e.Tenant.ID, nobody.As.ID)
		nobody.In(t).Problem(403, "identity.permission_missing", "GET", "/v1/sales-prices", "", "")
		nobody.In(t).Problem(403, "identity.permission_missing", "POST", "/v1/sales-prices", e.priceBody(rec{}), "")
	})
}

// TestPriceDatabaseFailures calls every price handler, and a quotation
// whose item takes its price from a record, outside a unit of work and
// with each of its database statements failing in turn.
func TestPriceDatabaseFailures(t *testing.T) {
	e := setup(t)
	h := endpoints(t, time.Now())
	id := uuid.MustParse(e.priced(t, rec{customer: e.customer}))
	calls := map[string]func(ctx context.Context) error{
		"record": func(ctx context.Context) error {
			_, err := h.CreateSalesPrice(ctx, &gen.SalesPriceCreate{LegalEntityID: e.DE, ItemKey: "BRACKET-M8", CustomerID: gen.NewOptUUID(e.customer),
				Unit: gen.NewOptString("EA"), UnitPrice: gen.NewOptMoney(gen.Money{Amount: "10.00", Currency: "EUR"}), ValidFrom: instant(t, "2026-01-01T00:00:00Z"),
				ValidTo: gen.NewOptDate(instant(t, "2026-12-31T00:00:00Z"))}, gen.CreateSalesPriceParams{})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetSalesPrice(ctx, gen.GetSalesPriceParams{ID: id})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListSalesPrices(ctx, gen.ListSalesPricesParams{ValidOn: gen.NewOptDate(instant(t, "2026-06-01T00:00:00Z"))})
			return err
		},
		"a quotation priced from a record": func(ctx context.Context) error {
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
	t.Run("list page size", func(t *testing.T) {
		err := e.inRequest(t, func(ctx context.Context) error {
			_, err := h.ListSalesPrices(ctx, gen.ListSalesPricesParams{PageSize: gen.NewOptInt32(0)})
			return err
		})
		if p := problem.From(err); p.Code != "request.invalid_page_size" {
			t.Errorf("got %v, want request.invalid_page_size", p)
		}
	})
}
