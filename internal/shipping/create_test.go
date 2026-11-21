package shipping_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/sales"
	"github.com/mustie7iy/agentic-erp/internal/shipping"
)

// TestCreate: a delivery without items takes what is open of every item
// of a material kept in stock at the location, here the bracket's 5 BOX
// and not the free-text or the service item; it is numbered from the OD
// range, created and not picked, names the order, its customer and where
// it ships from, and the order holds the quantity in delivery; it is
// audited, and published. A second delivery without items finds nothing
// open.
func TestCreate(t *testing.T) {
	e := setup(t)
	order := e.order(t, false, e.ofMaterial(e.bracket, "5 BOX"), e.freeText(), e.ofMaterial(e.service, "1 EA"))
	d, tag := e.Must(201, "POST", deliveries, e.create(order), "")
	id := d["id"].(string)
	for k, want := range map[string]any{"status": "created", "picking_status": "not_picked", "sales_order_id": order, "customer_number": "BP70000001",
		"customer_name": "Northwind Traders",
		"plant_code":    "P1", "storage_location_code": "S1", "storage_location_name": "Unit S1", "company_code": "2000", "version": float64(1)} {
		if d[k] != want {
			t.Errorf("%s: got %v, want %v", k, d[k], want)
		}
	}
	if n, _ := d["number"].(string); !strings.HasPrefix(n, "OD") {
		t.Errorf("number: got %v, want one of the OD range", d["number"])
	}
	if tag != `"1"` {
		t.Errorf("ETag: got %s, want \"1\"", tag)
	}
	if got := items(t, d); got != "1: 5 BOX, picked 0" {
		t.Errorf("items: got %s, want the bracket's 5 BOX alone", got)
	}
	if got := rows(t, d, "items")[0]["material_number"]; got == nil || got == "" {
		t.Errorf("the item's material number: got %v, want the bracket's", got)
	}
	if got := e.inDelivery(t, order); got != "5 BOX, 0 EA, 0 EA" {
		t.Errorf("in delivery: got %s, want 5 BOX, 0 EA, 0 EA", got)
	}
	read, _ := e.Must(200, "GET", deliveries+"/"+id, "", "")
	if items(t, read) != items(t, d) || read["number"] != d["number"] {
		t.Errorf("read back: got %v, want %v", read, d)
	}
	if ev := e.events(t, id); len(ev) != 1 || ev[0]["status"] != "created" || ev[0]["customer_number"] != "BP70000001" || ev[0]["plant_code"] != "P1" {
		t.Errorf("events: got %v, want one OutboundDeliveryChanged, created, naming the customer and plant", ev)
	}
	if a := e.audits(t, id, "shipping.outbound_delivery.create"); len(a) != 1 || a[0]["number"] != d["number"] {
		t.Errorf("audits: got %v, want one naming the delivery", a)
	}
	if a := e.audits(t, order, "sales.sales_order.ship"); len(a) != 1 || a[0]["outbound_delivery_number"] != d["number"] {
		t.Errorf("the order's audits: got %v, want one naming the delivery", a)
	}
	m := e.Problem(422, "sales.nothing_to_deliver", "POST", deliveries, e.create(order), "")
	if f := field(m); f != "sales_order_id" {
		t.Errorf("the field: got %v, want sales_order_id", f)
	}
}

// TestCreateItems: a delivery of named items takes their quantities, a
// second takes what is left open, and a third more than is open is
// refused naming what the deliveries hold; the order's item then neither
// falls below that, nor is removed, nor is its order cancelled.
func TestCreateItems(t *testing.T) {
	e := setup(t)
	order := e.order(t, false, e.ofMaterial(e.bracket, "5 BOX"), e.freeText())
	first, _ := e.Must(201, "POST", deliveries, e.create(order, "1 2 BOX"), "")
	second, _ := e.Must(201, "POST", deliveries, e.create(order, "1 3 BOX"), "")
	if got := items(t, first) + " | " + items(t, second); got != "1: 2 BOX, picked 0 | 1: 3 BOX, picked 0" {
		t.Errorf("the deliveries: got %s", got)
	}
	m := e.Problem(422, "sales.exceeds_open_quantity", "POST", deliveries, e.create(order, "1 0.5 BOX"), "")
	if f, d := field(m), fmt.Sprint(m["detail"]); f != "items[0].quantity.value" || !strings.Contains(d, "5 in outbound deliveries") {
		t.Errorf("the refusal: got %v %q, want items[0].quantity.value naming 5 in outbound deliveries", f, d)
	}
	path := "/v1/sales-orders/" + order
	_, tag := e.Must(200, "GET", path, "", "")
	e.Problem(422, "sales.quantity_below_delivered", "POST", path+":change", `{"reason": "Fewer", "items": [{"item_no": 1, "quantity": {"value": "4", "unit": "BOX"}}]}`, tag)
	e.Problem(422, "sales.order_item_delivered", "POST", path+":change", `{"reason": "Gone", "removed_items": [1]}`, tag)
	m = e.Problem(409, "sales.order_delivered", "POST", path+":cancel", `{"reason": "Not needed"}`, tag)
	if d := fmt.Sprint(m["detail"]); !strings.Contains(d, "5 BOX in outbound deliveries") {
		t.Errorf("the cancellation's refusal: got %q, want it to name 5 BOX in outbound deliveries", d)
	}
}

// TestCreateRefusals: an order the caller cannot see, a plant of another
// company, a storage location of another plant, an item without a
// material or of a material kept in no stock, a material not kept at the
// location or blocked, a draft order and a quantity in another unit are
// refused naming their fields, and nothing is created.
func TestCreateRefusals(t *testing.T) {
	e := setup(t)
	order := e.order(t, false, e.ofMaterial(e.bracket, "5 BOX"), e.freeText(), e.ofMaterial(e.service, "1 EA"))
	draft := e.order(t, true, e.ofMaterial(e.bracket, "1 BOX"))
	for _, c := range []struct {
		name, body string
		status     int
		code       string
		field      any
	}{
		{"an unknown order", e.create("01929a1e-4c2b-7a10-9b3e-1f2a3b4c5e01"), 422, "request.reference_not_found", "sales_order_id"},
		{"a plant of another company", e.createAt(order, e.p9, e.s9), 422, "shipping.plant_invalid", "plant_id"},
		{"a storage location of another plant", e.createAt(order, e.p1, e.s9), 422, "shipping.storage_location_invalid", "storage_location_id"},
		{"a free-text item", e.create(order, "2 1 EA"), 422, "shipping.item_not_stocked", "items[0].order_item_no"},
		{"a service material", e.create(order, "3 1 EA"), 422, "shipping.item_not_stocked", "items[0].order_item_no"},
		{"a material not kept at the location", e.createAt(order, e.p1, e.s2, "1 1 BOX"), 422, "shipping.material_not_at_location", "items[0].order_item_no"},
		{"an item the order lacks", e.create(order, "9 1 BOX"), 422, "sales.unknown_order_item", "items[0].order_item_no"},
		{"another unit", e.create(order, "1 20 EA"), 422, "sales.unit_mismatch", "items[0].quantity.unit"},
		{"an item twice", e.create(order, "1 1 BOX", "1 1 BOX"), 422, "sales.item_repeated", "items[1].order_item_no"},
		{"a draft order", e.create(draft), 409, "sales.order_not_confirmed", nil},
		{"nothing kept at the location", e.createAt(order, e.p1, e.s2), 422, "sales.nothing_to_deliver", "sales_order_id"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := e.Problem(c.status, c.code, "POST", deliveries, c.body, "")
			if f := field(m); f != c.field {
				t.Errorf("the field: got %v, want %v", f, c.field)
			}
		})
	}
	pgtest.Exec(t, `UPDATE materials.material SET status = 'blocked', version = version + 1 WHERE id = $1`, e.bracket)
	if f := field(e.Problem(422, "shipping.material_not_usable", "POST", deliveries, e.create(order, "1 1 BOX"), "")); f != "items[0].order_item_no" {
		t.Errorf("a blocked material's field: got %v, want items[0].order_item_no", f)
	}
	// Batch management is fixed once a material is created (material_fixed);
	// the bracket is made batch-managed past its trigger, as a material
	// created so would be.
	pgtest.Unguarded(t, `UPDATE materials.material SET status = 'active', batch_managed = true, version = version + 1 WHERE id = $1`, e.bracket)
	if f := field(e.Problem(422, "shipping.material_not_usable", "POST", deliveries, e.create(order, "1 1 BOX"), "")); f != "items[0].order_item_no" {
		t.Errorf("a batch-managed material's field: got %v, want items[0].order_item_no", f)
	}
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM shipping.outbound_delivery WHERE tenant_id = $1`, []any{e.Tenant.ID}, &n)
	if n != 0 {
		t.Errorf("deliveries: got %d, want none", n)
	}
	if got := e.inDelivery(t, order); got != "0 BOX, 0 EA, 0 EA" {
		t.Errorf("in delivery: got %s, want nothing", got)
	}
}

// staleOrders answers the order as it was before a change, as a read
// without the order's lock may.
type staleOrders struct{ order sales.Order }

func (s staleOrders) Order(context.Context, uuid.UUID) (sales.Order, bool, error) {
	return s.order, true, nil
}

// TestCreateJudgedUnderTheLock: an item an order change adds after the
// delivery's first read of the order is judged on the order as sales
// locked it: a free-text item added since is refused, not written.
func TestCreateJudgedUnderTheLock(t *testing.T) {
	e := setup(t)
	order := e.order(t, false, e.ofMaterial(e.bracket, "5 BOX"))
	id := uuid.MustParse(order)
	var before sales.Order
	if err := e.inRequest(t, func(ctx context.Context) error {
		var err error
		before, _, err = sales.Orders{}.Order(ctx, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, tag := e.Must(200, "GET", "/v1/sales-orders/"+order, "", "")
	e.Must(200, "POST", "/v1/sales-orders/"+order+":change", `{"reason": "Installation added", "new_items": [`+e.freeText()+`]}`, tag)
	h := endpoints(t)
	h.Orders = staleOrders{order: before}
	err := e.inRequest(t, func(ctx context.Context) error {
		_, err := h.Create(ctx, shipping.Input{OrderID: id, PlantID: e.p1, StorageLocationID: e.s1,
			Items: []sales.ItemQuantity{{ItemNo: 2, Quantity: "1", Unit: "EA"}}})
		return err
	})
	if p := problem.From(err); p.Code != "shipping.item_not_stocked" {
		t.Errorf("got %v, want shipping.item_not_stocked", err)
	}
}
