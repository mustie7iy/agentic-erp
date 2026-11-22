package procurement_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
)

// Purchasing items by material and the catalog's keys mapped (S1f2,
// ADR-0082 As built S1f).

// material inserts a material of the tenant in the status with its
// number, the description "Machined bracket M10", the base unit EA and
// the alternative units BOX (20 EA) and KG (a third of an EA), and
// returns its id.
func (e env) material(t *testing.T, number, status string) string {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO materials.material (tenant_id, id, number, legal_entity_id, description, base_uom_code, material_type, material_group,
		status, created_by, updated_by) VALUES ($1, $2, $3, $4, 'Machined bracket M10', 'EA', 'trading_good', 'BRACKETS', $5, $6, $6)`,
		e.Tenant.ID, id, number, e.DE, status, e.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO materials.unit_conversion (tenant_id, material_id, uom_code, numerator, denominator, created_by)
		VALUES ($1, $2, 'BOX', 20, 1, $3), ($1, $2, 'KG', 1, 3, $3)`, e.Tenant.ID, id, e.Tenant.Human.ID)
	return id.String()
}

// ofMaterial is a requisition item of the material: quantity "value
// unit" at 1.00 EUR on the office account, with what extra adds.
func (e env) ofMaterial(material, quantity, extra string) string {
	var value, unit string
	fmt.Sscan(quantity, &value, &unit) //nolint:errcheck // the tests' own literal
	return fmt.Sprintf(`{"material_id": %q, "quantity": {"value": %q, "unit": %q}, "unit_price": {"amount": "1.00", "currency": "EUR"}, "account_id": %q%s}`,
		material, value, unit, e.office, extra)
}

// rfqOfMaterial is an RFQ item of the material: quantity "value unit" on
// the office account, with what extra adds.
func (e env) rfqOfMaterial(material, quantity, extra string) string {
	var value, unit string
	fmt.Sscan(quantity, &value, &unit) //nolint:errcheck // the tests' own literal
	return fmt.Sprintf(`{"material_id": %q, "quantity": {"value": %q, "unit": %q}, "account_id": %q%s}`, material, value, unit, e.office, extra)
}

// fieldOf is the field a problem names first.
func fieldOf(t *testing.T, p map[string]any) any {
	t.Helper()
	errs := apitest.List(t, p, "errors")
	if len(errs) == 0 {
		return nil
	}
	return errs[0].(map[string]any)["field"]
}

// TestMaterialRequisitionItems: a requisition item names a material,
// whose description it takes; a catalog item of a material has its
// number as its key, and a requisition item from it names the material;
// an RFQ item has its material's number as its key, and its award's
// contract item names the material.
func TestMaterialRequisitionItems(t *testing.T) {
	e := setupIn2026(t)
	bracket := e.material(t, "100000001", "active")
	r := e.requisition(t, items(e.ofMaterial(bracket, "2 BOX", ""), e.ofMaterial(bracket, "3 EA", `, "description": "Loose brackets"`)), "")
	for i, want := range []string{"Machined bracket M10", "Loose brackets"} {
		it := apitest.List(t, r, "items")[i].(map[string]any)
		if got := fmt.Sprintf("%v %v %v", it["material_id"], it["item_key"], it["description"]); got != bracket+" 100000001 "+want {
			t.Errorf("requisition item %d: got %s, want %s 100000001 %s", i+1, got, bracket, want)
		}
	}

	c, _ := e.Must(201, "POST", "/v1/catalog-items", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "material_id": %q, "unit": "BOX",
		"unit_price": {"amount": "40.00", "currency": "EUR"}}`, e.DE, e.Supplier, bracket), "")
	if c["material_id"] != bracket || c["item_key"] != "100000001" || c["description"] != "Machined bracket M10" {
		t.Errorf("catalog item: got %v %v %v, want the bracket, its number and description", c["material_id"], c["item_key"], c["description"])
	}
	from := e.requisition(t, items(fmt.Sprintf(`{"catalog_item_id": %q, "quantity": {"value": "1", "unit": "BOX"}, "account_id": %q}`, id(c), e.office)), "")
	if got := firstItem(t, from)["material_id"]; got != bracket {
		t.Errorf("a requisition item from the catalog: got material %v, want %s", got, bracket)
	}
	other := e.material(t, "100000002", "active")
	p := e.Problem(422, "procurement.catalog_item_override", "POST", "/v1/requisitions", fmt.Sprintf(`{"legal_entity_id": %q, "currency": "EUR", "items": [{"catalog_item_id": %q,
		"material_id": %q, "quantity": {"value": "1", "unit": "BOX"}, "account_id": %q}]}`, e.DE, id(c), other, e.office), "")
	if f := fieldOf(t, p); f != "items[0].material_id" {
		t.Errorf("another material than the catalog item's: got field %v, want items[0].material_id", f)
	}

	q, _ := e.Must(201, "POST", "/v1/rfqs", e.rfqBody("2026-07-31", items(e.rfqOfMaterial(bracket, "5 BOX", ""), rfqItem("DESK", "2", "EA", e.office, "")), e.Supplier), "")
	first := apitest.List(t, q, "items")[0].(map[string]any)
	if first["material_id"] != bracket || first["material_key"] != "100000001" || first["description"] != "Machined bracket M10" {
		t.Errorf("RFQ item: got %v %v %v, want the bracket, its number and description", first["material_id"], first["material_key"], first["description"])
	}
	if m, ok := apitest.List(t, q, "items")[1].(map[string]any)["material_id"]; ok {
		t.Errorf("an RFQ item of a key: got material %v, want none", m)
	}
	path := "/v1/rfqs/" + id(q)
	e.act(t, path, "publish", "")
	quotation, _ := e.Must(201, "POST", path+"/quotations", quotationBody(e.Supplier, "Q-1", price(1, "40.00", "EUR", ""), price(2, "100.00", "EUR", "")), "")
	_, tag := e.Must(200, "GET", path, "", "")
	awarded, _ := e.Must(200, "POST", path+":award", fmt.Sprintf(`{"quotation_id": %q, "valid_from": "2026-07-01", "valid_to": "2027-06-30"}`, id(quotation)), tag)
	contract, _ := e.Must(200, "GET", "/v1/contracts/"+fmt.Sprint(awarded["contract_id"]), "", "")
	if got := apitest.List(t, contract, "items")[0].(map[string]any)["material_id"]; got != bracket {
		t.Errorf("the awarded contract's item: got material %v, want %s", got, bracket)
	}
}

// TestMaterialSourced: an RFQ from a requisition asks for a material named
// by one requisition item by its number, with the material; items naming
// one material twice are asked for by the requisition's number and their
// own, without it.
func TestMaterialSourced(t *testing.T) {
	e := setupIn2026(t)
	bracket, bolt := e.material(t, "100000001", "active"), e.material(t, "100000002", "active")
	req := e.requisition(t, items(e.ofMaterial(bracket, "2 BOX", ""), e.ofMaterial(bolt, "1 EA", ""), e.ofMaterial(bolt, "2 EA", "")), `, "needed_by": "2026-07-20"`)
	e.act(t, "/v1/requisitions/"+id(req), "submit", "")
	e.Approve(t, id(req))
	r, _ := e.Must(201, "POST", "/v1/rfqs", e.sourcingBody(id(req), "", e.Supplier), "")
	number := fmt.Sprint(req["document_number"])
	want := []string{"100000001 " + bracket, number + "/2 <nil>", number + "/3 <nil>"}
	for i, v := range apitest.List(t, r, "items") {
		it := v.(map[string]any)
		if got := fmt.Sprintf("%v %v", it["material_key"], it["material_id"]); got != want[i] {
			t.Errorf("RFQ item %d: got %s, want %s", i+1, got, want[i])
		}
	}
}

// TestMaterialSourcedKeyShared: a material whose number is also an
// unmapped catalog item's key on the same requisition is asked for by
// the requisition's number and item number, both items without the
// material, rather than refused: the RFQ's keys are once each.
func TestMaterialSourcedKeyShared(t *testing.T) {
	e := setupIn2026(t)
	bracket := e.material(t, "100000001", "active")
	c := e.catalogItem(t, e.Supplier, "100000001", "Brackets", "2.00", "")
	pgtest.Exec(t, `UPDATE procurement.catalog_item SET uom_code = 'M', version = version + 1 WHERE id = $1`, id(c)) // left unmapped
	req := e.requisition(t, items(fmt.Sprintf(`{"catalog_item_id": %q, "quantity": {"value": "1", "unit": "M"}, "account_id": %q}`, id(c), e.office),
		e.ofMaterial(bracket, "2 EA", "")), `, "needed_by": "2026-07-20"`)
	e.act(t, "/v1/requisitions/"+id(req), "submit", "")
	e.Approve(t, id(req))
	r, _ := e.Must(201, "POST", "/v1/rfqs", e.sourcingBody(id(req), "", e.Supplier), "")
	number := fmt.Sprint(req["document_number"])
	for i, v := range apitest.List(t, r, "items") {
		it := v.(map[string]any)
		if got, want := fmt.Sprintf("%v %v", it["material_key"], it["material_id"]), fmt.Sprintf("%s/%d <nil>", number, i+1); got != want {
			t.Errorf("RFQ item %d: got %s, want %s", i+1, got, want)
		}
	}
}

// TestMaterialItemRefusals: an item of a material that is unknown, not
// active, keyed otherwise or in a unit that is none of its own or does
// not convert exactly is refused naming its field; an RFQ item and a
// catalog item without a material still need their key.
func TestMaterialItemRefusals(t *testing.T) {
	e := setupIn2026(t)
	bracket := e.material(t, "100000001", "active")
	blocked, pending := e.material(t, "100000002", "blocked"), e.material(t, "100000003", "pending")
	requisition := func(it string) string {
		return fmt.Sprintf(`{"legal_entity_id": %q, "currency": "EUR", "items": [%s]}`, e.DE, it)
	}
	rfq := func(it string) string { return e.rfqBody("2026-07-31", items(it), e.Supplier) }
	catalog := func(extra string) string {
		return fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "unit_price": {"amount": "40.00", "currency": "EUR"}%s}`, e.DE, e.Supplier, extra)
	}
	for _, c := range []struct{ name, path, body, code, field string }{
		{"an unknown material", "/v1/requisitions", requisition(e.ofMaterial(uuid.NewString(), "1 EA", "")), "request.reference_not_found", "items[0].material_id"},
		{"a blocked material", "/v1/requisitions", requisition(e.ofMaterial(blocked, "1 EA", "")), "procurement.material_not_usable", "items[0].material_id"},
		{"a pending material", "/v1/rfqs", rfq(e.rfqOfMaterial(pending, "1 EA", "")), "procurement.material_not_usable", "items[0].material_id"},
		{"a unit not the material's", "/v1/requisitions", requisition(e.ofMaterial(bracket, "1 M", "")), "procurement.invalid_material_quantity", "items[0].quantity.unit"},
		{"a quantity not exact", "/v1/rfqs", rfq(e.rfqOfMaterial(bracket, "1 KG", "")), "procurement.invalid_material_quantity", "items[0].quantity.unit"},
		{"another material key", "/v1/rfqs", rfq(e.rfqOfMaterial(bracket, "1 EA", `, "material_key": "BRACKET"`)), "procurement.key_not_material", "items[0].material_key"},
		{"an RFQ item without a key", "/v1/rfqs", rfq(fmt.Sprintf(`{"description": "Desk", "quantity": {"value": "1", "unit": "EA"}, "account_id": %q}`, e.office)),
			"request.validation_failed", "items[0].material_key"},
		{"a catalog item of a blocked material", "/v1/catalog-items", catalog(fmt.Sprintf(`, "material_id": %q, "unit": "EA"`, blocked)),
			"procurement.material_not_usable", "material_id"},
		{"a catalog item keyed otherwise", "/v1/catalog-items", catalog(fmt.Sprintf(`, "material_id": %q, "unit": "EA", "item_key": "BRACKET"`, bracket)),
			"procurement.key_not_material", "item_key"},
		{"a catalog item in a unit not the material's", "/v1/catalog-items", catalog(fmt.Sprintf(`, "material_id": %q, "unit": "M"`, bracket)),
			"procurement.invalid_material_quantity", "unit"},
		{"a catalog item without a key", "/v1/catalog-items", catalog(`, "description": "Chair", "unit": "EA"`), "procurement.invalid_item_key", "item_key"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := e.in(t).Problem(422, c.code, "POST", c.path, c.body, "")
			if f := fieldOf(t, p); f != c.field {
				t.Errorf("the field: got %v, want %s", f, c.field)
			}
		})
	}
}

// TestCatalogMapping: a company's active catalog items without a material
// whose key is an active material's number, in one of its units, are
// given it once, audited and published; a key of no material, of a
// blocked material or of items in another unit is reported and left; a
// second run maps nothing.
func TestCatalogMapping(t *testing.T) {
	e := setupIn2026(t)
	bracket := e.material(t, "100000001", "active")
	blocked := e.material(t, "100000002", "blocked")
	box := e.catalogItem(t, e.Supplier, "100000001", "Brackets by the box", "40.00", "")
	other := e.catalogItem(t, e.Vendor, "100000001", "Brackets", "2.00", "")
	pgtest.Exec(t, `UPDATE procurement.catalog_item SET uom_code = 'M', version = version + 1 WHERE id = $1`, id(other))
	e.catalogItem(t, e.Supplier, "100000002", "Blocked bolts", "1.00", "")
	e.catalogItem(t, e.Supplier, "CHAIR-01", "Office chair", "450.00", "")
	retired := e.catalogItem(t, e.Vendor, "CHAIR-02", "Old chair", "300.00", "")
	e.act(t, "/v1/catalog-items/"+id(retired), "retire", "")
	body := fmt.Sprintf(`{"legal_entity_id": %q}`, e.DE)
	got, _ := e.Must(200, "POST", "/v1/catalog-material-mappings", body, "")
	for field, want := range map[string]any{"company_code": "2000", "mapped_items": float64(1), "no_material_items": float64(1), "not_mappable_items": float64(2)} {
		if got[field] != want {
			t.Errorf("%s: got %v, want %v", field, got[field], want)
		}
	}
	want := []string{"100000001 mapped " + bracket + " 1", "100000001 not_mappable " + bracket + " 1", "100000002 not_mappable " + blocked + " 1",
		"CHAIR-01 no_material <nil> 1"}
	keys := apitest.List(t, got, "keys")
	if len(keys) != len(want) {
		t.Fatalf("keys: got %v, want %d", keys, len(want))
	}
	for i, v := range keys {
		k := v.(map[string]any)
		if s := fmt.Sprintf("%v %v %v %v", k["item_key"], k["outcome"], k["material_id"], k["items"]); s != want[i] {
			t.Errorf("key %d: got %s, want %s", i, s, want[i])
		}
	}
	if r := keys[2].(map[string]any)["reason"]; r != "material 100000002 is blocked" {
		t.Errorf("the blocked material's reason: got %v", r)
	}
	if r := keys[1].(map[string]any)["reason"]; r != "ordered in M, none of material 100000001's units (its base unit EA or an alternative unit)" {
		t.Errorf("the other unit's reason: got %v", r)
	}
	mapped, tag := e.Must(200, "GET", "/v1/catalog-items/"+id(box), "", "")
	if mapped["material_id"] != bracket || tag != `"2"` {
		t.Errorf("the mapped item: got material %v at %s, want %s at \"2\"", mapped["material_id"], tag, bracket)
	}
	if n := audits(t, e, "procurement.catalog_item.map_material", id(box)); n != 1 {
		t.Errorf("audit records of the mapping: got %d, want 1", n)
	}
	published := 0
	for _, p := range events(t, e, "catalog_item_changed") {
		if p["catalog_item_id"] == id(box) && p["material_id"] == bracket {
			published++
		}
	}
	if published != 1 {
		t.Errorf("CatalogItemChanged with the material: got %d, want 1", published)
	}
	again, _ := e.Must(200, "POST", "/v1/catalog-material-mappings", body, "")
	if again["mapped_items"] != float64(0) {
		t.Errorf("a second run: got %v mapped, want none", again["mapped_items"])
	}
	if n := len(apitest.List(t, again, "keys")); n != 3 {
		t.Errorf("a second run's keys: got %d, want the three left", n)
	}
}

var errMaterials = errors.New("materials unreachable")

// failingMaterials fails every read of materials.
type failingMaterials struct{}

func (failingMaterials) Materials(context.Context, []uuid.UUID) (map[uuid.UUID]materials.Material, error) {
	return nil, errMaterials
}

func (failingMaterials) ByNumber(context.Context, []string) (map[string]materials.Material, error) {
	return nil, errMaterials
}

// TestMaterialReadFails: a failed read of the materials fails the
// requisition, the catalog item and the mapping that read them.
func TestMaterialReadFails(t *testing.T) {
	e := setupIn2026(t)
	bracket := e.material(t, "100000001", "active")
	e.catalogItem(t, e.Supplier, "CHAIR-01", "Office chair", "450.00", "")
	h := app.Procurement(app.Config{})
	h.Materials = failingMaterials{}
	material := uuid.MustParse(bracket)
	for name, call := range map[string]func(ctx context.Context) error{
		"a requisition": func(ctx context.Context) error {
			_, err := h.CreateRequisition(ctx, &gen.RequisitionCreate{LegalEntityID: e.DE, Currency: "EUR", Items: []gen.RequisitionItemInput{{
				MaterialID: gen.NewOptUUID(material), Quantity: gen.Quantity{Value: "1", Unit: "EA"}, UnitPrice: gen.NewOptMoney(gen.Money{Amount: "1.00", Currency: "EUR"}),
				AccountID: uuid.MustParse(e.office)}}}, gen.CreateRequisitionParams{})
			return err
		},
		"a catalog item": func(ctx context.Context) error {
			_, err := h.CreateCatalogItem(ctx, &gen.CatalogItemCreate{LegalEntityID: e.DE, SupplierID: e.Supplier, MaterialID: gen.NewOptUUID(material), Unit: "EA",
				UnitPrice: gen.Money{Amount: "5.00", Currency: "EUR"}}, gen.CreateCatalogItemParams{})
			return err
		},
		"a mapping": func(ctx context.Context) error {
			_, err := h.MapCatalogItemMaterials(ctx, &gen.CatalogMaterialMapping{LegalEntityID: e.DE}, gen.MapCatalogItemMaterialsParams{})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, call)
			if !errors.Is(err, errMaterials) {
				t.Errorf("got %v, want the materials' read error", err)
			}
		})
	}
}

var _ procurement.Materials = failingMaterials{}

// TestCatalogMappingsWait: a mapping of a company's catalog waits for
// another one holding its unmapped items, and then finds them mapped and
// maps nothing twice.
func TestCatalogMappingsWait(t *testing.T) {
	e := setupIn2026(t)
	e.material(t, "100000001", "active")
	e.catalogItem(t, e.Supplier, "100000001", "Brackets", "2.00", "")
	ctx := context.Background()
	first, err := pgtest.Pool(t).Begin(ctx, database.Scope{Role: "erp_app", TenantID: e.Tenant.ID})
	if err != nil {
		t.Fatalf("begin the first mapping: %v", err)
	}
	second := make(chan map[string]any, 1)
	drained := true // nothing to drain until the second mapping starts
	defer func() {  // release the second mapping, then wait for it, however the test ends
		_ = first.Rollback(context.WithoutCancel(ctx)) // after a commit there is nothing to roll back
		if !drained {
			<-second
		}
	}()
	if err := database.SetActor(ctx, first, e.Tenant.Human.ID); err != nil {
		t.Fatalf("first mapping's actor: %v", err)
	}
	fctx := reqctx.WithPrincipal(reqctx.WithTx(ctx, first), reqctx.Principal{ID: e.Tenant.Human.ID, Kind: e.Tenant.Human.Kind, TenantID: e.Tenant.ID})
	fctx, _ = audit.WithBuffer(fctx)
	got, err := endpoints().MapCatalogItemMaterials(fctx, &gen.CatalogMaterialMapping{LegalEntityID: e.DE}, gen.MapCatalogItemMaterialsParams{})
	if err != nil {
		t.Fatalf("the first mapping: %v", err)
	}
	if n := got.(*gen.CatalogMaterialMappingReportHeaders).Response.MappedItems; n != 1 {
		t.Fatalf("the first mapping: got %d mapped, want 1", n)
	}
	auth := http.Header{"Authorization": {"Bearer " + e.API.Issuer.Token(t, e.Tenant.ID, e.Tenant.Human.Subject, nil)}}
	drained = false
	go func() {
		r := e.API.Do(apitest.Req{Method: "POST", Path: "/v1/catalog-material-mappings", Key: apitest.NewKey(), Header: auth,
			Body: fmt.Sprintf(`{"legal_entity_id": %q}`, e.DE)})
		var m map[string]any
		_ = json.Unmarshal(r.Body.Bytes(), &m) // a body that is no JSON leaves m nil, which the check below reports
		second <- m
	}()
	admin := pgtest.Admin(t)
	pgtest.Eventually(t, 10*time.Second, func() bool {
		var waiting bool
		err := admin.QueryRow(ctx, `SELECT EXISTS (SELECT FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'
			AND query LIKE '%procurement.catalog_item%FOR UPDATE%')`).Scan(&waiting)
		return err == nil && waiting
	}, "the second mapping never waited for the first")
	if err := first.Commit(ctx); err != nil {
		t.Fatalf("commit the first mapping: %v", err)
	}
	m := <-second
	drained = true
	if m["mapped_items"] != float64(0) {
		t.Errorf("the second mapping: got %v mapped, want none (the first mapped the item)", m["mapped_items"])
	}
}

// TestListByMaterial: GET /v1/purchase-orders and GET /v1/contracts with
// material_id answer the orders and contracts with an item of the
// material, each whole, and none without one (S2l: the material's
// purchasing history and outline agreements); a page of them stays in
// the filter.
func TestListByMaterial(t *testing.T) {
	e := setup(t)
	bracket, other := e.material(t, "MA-LIST-1", "active"), e.material(t, "MA-LIST-2", "active")
	order := func(items ...string) string {
		o, _ := e.Must(201, "POST", "/v1/purchase-orders", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "currency": "EUR", "order_date": "2026-03-01",
			"items": [%s]}`, e.DE, e.Supplier, strings.Join(items, ", ")), "")
		return o["id"].(string)
	}
	poItem := func(material, extra string) string {
		return fmt.Sprintf(`{"description": "Bracket", "quantity": {"value": "2", "unit": "EA"}, "unit_price": {"amount": "1.00", "currency": "EUR"},
			"account_id": %q%s}`, e.office, map[bool]string{true: `, "material_id": "` + material + `"`, false: ""}[material != ""]+extra)
	}
	both, only := order(poItem("", ""), poItem(bracket, "")), order(poItem(bracket, ""))
	order(poItem(other, ""))
	order(poItem("", ""))
	// An order and a contract whose item of the bracket an update removed
	// have no item of it.
	dropped := order(poItem("", ""), poItem(bracket, ""))
	e.Must(200, "PATCH", "/v1/purchase-orders/"+dropped, `{"items": [`+poItem("", "")+`]}`, `"1"`)
	contract := func(key, material string) string {
		c, _ := e.Must(201, "POST", "/v1/contracts", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "currency": "EUR", "valid_from": "2026-01-01",
			"valid_to": "2026-12-31", "target_value": {"amount": "100.00", "currency": "EUR"}, "items": [{"material_key": "FREE", "description": "X",
			"unit_price": {"amount": "1.00", "currency": "EUR"}, "uom_code": "EA", "account_id": %q}, {"material_key": %q, "description": "Bracket",
			"unit_price": {"amount": "1.00", "currency": "EUR"}, "uom_code": "EA", "account_id": %q%s}]}`, e.DE, e.Supplier, e.office, key, e.office,
			map[bool]string{true: `, "material_id": "` + material + `"`, false: ""}[material != ""]), "")
		return c["id"].(string)
	}
	agreement := contract("MA-LIST-1", bracket)
	contract("MA-LIST-2", other)
	contract("KEY-ONLY", "")
	changed := contract("MA-LIST-1", bracket)
	e.Must(200, "PATCH", "/v1/contracts/"+changed, fmt.Sprintf(`{"items": [{"material_key": "FREE", "description": "X",
		"unit_price": {"amount": "1.00", "currency": "EUR"}, "uom_code": "EA", "account_id": %q}]}`, e.office), `"1"`)
	ids := func(path string) []string {
		page, _ := e.Must(200, "GET", path, "", "")
		var out []string
		for _, it := range page["items"].([]any) {
			out = append(out, it.(map[string]any)["id"].(string))
		}
		return out
	}
	for name, c := range map[string]struct {
		path string
		want []string
	}{
		"orders of the bracket":      {"/v1/purchase-orders?order_by=document_number&material_id=" + bracket, []string{both, only}},
		"a page of them":             {"/v1/purchase-orders?order_by=document_number&page_size=1&material_id=" + bracket, []string{both}},
		"orders of no such material": {"/v1/purchase-orders?material_id=" + uuid.NewString(), nil},
		"contracts of the bracket":   {"/v1/contracts?material_id=" + bracket, []string{agreement}},
		"contracts of no such one":   {"/v1/contracts?material_id=" + uuid.NewString(), nil},
	} {
		t.Run(name, func(t *testing.T) {
			if got := ids(c.path); !slices.Equal(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
	page, _ := e.Must(200, "GET", "/v1/purchase-orders?order_by=document_number&material_id="+bracket, "", "")
	if n := len(page["items"].([]any)[0].(map[string]any)["items"].([]any)); n != 2 {
		t.Errorf("the first order's items: got %d, want 2: an order is answered whole", n)
	}
	page, _ = e.Must(200, "GET", "/v1/contracts?material_id="+bracket, "", "")
	if n := len(page["items"].([]any)[0].(map[string]any)["items"].([]any)); n != 2 {
		t.Errorf("the contract's items: got %d, want 2: a contract is answered whole", n)
	}
}
