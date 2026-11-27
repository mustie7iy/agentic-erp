package shipping_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/inventory"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/sales"
)

// TestPick: picking sets an item's picked quantity at the version read,
// replacing what was recorded: part of it is partly picked, all of it
// picked, and back to none not picked; each pick moves the version, is
// audited and published. A stale version is refused.
func TestPick(t *testing.T) {
	e := setup(t)
	order := e.order(t, false, e.ofMaterial(e.bracket, "5 BOX"), e.ofMaterial(e.bracket, "30 KG"))
	d, tag := e.Must(201, "POST", deliveries, e.create(order), "")
	path := deliveries + "/" + d["id"].(string) + ":pick"
	for _, c := range []struct{ pick, status, items string }{
		{"1 3 BOX", "partly_picked", "1: 5 BOX, picked 3; 2: 30 KG, picked 0"},
		{"1 5 BOX", "partly_picked", "1: 5 BOX, picked 5; 2: 30 KG, picked 0"},
		{"2 30 KG", "picked", "1: 5 BOX, picked 5; 2: 30 KG, picked 30"},
		{"1 0 BOX", "partly_picked", "1: 5 BOX, picked 0; 2: 30 KG, picked 30"},
	} {
		var got map[string]any
		got, tag = e.Must(200, "POST", path, pick(c.pick), tag)
		if got["picking_status"] != c.status || items(t, got) != c.items {
			t.Errorf("after picking %s: got %v, %s; want %s, %s", c.pick, got["picking_status"], items(t, got), c.status, c.items)
		}
	}
	e.Must(200, "POST", path, pick("1 0 BOX", "2 0.000 KG"), tag)
	got, _ := e.Must(200, "GET", deliveries+"/"+d["id"].(string), "", "")
	if got["picking_status"] != "not_picked" || got["version"] != float64(6) {
		t.Errorf("all put back: got %v at version %v, want not_picked at 6", got["picking_status"], got["version"])
	}
	e.Problem(412, "request.version_mismatch", "POST", path, pick("1 1 BOX"), `"1"`)
	if ev := e.events(t, d["id"].(string)); len(ev) != 6 || ev[1]["picking_status"] != "partly_picked" {
		t.Errorf("events: got %d, want 6 (created, five picks)", len(ev))
	}
	if a := e.audits(t, d["id"].(string), "shipping.outbound_delivery.pick"); len(a) != 5 || a[0]["picked_items"] != float64(1) {
		t.Errorf("audits: got %v, want five picks", a)
	}
}

// TestPickRefusals: an item named twice, one the delivery lacks, a
// quantity that is not one, in another unit, beyond the delivery quantity,
// beyond what the storage location holds with the delivery's other items
// of the material, and one that is no exact quantity of the base unit
// are refused naming their fields, and nothing changes.
func TestPickRefusals(t *testing.T) {
	e := setup(t)
	order := e.order(t, false, e.ofMaterial(e.bracket, "9 BOX"), e.ofMaterial(e.bracket, "90 KG"))
	d, tag := e.Must(201, "POST", deliveries, e.create(order), "")
	path := deliveries + "/" + d["id"].(string) + ":pick"
	for _, c := range []struct{ name, body, code, field string }{
		{"an item twice", pick("1 1 BOX", "1 2 BOX"), "shipping.item_repeated", "items[1].item_no"},
		{"an item it lacks", pick("3 1 BOX"), "shipping.unknown_item", "items[0].item_no"},
		{"no quantity", pick("1 -1 BOX"), "shipping.invalid_quantity", "items[0].quantity.value"},
		{"another unit", pick("1 20 EA"), "shipping.unit_mismatch", "items[0].quantity.unit"},
		{"beyond the delivery quantity", pick("1 9.5 BOX"), "shipping.exceeds_delivery_quantity", "items[0].quantity.value"},
		{"beyond the stock with the other item", pick("1 9 BOX", "2 90 KG"), "shipping.insufficient_stock", "items[0].quantity.value"},
		{"not exact in the base unit", pick("2 1 KG"), "shipping.inexact_quantity", "items[0].quantity.value"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := e.Problem(422, c.code, "POST", path, c.body, tag)
			if f := field(m); f != c.field {
				t.Errorf("the field: got %v, want %s", f, c.field)
			}
		})
	}
	m := e.Problem(422, "shipping.insufficient_stock", "POST", path, pick("1 9 BOX", "2 90 KG"), tag)
	if d := fmt.Sprint(m["detail"]); !strings.Contains(d, "would pick 210 EA") || !strings.Contains(d, "holds 200 EA") {
		t.Errorf("the refusal: got %q, want it to name 210 EA picked of the 200 EA held", d)
	}
	got, _ := e.Must(200, "GET", deliveries+"/"+d["id"].(string), "", "")
	if got["version"] != float64(1) || got["picking_status"] != "not_picked" {
		t.Errorf("after the refusals: got version %v, %v; want 1, not_picked", got["version"], got["picking_status"])
	}
}

// TestPickNothingHeld: a material the location has never held is not
// picked; and a delivery that does not exist is not found.
func TestPickNothingHeld(t *testing.T) {
	e := setup(t)
	bolt := e.material(t, "Hex bolt M10", "raw_material")
	order := e.order(t, false, e.ofMaterial(bolt, "1 EA"))
	d, tag := e.Must(201, "POST", deliveries, e.create(order), "")
	m := e.Problem(422, "shipping.insufficient_stock", "POST", deliveries+"/"+d["id"].(string)+":pick", pick("1 1 EA"), tag)
	if d := fmt.Sprint(m["detail"]); !strings.Contains(d, "holds 0 EA") {
		t.Errorf("the refusal: got %q, want it to say the location holds 0 EA", d)
	}
	e.Problem(404, "request.not_found", "GET", deliveries+"/"+uuid.NewString(), "", "")
}

// TestCreditBlocked: an order blocked for credit takes no delivery.
func TestCreditBlocked(t *testing.T) {
	e := setup(t)
	order := e.order(t, true, e.ofMaterial(e.bracket, "1 BOX"))
	pgtest.Exec(t, `UPDATE sales.sales_order SET status = 'confirmed', credit_block = 'blocked', version = version + 1 WHERE id = $1`, order)
	e.Problem(409, "sales.order_credit_blocked", "POST", deliveries, e.create(order), "")
}

// TestList: the deliveries of the companies the caller may act in, the
// latest first, by company, number, order and status, in pages; an unknown sort
// field is refused.
func TestList(t *testing.T) {
	e := setup(t)
	first := e.order(t, false, e.ofMaterial(e.bracket, "5 BOX"))
	second := e.order(t, false, e.ofMaterial(e.bracket, "2 BOX"))
	a, _ := e.Must(201, "POST", deliveries, e.create(first, "1 1 BOX"), "")
	b, _ := e.Must(201, "POST", deliveries, e.create(first, "1 1 BOX"), "")
	c, _ := e.Must(201, "POST", deliveries, e.create(second), "")
	ids := func(page map[string]any) string {
		var out []string
		for _, d := range rows(t, page, "items") {
			out = append(out, fmt.Sprint(d["id"]))
		}
		return strings.Join(out, " ")
	}
	for _, q := range []struct{ query, want string }{
		{"", fmt.Sprintf("%v %v %v", c["id"], b["id"], a["id"])},
		{"?sales_order_id=" + first, fmt.Sprintf("%v %v", b["id"], a["id"])},
		{"?legal_entity_id=" + e.US.String(), ""},
		{"?status=goods_issued", ""},
		{"?status=created&order_by=created_at", fmt.Sprintf("%v %v %v", a["id"], b["id"], c["id"])},
		{"?number=" + b["number"].(string), fmt.Sprint(b["id"])},
		{"?number=" + b["number"].(string) + "&legal_entity_id=" + e.DE.String() + "&sales_order_id=" + first, fmt.Sprint(b["id"])},
		{"?number=" + b["number"].(string) + "&sales_order_id=" + second, ""},
		{"?number=OD-NONE", ""},
	} {
		page, _ := e.Must(200, "GET", deliveries+q.query, "", "")
		if got := ids(page); got != q.want {
			t.Errorf("%s: got %s, want %s", q.query, got, q.want)
		}
	}
	page, _ := e.Must(200, "GET", deliveries+"?page_size=2", "", "")
	next, _ := e.Must(200, "GET", deliveries+"?page_size=2&page_token="+page["next_page_token"].(string), "", "")
	if got := ids(page) + " | " + ids(next); got != fmt.Sprintf("%v %v | %v", c["id"], b["id"], a["id"]) {
		t.Errorf("pages: got %s", got)
	}
	e.Problem(400, "request.invalid_order_by", "GET", deliveries+"?order_by=number", "", "")
	reader := e.Reader(t, "Una US", e.US)
	if status, _, _ := e.As(reader).Do("GET", deliveries+"/"+a["id"].(string), "", ""); status != 404 {
		t.Errorf("a delivery of a company the caller may not act in: got %d, want 404", status)
	}
}

// TestDatabaseFailures calls every handler outside a unit of work and
// with each of its database statements failing in turn.
func TestDatabaseFailures(t *testing.T) {
	e := setup(t)
	h := endpoints(t)
	order := e.order(t, false, e.ofMaterial(e.bracket, "5 BOX"))
	o := uuid.MustParse(order)
	d, tag := e.Must(201, "POST", deliveries, e.create(order, "1 1 BOX"), "")
	id := uuid.MustParse(d["id"].(string))
	p, ptag := e.Must(201, "POST", deliveries, e.create(order, "1 2 BOX"), "")
	_, ptag = e.Must(200, "POST", deliveries+"/"+p["id"].(string)+":pick", pick("1 2 BOX"), ptag)
	picked := uuid.MustParse(p["id"].(string))
	shippedOrder := e.order(t, false, e.ofMaterial(e.bracket, "1 BOX"))
	gi, gtag := e.shipped(t, shippedOrder, "1 1 BOX")
	issuedID, issueDoc := uuid.MustParse(gi["id"].(string)), uuid.MustParse(gi["material_document_id"].(string))
	calls := map[string]func(ctx context.Context) error{
		"reverse the goods issue": func(ctx context.Context) error {
			_, err := h.ReverseOutboundDeliveryGoodsIssue(ctx, gen.ReverseOutboundDeliveryGoodsIssueParams{ID: issuedID, IfMatch: gtag})
			return err
		},
		"sales' reverse issue": func(ctx context.Context) error {
			_, err := h.Shipments.ReverseIssue(ctx, salesReversal(uuid.MustParse(shippedOrder), issuedID, e.p1))
			return err
		},
		"inventory's reverse for delivery": func(ctx context.Context) error {
			_, err := h.Stock.ReverseForDelivery(ctx, issueDoc, "OD9")
			return err
		},
		"post the goods issue": func(ctx context.Context) error {
			_, err := h.PostOutboundDeliveryGoodsIssue(ctx, gen.OptOutboundDeliveryGoodsIssue{}, gen.PostOutboundDeliveryGoodsIssueParams{ID: picked, IfMatch: ptag})
			return err
		},
		"create": func(ctx context.Context) error {
			_, err := h.CreateOutboundDelivery(ctx, &gen.OutboundDeliveryCreate{SalesOrderID: o, PlantID: e.p1, StorageLocationID: e.s1},
				gen.CreateOutboundDeliveryParams{})
			return err
		},
		"create with items": func(ctx context.Context) error {
			_, err := h.CreateOutboundDelivery(ctx, &gen.OutboundDeliveryCreate{SalesOrderID: o, PlantID: e.p1, StorageLocationID: e.s1,
				Items: []gen.OutboundDeliveryItemInput{{OrderItemNo: 1, Quantity: gen.Quantity{Value: "1", Unit: "BOX"}}}}, gen.CreateOutboundDeliveryParams{})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetOutboundDelivery(ctx, gen.GetOutboundDeliveryParams{ID: id})
			return err
		},
		"pick": func(ctx context.Context) error {
			_, err := h.PickOutboundDelivery(ctx, &gen.OutboundDeliveryPick{Items: []gen.OutboundDeliveryPickItem{{ItemNo: 1,
				Quantity: gen.Quantity{Value: "1", Unit: "BOX"}}}}, gen.PickOutboundDeliveryParams{ID: id, IfMatch: tag})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListOutboundDeliveries(ctx, gen.ListOutboundDeliveriesParams{})
			return err
		},
		"sales' issue": func(ctx context.Context) error {
			_, err := h.Shipments.Issue(ctx, sales.GoodsIssue{OrderID: o, DeliveryID: picked, DeliveryNumber: "OD9", PlantID: e.p1, MaterialDocumentID: picked,
				MaterialDocumentNumber: "MD9", Date: time.Now().UTC(), Currency: "EUR", Items: []sales.IssuedItem{{ItemNo: 1, Held: "2", Issued: "2", Cost: 1}}})
			return err
		},
		"inventory's issue": func(ctx context.Context) error {
			_, err := h.Stock.IssueForDelivery(ctx, "OD9", []inventory.DeliveryItem{{MaterialID: uuid.MustParse(e.bracket), PlantID: e.p1,
				StorageLocationID: e.s1, Quantity: "1", Unit: "EA"}})
			return err
		},
		"ship": func(ctx context.Context) error {
			_, err := h.Shipments.Ship(ctx, sales.ShipInput{OrderID: o, DeliveryID: id, DeliveryNumber: "OD9", PlantID: e.p1, Open: []int32{1}})
			return err
		},
		"stock held": func(ctx context.Context) error {
			_, err := h.Stock.Held(ctx, uuid.MustParse(e.bracket), e.p1, e.s1)
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
	t.Run("ship without an audit buffer", func(t *testing.T) {
		ctx := context.Background()
		tx, err := pgtest.Pool(t).Begin(ctx, database.Scope{Role: "erp_app", TenantID: e.Tenant.ID})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if err := database.SetActor(ctx, tx, e.Tenant.Human.ID); err != nil {
			t.Fatal(err)
		}
		bare := reqctx.WithPrincipal(reqctx.WithTx(ctx, tx), reqctx.Principal{ID: e.Tenant.Human.ID, Kind: e.Tenant.Human.Kind, TenantID: e.Tenant.ID})
		if _, err := h.Shipments.Ship(bare, sales.ShipInput{OrderID: o, DeliveryID: id, DeliveryNumber: "OD9", PlantID: e.p1, Open: []int32{1}}); !errors.Is(err, audit.ErrNoBuffer) {
			t.Errorf("got %v, want %v", err, audit.ErrNoBuffer)
		}
	})
	t.Run("reverse an issue without an audit buffer", func(t *testing.T) {
		ctx := context.Background()
		tx, err := pgtest.Pool(t).Begin(ctx, database.Scope{Role: "erp_app", TenantID: e.Tenant.ID})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if err := database.SetActor(ctx, tx, e.Tenant.Human.ID); err != nil {
			t.Fatal(err)
		}
		bare := reqctx.WithPrincipal(reqctx.WithTx(ctx, tx), reqctx.Principal{ID: e.Tenant.Human.ID, Kind: e.Tenant.Human.Kind, TenantID: e.Tenant.ID})
		if _, err := h.Shipments.ReverseIssue(bare, salesReversal(uuid.MustParse(shippedOrder), issuedID, e.p1)); !errors.Is(err, audit.ErrNoBuffer) {
			t.Errorf("got %v, want %v", err, audit.ErrNoBuffer)
		}
	})
	t.Run("page size", func(t *testing.T) {
		err := e.inRequest(t, func(ctx context.Context) error {
			_, err := h.ListOutboundDeliveries(ctx, gen.ListOutboundDeliveriesParams{PageSize: gen.NewOptInt32(0)})
			return err
		})
		if p := problem.From(err); p.Code != "request.invalid_page_size" {
			t.Errorf("got %v, want request.invalid_page_size", p)
		}
	})
}
