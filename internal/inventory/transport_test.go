package inventory_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/inventory"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

const transports = "/v1/stock-transport-orders"

var transportPerms = append([]string{"procurement.stock_transport.read", "procurement.stock_transport.write", "procurement.stock_transport.post",
	"inventory.count.write"}, perms...)

// transportItem is an order's item of the material from S1 to S3, with
// fields replaced or added by over.
func (e env) transportItem(material, qty, unit string, over map[string]any) map[string]any {
	it := map[string]any{"material_id": material, "quantity": map[string]any{"value": qty, "unit": unit}, "supplying_storage_location_id": e.s1,
		"receiving_storage_location_id": e.s3}
	for k, v := range over {
		it[k] = v
	}
	return it
}

// transportBody is a request creating an order from P1 to P2 of the items.
func (e env) transportBody(t *testing.T, items ...map[string]any) string {
	t.Helper()
	list := make([]any, len(items))
	for i, it := range items {
		list[i] = it
	}
	return mustJSON(t, map[string]any{"supplying_plant_id": e.p1, "receiving_plant_id": e.p2, "items": list})
}

// transport creates an order from P1 to P2 of the items.
func (e env) transport(t *testing.T, items ...map[string]any) map[string]any {
	t.Helper()
	got, _ := e.Must(201, "POST", transports, e.transportBody(t, items...), "")
	return got
}

// step posts the order's goods issue or receipt at its version.
func (e env) step(t *testing.T, o map[string]any, action string) map[string]any {
	t.Helper()
	_, tag := e.Must(200, "GET", transports+"/"+o["id"].(string), "", "")
	got, _ := e.Must(200, "POST", transports+"/"+o["id"].(string)+":"+action, "", tag)
	return got
}

// transit is the bracket's stock in transit to S3 of P2.
func (e env) transit(t *testing.T) string {
	t.Helper()
	var q string
	pgtest.Scan(t, `SELECT coalesce((SELECT trim_scale(quantity)::text FROM inventory.transit_stock WHERE tenant_id = $1 AND material_id = $2
		AND storage_location_id = $3), 'none')`, []any{e.Tenant.ID, e.bracket, e.s3}, &q)
	return q
}

// inTransit are the lines' in_transit flags.
func inTransit(t *testing.T, d map[string]any) string {
	t.Helper()
	var out []any
	for _, l := range rows(t, d, "lines") {
		out = append(out, l["in_transit"])
	}
	return fmt.Sprint(out)
}

// TestStockTransportOrder: an order from P1 to P2 is created open; its
// goods issue (351) takes the stock out of S1 into transit to S3, which is
// neither location's stock but P2's valuation, its value posted between
// the plants' stock accounts; its goods receipt (101) takes it out of
// transit into S3 and posts nothing; each is a material document naming
// the order, audited and published.
func TestStockTransportOrder(t *testing.T) {
	e := setupWith(t, transportPerms...)
	e.post(t, "561", e.item("10", "EA", nil))
	o := e.transport(t, e.transportItem(e.bracket, "1", "BOX", map[string]any{"quantity": map[string]any{"value": "0.2", "unit": "BOX"}}))
	it := rows(t, o, "items")[0]
	if got := fmt.Sprint(o["status"], " ", o["supplying_plant_code"], ">", o["receiving_plant_code"], " ", it["base_quantity"], it["in_transit_quantity"],
		" ", it["supplying_storage_location_code"], ">", it["receiving_storage_location_code"]); got != "open P1>P2 map[unit:EA value:4] map[unit:EA value:0] S1>S3" {
		t.Errorf("the order: got %s", got)
	}
	if n := o["number"].(string); n[:2] != "TO" {
		t.Errorf("its number: got %s, want one of the TO range", n)
	}
	issued := e.step(t, o, "postGoodsIssue")
	issue, _ := e.Must(200, "GET", docs+"/"+issued["issue_document_id"].(string), "", "")
	if got := fmt.Sprint(issue["movement_type"], " ", issue["reference"] == o["number"], " ", e.lines(t, issue), " ", inTransit(t, issue)); got !=
		"351 true P1:S1 -4 EA (0.2 BOX); P2:S3 4 EA (0.2 BOX) [false true]" {
		t.Errorf("the goods issue: got %s", got)
	}
	if got, want := fmt.Sprint(issued["status"], " ", rows(t, issued, "items")[0]["in_transit_quantity"]), "in_transit map[unit:EA value:4]"; got != want {
		t.Errorf("the order in transit: got %s, want %s", got, want)
	}
	if got := e.stock(t, e.s1, "") + "|" + e.stock(t, e.s3, "") + "|" + e.transit(t); got != "6 EA||4" {
		t.Errorf("stock at S1, at S3 and in transit: got %s, want 6 EA||4", got)
	}
	if got := e.valuation(t, e.bracket, e.p1) + "|" + e.valuation(t, e.bracket, e.p2); got != "6 EA 12.00 2.00/1|4 EA 8.00 2.00/1" {
		t.Errorf("valuations: got %s", got)
	}
	if got := e.entry(t, issue); got != "WA 150000:-800 150000:800" {
		t.Errorf("the issue's entry: got %s", got)
	}
	if got := fmt.Sprint(rows(t, e.events(t, issue["id"].(string))[0], "lines")[1]["in_transit"]); got != "true" {
		t.Errorf("the event's line in transit: got %s, want true", got)
	}
	received := e.step(t, issued, "postGoodsReceipt")
	receipt, _ := e.Must(200, "GET", docs+"/"+received["receipt_document_id"].(string), "", "")
	if got := fmt.Sprint(receipt["movement_type"], " ", receipt["movement_type_name"], " ", receipt["reference"] == o["number"], " ", e.lines(t, receipt), " ",
		inTransit(t, receipt), " ", e.entry(t, receipt)); got != "101 Goods receipt for a stock transport order true P2:S3 -4 EA (0.2 BOX); P2:S3 4 EA (0.2 BOX) [true false] none" {
		t.Errorf("the goods receipt: got %s", got)
	}
	if got := fmt.Sprint(received["status"], " ", rows(t, received, "items")[0]["in_transit_quantity"], " ", received["receipt_date"] != nil); got !=
		"received map[unit:EA value:0] true" {
		t.Errorf("the order received: got %s", got)
	}
	if got := e.stock(t, e.s3, "") + "|" + e.transit(t) + "|" + e.valuation(t, e.bracket, e.p2); got != "4 EA|0|4 EA 8.00 2.00/1" {
		t.Errorf("after the receipt: got %s, want 4 EA|0|4 EA 8.00 2.00/1", got)
	}
	for _, action := range []string{"create", "goods_issue", "goods_receipt"} {
		if n := len(e.audits(t, o["id"].(string), "procurement.stock_transport_order."+action)); n != 1 {
			t.Errorf("audit records of %s: got %d, want 1", action, n)
		}
	}
}

// TestStockTransportAtStandardPrice: stock in transit to a plant valuing
// the material at a standard price is worth its quantity at that price;
// the rest of what the issue took is a price difference.
func TestStockTransportAtStandardPrice(t *testing.T) {
	e := setupWith(t, transportPerms...)
	bolt := e.material(t, "Hex bolt M10", "raw_material", true)
	e.value(t, bolt, e.p1, "moving_average", "2.00")
	e.value(t, bolt, e.p2, "standard", "2.50")
	e.post(t, "561", e.item("10", "EA", map[string]any{"material_id": bolt}))
	issued := e.step(t, e.transport(t, e.transportItem(bolt, "4", "EA", nil)), "postGoodsIssue")
	issue, _ := e.Must(200, "GET", docs+"/"+issued["issue_document_id"].(string), "", "")
	if got := e.entry(t, issue); got != "WA 150000:-800 150000:1000 517000:-200" {
		t.Errorf("the issue's entry: got %s", got)
	}
	e.step(t, issued, "postGoodsReceipt")
	if got := e.valuation(t, bolt, e.p2); got != "4 EA 10.00 2.50/1" {
		t.Errorf("the receiving valuation: got %s", got)
	}
}

// TestStockTransportRefused: an order is checked as its goods issue will
// be and orders a material once; it is issued once, from enough stock,
// and received once it is in transit; neither document is posted by hand
// nor reversed; a count at the receiving location blocks the receipt, not
// the issue.
func TestStockTransportRefused(t *testing.T) {
	e := setupWith(t, transportPerms...)
	e.post(t, "561", e.item("10", "EA", nil))
	for _, c := range []struct {
		name, code, body string
	}{
		{"the same plant", "inventory.same_location", mustJSON(t, map[string]any{"supplying_plant_id": e.p1, "receiving_plant_id": e.p1,
			"items": []any{e.transportItem(e.bracket, "1", "EA", map[string]any{"receiving_storage_location_id": e.s2})}})},
		{"another company's plant", "inventory.plant_not_extended", mustJSON(t, map[string]any{"supplying_plant_id": e.p1, "receiving_plant_id": e.p3,
			"items": []any{e.transportItem(e.bracket, "1", "EA", map[string]any{"receiving_storage_location_id": e.s4})}})},
		{"a material twice", "procurement.stock_transport_material_repeated", e.transportBody(t, e.transportItem(e.bracket, "1", "EA", nil),
			e.transportItem(e.bracket, "2", "EA", nil))},
		{"an inexact quantity", "inventory.inexact_quantity", e.transportBody(t, e.transportItem(e.bracket, "1", "KG", nil))},
	} {
		t.Run(c.name, func(t *testing.T) { e.Problem(422, c.code, "POST", transports, c.body, "") })
	}
	e.Problem(422, "inventory.unknown_movement_type", "POST", docs, body(t, "351", e.item("1", "EA", map[string]any{"to_plant_id": e.p2,
		"to_storage_location_id": e.s3})), "")
	big := e.transport(t, e.transportItem(e.bracket, "11", "EA", nil))
	_, tag := e.Must(200, "GET", transports+"/"+big["id"].(string), "", "")
	e.Problem(409, "inventory.insufficient_stock", "POST", transports+"/"+big["id"].(string)+":postGoodsIssue", "", tag)
	e.Problem(409, "procurement.stock_transport_not_in_transit", "POST", transports+"/"+big["id"].(string)+":postGoodsReceipt", "", tag)
	e.Problem(412, "request.version_mismatch", "POST", transports+"/"+big["id"].(string)+":postGoodsIssue", "", `"9"`)
	e.Must(201, "POST", counts, e.countBody(t, e.p2, e.s3, e.bracket), "")
	o := e.transport(t, e.transportItem(e.bracket, "4", "EA", nil))
	issued := e.step(t, o, "postGoodsIssue")
	_, tag = e.Must(200, "GET", transports+"/"+o["id"].(string), "", "")
	e.Problem(409, "procurement.stock_transport_issued", "POST", transports+"/"+o["id"].(string)+":postGoodsIssue", "", tag)
	e.Problem(409, "inventory.posting_blocked", "POST", transports+"/"+o["id"].(string)+":postGoodsReceipt", "", tag)
	issue, docTag := e.Must(200, "GET", docs+"/"+issued["issue_document_id"].(string), "", "")
	e.Problem(409, "inventory.reverse_stock_transport", "POST", docs+"/"+issue["id"].(string)+":reverse", "", docTag)
	if got := e.transit(t); got != "4" {
		t.Errorf("in transit after the refused receipt: got %s, want 4", got)
	}
}

// TestStockTransportReceiptRefused: the receipt's own checks, which an
// order created through the API cannot reach: an unknown material, an
// inexact quantity, and plants of two companies.
func TestStockTransportReceiptRefused(t *testing.T) {
	e := setupWith(t, transportPerms...)
	h := endpoints(t)
	m, _ := e.Must(201, "POST", "/v1/materials", mustJSON(t, map[string]any{"legal_entity_id": e.DE, "description": "Bracket of two companies",
		"base_unit": "EA", "material_type": "trading_good", "material_group": "BRACKETS", "plants": []any{
			map[string]any{"plant_id": e.p2, "storage_location_ids": []any{e.s3}}, map[string]any{"plant_id": e.p3, "storage_location_ids": []any{e.s4}}}}), "")
	e.Approve(t, m["pending_change_id"].(string))
	us := m["id"].(string)
	item := inventory.TransportItem{MaterialID: uuid.MustParse(e.bracket), ReceivingPlantID: e.p2, ReceivingStorageLocation: e.s3, Quantity: "1", Unit: "EA"}
	for _, c := range []struct {
		name, code string
		items      []inventory.TransportItem
	}{
		{"an unknown material", "inventory.material_invalid", []inventory.TransportItem{{MaterialID: uuid.New(), ReceivingPlantID: e.p2,
			ReceivingStorageLocation: e.s3, Quantity: "1", Unit: "EA"}}},
		{"an inexact quantity", "inventory.inexact_quantity", []inventory.TransportItem{{MaterialID: item.MaterialID, ReceivingPlantID: e.p2,
			ReceivingStorageLocation: e.s3, Quantity: "1", Unit: "KG"}}},
		{"a storage location of another plant", "inventory.storage_location_invalid", []inventory.TransportItem{{MaterialID: item.MaterialID,
			ReceivingPlantID: e.p2, ReceivingStorageLocation: e.s1, Quantity: "1", Unit: "EA"}}},
		{"two companies", "inventory.mixed_companies", []inventory.TransportItem{item, {MaterialID: uuid.MustParse(us), ReceivingPlantID: e.p3,
			ReceivingStorageLocation: e.s4, Quantity: "1", Unit: "EA"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := e.inRequest(t, func(ctx context.Context) error {
				_, err := h.ReceiveForTransport(ctx, "TO1", c.items)
				return err
			})
			if p := problem.From(err); p.Code != c.code {
				t.Errorf("got %v, want %s", err, c.code)
			}
		})
	}
}

// TestStockTransportsInOppositeOrder: orders from P1 to P2 and from P2 to
// P1 issued at once all post, none waiting on another for good.
func TestStockTransportsInOppositeOrder(t *testing.T) {
	e := setupWith(t, transportPerms...)
	e.post(t, "561", e.item("10", "EA", nil), e.item("10", "EA", map[string]any{"plant_id": e.p2, "storage_location_id": e.s3}))
	orders := make([]map[string]any, 10)
	for i := range orders {
		b := e.transportBody(t, e.transportItem(e.bracket, "1", "EA", nil))
		if i%2 == 1 {
			b = mustJSON(t, map[string]any{"supplying_plant_id": e.p2, "receiving_plant_id": e.p1, "items": []any{e.transportItem(e.bracket, "1", "EA",
				map[string]any{"supplying_storage_location_id": e.s3, "receiving_storage_location_id": e.s1})}})
		}
		orders[i], _ = e.Must(201, "POST", transports, b, "")
	}
	var wg sync.WaitGroup
	statuses := make([]int, len(orders))
	for i, o := range orders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i], _, _ = e.Do("POST", transports+"/"+o["id"].(string)+":postGoodsIssue", "", `"1"`)
		}()
	}
	wg.Wait()
	for i, s := range statuses {
		if s != 200 {
			t.Errorf("issue %d: got %d, want 200", i, s)
		}
	}
	if got := e.stock(t, e.s1, "") + " " + e.stock(t, e.s3, ""); got != "5 EA 5 EA" {
		t.Errorf("stock: got %s, want 5 EA at each", got)
	}
}

// TestListStockTransportOrders: the list filters by company, plant and
// status, the latest first.
func TestListStockTransportOrders(t *testing.T) {
	e := setupWith(t, transportPerms...)
	e.post(t, "561", e.item("10", "EA", nil))
	first := e.transport(t, e.transportItem(e.bracket, "1", "EA", nil))
	second := e.step(t, e.transport(t, e.transportItem(e.bracket, "2", "EA", nil)), "postGoodsIssue")
	for _, c := range []struct{ query, want string }{
		{"", fmt.Sprint(second["number"], " ", first["number"])},
		{"?status=open", fmt.Sprint(first["number"])},
		{"?plant_id=" + e.p2.String() + "&legal_entity_id=" + e.DE.String(), fmt.Sprint(second["number"], " ", first["number"])},
		{"?plant_id=" + e.p3.String(), ""},
	} {
		t.Run(c.query, func(t *testing.T) {
			page, _ := e.Must(200, "GET", transports+c.query, "", "")
			var got []any
			for _, o := range rows(t, page, "items") {
				got = append(got, o["number"])
			}
			if s := fmt.Sprint(got); s != "["+c.want+"]" {
				t.Errorf("got %s, want [%s]", s, c.want)
			}
		})
	}
}

// TestStockTransportDatabaseFailures: every database error of the orders'
// operations is answered as an error, none outside a unit of work.
func TestStockTransportDatabaseFailures(t *testing.T) {
	e := setupWith(t, transportPerms...)
	pages, err := pagination.NewCodec([]byte("pppppppppppppppppppppppppppppppp"))
	if err != nil {
		t.Fatal(err)
	}
	h := app.Procurement(app.Config{Pages: pages})
	e.post(t, "561", e.item("10", "EA", nil))
	open := e.transport(t, e.transportItem(e.bracket, "1", "EA", nil))
	issued := e.step(t, e.transport(t, e.transportItem(e.bracket, "1", "EA", nil)), "postGoodsIssue")
	openID, issuedID := uuid.MustParse(open["id"].(string)), uuid.MustParse(issued["id"].(string))
	calls := map[string]func(ctx context.Context) error{
		"create": func(ctx context.Context) error {
			_, err := h.CreateStockTransportOrder(ctx, &gen.StockTransportOrderCreate{SupplyingPlantID: e.p1, ReceivingPlantID: e.p2,
				Items: []gen.StockTransportOrderItemInput{{MaterialID: uuid.MustParse(e.bracket), Quantity: gen.Quantity{Value: "1", Unit: "EA"},
					SupplyingStorageLocationID: e.s1, ReceivingStorageLocationID: e.s3}}}, gen.CreateStockTransportOrderParams{})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetStockTransportOrder(ctx, gen.GetStockTransportOrderParams{ID: openID})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListStockTransportOrders(ctx, gen.ListStockTransportOrdersParams{})
			return err
		},
		"goods issue": func(ctx context.Context) error {
			_, err := h.PostStockTransportGoodsIssue(ctx, gen.PostStockTransportGoodsIssueParams{ID: openID, IfMatch: `"1"`})
			return err
		},
		"goods receipt": func(ctx context.Context) error {
			_, err := h.PostStockTransportGoodsReceipt(ctx, gen.PostStockTransportGoodsReceiptParams{ID: issuedID, IfMatch: `"2"`})
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
	t.Run("page size", func(t *testing.T) {
		err := e.inRequest(t, func(ctx context.Context) error {
			_, err := h.ListStockTransportOrders(ctx, gen.ListStockTransportOrdersParams{PageSize: gen.NewOptInt32(0)})
			return err
		})
		if p := problem.From(err); p.Code != "request.invalid_page_size" {
			t.Errorf("got %v, want request.invalid_page_size", p)
		}
	})
	t.Run("not found", func(t *testing.T) {
		err := e.inRequest(t, func(ctx context.Context) error {
			_, err := h.GetStockTransportOrder(ctx, gen.GetStockTransportOrderParams{ID: uuid.New()})
			return err
		})
		if p := problem.From(err); p.Status != 404 {
			t.Errorf("got %v, want 404", err)
		}
	})
}
