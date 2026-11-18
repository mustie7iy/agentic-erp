package sales_test

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// setupGroups is setupPricing with the price groups 01 Wholesale and 02
// Retail (ADR-0067 §13).
func setupGroups(t *testing.T, steps ...string) env {
	t.Helper()
	return setupWith(t, func(doc *config.Config) {
		outputCodes(doc)
		doc.Pricing.ConditionTypes = conditionTypes
		for _, s := range steps {
			doc.Pricing.Procedure = append(doc.Pricing.Procedure, config.ProcedureStep{ConditionType: s})
		}
		doc.Pricing.PriceGroups = []config.PriceGroup{{Code: "01", Name: "Wholesale"}, {Code: "02", Name: "Retail"}}
	})
}

// salesData is the JSON of a customer's sales data in company 2000 with
// the price group, none when it is empty.
func (e env) salesData(customer uuid.UUID, group string) string {
	if group == "" {
		return fmt.Sprintf(`{"legal_entity_id": %q, "customer_id": %q}`, e.DE, customer)
	}
	return fmt.Sprintf(`{"legal_entity_id": %q, "customer_id": %q, "price_group": %q}`, e.DE, customer, group)
}

// inGroup sets the customer up in company 2000 in the price group and
// returns its sales data's id.
func (e env) inGroup(t *testing.T, customer uuid.UUID, group string) string {
	t.Helper()
	got, _ := e.Must(201, "POST", "/v1/customer-sales-data", e.salesData(customer, group), "")
	return got["id"].(string)
}

// TestSalesDataSetUpAndChanged: a customer's sales data is set up with
// its price group, read back and listed with the names of what it names,
// changed with its version to another group and to none, each audited and
// published without the customer's name (T80f).
func TestSalesDataSetUpAndChanged(t *testing.T) {
	e := setupGroups(t)
	made, tag := e.Must(201, "POST", "/v1/customer-sales-data", e.salesData(e.customer, "01"), "")
	fields(t, made, map[string]any{"company_code": "2000", "customer_number": "BP70000001", "customer_name": "Northwind Traders",
		"price_group": "01", "price_group_name": "Wholesale", "version": float64(1)})
	if tag != `"1"` {
		t.Errorf("ETag: got %s, want \"1\"", tag)
	}
	id := made["id"].(string)
	read, _ := e.Must(200, "GET", "/v1/customer-sales-data/"+id, "", "")
	if a, b := mustJSON(t, read), mustJSON(t, made); a != b {
		t.Errorf("read back: got %s, want %s", a, b)
	}
	other := e.partner(t, "BP70000002", "Contoso", "active", "customer")
	e.inGroup(t, other, "")
	for query, want := range map[string]string{"": "BP70000001 BP70000002", "price_group=01": "BP70000001", "customer_id=" + other.String(): "BP70000002",
		"legal_entity_id=" + e.DE.String(): "BP70000001 BP70000002", "price_group=02": ""} {
		t.Run("list "+query, func(t *testing.T) {
			got, _ := e.In(t).Must(200, "GET", "/v1/customer-sales-data?"+query, "", "")
			var numbers []string
			for _, it := range items(t, got) {
				numbers = append(numbers, it["customer_number"].(string))
			}
			if s := strings.Join(numbers, " "); s != want {
				t.Errorf("got %s, want %s", s, want)
			}
		})
	}
	path := "/v1/customer-sales-data/" + id
	changed, tag := e.Must(200, "PATCH", path, `{"price_group": "02"}`, tag)
	fields(t, changed, map[string]any{"price_group": "02", "price_group_name": "Retail", "version": float64(2)})
	none, _ := e.Must(200, "PATCH", path, `{"price_group": null}`, tag)
	if _, has := none["price_group"]; has || none["version"] != float64(3) {
		t.Errorf("without a group: got %v, want no price_group at version 3", none)
	}
	if _, has := none["price_group_name"]; has {
		t.Errorf("without a group: got a price_group_name %v, want none", none["price_group_name"])
	}
	if n := e.audits(t, "sales.customer_sales_data.create", id); n != 1 {
		t.Errorf("audit records of the set-up: got %d, want 1", n)
	}
	if n := e.audits(t, "sales.customer_sales_data.change", id); n != 2 {
		t.Errorf("audit records of the changes: got %d, want 2", n)
	}
	var groups []string
	for _, ev := range e.events(t, "customer_sales_data_changed", "customer_sales_data_id", id) {
		groups = append(groups, fmt.Sprintf("%v<%v v%v %v", ev["price_group"], ev["price_group_before"], ev["version"], ev["customer_number"]))
		if _, named := ev["customer_name"]; named {
			t.Errorf("event names the customer: %v", ev)
		}
	}
	if got, want := strings.Join(groups, ", "), "01<<nil> v1 BP70000001, 02<01 v2 BP70000001, <nil><02 v3 BP70000001"; got != want {
		t.Errorf("CustomerSalesDataChanged: got %s, want %s", got, want)
	}
}

// TestSalesDataRefusals: an unknown customer, one that cannot be sold
// to, an unknown price group and a second sales data of a customer in a
// company are refused naming the field, as are a change to an unknown
// group or to the group it has, a stale version and one without If-Match;
// an unknown id is not found (T80f acceptance).
func TestSalesDataRefusals(t *testing.T) {
	e := setupGroups(t)
	supplier := e.partner(t, "BP70000002", "Only a supplier", "active", "supplier")
	id := e.inGroup(t, e.customer, "01")
	path := "/v1/customer-sales-data/" + id
	cases := []struct {
		name, method, path, body, tag string
		status                        int
		code, field                   string
	}{
		{"an unknown customer", "POST", "/v1/customer-sales-data", e.salesData(uuid.New(), "01"), "", 422, "request.reference_not_found", "customer_id"},
		{"a partner that is no customer", "POST", "/v1/customer-sales-data", e.salesData(supplier, ""), "", 422, "sales.customer_not_usable", "customer_id"},
		{"an unknown company", "POST", "/v1/customer-sales-data", strings.Replace(e.salesData(e.customer, ""), e.DE.String(), uuid.NewString(), 1), "",
			422, "request.reference_not_found", "legal_entity_id"},
		{"an unknown price group", "POST", "/v1/customer-sales-data", e.salesData(e.partner(t, "BP70000003", "Fabrikam", "active", "customer"), "03"), "",
			422, "sales.unknown_price_group", "price_group"},
		{"a second sales data", "POST", "/v1/customer-sales-data", e.salesData(e.customer, "02"), "", 409, "sales.customer_sales_data_exists", ""},
		{"a change to an unknown group", "PATCH", path, `{"price_group": "03"}`, `"1"`, 422, "sales.unknown_price_group", "price_group"},
		{"a change to the group it has", "PATCH", path, `{"price_group": "01"}`, `"1"`, 422, "sales.nothing_to_change", ""},
		{"a stale version", "PATCH", path, `{"price_group": "02"}`, `"7"`, 412, "request.version_mismatch", ""},
		{"no If-Match", "PATCH", path, `{"price_group": "02"}`, "", 428, "request.precondition_required", ""},
		{"an unknown id", "GET", "/v1/customer-sales-data/" + uuid.NewString(), "", "", 404, "request.not_found", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := e.In(t).Problem(c.status, c.code, c.method, c.path, c.body, c.tag)
			if f := field(got); c.field != "" && f != c.field {
				t.Errorf("field: got %v, want %v", f, c.field)
			}
		})
	}
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM sales.customer_sales_data WHERE tenant_id = $1`, []any{e.Tenant.ID}, &n)
	if n != 1 {
		t.Errorf("sales data after the refusals: got %d, want the first alone", n)
	}
}

// TestSalesDataChangesRacing: two changes with the same version wait for
// one another on the row lock; one is made and the other finds the
// version gone (ADR-0008; T80f acceptance).
func TestSalesDataChangesRacing(t *testing.T) {
	e := setupGroups(t)
	path := "/v1/customer-sales-data/" + e.inGroup(t, e.customer, "01")
	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i, body := range []string{`{"price_group": "02"}`, `{"price_group": null}`} {
		wg.Go(func() {
			codes[i] = e.API.Do(apitest.Req{Method: "PATCH", Path: path, Body: body,
				Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey(), IfMatch: `"1"`,
				Header: http.Header{"Content-Type": {"application/merge-patch+json"}}}).Code
		})
	}
	wg.Wait()
	slices.Sort(codes)
	if !slices.Equal(codes, []int{200, 412}) {
		t.Errorf("statuses: got %v, want one 200 and one 412", codes)
	}
	got, _ := e.Must(200, "GET", path, "", "")
	if got["version"] != float64(2) {
		t.Errorf("version: got %v, want 2, one change made", got["version"])
	}
}

// accessed are an item's conditions, each as "type rate access[ group]".
func accessed(t *testing.T, it map[string]any) string {
	t.Helper()
	raw, _ := it["conditions"].([]any)
	out := make([]string, len(raw))
	for i, r := range raw {
		c, _ := r.(map[string]any)
		rate := fmt.Sprintf("%v%%", c["percent"])
		if c["kind"] == "price" {
			rate = amount(t, c, "unit_price")
		}
		out[i] = fmt.Sprintf("%v %s %v", c["condition_type"], rate, c["access"])
		if g, ok := c["price_group"]; ok {
			out[i] += fmt.Sprintf(" %v %v", g, c["price_group_name"])
		}
	}
	return strings.Join(out, "; ")
}

// TestPriceGroupRecords: a price and a discount are recorded for a price
// group, read back and listed by it, and published with it; a record for
// a customer and a group, and one for an unknown group, are refused
// naming the group (T80f acceptance).
func TestPriceGroupRecords(t *testing.T) {
	e := setupGroups(t, "PR00", "K007")
	price, _ := e.Must(201, "POST", "/v1/sales-prices", e.record(t, map[string]any{"price_group": "01", "unit": "EA", "unit_price": eur("9.00")}), "")
	discount, _ := e.Must(201, "POST", "/v1/sales-prices", e.record(t, map[string]any{"price_group": "01", "condition_type": "K007", "percent": "5"}), "")
	e.priced(t, rec{})
	for name, made := range map[string]map[string]any{"price": price, "discount": discount} {
		t.Run(name, func(t *testing.T) {
			if made["price_group"] != "01" || made["price_group_name"] != "Wholesale" {
				t.Errorf("price group: got %v %v, want 01 Wholesale", made["price_group"], made["price_group_name"])
			}
			if _, has := made["customer_id"]; has {
				t.Errorf("customer_id: got %v, want none", made["customer_id"])
			}
			ev := e.events(t, "sales_price_changed", "price_id", made["id"].(string))
			if len(ev) != 1 || ev[0]["price_group"] != "01" {
				t.Errorf("SalesPriceChanged: got %v, want one with price_group 01", ev)
			}
		})
	}
	listed, _ := e.Must(200, "GET", "/v1/sales-prices?price_group=01&order_by=valid_from", "", "")
	if n := len(items(t, listed)); n != 2 {
		t.Errorf("records of group 01: got %d, want 2", n)
	}
	for name, c := range map[string]struct {
		fields map[string]any
		code   string
	}{
		"a customer and a group": {map[string]any{"price_group": "01", "customer_id": e.customer, "unit": "EA", "unit_price": eur("9.00")}, "sales.customer_or_group"},
		"an unknown group":       {map[string]any{"price_group": "03", "unit": "EA", "unit_price": eur("9.00")}, "sales.unknown_price_group"},
	} {
		t.Run(name, func(t *testing.T) {
			got := e.In(t).Problem(422, c.code, "POST", "/v1/sales-prices", e.record(t, c.fields), "")
			if f := field(got); f != "price_group" {
				t.Errorf("field: got %v, want price_group", f)
			}
		})
	}
}

// TestPriceGroupDetermination: an item takes the customer's own record,
// then its price group's, then the general one, prices and discounts
// alike, and says which step of the access found each; a customer without
// sales data or without a group takes the general records; a change of
// group holds for items priced after it, and the items priced before keep
// their prices (T80f acceptance).
func TestPriceGroupDetermination(t *testing.T) {
	e := setupGroups(t, "PR00", "K007")
	wholesale := e.partner(t, "BP70000002", "Contoso", "active", "customer")
	own := e.partner(t, "BP70000003", "Fabrikam", "active", "customer")
	ungrouped := e.partner(t, "BP70000004", "Litware", "active", "customer")
	stranger := e.partner(t, "BP70000005", "Tailspin", "active", "customer")
	id := e.inGroup(t, wholesale, "01")
	e.inGroup(t, own, "01")
	e.inGroup(t, ungrouped, "")
	e.priced(t, rec{amount: "10.00"})
	e.priced(t, rec{amount: "9.50", from: "2021-01-01"}) // a later general price: the group's still wins
	e.Must(201, "POST", "/v1/sales-prices", e.record(t, map[string]any{"price_group": "01", "unit": "EA", "unit_price": eur("9.00")}), "")
	e.Must(201, "POST", "/v1/sales-prices", e.record(t, map[string]any{"price_group": "02", "unit": "EA", "unit_price": eur("9.80")}), "")
	e.priced(t, rec{amount: "8.00", customer: own})
	e.priced(t, rec{condition: "K007", percent: "2"})
	e.Must(201, "POST", "/v1/sales-prices", e.record(t, map[string]any{"price_group": "01", "condition_type": "K007", "percent": "5"}), "")
	cases := []struct {
		name     string
		customer uuid.UUID
		want     string
	}{
		{"the group's price and discount", wholesale, "PR00 9.00 EUR price_group 01 Wholesale; K007 5% price_group 01 Wholesale"},
		{"the customer's own price before its group's", own, "PR00 8.00 EUR customer; K007 5% price_group 01 Wholesale"},
		{"sales data without a group", ungrouped, "PR00 9.50 EUR general; K007 2% general"},
		{"no sales data", stranger, "PR00 9.50 EUR general; K007 2% general"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := accessed(t, e.quotedItem(t, c.customer, e.unpriced(line{}))); got != c.want {
				t.Errorf("conditions: got %s, want %s", got, c.want)
			}
		})
	}
	before, _ := e.Must(201, "POST", "/v1/quotations", e.body(wholesale, far, e.unpriced(line{})), "")
	e.Must(200, "PATCH", "/v1/customer-sales-data/"+id, `{"price_group": "02"}`, `"1"`)
	t.Run("a new item after the group changed", func(t *testing.T) {
		if got, want := accessed(t, e.quotedItem(t, wholesale, e.unpriced(line{}))), "PR00 9.80 EUR price_group 02 Retail; K007 2% general"; got != want {
			t.Errorf("conditions: got %s, want %s", got, want)
		}
	})
	t.Run("an item priced before the change", func(t *testing.T) {
		read, _ := e.Must(200, "GET", "/v1/quotations/"+before["id"].(string), "", "")
		if got, want := accessed(t, items(t, read)[0]), "PR00 9.00 EUR price_group 01 Wholesale; K007 5% price_group 01 Wholesale"; got != want {
			t.Errorf("conditions: got %s, want %s", got, want)
		}
	})
	t.Run("an entered price names no access", func(t *testing.T) {
		if got, want := accessed(t, e.quotedItem(t, wholesale, e.item(line{price: "7.00", key: "BRACKET-M8"}))), "PR00 7.00 EUR <nil>"; got != want {
			t.Errorf("conditions: got %s, want %s", got, want)
		}
	})
}

// TestSalesDataDatabaseFailures: every statement setting up, reading,
// changing and listing sales data, recording a group's record and pricing
// an item by its customer's group runs fails into an error (T80f).
func TestSalesDataDatabaseFailures(t *testing.T) {
	e := setupGroups(t, "PR00", "K007")
	h := endpoints(t, time.Now())
	id := uuid.MustParse(e.inGroup(t, e.customer, "01"))
	other := e.partner(t, "BP70000002", "Contoso", "active", "customer")
	e.priced(t, rec{})
	from := instant(t, "2026-01-01T00:00:00Z")
	calls := map[string]func(ctx context.Context) error{
		"set up": func(ctx context.Context) error {
			_, err := h.CreateCustomerSalesData(ctx, &gen.CustomerSalesDataCreate{LegalEntityID: e.DE, CustomerID: other, PriceGroup: gen.NewOptString("02")},
				gen.CreateCustomerSalesDataParams{})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetCustomerSalesData(ctx, gen.GetCustomerSalesDataParams{ID: id})
			return err
		},
		"change": func(ctx context.Context) error {
			_, err := h.UpdateCustomerSalesData(ctx, &gen.CustomerSalesDataUpdate{PriceGroup: gen.NewNilString("02")},
				gen.UpdateCustomerSalesDataParams{ID: id, IfMatch: `"1"`})
			return err
		},
		"change to none": func(ctx context.Context) error {
			_, err := h.UpdateCustomerSalesData(ctx, &gen.CustomerSalesDataUpdate{PriceGroup: gen.NilString{Null: true}},
				gen.UpdateCustomerSalesDataParams{ID: id, IfMatch: `"1"`})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListCustomerSalesData(ctx, gen.ListCustomerSalesDataParams{PriceGroup: gen.NewOptString("01")})
			return err
		},
		"record a group's price": func(ctx context.Context) error {
			_, err := h.CreateSalesPrice(ctx, &gen.SalesPriceCreate{LegalEntityID: e.DE, ItemKey: "BRACKET-M8", PriceGroup: gen.NewOptString("01"),
				Unit: gen.NewOptString("EA"), UnitPrice: gen.NewOptMoney(gen.Money{Amount: "9.00", Currency: "EUR"}), ValidFrom: from}, gen.CreateSalesPriceParams{})
			return err
		},
		"a quotation of a customer in a group": func(ctx context.Context) error {
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
