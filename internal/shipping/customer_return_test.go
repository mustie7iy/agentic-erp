package shipping_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/sales"
)

// A customer's return received into stock (S2b4; ADR-0083 As built
// S2b4), through the API: the goods issue delivers, billing bills and
// posts, a returns order takes the customer's return, and its receipt
// posts the stock items into stock (653) at what their goods issues took,
// the cost of goods sold taken back.

const returnsOrders = "/v1/returns-orders"

// posted bills what the order delivered and posts the billing document:
// its id.
func (e env) posted(t *testing.T, order string) string {
	t.Helper()
	bill, tag := e.Must(201, "POST", "/v1/billing-documents", fmt.Sprintf(`{"sales_order_id": %q}`, order), "")
	id := bill["id"].(string)
	e.Must(200, "POST", "/v1/billing-documents/"+id+":post", "", tag)
	return id
}

// returning creates and confirms a returns order of the billing
// document's items, "billing item quantity unit" each: its id and
// number.
func (e env) returning(t *testing.T, doc string, items ...string) (string, string) {
	t.Helper()
	var list []any
	for _, it := range items {
		f := strings.Fields(it)
		var no int
		fmt.Sscan(f[0], &no) //nolint:errcheck // the tests' own literal
		list = append(list, map[string]any{"billing_item_no": no, "quantity": map[string]any{"value": f[1], "unit": f[2]}})
	}
	r, tag := e.Must(201, "POST", returnsOrders, mustJSON(t, map[string]any{"billing_document_id": doc, "reason": "defective", "items": list}), "")
	id := r["id"].(string)
	e.Must(200, "POST", returnsOrders+"/"+id+":confirm", "", tag)
	return id, r["document_number"].(string)
}

// received is a return receipt's item: "item quantity unit", with more.
func received(item string, more map[string]any) map[string]any {
	f := strings.Fields(item)
	var no int
	fmt.Sscan(f[0], &no) //nolint:errcheck // the tests' own literal
	it := map[string]any{"item_no": no, "quantity": map[string]any{"value": f[1], "unit": f[2]}}
	for k, v := range more {
		it[k] = v
	}
	return it
}

// receipt is the body of a return receipt on the day of the items.
func receipt(t *testing.T, day string, items ...map[string]any) string {
	t.Helper()
	return mustJSON(t, map[string]any{"receipt_date": day, "items": items})
}

// at names the storage location an item is received into.
func at(location uuid.UUID) map[string]any { return map[string]any{"storage_location_id": location} }

// handDelivered records a delivery of the order's item as a person
// confirmed one before goods issues (S2b1), which the API no longer takes
// for a stock item: the quantity in the item's unit, no cost.
func (e env) handDelivered(t *testing.T, order string, item int, quantity string) {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO sales.delivery (tenant_id, id, sales_order_id, delivery_no, delivery_date, created_by)
		SELECT $1, $2, $3, coalesce((SELECT max(delivery_no) FROM sales.delivery WHERE sales_order_id = $3), 0) + 1, '2026-01-02', $4
		FROM (SELECT set_config('app.tenant_id', $1::uuid::text, true)) scoped`, e.Tenant.ID, id, order, e.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO sales.delivery_item (tenant_id, delivery_id, sales_order_id, order_item_no, quantity)
		SELECT $1, $2, $3, $4, $5::numeric FROM (SELECT set_config('app.tenant_id', $1::uuid::text, true)) scoped`, e.Tenant.ID, id, order, item, quantity)
}

// returnedEvent is the ReturnReceived payload of the receipt, conforming
// to the event contract.
func (e env) returnedEvent(t *testing.T, receipt string) map[string]any {
	t.Helper()
	for _, p := range apitest.Published(t, e.Tenant, outbox.Event{Context: "sales", Name: "return_received"}) {
		if p["receipt_id"] == receipt {
			return p
		}
	}
	t.Fatalf("no ReturnReceived of receipt %s", receipt)
	return nil
}

// TestCustomerReturnIntoStock: a receipt of a returns order of a billed
// goods issue's bracket, received into S1, posts a 653 on the receipt's
// day referencing the returns order, which puts the boxes back at what
// the goods issue took for them, its WA entry crediting the cost of goods
// sold and debiting the stock; the receipt names the 653 and each stock
// item's cost, as its list and ReturnReceived do; its free-text item
// moves nothing. The 653 is not reversed, nor posted by a person.
func TestCustomerReturnIntoStock(t *testing.T) {
	e := setup(t)
	order := e.order(t, false, e.ofMaterial(e.bracket, "5 BOX"), e.freeText())
	d, _ := e.shipped(t, order, "1 5 BOX")
	day := d["goods_issue_date"].(string)
	e.Must(201, "POST", "/v1/sales-orders/"+order+"/deliveries", `{"delivery_date": "2026-01-02", "items": [{"item_no": 2, "quantity": {"value": "1", "unit": "EA"}}]}`, "")
	ret, number := e.returning(t, e.posted(t, order), "1 2 BOX", "2 1 EA")
	if v := e.valuationOf(t, e.bracket); v != "100 200.00 EUR" {
		t.Fatalf("the bracket's valuation after the goods issue: got %s, want 100 200.00 EUR", v)
	}
	got, _ := e.Must(201, "POST", returnsOrders+"/"+ret+"/receipts", receipt(t, day, received("1 2 BOX", at(e.s1)), received("2 1 EA", nil)), "")
	its := rows(t, got, "items")
	if c := money(its[0]["cost"]); c != "80.00 EUR" {
		t.Errorf("the bracket's cost: got %s, want 80.00 EUR, 2 of the 5 boxes the goods issue took 200.00 for", c)
	}
	if c, ok := its[1]["cost"]; ok {
		t.Errorf("the free-text item's cost: got %v, want none", c)
	}
	doc, docTag := e.Must(200, "GET", "/v1/material-documents/"+got["material_document_id"].(string), "", "")
	line := rows(t, doc, "lines")[0]
	if g, w := fmt.Sprintf("%v %v %v %v %v %v %s", doc["number"], doc["movement_type"], doc["reference"], doc["posting_date"], line["storage_location_id"],
		line["quantity"].(map[string]any)["value"], money(line["amount"])), fmt.Sprintf("%v 653 %s %s %s 40 80.00 EUR", got["material_document_number"], number, day, e.s1); g != w {
		t.Errorf("the 653: got %s, want %s", g, w)
	}
	cogs, stock := e.Account(t, "2000", "519000"), e.Account(t, "2000", "150000")
	if g, w := e.entryLines(t, doc["journal_entry_id"].(string)), sorted(fmt.Sprintf("%s -80.00 EUR", cogs), fmt.Sprintf("%s 80.00 EUR", stock)); !slices.Equal(g, w) {
		t.Errorf("the 653's entry: got %v, want %v (the cost of goods sold credited, the stock debited)", g, w)
	}
	if v := e.valuationOf(t, e.bracket); v != "140 280.00 EUR" {
		t.Errorf("the bracket's valuation: got %s, want 140 280.00 EUR", v)
	}
	page, _ := e.Must(200, "GET", returnsOrders+"/"+ret+"/receipts", "", "")
	if listed := rows(t, page, "items")[0]; listed["material_document_id"] != got["material_document_id"] || money(rows(t, listed, "items")[0]["cost"]) != "80.00 EUR" {
		t.Errorf("the listed receipt: got %v, want it naming the 653 and the cost", listed)
	}
	ev := e.returnedEvent(t, got["id"].(string))
	evItems, _ := ev["items"].([]any)
	if first, _ := evItems[0].(map[string]any); ev["material_document_number"] != got["material_document_number"] || money(first["cost"]) != "80.00 EUR" {
		t.Errorf("ReturnReceived: got %v, want the 653 and the bracket's cost", ev)
	}
	e.Problem(409, "inventory.reverse_customer_return", "POST", "/v1/material-documents/"+doc["id"].(string)+":reverse", "", docTag)
	e.Problem(422, "inventory.unknown_movement_type", "POST", "/v1/material-documents", mustJSON(t, map[string]any{"movement_type": "653",
		"items": []any{e.stockItem(e.bracket, "1", nil)}}), "")
}

// TestCustomerReturnRefused: a stock item's receipt names its storage
// location, one of the company kept for the material, and a batch or
// serial numbers only of a material managed so; another item names
// none; each refusal names the request's field and posts nothing.
func TestCustomerReturnRefused(t *testing.T) {
	e := setup(t)
	order := e.order(t, false, e.ofMaterial(e.bracket, "5 BOX"), e.freeText())
	d, _ := e.shipped(t, order, "1 5 BOX")
	day := d["goods_issue_date"].(string)
	e.Must(201, "POST", "/v1/sales-orders/"+order+"/deliveries", `{"delivery_date": "2026-01-02", "items": [{"item_no": 2, "quantity": {"value": "1", "unit": "EA"}}]}`, "")
	ret, _ := e.returning(t, e.posted(t, order), "1 2 BOX", "2 1 EA")
	before := e.valuationOf(t, e.bracket)
	free := func(more map[string]any) map[string]any { return received("2 1 EA", more) }
	bracket := func(more map[string]any) map[string]any { return received("1 2 BOX", more) }
	for _, c := range []struct {
		name, code, field string
		items             []map[string]any
	}{
		{"a stock item without its storage location", "sales.storage_location_required", "items[0].storage_location_id", []map[string]any{bracket(nil)}},
		{"a storage location for a free-text item", "sales.return_not_stocked", "items[0].storage_location_id", []map[string]any{free(at(e.s1))}},
		{"a batch for a free-text item", "sales.return_not_stocked", "items[0].batch_number", []map[string]any{free(map[string]any{"batch_number": "L1"})}},
		{"serial numbers for a free-text item", "sales.return_not_stocked", "items[0].serial_numbers",
			[]map[string]any{free(map[string]any{"serial_numbers": []string{"S1"}})}},
		{"a storage location of another company", "sales.storage_location_invalid", "items[1].storage_location_id", []map[string]any{free(nil), bracket(at(e.s9))}},
		{"a plant for a storage location", "sales.storage_location_invalid", "items[0].storage_location_id", []map[string]any{bracket(at(e.p1))}},
		{"a storage location the bracket is not kept at", "inventory.storage_location_not_extended", "items[1].storage_location_id",
			[]map[string]any{free(nil), bracket(at(e.s2))}},
		{"a batch of a material not managed in batches", "inventory.batch_not_allowed", "items[1].batch_number",
			[]map[string]any{free(nil), bracket(map[string]any{"storage_location_id": e.s1, "batch_number": "L1"})}},
		{"serial numbers of a material without a profile", "inventory.serial_not_allowed", "items[0].serial_numbers",
			[]map[string]any{bracket(map[string]any{"storage_location_id": e.s1, "serial_numbers": []string{"S1", "S2"}})}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := e.Problem(422, c.code, "POST", returnsOrders+"/"+ret+"/receipts", receipt(t, day, c.items...), "")
			if f := field(m); f != c.field {
				t.Errorf("the field: got %v, want %s", f, c.field)
			}
		})
	}
	if v := e.valuationOf(t, e.bracket); v != before {
		t.Errorf("the bracket's valuation: got %s, want %s: nothing posted", v, before)
	}
}

// TestCustomerReturnCosts: three receipts of a can each, out of three
// cans a goods issue took 1.00 EUR for, take back their shares of what is
// left, rounded half away from zero, the last the remainder, so the cost
// of goods sold gets back what it was debited and the stock its value.
func TestCustomerReturnCosts(t *testing.T) {
	e := setup(t)
	oil := e.material(t, "Cutting oil", "trading_good")
	e.value(t, oil, "3000", "moving_average", "0.01")
	e.Must(201, "POST", "/v1/material-documents", mustJSON(t, map[string]any{"movement_type": "561", "items": []any{
		e.stockItem(oil, "3", map[string]any{"amount": map[string]any{"amount": "1.00", "currency": "EUR"}})}}), "")
	order := e.order(t, false, e.ofMaterial(oil, "3 EA"))
	d, _ := e.shipped(t, order, "1 3 EA")
	day := d["goods_issue_date"].(string)
	ret, _ := e.returning(t, e.posted(t, order), "1 3 EA")
	var costs []string
	for range 3 {
		got, _ := e.Must(201, "POST", returnsOrders+"/"+ret+"/receipts", receipt(t, day, received("1 1 EA", at(e.s1))), "")
		costs = append(costs, money(rows(t, got, "items")[0]["cost"]))
	}
	if want := []string{"0.33 EUR", "0.34 EUR", "0.33 EUR"}; !slices.Equal(costs, want) {
		t.Errorf("the receipts' costs: got %v, want %v (1.00 ÷ 3, then 0.67 ÷ 2, then the remainder)", costs, want)
	}
	if v := e.valuationOf(t, oil); v != "3 1.00 EUR" {
		t.Errorf("the oil's valuation: got %s, want 3 1.00 EUR, as before the goods issue", v)
	}
}

// TestCustomerReturnWithoutGoodsIssue: of an order item delivered in
// part by a goods issue and in part by hand, as before goods issues, a
// receipt takes back what the goods issue delivered and no more; of one
// no goods issue delivered, the receipt goes back at the moving price.
func TestCustomerReturnWithoutGoodsIssue(t *testing.T) {
	e := setup(t)
	mixed := e.order(t, false, e.ofMaterial(e.bracket, "2 BOX"))
	d, _ := e.shipped(t, mixed, "1 1 BOX")
	day := d["goods_issue_date"].(string)
	e.handDelivered(t, mixed, 1, "1")
	ret, number := e.returning(t, e.posted(t, mixed), "1 2 BOX")
	m := e.Problem(422, "sales.exceeds_issued_quantity", "POST", returnsOrders+"/"+ret+"/receipts", receipt(t, day, received("1 2 BOX", at(e.s1))), "")
	if f := field(m); f != "items[0].quantity.value" || !strings.Contains(fmt.Sprint(m["detail"]), number) {
		t.Errorf("the refusal: got %v on %v, want items[0].quantity.value naming %s", m["detail"], f, number)
	}
	got, _ := e.Must(201, "POST", returnsOrders+"/"+ret+"/receipts", receipt(t, day, received("1 1 BOX", at(e.s1))), "")
	if c := money(rows(t, got, "items")[0]["cost"]); c != "40.00 EUR" {
		t.Errorf("the box the goods issue delivered: got %s, want 40.00 EUR", c)
	}
	e.Problem(422, "sales.exceeds_issued_quantity", "POST", returnsOrders+"/"+ret+"/receipts", receipt(t, day, received("1 1 BOX", at(e.s1))), "")
	byHand := e.order(t, false, e.ofMaterial(e.bracket, "1 BOX"))
	e.handDelivered(t, byHand, 1, "1")
	ret, _ = e.returning(t, e.posted(t, byHand), "1 1 BOX")
	got, _ = e.Must(201, "POST", returnsOrders+"/"+ret+"/receipts", receipt(t, day, received("1 1 BOX", at(e.s1))), "")
	if c := money(rows(t, got, "items")[0]["cost"]); c != "40.00 EUR" {
		t.Errorf("a box no goods issue delivered: got %s, want 40.00 EUR, 20 EA at the moving price of 2.00", c)
	}
}

// TestCustomerReturnTracked: a returned item of a batch-managed material
// goes back to the batch it names, and one of a serial-managed material
// takes its serial number into stock again; a receipt naming no batch,
// or a serial number in stock, is refused naming its field.
func TestCustomerReturnTracked(t *testing.T) {
	e := setupTracked(t)
	order := e.order(t, false, e.ofMaterial(e.sealant, "4 EA"), e.ofMaterial(e.gauge, "1 EA"))
	id, tag := e.picked(t, order, "1 4 EA", "2 1 EA")
	d, _ := e.Must(200, "POST", deliveries+"/"+id+":postGoodsIssue", issueBody(t, part("1 4 EA", "L2"), part("2 1 EA", "", "G1")), tag)
	day := d["goods_issue_date"].(string)
	ret, _ := e.returning(t, e.posted(t, order), "1 3 EA", "2 1 EA")
	e.Problem(422, "inventory.batch_required", "POST", returnsOrders+"/"+ret+"/receipts", receipt(t, day, received("1 3 EA", at(e.s1))), "")
	m := e.Problem(409, "inventory.serial_in_stock", "POST", returnsOrders+"/"+ret+"/receipts", receipt(t, day,
		received("2 1 EA", map[string]any{"storage_location_id": e.s1, "serial_numbers": []string{"G2"}})), "")
	if !strings.Contains(fmt.Sprint(m["detail"]), "G2") {
		t.Errorf("the refusal: got %v, want it naming G2", m["detail"])
	}
	e.Must(201, "POST", returnsOrders+"/"+ret+"/receipts", receipt(t, day,
		received("1 3 EA", map[string]any{"storage_location_id": e.s1, "batch_number": "L2"}),
		received("2 1 EA", map[string]any{"storage_location_id": e.s1, "serial_numbers": []string{"G1"}})), "")
	if got := e.stockByBatch(t, e.sealant); got != "L1 10, L2 4" {
		t.Errorf("the sealant by batch: got %s, want L1 10, L2 4 (1 of L2 left, 3 back)", got)
	}
	if got := e.inStock(t, e.gauge); got != "G1 G2 G3" {
		t.Errorf("the gauges in stock: got %s, want G1 G2 G3", got)
	}
}

// TestCustomerReturnAvailable: the stock a return puts back is what an
// order's availability check confirms (S2j), as it reads the stock on
// hand.
func TestCustomerReturnAvailable(t *testing.T) {
	e := setup(t)
	shipped := e.order(t, false, e.ofMaterial(e.bracket, "10 BOX"))
	d, _ := e.shipped(t, shipped, "1 10 BOX")
	ret, _ := e.returning(t, e.posted(t, shipped), "1 2 BOX")
	e.Must(201, "POST", returnsOrders+"/"+ret+"/receipts", receipt(t, d["goods_issue_date"].(string), received("1 2 BOX", at(e.s1))), "")
	order := e.order(t, true, e.ofMaterial(e.bracket, "3 BOX"))
	o, _ := e.Must(200, "GET", "/v1/sales-orders/"+order, "", "")
	o, _ = e.Must(200, "POST", "/v1/sales-orders/"+order+":confirm", "", fmt.Sprintf(`"%v"`, o["version"]))
	if q := rows(t, o, "items")[0]["confirmed_quantity"]; fmt.Sprint(q.(map[string]any)["value"]) != "2" {
		t.Errorf("the confirmed quantity: got %v, want the 2 boxes returned, the plant's whole stock", q)
	}
}

// TestCustomerReturnsAtOnce: two receipts of two returns orders of one
// order item, of which a goods issue delivered one box and a person the
// other, wait for one another on the bracket's valuation, so the second
// finds the goods-issued box back in stock and is refused.
func TestCustomerReturnsAtOnce(t *testing.T) {
	e := setup(t)
	order := e.order(t, false, e.ofMaterial(e.bracket, "2 BOX"))
	d, _ := e.shipped(t, order, "1 1 BOX")
	day, err := time.Parse(time.DateOnly, d["goods_issue_date"].(string))
	if err != nil {
		t.Fatal(err)
	}
	e.handDelivered(t, order, 1, "1")
	doc := e.posted(t, order)
	first, _ := e.returning(t, doc, "1 1 BOX")
	second, _ := e.returning(t, doc, "1 1 BOX")
	pages, err := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	h := app.Sales(app.Config{Pages: pages, Now: time.Now})
	receive := func(id string) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := h.ConfirmReceipt(ctx, uuid.MustParse(id), sales.ReceiptInput{Date: day, Items: []sales.ReceiptItemInput{{
				ItemQuantity: sales.ItemQuantity{ItemNo: 1, Quantity: "1", Unit: "BOX"}, StorageLocationID: &e.s1}}})
			return err
		}
	}
	a, b := apitest.Race(t, e.Tenant, e.Tenant.Human, e.Tenant.Human, receive(first), receive(second))
	if a != nil {
		t.Fatalf("the first receipt: got %v, want it posted", a)
	}
	if code := problem.From(b).Code; code != "sales.exceeds_issued_quantity" {
		t.Errorf("the second receipt: got %v, want sales.exceeds_issued_quantity", b)
	}
}
