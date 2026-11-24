package shipping_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/sales"
)

// shipped is a delivery of the order taking the item's quantity, picked
// and goods-issued: its representation and ETag.
func (e env) shipped(t *testing.T, order, item string) (map[string]any, string) {
	t.Helper()
	d, tag := e.Must(201, "POST", deliveries, e.create(order, item), "")
	f := strings.Fields(item)
	_, tag = e.Must(200, "POST", deliveries+"/"+d["id"].(string)+":pick", pick("1 "+f[1]+" "+f[2]), tag)
	return e.issued(t, d["id"].(string), tag)
}

// reverse reverses the delivery's goods issue at its version.
func (e env) reverse(t *testing.T, id, tag string) (map[string]any, string) {
	t.Helper()
	return e.Must(200, "POST", deliveries+"/"+id+":reverseGoodsIssue", "", tag)
}

// valuationOf is the material's valuation at P1 as "quantity value".
func (e env) valuationOf(t *testing.T, material string) string {
	t.Helper()
	page, _ := e.Must(200, "GET", "/v1/material-valuations?material_id="+material, "", "")
	v := rows(t, page, "items")[0]
	return fmt.Sprintf("%v %s", v["quantity"].(map[string]any)["value"], money(v["value"]))
}

// entryLines are the journal entry's lines as sorted "account amount
// currency".
func (e env) entryLines(t *testing.T, id string) []string {
	t.Helper()
	entry, _ := e.Must(200, "GET", "/v1/journal-entries/"+id, "", "")
	var lines []string
	for _, l := range rows(t, entry, "lines") {
		lines = append(lines, fmt.Sprintf("%v %s", l["account_id"], money(l["amount"])))
	}
	slices.Sort(lines)
	return lines
}

// TestGoodsIssueReversal (S2b3; ADR-0083 As built S2b3): the goods issue
// of a delivery not billed is reversed by a 602 naming its 601 and the
// delivery, giving the stock back at the value it took, its WA entry
// taking the cost of goods sold back; the order's item is no longer
// delivered by it, open again for another delivery; the delivery is
// goods_issue_reversed, naming the reversal, and changes no more. A
// reversal at a stale version is refused, as is one of a delivery whose
// goods issue is not posted; it posts once.
func TestGoodsIssueReversal(t *testing.T) {
	e := setup(t)
	before := e.valuationOf(t, e.bracket)
	order := e.order(t, false, e.ofMaterial(e.bracket, "5 BOX"), e.freeText())
	d, tag := e.shipped(t, order, "1 3 BOX")
	id := d["id"].(string)
	e.Problem(412, "request.version_mismatch", "POST", deliveries+"/"+id+":reverseGoodsIssue", "", `"1"`)
	got, gotTag := e.reverse(t, id, tag)
	if got["status"] != "goods_issue_reversed" || got["goods_issue_reversal_date"] != got["goods_issue_date"] ||
		got["material_document_id"] != d["material_document_id"] || money(got["cost_of_goods_sold"]) != "120.00 EUR" {
		t.Errorf("the delivery: got %v on %v, %v, %v; want goods_issue_reversed on its goods issue's day, the 601 and its cost kept", got["status"],
			got["goods_issue_reversal_date"], got["material_document_id"], got["cost_of_goods_sold"])
	}
	doc, _ := e.Must(200, "GET", "/v1/material-documents/"+got["reversal_material_document_id"].(string), "", "")
	if doc["movement_type"] != "602" || doc["reversal_of_id"] != d["material_document_id"] || doc["reference"] != d["number"] ||
		doc["number"] != got["reversal_material_document_number"] {
		t.Errorf("the reversal: got %v of %v referencing %v, number %v; want a 602 of the 601 referencing %v", doc["movement_type"], doc["reversal_of_id"],
			doc["reference"], doc["number"], d["number"])
	}
	issue, issueTag := e.Must(200, "GET", "/v1/material-documents/"+d["material_document_id"].(string), "", "")
	cogs, stock := e.Account(t, "2000", "519000"), e.Account(t, "2000", "150000")
	if got, want := e.entryLines(t, doc["journal_entry_id"].(string)), sorted(fmt.Sprintf("%s -120.00 EUR", cogs), fmt.Sprintf("%s 120.00 EUR", stock)); !slices.Equal(got, want) {
		t.Errorf("the reversal's entry: got %v, want %v (the cost of goods sold credited, the stock debited)", got, want)
	}
	if issue["reversed_by_id"] != doc["id"] {
		t.Errorf("the goods issue's reversal: got %v, want %v", issue["reversed_by_id"], doc["id"])
	}
	if v := e.valuationOf(t, e.bracket); v != before {
		t.Errorf("the bracket's valuation: got %s, want %s as before the goods issue", v, before)
	}
	o, _ := e.Must(200, "GET", "/v1/sales-orders/"+order, "", "")
	if q := rows(t, o, "items")[0]["delivered_quantity"].(map[string]any); q["value"] != "0" {
		t.Errorf("the order's item delivered: got %v, want 0", q)
	}
	sd, _ := e.Must(200, "GET", "/v1/sales-orders/"+order+"/deliveries", "", "")
	first := rows(t, sd, "items")[0]
	if first["reversal_date"] != got["goods_issue_reversal_date"] || first["reversal_material_document_id"] != doc["id"] ||
		first["reversal_material_document_number"] != doc["number"] {
		t.Errorf("the order's delivery: got %v, want it reversed by %v", first, doc["number"])
	}
	err := e.inRequest(t, func(ctx context.Context) error {
		o, _, err := sales.Orders{}.Order(ctx, uuid.MustParse(order))
		var got []string
		for _, d := range o.Items[0].Deliveries {
			got = append(got, d.Date.Format(time.DateOnly)+" "+d.Quantity)
		}
		day := fmt.Sprint(first["delivery_date"])
		if want := []string{day + " 3", day + " -3"}; !slices.Equal(got, want) {
			t.Errorf("the order item's deliveries as revenue recognition reads them: got %v, want %v", got, want)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	again, _ := e.Must(201, "POST", deliveries, e.create(order), "")
	if items(t, again) != "1: 5 BOX, picked 0" {
		t.Errorf("what is open again: got %s, want the 5 BOX", items(t, again))
	}

	e.Problem(409, "shipping.goods_issue_reversed", "POST", deliveries+"/"+id+":reverseGoodsIssue", "", gotTag)
	e.Problem(409, "shipping.goods_issue_reversed", "POST", deliveries+"/"+id+":postGoodsIssue", "", gotTag)
	e.Problem(409, "shipping.goods_issue_reversed", "POST", deliveries+"/"+id+":pick", pick("1 1 BOX"), gotTag)
	e.Problem(409, "shipping.goods_issue_not_posted", "POST", deliveries+"/"+again["id"].(string)+":reverseGoodsIssue", "", `"1"`)
	e.Problem(409, "inventory.already_reversed", "POST", "/v1/material-documents/"+d["material_document_id"].(string)+":reverse", "", issueTag)
	list, _ := e.Must(200, "GET", deliveries+"?status=goods_issue_reversed", "", "")
	if ids := rows(t, list, "items"); len(ids) != 1 || ids[0]["id"] != id {
		t.Errorf("the reversed deliveries: got %v, want %s", ids, id)
	}

	ev := e.events(t, id)
	if last := ev[len(ev)-1]; last["status"] != "goods_issue_reversed" || last["goods_issue_reversal"].(map[string]any)["material_document_number"] != doc["number"] {
		t.Errorf("the last event: got %v, want goods_issue_reversed naming the 602", last)
	}
	var reversed []map[string]any
	for _, p := range apitest.Published(t, e.Tenant, outbox.Event{Context: "sales", Name: "sales_order_delivery_reversed"}) {
		if p["outbound_delivery_id"] == id {
			reversed = append(reversed, p)
		}
	}
	if len(reversed) != 1 || reversed[0]["reversal_material_document_number"] != doc["number"] ||
		rows(t, reversed[0], "items")[0]["delivered_quantity"].(map[string]any)["value"] != "0" || money(rows(t, reversed[0], "items")[0]["cost"]) != "120.00 EUR" {
		t.Errorf("SalesOrderDeliveryReversed: got %v, want one naming the 602, 120.00 EUR given back and nothing delivered after", reversed)
	}
	if a := e.audits(t, id, "shipping.outbound_delivery.reverse_goods_issue"); len(a) != 1 || a[0]["reversal_material_document_number"] != doc["number"] {
		t.Errorf("audits: got %v, want one naming the 602", a)
	}
}

// TestGoodsIssueReversalBilled (S2b3): a delivery whose quantity is
// billed is refused, and nothing is posted; once the billing document is
// cancelled, it is reversed. A delivery of an order billed for less than
// stays delivered without it is reversed with the billing standing.
func TestGoodsIssueReversalBilled(t *testing.T) {
	e := setup(t)
	order := e.order(t, false, e.ofMaterial(e.bracket, "5 BOX"))
	d, tag := e.shipped(t, order, "1 2 BOX")
	bill, billTag := e.Must(201, "POST", "/v1/billing-documents", fmt.Sprintf(`{"sales_order_id": %q}`, order), "")
	m := e.Problem(409, "sales.delivery_billed", "POST", deliveries+"/"+d["id"].(string)+":reverseGoodsIssue", "", tag)
	if !strings.Contains(fmt.Sprint(m["detail"]), d["number"].(string)) {
		t.Errorf("the refusal: got %v, want it naming %v", m["detail"], d["number"])
	}
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM inventory.material_document WHERE tenant_id = $1 AND movement_type = '602'`, []any{e.Tenant.ID}, &n)
	if n != 0 {
		t.Errorf("reversals posted: got %d, want none", n)
	}
	if _, after := e.Must(200, "GET", deliveries+"/"+d["id"].(string), "", ""); after != tag {
		t.Errorf("the delivery's version after the refusal: got %s, want %s", after, tag)
	}
	second, _ := e.shipped(t, order, "1 3 BOX")
	e.Must(200, "POST", "/v1/billing-documents/"+bill["id"].(string)+":cancel", `{"reason": "Billed before the goods issue was reversed"}`, billTag)
	if got, _ := e.reverse(t, d["id"].(string), tag); got["status"] != "goods_issue_reversed" {
		t.Errorf("after the billing's cancellation: got %v, want goods_issue_reversed", got["status"])
	}
	e.Must(201, "POST", "/v1/billing-documents", fmt.Sprintf(`{"sales_order_id": %q}`, order), "")
	third, thirdTag := e.shipped(t, e.order(t, false, e.ofMaterial(e.bracket, "1 BOX")), "1 1 BOX")
	if got, _ := e.reverse(t, third["id"].(string), thirdTag); got["status"] != "goods_issue_reversed" {
		t.Errorf("a delivery of another order: got %v, want goods_issue_reversed", got["status"])
	}
	o, _ := e.Must(200, "GET", "/v1/sales-orders/"+order, "", "")
	if it := rows(t, o, "items")[0]; it["delivered_quantity"].(map[string]any)["value"] != "3" || it["billed_quantity"].(map[string]any)["value"] != "3" {
		t.Errorf("the order's item: got %v delivered, %v billed; want the second delivery's 3 billed", it["delivered_quantity"], it["billed_quantity"])
	}
	e.Problem(409, "sales.delivery_billed", "POST", deliveries+"/"+second["id"].(string)+":reverseGoodsIssue", "", fmt.Sprintf("%q", fmt.Sprint(second["version"])))
}

// TestGoodsIssueReversalStandardPrice (S2b3): a standard-priced
// material's reversal gives back what its goods issue took, the cost of
// goods sold, though its price changed since: the stock comes back at
// the new price and the rest is a price difference.
func TestGoodsIssueReversalStandardPrice(t *testing.T) {
	e := setup(t)
	bolt := e.material(t, "Hex bolt M10", "raw_material")
	e.value(t, bolt, "3200", "standard", "0.55")
	e.receiveOf(t, bolt, "1000")
	d, tag := e.shipped(t, e.order(t, false, e.ofMaterial(bolt, "333 EA")), "1 333 EA")
	page, _ := e.Must(200, "GET", "/v1/material-valuations?material_id="+bolt, "", "")
	v := rows(t, page, "items")[0]
	e.Must(200, "POST", "/v1/material-valuations/"+v["id"].(string)+":changePrice", `{"price": {"amount": "0.60", "currency": "EUR"}}`,
		fmt.Sprintf("%q", fmt.Sprint(v["version"])))
	got, _ := e.reverse(t, d["id"].(string), tag)
	doc, _ := e.Must(200, "GET", "/v1/material-documents/"+got["reversal_material_document_id"].(string), "", "")
	cogs, stock, diff := e.Account(t, "2000", "519000"), e.Account(t, "2000", "150000"), e.Account(t, "2000", "231000")
	want := sorted(fmt.Sprintf("%s -16.65 EUR", diff), fmt.Sprintf("%s -183.15 EUR", cogs), fmt.Sprintf("%s 199.80 EUR", stock))
	if got := e.entryLines(t, doc["journal_entry_id"].(string)); !slices.Equal(got, want) {
		t.Errorf("the reversal's entry: got %v, want %v (183.15 back from cost of goods sold, 333 at 0.60 into stock)", got, want)
	}
	if v := e.valuationOf(t, bolt); v != "1000 600.00 EUR" {
		t.Errorf("the bolt's valuation: got %s, want 1000 600.00 EUR", v)
	}
}

// TestGoodsIssueReversalRefusals (S2b3): a reversal while an open count
// document counts the material at the location is refused, as is one of
// a delivery of a company the caller may not act in (not found), and
// nothing is posted.
func TestGoodsIssueReversalRefusals(t *testing.T) {
	e := setup(t)
	d, tag := e.shipped(t, e.order(t, false, e.ofMaterial(e.bracket, "2 BOX")), "1 2 BOX")
	id := d["id"].(string)
	count, _ := e.Must(201, "POST", "/v1/physical-inventory-documents", mustJSON(t, map[string]any{"plant_id": e.p1, "storage_location_id": e.s1,
		"items": []any{map[string]any{"material_id": e.bracket}}}), "")
	m := e.Problem(409, "inventory.posting_blocked", "POST", deliveries+"/"+id+":reverseGoodsIssue", "", tag)
	if !strings.Contains(fmt.Sprint(m["detail"]), fmt.Sprint(count["number"])) {
		t.Errorf("the refusal: got %v, want it naming %v", m["detail"], count["number"])
	}
	reader := e.Reader(t, "Una US", e.US)
	if status, _, _ := e.As(reader).Do("POST", deliveries+"/"+id+":reverseGoodsIssue", "", tag); status != 404 {
		t.Errorf("a delivery of a company the caller may not act in: got %d, want 404", status)
	}
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM inventory.material_document WHERE tenant_id = $1 AND movement_type = '602'`, []any{e.Tenant.ID}, &n)
	if n != 0 {
		t.Errorf("reversals posted: got %d, want none", n)
	}
}

// TestReversalTakesTheAvailabilityLock (S2b3, the leaf src
// review:batch86): until the reversal commits, it holds the availability
// check's lock of each of its materials at the plant, so a check waits
// for it and counts the stock given back with the demand it opens again.
func TestReversalTakesTheAvailabilityLock(t *testing.T) {
	e := setup(t)
	h := endpoints(t)
	bolt := e.material(t, "Hex bolt M10", "raw_material")
	e.value(t, bolt, "3000", "moving_average", "1.00")
	e.receiveOf(t, bolt, "10")
	order := e.order(t, false, e.ofMaterial(e.bracket, "1 BOX"), e.ofMaterial(bolt, "2 EA"))
	d, tag := e.Must(201, "POST", deliveries, e.create(order), "")
	_, tag = e.Must(200, "POST", deliveries+"/"+d["id"].(string)+":pick", pick("1 1 BOX", "2 2 EA"), tag)
	_, tag = e.issued(t, d["id"].(string), tag)
	free := func(material string) bool {
		var ok bool
		pgtest.Scan(t, `SELECT pg_try_advisory_xact_lock(hashtextextended($1, 0))`,
			[]any{fmt.Sprintf("sales.availability:%s:%s:%s", e.Tenant.ID, e.p1, material)}, &ok)
		return ok
	}
	for _, m := range []string{e.bracket, bolt} {
		if !free(m) {
			t.Fatalf("the lock of %s before the reversal: got taken, want free", m)
		}
	}
	err := e.inRequest(t, func(ctx context.Context) error {
		if _, err := h.ReverseOutboundDeliveryGoodsIssue(ctx, gen.ReverseOutboundDeliveryGoodsIssueParams{ID: uuid.MustParse(d["id"].(string)),
			IfMatch: tag}); err != nil {
			return err
		}
		for _, m := range []string{e.bracket, bolt} {
			if free(m) {
				t.Errorf("the lock of %s during the reversal: got free, want held by the reversal", m)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestReversalOutsideTheContract (S2b3): sales' and inventory's
// published reversals refuse what shipping never asks of them: the
// reversal of a delivery the order does not have, of an order not
// found, and of a material document that is no delivery's goods issue.
// Each fails the request; nothing is written.
func TestReversalOutsideTheContract(t *testing.T) {
	e := setup(t)
	h := endpoints(t)
	order := e.order(t, false, e.ofMaterial(e.bracket, "1 BOX"))
	d, _ := e.shipped(t, order, "1 1 BOX")
	var initial uuid.UUID
	pgtest.Scan(t, `SELECT id FROM inventory.material_document WHERE tenant_id = $1 AND movement_type = '561'`, []any{e.Tenant.ID}, &initial)
	in := salesReversal(uuid.MustParse(order), uuid.New(), e.p1)
	cases := map[string]func(ctx context.Context) error{
		"a delivery the order does not have": func(ctx context.Context) error { _, err := h.Shipments.ReverseIssue(ctx, in); return err },
		"an order not found": func(ctx context.Context) error {
			_, err := h.Shipments.ReverseIssue(ctx, salesReversal(uuid.New(), uuid.MustParse(d["id"].(string)), e.p1))
			return err
		},
		"an initial entry": func(ctx context.Context) error { _, err := h.Stock.ReverseForDelivery(ctx, initial, "OD9"); return err },
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			if err := e.inRequest(t, call); err == nil {
				t.Error("got nil error, want the request failed")
			}
		})
	}
}

// salesReversal is sales' record of a reversal of the outbound delivery
// of the order, from the plant, by a 602 that does not exist.
func salesReversal(order, delivery, plant uuid.UUID) sales.IssueReversal {
	return sales.IssueReversal{OrderID: order, DeliveryID: delivery, DeliveryNumber: "OD9", PlantID: plant, MaterialDocumentID: uuid.New(),
		MaterialDocumentNumber: "MD9", Date: time.Now()}
}

// sorted are the lines, sorted.
func sorted(lines ...string) []string {
	slices.Sort(lines)
	return lines
}
