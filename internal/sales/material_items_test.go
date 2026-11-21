package sales_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/sales"
)

// material inserts a material of the tenant in the status with its
// number, of type non_stock (so its delivery is confirmed by hand, not
// shipped, ADR-0083), the description "Machined bracket M10", the base unit EA and
// the alternative units BOX (20 EA) and KG (a third of an EA), and
// returns its id.
func (e env) material(t *testing.T, number, status string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO materials.material (tenant_id, id, number, legal_entity_id, description, base_uom_code, material_type, material_group,
		status, created_by, updated_by) VALUES ($1, $2, $3, $4, 'Machined bracket M10', 'EA', 'non_stock', 'BRACKETS', $5, $6, $6)`,
		e.Tenant.ID, id, number, e.DE, status, e.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO materials.unit_conversion (tenant_id, material_id, uom_code, numerator, denominator, created_by)
		VALUES ($1, $2, 'BOX', 20, 1, $3), ($1, $2, 'KG', 1, 3, $3)`, e.Tenant.ID, id, e.Tenant.Human.ID)
	return id
}

// ofMaterial is the JSON of an item of the material without a price, a
// description or a key: quantity "value unit", more fields led by a
// comma.
func (e env) ofMaterial(material uuid.UUID, quantity, extra string) string {
	var value, unit string
	fmt.Sscan(quantity, &value, &unit) //nolint:errcheck // the tests' own literal
	return fmt.Sprintf(`{"material_id": %q, "quantity": {"value": %q, "unit": %q}, "account_id": %q%s}`, material, value, unit, e.revenue, extra)
}

// TestMaterialItems: an item names a material instead of a free-text key
// (S1f1): its key is the material's number, by which the price records
// price it, its description the material's unless it gives one, its
// quantity in one of the material's units; the order from the quotation
// and the order's billing document name the material, and an order
// change keeps it.
func TestMaterialItems(t *testing.T) {
	e := setup(t)
	bracket := e.material(t, "100000001", "active")
	e.priced(t, rec{key: "100000001", unit: "BOX", amount: "40.00"})
	q := e.accepted(t, e.customer, e.ofMaterial(bracket, "2 BOX", ""), e.ofMaterial(bracket, "3 EA", `, "description": "Bracket, loose", "unit_price": {"amount": "2.00", "currency": "EUR"}`))
	got, _ := e.Must(200, "GET", "/v1/quotations/"+q, "", "")
	want := []map[string]any{
		{"material_id": bracket.String(), "item_key": "100000001", "description": "Machined bracket M10", "price_source": "price_list"},
		{"material_id": bracket.String(), "item_key": "100000001", "description": "Bracket, loose", "price_source": "manual"},
	}
	for i, it := range items(t, got) {
		fields(t, it, want[i])
	}
	if a := amount(t, items(t, got)[0], "unit_price"); a != "40.00 EUR" {
		t.Errorf("the price of 2 BOX: got %s, want 40.00 EUR, the price recorded for the material's number", a)
	}
	order, _ := e.Must(201, "POST", "/v1/sales-orders", fromQuotation(q), "")
	for i, it := range items(t, order) {
		fields(t, it, want[i])
	}

	id, today := e.deliveredOrder(t, []string{e.ofMaterial(bracket, "2 BOX", "")}, "1 2 BOX")
	e.Must(200, "POST", changing(id), changeBody("", itemChange("1 3 BOX", "")), `"3"`)
	changed, _ := e.Must(200, "GET", "/v1/sales-orders/"+id, "", "")
	fields(t, items(t, changed)[0], map[string]any{"material_id": bracket.String(), "item_key": "100000001"})
	doc, _ := e.Must(201, "POST", billings, bill(id, fmt.Sprintf(`, "billing_date": %q`, today)), "")
	fields(t, items(t, doc)[0], map[string]any{"material_id": bracket.String(), "item_key": "100000001", "description": "Machined bracket M10"})

	free, _ := e.Must(201, "POST", "/v1/quotations", e.body(e.customer, far, e.item(line{key: "100000001"})), "")
	if m, ok := items(t, free)[0]["material_id"]; ok {
		t.Errorf("an item with a key alone: got material %v, want none (a key is free text unless the item names a material)", m)
	}
}

// TestMaterialItemRefusals: an item of a material that is unknown, not
// active, managed in batches or by serial number (on a quotation and on
// an order), keyed otherwise or in a unit that is none of its own or does
// not convert exactly is refused naming its field, and nothing is
// created; an item without a material still needs its description.
func TestMaterialItemRefusals(t *testing.T) {
	e := setup(t)
	bracket := e.material(t, "100000001", "active")
	blocked, pending := e.material(t, "100000002", "blocked"), e.material(t, "100000003", "pending")
	batched := e.material(t, "100000004", "active")
	pgtest.Unguarded(t, `UPDATE materials.material SET material_type = 'raw_material', batch_managed = true WHERE id = $1`, batched) // fixed once created
	serialized := e.material(t, "100000005", "active")
	pgtest.Unguarded(t, `UPDATE materials.material SET material_type = 'raw_material', serial_managed = true WHERE id = $1`, serialized) // fixed once created
	priced := `, "unit_price": {"amount": "1.00", "currency": "EUR"}`
	for _, c := range []struct{ name, item, code, field string }{
		{"an unknown material", e.ofMaterial(uuid.Must(uuid.NewV7()), "1 EA", priced), "request.reference_not_found", "items[0].material_id"},
		{"a blocked material", e.ofMaterial(blocked, "1 EA", priced), "sales.material_not_usable", "items[0].material_id"},
		{"a pending material", e.ofMaterial(pending, "1 EA", priced), "sales.material_not_usable", "items[0].material_id"},
		{"a batch-managed material", e.ofMaterial(batched, "1 EA", priced), "sales.material_not_usable", "items[0].material_id"},
		{"a serial-managed material", e.ofMaterial(serialized, "1 EA", priced), "sales.material_not_usable", "items[0].material_id"},
		{"another key", e.ofMaterial(bracket, "1 EA", priced+`, "item_key": "BRACKET-M8"`), "sales.item_key_not_material", "items[0].item_key"},
		{"a unit not the material's", e.ofMaterial(bracket, "1 M", priced), "sales.invalid_material_quantity", "items[0].quantity.unit"},
		{"a quantity not exact in the base unit", e.ofMaterial(bracket, "1 KG", priced), "sales.invalid_material_quantity", "items[0].quantity.unit"},
		{"a quantity of no value", e.ofMaterial(bracket, "0 EA", priced), "sales.invalid_quantity", "items[0].quantity.value"},
		{"no material and no description", fmt.Sprintf(`{"quantity": {"value": "1", "unit": "EA"}, "account_id": %q%s}`, e.revenue, priced),
			"request.validation_failed", "items[0].description"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := e.Problem(422, c.code, "POST", "/v1/quotations", e.body(e.customer, far, c.item), "")
			if f := field(m); f != c.field {
				t.Errorf("the field: got %v, want %s", f, c.field)
			}
		})
	}
	t.Run("a serial-managed material on an order", func(t *testing.T) {
		m := e.Problem(422, "sales.material_not_usable", "POST", "/v1/sales-orders", e.direct(e.customer, "", e.ofMaterial(serialized, "1 EA", priced)), "")
		if f := field(m); f != "items[0].material_id" {
			t.Errorf("the field: got %v, want items[0].material_id", f)
		}
	})
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM sales.quotation WHERE tenant_id = $1`, []any{e.Tenant.ID}, &n)
	if n != 0 {
		t.Errorf("quotations: got %d, want none", n)
	}
	pgtest.Scan(t, `SELECT count(*) FROM sales.sales_order WHERE tenant_id = $1`, []any{e.Tenant.ID}, &n)
	if n != 0 {
		t.Errorf("sales orders: got %d, want none", n)
	}
}

// TestMaterialItemKept: an item an order change keeps keeps its
// material, blocked since, and its new quantity must still convert
// exactly (1 KG is a third of an EA); an item the change adds is of an active material, its refusal
// naming new_items[i].
func TestMaterialItemKept(t *testing.T) {
	e := setup(t)
	bracket := e.material(t, "100000001", "active")
	e.priced(t, rec{key: "100000001", amount: "2.00"})
	id, _ := e.confirmed(t, e.ofMaterial(bracket, "6 EA", ""), e.ofMaterial(bracket, "3 KG", `, "unit_price": {"amount": "5.00", "currency": "EUR"}`))
	pgtest.Exec(t, `UPDATE materials.material SET status = 'blocked', version = version + 1 WHERE id = $1`, bracket)
	e.Must(200, "POST", changing(id), changeBody("", itemChange("1 9 EA", "")), e.version(t, id))
	inexact := e.Problem(422, "sales.invalid_material_quantity", "POST", changing(id), changeBody("", itemChange("2 1 KG", "")), e.version(t, id))
	if f := field(inexact); f != "items[0].quantity.unit" {
		t.Errorf("the field: got %v, want items[0].quantity.unit", f)
	}
	m := e.Problem(422, "sales.material_not_usable", "POST", changing(id),
		fmt.Sprintf(`{"new_items": [%s]}`, e.ofMaterial(bracket, "1 EA", "")), e.version(t, id))
	if f := field(m); f != "new_items[0].material_id" {
		t.Errorf("the field: got %v, want new_items[0].material_id", f)
	}
}

// TestMaterialItemsRead: a failed read of the items' materials fails the
// quotation, and the order change, that reads them.
func TestMaterialItemsRead(t *testing.T) {
	e := setup(t)
	bracket := e.material(t, "100000001", "active")
	s := endpoints(t, time.Now())
	s.Materials = failingMaterials{}
	key := "100000001"
	err := e.inRequest(t, func(ctx context.Context) error {
		_, err := s.MakeQuotation(ctx, sales.QuotationInput{LegalEntityID: e.DE, CustomerID: e.customer, Currency: "EUR", ValidUntil: instant(t, far+"T00:00:00Z"),
			Items: []sales.ItemInput{{ItemKey: &key, MaterialID: &bracket, Quantity: "1", Unit: "EA", AccountID: e.revenue}}})
		return err
	})
	if !errors.Is(err, errMaterials) {
		t.Errorf("a quotation: got %v, want the materials' read error", err)
	}
	got, _ := e.Must(201, "POST", "/v1/sales-orders", e.direct(e.customer, "", e.ofMaterial(bracket, "1 EA", `, "unit_price": {"amount": "2.00", "currency": "EUR"}`)), "")
	order := uuid.MustParse(got["id"].(string))
	nine := "9"
	err = e.inRequest(t, func(ctx context.Context) error {
		_, err := s.ChangeOrder(ctx, order, func(int32) error { return nil }, sales.OrderChangeInput{Items: []sales.ItemChangeInput{{ItemNo: 1, Quantity: &nine}}})
		return err
	})
	if !errors.Is(err, errMaterials) {
		t.Errorf("an order change: got %v, want the materials' read error", err)
	}
}

var errMaterials = errors.New("materials unreachable")

// failingMaterials fails every read of materials.
type failingMaterials struct{}

func (failingMaterials) Materials(context.Context, []uuid.UUID) (map[uuid.UUID]materials.Material, error) {
	return nil, errMaterials
}
