package shipping_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/sales"
)

// Available-to-promise on the sales order (S2j; ADR-0082 As built S2j),
// through the API: the setup's plant P1 holds 200 EA of the bracket, 10
// of its boxes of 20.

// placedAt creates Northwind's order delivered from the plant, of the
// items, and returns its id; it stays a draft.
func (e env) placedAt(t *testing.T, plant uuid.UUID, items ...string) string {
	t.Helper()
	o, _ := e.Must(201, "POST", "/v1/sales-orders", fmt.Sprintf(`{"legal_entity_id": %q, "customer_id": %q, "currency": "EUR", "plant_id": %q,
		"items": [%s]}`, e.DE, e.customer, plant, strings.Join(items, ", ")), "")
	return o["id"].(string)
}

// confirmedAt is placedAt confirmed, with the order as confirmed.
func (e env) confirmedAt(t *testing.T, plant uuid.UUID, items ...string) (string, map[string]any) {
	t.Helper()
	id := e.placedAt(t, plant, items...)
	o, _ := e.Must(200, "POST", "/v1/sales-orders/"+id+":confirm", "", `"1"`)
	return id, o
}

// confirmedOf is what the order's items have confirmed, "-" for one
// without a confirmed quantity, by item.
func confirmedOf(t *testing.T, o map[string]any) string {
	t.Helper()
	var out []string
	for _, it := range rows(t, o, "items") {
		c, ok := it["confirmed_quantity"].(map[string]any)
		if !ok {
			out = append(out, "-")
			continue
		}
		out = append(out, fmt.Sprintf("%v %v", c["value"], c["unit"]))
	}
	return strings.Join(out, ", ")
}

// confirmedNow reads the order and gives what its items have confirmed.
func (e env) confirmedNow(t *testing.T, id string) (string, string) {
	t.Helper()
	o, tag := e.Must(200, "GET", "/v1/sales-orders/"+id, "", "")
	return confirmedOf(t, o), tag
}

// TestConfirmationPromisesWhatIsAvailable: an order with a plant has its
// items of a material kept in stock confirmed as far as the plant has
// them available, its stock less what other confirmed orders promised:
// in full, in part or not at all, and a shortage refuses nothing; a
// service or free-text item, and every item of an order without a plant,
// is not checked. The order names its plant; the audit record says what
// was confirmed; deliveries take at most what is confirmed and ship from
// the order's plant.
func TestConfirmationPromisesWhatIsAvailable(t *testing.T) {
	e := setup(t)
	a, got := e.confirmedAt(t, e.p1, e.ofMaterial(e.bracket, "8 BOX"), e.ofMaterial(e.service, "1 EA"), e.freeText())
	if c := confirmedOf(t, got); c != "8 BOX, -, -" {
		t.Errorf("the first order: got %s, want all 8 BOX of the bracket and nothing checked of the others", c)
	}
	if got["plant_id"] != e.p1.String() || got["plant_code"] != "P1" || got["plant_name"] != "Unit P1" {
		t.Errorf("the plant: got %v %v %v, want P1", got["plant_id"], got["plant_code"], got["plant_name"])
	}
	b, got := e.confirmedAt(t, e.p1, e.ofMaterial(e.bracket, "5 BOX"))
	if c := confirmedOf(t, got); c != "2 BOX" {
		t.Errorf("the second order: got %s, want 2 BOX: 40 EA of 200 left after the first's 160", c)
	}
	c, got := e.confirmedAt(t, e.p1, e.ofMaterial(e.bracket, "3 EA"))
	if q := confirmedOf(t, got); q != "0 EA" {
		t.Errorf("the third order: got %s, want 0 EA: nothing left", q)
	}
	free, freeTag := e.Must(201, "POST", "/v1/sales-orders", fmt.Sprintf(`{"legal_entity_id": %q, "customer_id": %q, "currency": "EUR", "items": [%s]}`,
		e.DE, e.customer, e.ofMaterial(e.bracket, "10 BOX")), "")
	if confirmed, _ := e.Must(200, "POST", "/v1/sales-orders/"+free["id"].(string)+":confirm", "", freeTag); confirmedOf(t, confirmed) != "-" ||
		confirmed["plant_id"] != nil {
		t.Errorf("an order without a plant: got %s, plant %v; want nothing checked", confirmedOf(t, confirmed), confirmed["plant_id"])
	}
	if a := e.audits(t, b, "sales.sales_order.confirm"); len(a) != 1 || fmt.Sprint(a[0]["availability"]) != fmt.Sprintf(
		"map[items:[map[confirmed_before:<nil> confirmed_quantity:2 item_no:1 material_id:%s quantity:5 unit:BOX unvalued:false]] plant_id:%s]", e.bracket, e.p1) {
		t.Errorf("the second order's audit: got %v, want its item's check", a)
	}
	t.Run("a delivery takes at most what is confirmed", func(t *testing.T) {
		p := e.Problem(422, "sales.exceeds_open_quantity", "POST", deliveries, e.create(b, "1 3 BOX"), "")
		if d := fmt.Sprint(p["detail"]); !strings.Contains(d, "has 2 BOX open") || !strings.Contains(d, "2 confirmed as available") {
			t.Errorf("detail: got %q, want the 2 BOX confirmed", d)
		}
		d, _ := e.Must(201, "POST", deliveries, e.create(b), "")
		if got := items(t, d); got != "1: 2 BOX, picked 0" {
			t.Errorf("the delivery: got %s, want the 2 BOX confirmed", got)
		}
		e.Problem(422, "sales.nothing_to_deliver", "POST", deliveries, e.create(c), "")
	})
	t.Run("from another plant", func(t *testing.T) {
		p2 := e.orgUnit(t, e.DE, "plant", "P2", nil)
		s3 := e.orgUnit(t, e.DE, "storage_location", "S3", &p2)
		p := e.Problem(422, "shipping.plant_mismatch", "POST", deliveries, e.createAt(a, p2, s3), "")
		if f := field(p); f != "plant_id" {
			t.Errorf("the field: got %v, want plant_id", f)
		}
	})
}

// TestDeliveriesOfOrdersWithoutAPlantCount: what an outbound delivery
// from the plant holds of an order that names no plant is not available
// to an order that does; its goods issue gives it back and takes it out
// of the stock, so what is available stays.
func TestDeliveriesOfOrdersWithoutAPlantCount(t *testing.T) {
	e := setup(t)
	free := e.order(t, false, e.ofMaterial(e.bracket, "6 BOX"))
	d, tag := e.Must(201, "POST", deliveries, e.create(free), "")
	_, got := e.confirmedAt(t, e.p1, e.ofMaterial(e.bracket, "10 BOX"))
	if c := confirmedOf(t, got); c != "4 BOX" {
		t.Errorf("with 6 BOX in delivery for an order without a plant: got %s, want 4 BOX of the 200 EA", c)
	}
	_, tag = e.Must(200, "POST", deliveries+"/"+d["id"].(string)+":pick", pick("1 6 BOX"), tag)
	e.issued(t, d["id"].(string), tag)
	_, got = e.confirmedAt(t, e.p1, e.ofMaterial(e.bracket, "1 BOX"))
	if c := confirmedOf(t, got); c != "0 BOX" {
		t.Errorf("after its goods issue: got %s, want 0 BOX: 80 EA left, all promised", c)
	}
	t.Run("a delivery that names no plant", func(t *testing.T) {
		order := uuid.MustParse(e.order(t, false, e.ofMaterial(e.bracket, "1 EA")))
		err := e.inRequest(t, func(ctx context.Context) error {
			_, err := salesEndpoints(t).Ship(ctx, sales.ShipInput{OrderID: order, DeliveryID: uuid.New(), DeliveryNumber: "OD9",
				Items: []sales.ItemQuantity{{ItemNo: 1, Quantity: "1", Unit: "EA"}}})
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "names no plant") {
			t.Errorf("got %v, want it refused for naming no plant", err)
		}
	})
	var plants string
	pgtest.Scan(t, `SELECT string_agg(DISTINCT plant_id::text, ',') FROM sales.delivery_assignment WHERE outbound_delivery_id = $1`,
		[]any{d["id"]}, &plants)
	if plants != e.p1.String() {
		t.Errorf("the assignments' plant: got %s, want P1's", plants)
	}
}

// TestOrderPlantRefusals: an order's plant is an active plant of its
// company.
func TestOrderPlantRefusals(t *testing.T) {
	e := setup(t)
	inactive := func() uuid.UUID {
		got, _ := e.Must(201, "POST", "/v1/org-units", mustJSON(t, map[string]any{"legal_entity_id": e.DE, "kind": "plant", "code": "P8", "name": "New"}), "")
		return uuid.MustParse(got["id"].(string))
	}()
	for name, plant := range map[string]uuid.UUID{"another company's": e.p9, "a storage location": e.s1, "none known": uuid.New(), "not active": inactive} {
		t.Run(name, func(t *testing.T) {
			p := e.Problem(422, "sales.plant_invalid", "POST", "/v1/sales-orders", fmt.Sprintf(`{"legal_entity_id": %q, "customer_id": %q,
				"currency": "EUR", "plant_id": %q, "items": [%s]}`, e.DE, e.customer, plant, e.ofMaterial(e.bracket, "1 EA")), "")
			if f := field(p); f != "plant_id" {
				t.Errorf("the field: got %v, want plant_id", f)
			}
		})
	}
}

// TestAvailabilityFollowsStockAndChanges: a goods issue takes from the
// stock what it delivers of a promise, so what is available stays; a
// change of a confirmed order keeps what an item had confirmed up to its
// new quantity and confirms an increase, or an item added, as far as the
// plant has it; a cancelled order promises nothing more; a draft's change
// checks nothing.
func TestAvailabilityFollowsStockAndChanges(t *testing.T) {
	e := setup(t)
	a, _ := e.confirmedAt(t, e.p1, e.ofMaterial(e.bracket, "8 BOX"))
	d, tag := e.Must(201, "POST", deliveries, e.create(a), "")
	_, tag = e.Must(200, "POST", deliveries+"/"+d["id"].(string)+":pick", pick("1 8 BOX"), tag)
	e.issued(t, d["id"].(string), tag)
	b, got := e.confirmedAt(t, e.p1, e.ofMaterial(e.bracket, "3 BOX"))
	if c := confirmedOf(t, got); c != "2 BOX" {
		t.Errorf("after the goods issue: got %s, want 2 BOX: 40 EA left, the first order's promise delivered", c)
	}
	change := func(id, tag, body string) string {
		t.Helper()
		got, _ := e.Must(200, "POST", "/v1/sales-orders/"+id+":change", body, tag)
		return confirmedOf(t, got)
	}
	_, tag = e.confirmedNow(t, b)
	if c := change(b, tag, `{"items": [{"item_no": 1, "quantity": {"value": "1", "unit": "BOX"}}]}`); c != "1 BOX" {
		t.Errorf("lowered to 1 BOX: got %s, want 1 BOX", c)
	}
	e.receive(t, "100")
	_, tag = e.confirmedNow(t, b)
	if c := change(b, tag, `{"items": [{"item_no": 1, "quantity": {"value": "6", "unit": "BOX"}}]}`); c != "6 BOX" {
		t.Errorf("raised to 6 BOX with 140 EA at the plant: got %s, want 6 BOX", c)
	}
	_, tag = e.confirmedNow(t, b)
	if c := change(b, tag, `{"new_items": [`+e.ofMaterial(e.bracket, "2 BOX")+`]}`); c != "6 BOX, 1 BOX" {
		t.Errorf("an item added: got %s, want 6 BOX kept and 1 BOX of the 20 EA left", c)
	}
	if a := e.audits(t, b, "sales.sales_order.change"); len(a) != 3 || a[2]["availability"] == nil {
		t.Errorf("the change's audit: got %v, want the added item's check", a)
	}
	_, tag = e.confirmedNow(t, b)
	if c := change(b, tag, `{"requested_delivery_date": "2099-01-01"}`); c != "6 BOX, 1 BOX" {
		t.Errorf("a change of the date alone: got %s, want what was confirmed", c)
	}
	_, tag = e.confirmedNow(t, b)
	e.Must(200, "POST", "/v1/sales-orders/"+b+":cancel", `{"reason": "Customer withdrew"}`, tag)
	_, got = e.confirmedAt(t, e.p1, e.ofMaterial(e.bracket, "7 BOX"))
	if c := confirmedOf(t, got); c != "7 BOX" {
		t.Errorf("after the cancellation: got %s, want 7 BOX of the 140 EA", c)
	}
	draft := e.placedAt(t, e.p1, e.ofMaterial(e.bracket, "1 BOX"))
	if c := change(draft, `"1"`, `{"items": [{"item_no": 1, "quantity": {"value": "2", "unit": "BOX"}}]}`); c != "-" {
		t.Errorf("a draft changed: got %s, want nothing checked", c)
	}
}

// TestTwoOrdersRaceForTheLastQuantity: two customers' orders of the
// bracket at P1, each for 8 of its 10 boxes, confirmed in two
// transactions at once: the second waits for the first's lock on the
// material at the plant and confirms what the first left, so the last
// quantity is confirmed once (S2j's acceptance; ADR-0008).
func TestTwoOrdersRaceForTheLastQuantity(t *testing.T) {
	e := setup(t)
	other := e.Customer(t, "BP70000002", "Contoso")
	pgtest.Exec(t, `INSERT INTO partner.customer_company (tenant_id, partner_id, legal_entity_id, reconciliation_account_id, payment_term_code, created_by,
		updated_by) VALUES ($1, $2, $3, $4, 'NT30', $5, $5)`, e.Tenant.ID, other, e.DE, e.Account(t, "2000", "120000"), e.Tenant.Human.ID)
	first := e.placedAt(t, e.p1, e.ofMaterial(e.bracket, "8 BOX"))
	o, _ := e.Must(201, "POST", "/v1/sales-orders", fmt.Sprintf(`{"legal_entity_id": %q, "customer_id": %q, "currency": "EUR", "plant_id": %q,
		"items": [%s]}`, e.DE, other, e.p1, e.ofMaterial(e.bracket, "8 BOX")), "")
	second := o["id"].(string)
	h := salesEndpoints(t)
	confirm := func(id string, confirmed *string) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			o, err := h.ConfirmOrder(ctx, uuid.MustParse(id), etag.IfMatch(`"1"`))
			if err == nil {
				*confirmed = *o.Items[0].Confirmed
			}
			return err
		}
	}
	var one, two string
	errA, errB := apitest.Serialized(t, e.Tenant, e.Tenant.Human, confirm(first, &one), confirm(second, &two))
	if errA != nil || errB != nil {
		t.Fatalf("the confirmations: got %v and %v, want both confirmed", errA, errB)
	}
	if one != "8" {
		t.Errorf("the first: got %s BOX, want 8", one)
	}
	if two != "2" {
		t.Errorf("the second: got %s BOX, want the 2 the first left", two)
	}
}

// TestChangeAndConfirmationRace: a change raising a confirmed order's
// quantity and another order's confirmation, of the same material at the
// plant, in two transactions at once: the confirmation waits for the
// change's lock and confirms what the change left.
func TestChangeAndConfirmationRace(t *testing.T) {
	e := setup(t)
	changed, _ := e.confirmedAt(t, e.p1, e.ofMaterial(e.bracket, "2 BOX"))
	_, tag := e.confirmedNow(t, changed)
	other := e.placedAt(t, e.p1, e.ofMaterial(e.bracket, "10 BOX"))
	h := salesEndpoints(t)
	var raised, second string
	errA, errB := apitest.Serialized(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		o, err := h.ChangeOrder(ctx, uuid.MustParse(changed), etag.IfMatch(tag), sales.OrderChangeInput{Items: []sales.ItemChangeInput{{ItemNo: 1,
			Quantity: ptr("8"), Unit: ptr("BOX")}}})
		if err == nil {
			raised = *o.Items[0].Confirmed
		}
		return err
	}, func(ctx context.Context) error {
		o, err := h.ConfirmOrder(ctx, uuid.MustParse(other), etag.IfMatch(`"1"`))
		if err == nil {
			second = *o.Items[0].Confirmed
		}
		return err
	})
	if errA != nil || errB != nil {
		t.Fatalf("got %v and %v, want both done", errA, errB)
	}
	if raised != "8" || second != "2" {
		t.Errorf("confirmed: got the change %s and the other %s BOX, want 8 and the 2 left", raised, second)
	}
}

// TestAvailabilityOfAnOrdersOwnItems: two items of one material in one
// order are confirmed in item order; a change of price alone keeps what
// is confirmed; a removed item's promise is free for the next order.
func TestAvailabilityOfAnOrdersOwnItems(t *testing.T) {
	e := setup(t)
	id, got := e.confirmedAt(t, e.p1, e.ofMaterial(e.bracket, "6 BOX"), e.ofMaterial(e.bracket, "6 BOX"))
	if c := confirmedOf(t, got); c != "6 BOX, 4 BOX" {
		t.Errorf("two items of the bracket: got %s, want 6 BOX and the 4 left", c)
	}
	_, tag := e.confirmedNow(t, id)
	got, _ = e.Must(200, "POST", "/v1/sales-orders/"+id+":change", `{"items": [{"item_no": 2, "unit_price": {"amount": "2.00", "currency": "EUR"}}]}`, tag)
	if c := confirmedOf(t, got); c != "6 BOX, 4 BOX" {
		t.Errorf("a change of price: got %s, want what was confirmed", c)
	}
	_, tag = e.confirmedNow(t, id)
	e.Must(200, "POST", "/v1/sales-orders/"+id+":change", `{"removed_items": [1]}`, tag)
	_, got = e.confirmedAt(t, e.p1, e.ofMaterial(e.bracket, "9 BOX"))
	if c := confirmedOf(t, got); c != "6 BOX" {
		t.Errorf("after the first item's removal: got %s, want the 6 BOX it freed", c)
	}
}

// salesEndpoints are the sales context's endpoints as the API wires them.
func salesEndpoints(t *testing.T) *sales.Endpoints {
	t.Helper()
	pages, err := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	return app.Sales(app.Config{Pages: pages, Now: time.Now})
}

// TestAvailabilityDatabaseFailures calls what the availability check
// adds outside a unit of work and with each of its database statements
// failing in turn: an order with a plant created, confirmed, changed and
// read, and the plant's stock.
func TestAvailabilityDatabaseFailures(t *testing.T) {
	e := setup(t)
	h, stock := salesEndpoints(t), app.Inventory(app.Config{Now: time.Now})
	draft := uuid.MustParse(e.placedAt(t, e.p1, e.ofMaterial(e.bracket, "2 BOX")))
	confirmed, _ := e.confirmedAt(t, e.p1, e.ofMaterial(e.bracket, "2 BOX"), e.ofMaterial(e.bracket, "1 BOX"))
	id := uuid.MustParse(confirmed)
	_, tag := e.confirmedNow(t, confirmed)
	calls := map[string]func(ctx context.Context) error{
		"create with a plant": func(ctx context.Context) error {
			_, err := h.CreateSalesOrder(ctx, &gen.SalesOrderCreate{LegalEntityID: gen.NewOptUUID(e.DE), CustomerID: gen.NewOptUUID(e.customer),
				Currency: gen.NewOptString("EUR"), PlantID: gen.NewOptUUID(e.p1), Items: []gen.SalesItemCreate{{MaterialID: gen.NewOptUUID(uuid.MustParse(e.bracket)),
					Quantity: gen.Quantity{Value: "1", Unit: "EA"}, UnitPrice: gen.NewOptMoney(gen.Money{Amount: "1.00", Currency: "EUR"}), AccountID: e.revenue}}},
				gen.CreateSalesOrderParams{})
			return err
		},
		"confirm with a plant": func(ctx context.Context) error {
			_, err := h.ConfirmSalesOrder(ctx, gen.ConfirmSalesOrderParams{ID: draft, IfMatch: `"1"`})
			return err
		},
		"change with a plant": func(ctx context.Context) error {
			_, err := h.ChangeOrder(ctx, id, etag.IfMatch(tag), sales.OrderChangeInput{Items: []sales.ItemChangeInput{{ItemNo: 1, Quantity: ptr("3"),
				Unit: ptr("BOX")}}})
			return err
		},
		"get with a plant": func(ctx context.Context) error {
			_, err := h.GetSalesOrder(ctx, gen.GetSalesOrderParams{ID: id})
			return err
		},
		"the plant's stock": func(ctx context.Context) error {
			_, err := stock.PlantStock(ctx, e.p1, []uuid.UUID{uuid.MustParse(e.bracket)})
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

func ptr(s string) *string { return &s }
