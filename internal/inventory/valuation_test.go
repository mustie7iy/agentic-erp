package inventory_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

const valuations = "/v1/material-valuations"

// valuation is the material's valuation in the plant as "quantity unit
// value price/unit".
func (e env) valuation(t *testing.T, material string, plant uuid.UUID) string {
	t.Helper()
	page, _ := e.Must(200, "GET", valuations+"?material_id="+material+"&plant_id="+plant.String(), "", "")
	v := rows(t, page, "items")[0]
	q, value, price := v["quantity"].(map[string]any), v["value"].(map[string]any), v["price"].(map[string]any)
	return fmt.Sprintf("%v %v %v %v/%v", q["value"], q["unit"], value["amount"], price["amount"], v["price_unit"])
}

// balance is company 2000's leading ledger balance of the account, debit
// positive, in minor units.
func (e env) balance(t *testing.T, account string) int64 {
	t.Helper()
	var b int64
	pgtest.Scan(t, `SELECT coalesce(sum(l.amount_lc), 0)::bigint FROM ledger.journal_line l JOIN ledger.gl_account a ON a.id = l.account_id
		WHERE l.tenant_id = $1 AND l.legal_entity_id = $2 AND a.number = $3 AND l.ledger_id = (SELECT g.id FROM ledger.ledger g WHERE g.tenant_id = l.tenant_id AND g.is_leading)`,
		[]any{e.Tenant.ID, e.DE, account}, &b)
	return b
}

// entry is the document's journal entry as "type account:amount ..." by
// line, amounts in minor units.
func (e env) entry(t *testing.T, d map[string]any) string {
	t.Helper()
	id, ok := d["journal_entry_id"].(string)
	if !ok {
		return "none"
	}
	var out string
	pgtest.Scan(t, `SELECT e.document_type || ' ' || string_agg(a.number || ':' || l.amount_tc, ' ' ORDER BY l.line_no) FROM ledger.journal_line l
		JOIN ledger.journal_entry e ON e.tenant_id = l.tenant_id AND e.fiscal_year = l.fiscal_year AND e.id = l.journal_entry_id
		JOIN ledger.gl_account a ON a.id = l.account_id
		WHERE l.journal_entry_id = $1 AND l.ledger_id = (SELECT g.id FROM ledger.ledger g WHERE g.tenant_id = l.tenant_id AND g.is_leading) GROUP BY e.document_type`, []any{id}, &out)
	return out
}

func amount(value string) map[string]any { return map[string]any{"amount": value, "currency": "EUR"} }

// TestMovingAverage: at a moving price a receipt adds what it is worth or
// its quantity at the price, an issue takes the value in proportion and
// the last issue the rest, each posted between the stock account and its
// movement type's; the stock account holds the valuation's value.
func TestMovingAverage(t *testing.T) {
	e := setup(t)
	center := map[string]any{"cost_center_id": e.c1}
	for _, s := range []struct {
		name, movementType string
		item               map[string]any
		entry, valuation   string
	}{
		{"an initial entry at the moving price", "561", e.item("10", "EA", nil), "WE 150000:2000 391000:-2000", "10 EA 20.00 2.00/1"},
		{"a receipt at its amount", "501", e.item("10", "EA", map[string]any{"amount": amount("40.00")}), "WE 150000:4000 219000:-4000", "20 EA 60.00 3.00/1"},
		{"an issue of a third", "201", e.item("7", "EA", center), "WA 150000:-2100 515000:2100", "13 EA 39.00 3.00/1"},
		{"a scrapping of the rest", "551", e.item("13", "EA", center), "WA 150000:-3900 516000:3900", "0 EA 0.00 3.00/1"},
	} {
		t.Run(s.name, func(t *testing.T) {
			d := e.post(t, s.movementType, s.item)
			if got := e.entry(t, d); got != s.entry {
				t.Errorf("entry: got %s, want %s", got, s.entry)
			}
			if got := e.valuation(t, e.bracket, e.p1); got != s.valuation {
				t.Errorf("valuation: got %s, want %s", got, s.valuation)
			}
		})
	}
	if got := e.balance(t, "150000"); got != 0 {
		t.Errorf("the stock account: got %d, want 0", got)
	}
}

// TestStandardPrice: at a standard price the stock is always worth its
// quantity at the price, an issue's rounding taken by what is left, a
// receipt's amount beyond that a price difference; a price change
// revalues the stock against the revaluation account and is published.
func TestStandardPrice(t *testing.T) {
	e := setup(t)
	bolt := e.material(t, "Hex bolt M10", "raw_material", true)
	got, _ := e.Must(201, "POST", valuations, mustJSON(t, map[string]any{"material_id": bolt, "plant_id": e.p1, "valuation_class": "3000",
		"price_control": "standard", "price": amount("1.00"), "price_unit": 3}), "")
	id := got["id"].(string)
	item := func(qty string, over map[string]any) map[string]any {
		it := e.item(qty, "EA", over)
		it["material_id"] = bolt
		return it
	}
	center := map[string]any{"cost_center_id": e.c1}
	for _, s := range []struct {
		name, movementType string
		item               map[string]any
		entry, valuation   string
	}{
		{"an initial entry worth more", "561", item("10", map[string]any{"amount": amount("5.00")}), "WE 150000:333 517000:167 391000:-500", "10 EA 3.33 1.00/3"},
		{"an issue", "201", item("1", center), "WA 150000:-33 515000:33", "9 EA 3.00 1.00/3"},
		{"an issue the rounding falls to", "201", item("1", center), "WA 150000:-33 515000:33", "8 EA 2.67 1.00/3"},
	} {
		t.Run(s.name, func(t *testing.T) {
			d := e.post(t, s.movementType, s.item)
			if got := e.entry(t, d); got != s.entry {
				t.Errorf("entry: got %s, want %s", got, s.entry)
			}
			if got := e.valuation(t, bolt, e.p1); got != s.valuation {
				t.Errorf("valuation: got %s, want %s", got, s.valuation)
			}
		})
	}
	_, tag := e.Must(200, "GET", valuations+"/"+id, "", "")
	changed, _ := e.Must(200, "POST", valuations+"/"+id+":changePrice", mustJSON(t, map[string]any{"price": amount("1.50")}), tag)
	if got := fmt.Sprint(changed["value"].(map[string]any)["amount"]); got != "4.00" {
		t.Errorf("revalued: got %s, want 4.00", got)
	}
	last, _ := changed["last_price_change"].(map[string]any)
	if got := fmt.Sprint(last["revaluation"].(map[string]any)["amount"], " ", e.entry(t, last)); got != "1.33 PR 150000:133 518000:-133" {
		t.Errorf("the change answered: got %s, want 1.33 PR 150000:133 518000:-133", got)
	}
	if got := e.balance(t, "518000"); got != -133 {
		t.Errorf("the revaluation account: got %d, want -133", got)
	}
	if got := e.balance(t, "150000"); got != 400 {
		t.Errorf("the stock account: got %d, want 400, the valuation's value", got)
	}
	events := e.priceEvents(t, id)
	if len(events) != 1 {
		t.Fatalf("events: got %d, want 1", len(events))
	}
	if got := fmt.Sprint(events[0]["revaluation"].(map[string]any)["amount"]); got != "1.33" {
		t.Errorf("the event's revaluation: got %s, want 1.33", got)
	}
	if got := events[0]["material_number"]; got != changed["material_number"] {
		t.Errorf("the event's material: got %v, want %v", got, changed["material_number"])
	}
}

func (e env) priceEvents(t *testing.T, valuation string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, p := range apitest.Published(t, e.Tenant, outbox.Event{Context: "inventory", Name: "material_price_changed"}) {
		if p["valuation_id"] == valuation {
			out = append(out, p)
		}
	}
	return out
}

// TestValuedReversalsAndTransfers: a reversal is valued at what its
// original was, the stock's share of it at the price now and the rest a
// price difference; a transfer between plants moves value from one
// plant's stock to the other's, each on its class's stock account, the
// receiving plant's price following; within a plant it posts nothing.
func TestValuedReversalsAndTransfers(t *testing.T) {
	e := setup(t)
	first := e.post(t, "501", e.item("10", "EA", map[string]any{"amount": amount("40.00")}))
	e.post(t, "501", e.item("10", "EA", map[string]any{"amount": amount("20.00")}))
	_, tag := e.Must(200, "GET", docs+"/"+first["id"].(string), "", "")
	rev, _ := e.Must(200, "POST", docs+"/"+first["id"].(string)+":reverse", "", tag)
	if got := e.entry(t, rev); got != "WE 150000:-3000 517000:-1000 219000:4000" {
		t.Errorf("the reversal's entry: got %s", got)
	}
	if got := e.valuation(t, e.bracket, e.p1); got != "10 EA 30.00 3.00/1" {
		t.Errorf("after the reversal: got %s, want 10 EA 30.00 3.00/1", got)
	}
	move := e.post(t, "301", e.item("5", "EA", map[string]any{"to_plant_id": e.p2, "to_storage_location_id": e.s3}))
	if got := e.entry(t, move) + " | " + e.valuation(t, e.bracket, e.p2); got != "WA 150000:-1500 150000:1500 | 5 EA 15.00 3.00/1" {
		t.Errorf("the transfer: got %s, want WA 150000:-1500 150000:1500 | 5 EA 15.00 3.00/1", got)
	}
	within := e.post(t, "311", e.item("1", "EA", map[string]any{"to_storage_location_id": e.s2}))
	if got := e.entry(t, within); got != "none" {
		t.Errorf("a transfer within the plant: got %s", got)
	}
	if got := e.balance(t, "150000"); got != 3000 {
		t.Errorf("the stock account: got %d, want 3000, the valuations' values", got)
	}
}

// TestValuationRefused: what a valuation, its class change, its price
// change and a valued movement are refused for.
func TestValuationRefused(t *testing.T) {
	e := setup(t)
	bare := e.material(t, "Unvalued bracket", "trading_good", true)
	service := e.material(t, "Assembly service", "service", true)
	cheap := e.material(t, "Free sample", "trading_good", true)
	e.value(t, cheap, e.p1, "moving_average", "0.00")
	thin := e.material(t, "Thin class bracket", "trading_good", true)
	e.Must(201, "POST", valuations, mustJSON(t, map[string]any{"material_id": thin, "plant_id": e.p1, "valuation_class": "3100",
		"price_control": "moving_average", "price": amount("1.00")}), "")
	create := func(over map[string]any) string {
		v := map[string]any{"material_id": bare, "plant_id": e.p1, "valuation_class": "3000", "price_control": "standard", "price": amount("1.00")}
		for k, x := range over {
			v[k] = x
		}
		return mustJSON(t, v)
	}
	for _, c := range []struct {
		name, path, body string
		status           int
		code             string
	}{
		{"a valuation of a service", valuations, create(map[string]any{"material_id": service}), 422, "inventory.material_not_stocked"},
		{"a valuation in a plant it is not at", valuations, create(map[string]any{"plant_id": e.p3}), 422, "inventory.plant_not_extended"},
		{"an unknown class", valuations, create(map[string]any{"valuation_class": "9999"}), 422, "inventory.unknown_valuation_class"},
		{"an unknown price control", valuations, create(map[string]any{"price_control": "fifo"}), 422, "inventory.unknown_price_control"},
		{"a standard price of zero", valuations, create(map[string]any{"price": amount("0.00")}), 422, "inventory.invalid_price"},
		{"a negative price", valuations, create(map[string]any{"price": amount("-1.00")}), 422, "inventory.invalid_price"},
		{"a price in dollars", valuations, create(map[string]any{"price": map[string]any{"amount": "1.00", "currency": "USD"}}), 422,
			"inventory.currency_mismatch"},
		{"a second valuation", valuations, create(map[string]any{"material_id": e.bracket}), 409, "inventory.valuation_exists"},
		{"a movement of an unvalued material", docs, body(t, "561", e.item("1", "EA", map[string]any{"material_id": bare})), 422, "inventory.no_valuation"},
		{"an amount on an issue", docs, body(t, "201", e.item("1", "EA", map[string]any{"cost_center_id": e.c1, "amount": amount("1.00")})), 422,
			"inventory.amount_not_allowed"},
		{"an amount in dollars", docs, body(t, "501", e.item("1", "EA", map[string]any{"amount": map[string]any{"amount": "1.00", "currency": "USD"}})), 422,
			"inventory.currency_mismatch"},
		{"a negative amount", docs, body(t, "501", e.item("1", "EA", map[string]any{"amount": amount("-1.00")})), 422, "inventory.invalid_amount"},
		{"a receipt at no price", docs, body(t, "561", e.item("1", "EA", map[string]any{"material_id": cheap})), 422, "inventory.no_price"},
		{"a class without the account", docs, body(t, "561", e.item("1", "EA", map[string]any{"material_id": thin})), 422, "inventory.no_inventory_account"},
	} {
		t.Run(c.name, func(t *testing.T) {
			status, got, _ := e.Do("POST", c.path, c.body, "")
			if status != c.status || got["code"] != c.code {
				t.Errorf("got %d %v, want %d %s", status, got, c.status, c.code)
			}
		})
	}
	if got := e.entryCount(t); got != 0 {
		t.Errorf("entries after the refusals: got %d, want none", got)
	}
}

func (e env) entryCount(t *testing.T) int {
	t.Helper()
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM ledger.journal_entry WHERE tenant_id = $1 AND source_context = 'inventory'`, []any{e.Tenant.ID}, &n)
	return n
}

// TestValuationChanges: a class changes while there is no stock, a price
// at the version read and not while the period is closed; a valuation
// of stock moved before valuation is refused.
func TestValuationChanges(t *testing.T) {
	e := setup(t)
	page, _ := e.Must(200, "GET", valuations+"?material_id="+e.bracket+"&plant_id="+e.p1.String(), "", "")
	id := rows(t, page, "items")[0]["id"].(string)
	_, tag := e.Must(200, "GET", valuations+"/"+id, "", "")
	changed, tag := e.Must(200, "PATCH", valuations+"/"+id, `{"valuation_class": "3100"}`, tag)
	if changed["valuation_class"] != "3100" {
		t.Errorf("class: got %v, want 3100", changed["valuation_class"])
	}
	for _, c := range []struct {
		name, body string
		status     int
		code       string
	}{
		{"the same class", `{"valuation_class": "3100"}`, 422, "inventory.nothing_to_change"},
		{"an unknown class", `{"valuation_class": "77"}`, 422, "inventory.unknown_valuation_class"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if status, got, _ := e.Do("PATCH", valuations+"/"+id, c.body, tag); status != c.status || got["code"] != c.code {
				t.Errorf("got %d %v, want %d %s", status, got, c.status, c.code)
			}
		})
	}
	e.Must(200, "PATCH", valuations+"/"+id, `{"valuation_class": "3000"}`, tag)
	e.post(t, "561", e.item("10", "EA", nil))
	_, tag = e.Must(200, "GET", valuations+"/"+id, "", "")
	for _, c := range []struct {
		name, path, body, tag string
		status                int
		code                  string
	}{
		{"a class with stock", valuations + "/" + id, `{"valuation_class": "3100"}`, tag, 409, "inventory.valuation_class_with_stock"},
		{"a stale version", valuations + "/" + id + ":changePrice", `{"price": {"amount": "2.50", "currency": "EUR"}}`, `"1"`, 412, "request.version_mismatch"},
		{"the same price", valuations + "/" + id + ":changePrice", `{"price": {"amount": "2.00", "currency": "EUR"}}`, tag, 422, "inventory.nothing_to_change"},
	} {
		t.Run(c.name, func(t *testing.T) {
			method := "POST"
			if !strings.Contains(c.path, ":") {
				method = "PATCH"
			}
			if status, got, _ := e.Do(method, c.path, c.body, c.tag); status != c.status || got["code"] != c.code {
				t.Errorf("got %d %v, want %d %s", status, got, c.status, c.code)
			}
		})
	}
	// Company 2000's time zone is UTC (the fixture's configuration), so its
	// day is UTC's.
	day := time.Now().UTC()
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE tenant_id = $1 AND legal_entity_id = $2
		AND fiscal_year = $3 AND period = $4`, e.Tenant.ID, e.DE, day.Year(), int(day.Month()))
	if status, got, _ := e.Do("POST", valuations+"/"+id+":changePrice", `{"price": {"amount": "2.50", "currency": "EUR"}}`, tag); status != 409 ||
		got["code"] != "ledger.period_not_open" {
		t.Errorf("a closed period: got %d %v, want 409 ledger.period_not_open", status, got)
	}
	e.unvaluedStock(t)
}

// unvaluedStock writes stock moved before valuation, as S1c could, past
// the triggers, and checks a valuation of it is refused.
func (e env) unvaluedStock(t *testing.T) {
	t.Helper()
	old := e.material(t, "Bracket of S1c", "trading_good", true)
	admin := pgtest.Admin(t)
	ctx := context.Background()
	for _, sql := range []string{`SET session_replication_role = replica`,
		`INSERT INTO inventory.stock (tenant_id, id, legal_entity_id, material_id, plant_id, storage_location_id, quantity)
			VALUES ($1, gen_random_uuid(), $2, $3, $4, $5, 4)`, `RESET session_replication_role`} {
		var args []any
		if strings.Contains(sql, "$1") {
			args = []any{e.Tenant.ID, e.DE, old, e.p1, e.s1}
		}
		if _, err := admin.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	status, got, _ := e.Do("POST", valuations, mustJSON(t, map[string]any{"material_id": old, "plant_id": e.p1, "valuation_class": "3000",
		"price_control": "standard", "price": amount("1.00")}), "")
	if status != 409 || got["code"] != "inventory.stock_without_valuation" {
		t.Errorf("stock moved unvalued: got %d %v, want 409 inventory.stock_without_valuation", status, got)
	}
}

// TestValuationEdges: a valuation's refusals of its plant and amounts'
// forms, values beyond an amount's range, a class without a price
// difference account, price changes that post nothing or are refused,
// another company's valuation, and valuations in pages.
func TestValuationEdges(t *testing.T) {
	e := setup(t)
	huge := e.material(t, "Gold bar", "trading_good", true)
	e.Must(201, "POST", valuations, mustJSON(t, map[string]any{"material_id": huge, "plant_id": e.p1, "valuation_class": "3000",
		"price_control": "standard", "price": amount("90000000000000000.00")}), "")
	fine := e.material(t, "Bracket by the ten thousand", "trading_good", true)
	e.Must(201, "POST", valuations, mustJSON(t, map[string]any{"material_id": fine, "plant_id": e.p1, "valuation_class": "3000",
		"price_control": "moving_average", "price": amount("1.00"), "price_unit": 10000}), "")
	diff := e.material(t, "Bracket without differences", "trading_good", true)
	e.Must(201, "POST", valuations, mustJSON(t, map[string]any{"material_id": diff, "plant_id": e.p1, "valuation_class": "3200",
		"price_control": "standard", "price": amount("1.00")}), "")
	page, _ := e.Must(200, "GET", valuations+"?material_id="+e.bracket+"&plant_id="+e.p2.String(), "", "")
	empty := rows(t, page, "items")[0]["id"].(string)
	_, emptyTag := e.Must(200, "GET", valuations+"/"+empty, "", "")
	for _, c := range []struct {
		name, method, path, body, tag string
		status                        int
		code                          string
	}{
		{"a valuation in no plant", "POST", valuations, mustJSON(t, map[string]any{"material_id": diff, "plant_id": e.s1, "valuation_class": "3000",
			"price_control": "standard", "price": amount("1.00")}), "", 422, "inventory.plant_invalid"},
		{"a price of three decimals", "POST", valuations, mustJSON(t, map[string]any{"material_id": diff, "plant_id": e.p2, "valuation_class": "3000",
			"price_control": "standard", "price": amount("1.005")}), "", 400, "request.invalid_amount"},
		{"an amount of one decimal", "POST", docs, body(t, "501", e.item("1", "EA", map[string]any{"amount": amount("1.5")})), "", 400, "request.invalid_amount"},
		{"a value beyond an amount", "POST", docs, body(t, "561", e.item("100", "EA", map[string]any{"material_id": huge})), "", 422,
			"inventory.amount_too_large"},
		{"a moving price beyond an amount", "POST", docs, body(t, "501", e.item("1", "EA", map[string]any{"material_id": fine,
			"amount": amount("90000000000000.00")})), "", 422, "inventory.amount_too_large"},
		{"receipts worth more than an amount", "POST", docs, body(t, "501", e.item("1", "EA", map[string]any{"amount": amount("90000000000000000.00")}),
			e.item("1", "EA", map[string]any{"amount": amount("90000000000000000.00")})), "", 422, "inventory.amount_too_large"},
		{"a price difference without its account", "POST", docs, body(t, "561", e.item("1", "EA", map[string]any{"material_id": diff,
			"amount": amount("2.00")})), "", 422, "inventory.no_inventory_account"},
		{"a stale class change", "PATCH", valuations + "/" + empty, `{"valuation_class": "3100"}`, `"9"`, 412, "request.version_mismatch"},
		{"a price in dollars", "POST", valuations + "/" + empty + ":changePrice", `{"price": {"amount": "1.00", "currency": "USD"}}`, emptyTag, 422,
			"inventory.currency_mismatch"},
		{"a moving price of zero without stock", "POST", valuations + "/" + empty + ":changePrice", `{"price": {"amount": "0.00", "currency": "EUR"}}`,
			emptyTag, 200, ""},
		{"a valuation of no material", "POST", valuations, mustJSON(t, map[string]any{"material_id": e.s1, "plant_id": e.p1, "valuation_class": "3000",
			"price_control": "standard", "price": amount("1.00")}), "", 422, "inventory.material_invalid"},
	} {
		t.Run(c.name, func(t *testing.T) {
			status, got, _ := e.Do(c.method, c.path, c.body, c.tag)
			if status != c.status || (c.code != "" && got["code"] != c.code) {
				t.Errorf("got %d %v, want %d %s", status, got, c.status, c.code)
			}
		})
	}
	if got := e.entryCount(t); got != 0 {
		t.Errorf("entries: got %d, want none, a price change without stock posting nothing", got)
	}
	e.post(t, "561", e.item("100", "EA", map[string]any{"material_id": diff}))
	page, _ = e.Must(200, "GET", valuations+"?material_id="+diff, "", "")
	id := rows(t, page, "items")[0]["id"].(string)
	_, tag := e.Must(200, "GET", valuations+"/"+id, "", "")
	for _, c := range []struct{ name, price, code string }{
		{"to zero", "0.00", "inventory.invalid_price"},
		{"beyond an amount", "90000000000000000.00", "inventory.amount_too_large"},
		{"without a revaluation account", "2.00", "inventory.no_inventory_account"},
	} {
		if status, got, _ := e.Do("POST", valuations+"/"+id+":changePrice", mustJSON(t, map[string]any{"price": amount(c.price)}), tag); got["code"] != c.code {
			t.Errorf("a standard price %s: got %d %v, want %s", c.name, status, got, c.code)
		}
	}
	outsider := e.Reader(t, "Outsider", e.US)
	if status, _, _ := e.As(outsider).Do("GET", valuations+"/"+id, "", ""); status != 404 {
		t.Errorf("another company's valuation: got %d, want 404", status)
	}
	first, _ := e.Must(200, "GET", valuations+"?page_size=2", "", "")
	next, _ := e.Must(200, "GET", valuations+"?page_size=2&page_token="+first["next_page_token"].(string), "", "")
	if got := len(rows(t, first, "items")) + len(rows(t, next, "items")); got < 4 {
		t.Errorf("valuations in pages: got %d, want 4 or more", got)
	}
}

// TestTransferReversalAfterReceipt: a transfer within a plant reversed
// after a receipt changed the moving price leaves the plant's value as
// it was, so the stock account still holds it.
func TestTransferReversalAfterReceipt(t *testing.T) {
	e := setup(t)
	e.post(t, "501", e.item("10", "EA", map[string]any{"amount": amount("10.00")}))
	move := e.post(t, "311", e.item("5", "EA", map[string]any{"to_storage_location_id": e.s2}))
	e.post(t, "501", e.item("10", "EA", map[string]any{"amount": amount("30.00")}))
	_, tag := e.Must(200, "GET", docs+"/"+move["id"].(string), "", "")
	rev, _ := e.Must(200, "POST", docs+"/"+move["id"].(string)+":reverse", "", tag)
	if got := e.entry(t, rev); got != "none" {
		t.Errorf("the reversal's entry: got %s, want none", got)
	}
	if got := e.valuation(t, e.bracket, e.p1); got != "20 EA 40.00 2.00/1" {
		t.Errorf("the valuation: got %s, want 20 EA 40.00 2.00/1", got)
	}
	if got := e.balance(t, "150000"); got != 4000 {
		t.Errorf("the stock account: got %d, want 4000, the valuation's value", got)
	}
}

// TestStandardTransfersAndReversals: a transfer into a standard-priced
// plant puts what it is valued at beyond the standard price on the price
// difference account, and its reversal takes it back; an issue's
// reversal at a standard price gives back what the issue took, its
// rounding a price difference; a transfer within a plant reversed after
// a price change leaves the value as it is. The stock account always
// holds the valuations' value.
func TestStandardTransfersAndReversals(t *testing.T) {
	e := setup(t)
	washer := e.material(t, "Washer M10", "raw_material", true)
	e.value(t, washer, e.p1, "moving_average", "1.00")
	e.Must(201, "POST", valuations, mustJSON(t, map[string]any{"material_id": washer, "plant_id": e.p2, "valuation_class": "3000",
		"price_control": "standard", "price": amount("0.40")}), "")
	item := func(qty string, over map[string]any) map[string]any {
		it := e.item(qty, "EA", over)
		it["material_id"] = washer
		return it
	}
	e.post(t, "501", item("10", map[string]any{"amount": amount("10.00")}))
	move := e.post(t, "301", item("5", map[string]any{"to_plant_id": e.p2, "to_storage_location_id": e.s3}))
	if got := e.entry(t, move); got != "WA 150000:-500 150000:200 517000:300" {
		t.Errorf("the transfer: got %s, want WA 150000:-500 150000:200 517000:300", got)
	}
	_, tag := e.Must(200, "GET", docs+"/"+move["id"].(string), "", "")
	back, _ := e.Must(200, "POST", docs+"/"+move["id"].(string)+":reverse", "", tag)
	if got := e.entry(t, back); got != "WA 150000:-200 517000:-300 150000:500" {
		t.Errorf("its reversal: got %s, want WA 150000:-200 517000:-300 150000:500", got)
	}
	bolt := e.material(t, "Bolt by the three", "raw_material", true)
	e.Must(201, "POST", valuations, mustJSON(t, map[string]any{"material_id": bolt, "plant_id": e.p1, "valuation_class": "3000",
		"price_control": "standard", "price": amount("1.00"), "price_unit": 3}), "")
	boltItem := func(qty string, over map[string]any) map[string]any {
		it := e.item(qty, "EA", over)
		it["material_id"] = bolt
		return it
	}
	e.post(t, "561", boltItem("10", nil))
	first := e.post(t, "201", boltItem("1", map[string]any{"cost_center_id": e.c1}))
	e.post(t, "201", boltItem("1", map[string]any{"cost_center_id": e.c1}))
	e.post(t, "201", boltItem("1", map[string]any{"cost_center_id": e.c1}))
	_, tag = e.Must(200, "GET", docs+"/"+first["id"].(string), "", "")
	rev, _ := e.Must(200, "POST", docs+"/"+first["id"].(string)+":reverse", "", tag)
	if got := e.entry(t, rev); got != "WA 150000:34 517000:-1 515000:-33" {
		t.Errorf("the issue's reversal: got %s, want WA 150000:34 517000:-1 515000:-33", got)
	}
	shift := e.post(t, "311", boltItem("3", map[string]any{"to_storage_location_id": e.s2}))
	page, _ := e.Must(200, "GET", valuations+"?material_id="+bolt, "", "")
	id := rows(t, page, "items")[0]["id"].(string)
	_, vtag := e.Must(200, "GET", valuations+"/"+id, "", "")
	e.Must(200, "POST", valuations+"/"+id+":changePrice", `{"price": {"amount": "2.00", "currency": "EUR"}}`, vtag)
	_, tag = e.Must(200, "GET", docs+"/"+shift["id"].(string), "", "")
	e.Must(200, "POST", docs+"/"+shift["id"].(string)+":reverse", "", tag)
	if got := e.valuation(t, bolt, e.p1); got != "8 EA 5.33 2.00/3" {
		t.Errorf("the bolt after its transfer's reversal: got %s, want 8 EA 5.33 2.00/3", got)
	}
	var total int64
	pgtest.Scan(t, `SELECT sum(value)::bigint FROM inventory.valuation WHERE tenant_id = $1`, []any{e.Tenant.ID}, &total)
	if got := e.balance(t, "150000"); got != total {
		t.Errorf("the stock account: got %d, want %d, the valuations' value", got, total)
	}
}

// TestValuationAtOnce: receipts and price changes of one valuation at
// once each post, none waiting on another for good, and the stock
// account holds the valuation's value after them.
func TestValuationAtOnce(t *testing.T) {
	e := setup(t)
	e.post(t, "501", e.item("10", "EA", map[string]any{"amount": amount("20.00")}))
	page, _ := e.Must(200, "GET", valuations+"?material_id="+e.bracket+"&plant_id="+e.p1.String(), "", "")
	id := rows(t, page, "items")[0]["id"].(string)
	var wg sync.WaitGroup
	statuses := make([]int, 10)
	for i := range statuses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%5 != 0 {
				statuses[i], _, _ = e.Do("POST", docs, body(t, "501", e.item("1", "EA", map[string]any{"amount": amount(fmt.Sprintf("%d.00", i))})), "")
				return
			}
			for {
				_, tag := e.Must(200, "GET", valuations+"/"+id, "", "")
				status, _, _ := e.Do("POST", valuations+"/"+id+":changePrice", fmt.Sprintf(`{"price": {"amount": "%d.00", "currency": "EUR"}}`, 5+i), tag)
				if status != 412 { // another write came first: read again
					statuses[i] = status
					return
				}
			}
		}()
	}
	wg.Wait()
	for i, s := range statuses {
		if s != 200 && s != 201 {
			t.Errorf("write %d: got %d, want it to succeed", i, s)
		}
	}
	page, _ = e.Must(200, "GET", valuations+"/"+id, "", "")
	value, _ := money.ParseAmount(page["value"].(map[string]any)["amount"].(string), 2)
	if got := e.balance(t, "150000"); got != int64(value) {
		t.Errorf("the stock account: got %d, want %d, the valuation's value", got, value)
	}
}
