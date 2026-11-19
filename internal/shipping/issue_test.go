package shipping_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// issued posts the delivery's goods issue at its version and returns it.
func (e env) issued(t *testing.T, id, tag string) (map[string]any, string) {
	t.Helper()
	return e.Must(200, "POST", deliveries+"/"+id+":postGoodsIssue", "", tag)
}

// money is an amount as "amount currency".
func money(v any) string {
	m, _ := v.(map[string]any)
	return fmt.Sprintf("%v %v", m["amount"], m["currency"])
}

// TestGoodsIssue: the goods issue of a delivery picked in part issues
// what is picked as a 601 material document referencing the delivery,
// valued at the moving price and posted from stock to the cost of goods
// sold; the order delivers what was issued, with its cost, and has the
// rest open again; billing bills the quantity issued. It posts once; the
// delivery changes no more; its document is not reversed by hand and a
// 601 is not posted by hand; the stock item is not confirmed by hand,
// while the free-text item is.
func TestGoodsIssue(t *testing.T) {
	e := setup(t)
	order := e.order(t, false, e.ofMaterial(e.bracket, "5 BOX"), e.freeText())
	d, tag := e.Must(201, "POST", deliveries, e.create(order), "")
	id := d["id"].(string)
	_, tag = e.Must(200, "POST", deliveries+"/"+id+":pick", pick("1 3 BOX"), tag)
	got, gotTag := e.issued(t, id, tag)
	if got["status"] != "goods_issued" || got["goods_issue_date"] == nil || money(got["cost_of_goods_sold"]) != "120.00 EUR" {
		t.Errorf("the delivery: got %v, %v, %v; want goods_issued on a day, 120.00 EUR (60 EA at 2.00)", got["status"], got["goods_issue_date"],
			got["cost_of_goods_sold"])
	}
	if c := money(rows(t, got, "items")[0]["cost"]); c != "120.00 EUR" {
		t.Errorf("the item's cost: got %s, want 120.00 EUR", c)
	}
	doc, _ := e.Must(200, "GET", "/v1/material-documents/"+got["material_document_id"].(string), "", "")
	if doc["movement_type"] != "601" || doc["reference"] != got["number"] || doc["number"] != got["material_document_number"] {
		t.Errorf("the material document: got %v %v %v, want a 601 referencing %v", doc["movement_type"], doc["reference"], doc["number"], got["number"])
	}
	entry, _ := e.Must(200, "GET", "/v1/journal-entries/"+doc["journal_entry_id"].(string), "", "")
	var lines []string
	for _, l := range rows(t, entry, "lines") {
		lines = append(lines, money(l["amount"]))
	}
	slices.Sort(lines)
	if entry["document_type"] != "WA" || strings.Join(lines, ", ") != "-120.00 EUR, 120.00 EUR" {
		t.Errorf("the entry: got %v %v, want WA -120.00 / 120.00", entry["document_type"], lines)
	}
	page, _ := e.Must(200, "GET", "/v1/stock?material_id="+e.bracket, "", "")
	if q := rows(t, page, "items")[0]["quantity"].(map[string]any); q["value"] != "140" {
		t.Errorf("the stock: got %v, want 140 EA", q)
	}
	o, _ := e.Must(200, "GET", "/v1/sales-orders/"+order, "", "")
	item := rows(t, o, "items")[0]
	if q, h := item["delivered_quantity"].(map[string]any), item["in_delivery_quantity"].(map[string]any); q["value"] != "3" || h["value"] != "0" {
		t.Errorf("the order's item: delivered %v, in delivery %v; want 3 and 0", q, h)
	}
	sd, _ := e.Must(200, "GET", "/v1/sales-orders/"+order+"/deliveries", "", "")
	first := rows(t, sd, "items")[0]
	if first["outbound_delivery_number"] != got["number"] || first["material_document_number"] != doc["number"] ||
		money(rows(t, first, "items")[0]["cost"]) != "120.00 EUR" {
		t.Errorf("the order's delivery: got %v", first)
	}
	bill, _ := e.Must(201, "POST", "/v1/billing-documents", fmt.Sprintf(`{"sales_order_id": %q}`, order), "")
	if its := rows(t, bill, "items"); len(its) != 1 || its[0]["quantity"].(map[string]any)["value"] != "3" {
		t.Errorf("the billing document: got %v, want the 3 BOX issued alone", its)
	}

	e.Problem(409, "shipping.goods_issued", "POST", deliveries+"/"+id+":postGoodsIssue", "", gotTag)
	e.Problem(409, "shipping.goods_issued", "POST", deliveries+"/"+id+":pick", pick("1 1 BOX"), gotTag)
	_, docTag := e.Must(200, "GET", "/v1/material-documents/"+doc["id"].(string), "", "")
	e.Problem(409, "inventory.reverse_goods_issue", "POST", "/v1/material-documents/"+doc["id"].(string)+":reverse", "", docTag)
	e.Problem(422, "inventory.unknown_movement_type", "POST", "/v1/material-documents", mustJSON(t, map[string]any{"movement_type": "601",
		"items": []any{map[string]any{"material_id": e.bracket, "plant_id": e.p1, "storage_location_id": e.s1, "quantity": map[string]any{"value": "1", "unit": "EA"}}}}), "")
	m := e.Problem(422, "sales.delivery_by_goods_issue", "POST", "/v1/sales-orders/"+order+"/deliveries",
		`{"delivery_date": "2026-01-02", "items": [{"item_no": 1, "quantity": {"value": "1", "unit": "BOX"}}]}`, "")
	if f := field(m); f != "items[0].item_no" {
		t.Errorf("the field: got %v, want items[0].item_no", f)
	}
	e.Must(201, "POST", "/v1/sales-orders/"+order+"/deliveries", `{"delivery_date": "2026-01-02", "items": [{"item_no": 2, "quantity": {"value": "1", "unit": "EA"}}]}`, "")
	again, _ := e.Must(201, "POST", deliveries, e.create(order), "")
	if items(t, again) != "1: 2 BOX, picked 0" {
		t.Errorf("what is open again: got %s, want the 2 BOX not picked", items(t, again))
	}

	ev := e.events(t, id)
	if last := ev[len(ev)-1]; last["status"] != "goods_issued" || last["material_document_number"] != doc["number"] {
		t.Errorf("the last event: got %v, want goods_issued naming the document", last)
	}
	var delivered []map[string]any
	for _, p := range apitest.Published(t, e.Tenant, outbox.Event{Context: "sales", Name: "sales_order_delivered"}) {
		if p["outbound_delivery_id"] == id {
			delivered = append(delivered, p)
		}
	}
	if len(delivered) != 1 || delivered[0]["material_document_number"] != doc["number"] {
		t.Errorf("SalesOrderDelivered of the goods issue: got %v, want one naming the document", delivered)
	}
	if a := e.audits(t, id, "shipping.outbound_delivery.goods_issue"); len(a) != 1 || a[0]["journal_entry_number"] != entry["document_number"] {
		t.Errorf("audits: got %v, want one naming the entry", a)
	}
}

// TestGoodsIssueStandardPrice: a standard-priced material's goods issue
// costs its quantity at the standard price, which is what its stock's
// value falls by; an item picked at nothing issues nothing and costs
// nothing.
func TestGoodsIssueStandardPrice(t *testing.T) {
	e := setup(t)
	bolt := e.material(t, "Hex bolt M10", "raw_material")
	e.value(t, bolt, "3000", "standard", "0.55")
	e.receiveOf(t, bolt, "1000")
	order := e.order(t, false, e.ofMaterial(bolt, "333 EA"), e.ofMaterial(bolt, "3 EA"))
	d, tag := e.Must(201, "POST", deliveries, e.create(order), "")
	_, tag = e.Must(200, "POST", deliveries+"/"+d["id"].(string)+":pick", pick("1 333 EA"), tag)
	got, _ := e.issued(t, d["id"].(string), tag)
	if its := rows(t, got, "items"); its[1]["cost"] != nil {
		t.Errorf("the item picked at nothing: got cost %v, want none", its[1]["cost"])
	}
	page, _ := e.Must(200, "GET", "/v1/material-valuations?material_id="+bolt, "", "")
	if c, v := money(got["cost_of_goods_sold"]), money(rows(t, page, "items")[0]["value"]); c != "183.15 EUR" || v != "366.85 EUR" {
		t.Errorf("cost and the stock's value after: got %s and %s, want 183.15 EUR (333 at 0.55) and 366.85 EUR (667 at 0.55)", c, v)
	}
}

// TestGoodsIssueRefusals: a delivery with nothing picked, one of an order
// blocked for credit since, one whose valuation class names no cost of
// goods sold account, one whose stock has gone since its picking and one
// of a material blocked since are refused, and nothing is posted.
func TestGoodsIssueRefusals(t *testing.T) {
	e := setup(t)
	order := e.order(t, false, e.ofMaterial(e.bracket, "5 BOX"))
	d, tag := e.Must(201, "POST", deliveries, e.create(order, "1 1 BOX"), "")
	id := d["id"].(string)
	e.Problem(409, "shipping.nothing_picked", "POST", deliveries+"/"+id+":postGoodsIssue", "", tag)
	_, tag = e.Must(200, "POST", deliveries+"/"+id+":pick", pick("1 1 BOX"), tag)
	e.Problem(412, "request.version_mismatch", "POST", deliveries+"/"+id+":postGoodsIssue", "", `"1"`)
	pgtest.Exec(t, `DO $$ BEGIN SET LOCAL session_replication_role = replica;
		UPDATE sales.sales_order SET credit_block = 'blocked' WHERE id = '`+order+`'; END $$`)
	e.Problem(409, "sales.order_credit_blocked", "POST", deliveries+"/"+id+":postGoodsIssue", "", tag)

	bolt := e.material(t, "Hex bolt M10", "raw_material")
	e.value(t, bolt, "3100", "moving_average", "1.00")
	e.receiveOf(t, bolt, "10")
	other := e.order(t, false, e.ofMaterial(bolt, "10 EA"))
	b, btag := e.Must(201, "POST", deliveries, e.create(other), "")
	_, btag = e.Must(200, "POST", deliveries+"/"+b["id"].(string)+":pick", pick("1 10 EA"), btag)
	e.Problem(422, "inventory.no_inventory_account", "POST", deliveries+"/"+b["id"].(string)+":postGoodsIssue", "", btag)

	third := e.order(t, false, e.ofMaterial(e.bracket, "10 BOX"))
	c, ctag := e.Must(201, "POST", deliveries, e.create(third), "")
	_, ctag = e.Must(200, "POST", deliveries+"/"+c["id"].(string)+":pick", pick("1 10 BOX"), ctag)
	e.Must(201, "POST", "/v1/material-documents", mustJSON(t, map[string]any{"movement_type": "201", "items": []any{map[string]any{"material_id": e.bracket,
		"plant_id": e.p1, "storage_location_id": e.s1, "quantity": map[string]any{"value": "1", "unit": "EA"}, "cost_center_id": e.CostCenterFrom(t, e.DE, "C1", "2026-01-01", nil)}}}), "")
	e.Problem(409, "inventory.insufficient_stock", "POST", deliveries+"/"+c["id"].(string)+":postGoodsIssue", "", ctag)
	pgtest.Exec(t, `UPDATE materials.material SET status = 'blocked', version = version + 1 WHERE id = $1`, e.bracket)
	e.Problem(422, "inventory.material_not_usable", "POST", deliveries+"/"+c["id"].(string)+":postGoodsIssue", "", ctag)
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM inventory.material_document WHERE tenant_id = $1 AND movement_type = '601'`, []any{e.Tenant.ID}, &n)
	if n != 0 {
		t.Errorf("goods issues posted: got %d, want none", n)
	}
}
