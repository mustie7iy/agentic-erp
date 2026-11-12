package procurement_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// fromCatalog is a requisition item taken from a catalog item: a quantity
// in EA and the account, with what extra adds.
func (e env) fromCatalog(catalogItem any, qty, extra string) string {
	return fmt.Sprintf(`{"catalog_item_id": %q, "quantity": {"value": %q, "unit": "EA"}, "account_id": %q%s}`, catalogItem, qty, e.office, extra)
}

// firstItem is a requisition's first item.
func firstItem(t *testing.T, r map[string]any) map[string]any {
	t.Helper()
	return apitest.List(t, r, "items")[0].(map[string]any)
}

// TestRequisitionFromCatalog (T84b, ADR-0071 §6): an item taken from the
// catalog has the catalog item's description, key, unit and price, the
// requisition its supplier, and the requisition is submitted, approved
// and ordered as any other.
func TestRequisitionFromCatalog(t *testing.T) {
	e := setupIn2026(t)
	chair := e.catalogItem(t, e.Supplier, "CHAIR-01", "Office chair, ergonomic", "450.00", `, "price_unit": 2`)
	r := e.requisition(t, items(e.fromCatalog(id(chair), "4", ""), item(e.expense, "1", "EA", "10.00", "EUR", "")), "")
	path := "/v1/requisitions/" + id(r)
	check := func(t *testing.T, what string, r map[string]any) {
		t.Helper()
		it := firstItem(t, r)
		for field, want := range map[string]any{"catalog_item_id": id(chair), "item_key": "CHAIR-01", "description": "Office chair, ergonomic",
			"price_unit": float64(2), "account_id": e.office} {
			if it[field] != want {
				t.Errorf("%s item %s: got %v, want %v", what, field, it[field], want)
			}
		}
		if got := fmt.Sprint(it["unit_price"], it["quantity"]); got != "map[amount:450.00 currency:EUR] map[unit:EA value:4]" {
			t.Errorf("%s item price and quantity: got %s, want 450.00 EUR and 4 EA", what, got)
		}
		if r["supplier_id"] != e.Supplier.String() {
			t.Errorf("%s supplier: got %v, want the catalog item's %v", what, r["supplier_id"], e.Supplier)
		}
		if got := apitest.String(t, apitest.Object(t, r, "total_amount"), "amount"); got != "910.00" {
			t.Errorf("%s total: got %s, want 910.00 (4 at 450.00 per 2, and 10.00)", what, got)
		}
		other := apitest.List(t, r, "items")[1].(map[string]any)
		if other["catalog_item_id"] != nil || other["item_key"] != nil {
			t.Errorf("%s described item: got catalog item %v key %v, want none", what, other["catalog_item_id"], other["item_key"])
		}
	}
	check(t, "created", r)
	got, tag := e.Must(200, "GET", path, "", "")
	check(t, "read back", got)
	page, _ := e.Must(200, "GET", "/v1/requisitions?document_number="+apitest.String(t, r, "document_number"), "", "")
	check(t, "listed", apitest.List(t, page, "items")[0].(map[string]any))
	// As any other: submitted, approved and ordered.
	sub, _ := e.Must(200, "POST", path+":submit", "", tag)
	if sub["status"] != "pending_approval" {
		t.Fatalf("submitted: got %v, want pending_approval", sub["status"])
	}
	e.Approve(t, id(r))
	o := e.order(t, items(item(e.office, "4", "EA", "450.00", "EUR", `, "price_unit": 2`)), fmt.Sprintf(`, "requisition_id": %q`, id(r)))
	if o["requisition_id"] != id(r) {
		t.Errorf("the order's requisition: got %v, want %v", o["requisition_id"], id(r))
	}
	got, _ = e.Must(200, "GET", path, "", "")
	if got["status"] != "ordered" {
		t.Errorf("after the order: got %v, want ordered", got["status"])
	}
	check(t, "ordered", got)
	// A retired catalog item stays on the requisitions that took it.
	e.act(t, "/v1/catalog-items/"+id(chair), "retire", "")
	got, _ = e.Must(200, "GET", path, "", "")
	check(t, "after the catalog item is retired", got)
}

// TestRequisitionCatalogContract (T84b): a catalog item that names a
// contract item gives the requisition its contract; another contract or
// supplier is refused, naming the item.
func TestRequisitionCatalogContract(t *testing.T) {
	e := setupIn2026(t)
	active := e.contract(t, e.Supplier.String(), "")
	e.act(t, "/v1/contracts/"+id(active), "release", "")
	other := e.contract(t, e.Supplier.String(), "")
	e.act(t, "/v1/contracts/"+id(other), "release", "")
	paper := e.catalogItem(t, e.Supplier, "PAPER", "Paper", "5.00", fmt.Sprintf(`, "contract_id": %q, "contract_item_no": 1`, id(active)))
	pens := e.catalogItem(t, e.Supplier, "PENS", "Pens", "2.00", "")
	theirs := e.catalogItem(t, e.Vendor, "TONER", "Toner", "30.00", "")
	r := e.requisition(t, items(e.fromCatalog(id(pens), "1", ""), e.fromCatalog(id(paper), "10", "")), "")
	if r["contract_id"] != id(active) {
		t.Errorf("contract taken from the catalog: got %v, want %v", r["contract_id"], id(active))
	}
	if r["supplier_id"] != e.Supplier.String() {
		t.Errorf("supplier taken from the catalog: got %v, want %v", r["supplier_id"], e.Supplier)
	}
	path := "/v1/requisitions/" + id(r)
	// Named as the catalog item's, they are accepted.
	e.requisition(t, items(e.fromCatalog(id(paper), "1", "")), fmt.Sprintf(`, "supplier_id": %q, "contract_id": %q`, e.Supplier, id(active)))
	// A catalog item without a contract leaves the contract the requester's.
	free := e.requisition(t, items(e.fromCatalog(id(pens), "1", "")), fmt.Sprintf(`, "contract_id": %q`, id(other)))
	if free["contract_id"] != id(other) {
		t.Errorf("the requester's contract: got %v, want %v", free["contract_id"], id(other))
	}
	body := func(its, extra string) string {
		return fmt.Sprintf(`{"legal_entity_id": %q, "currency": "EUR", "items": %s%s}`, e.DE, its, extra)
	}
	for _, c := range []struct{ name, body, code, field string }{
		{"another supplier than the catalog item's", body(items(e.fromCatalog(id(pens), "1", "")), fmt.Sprintf(`, "supplier_id": %q`, e.Vendor)),
			"procurement.catalog_item_supplier", "items[0].catalog_item_id"},
		{"catalog items of two suppliers", body(items(e.fromCatalog(id(pens), "1", ""), e.fromCatalog(id(theirs), "1", "")), ""),
			"procurement.catalog_item_supplier", "items[1].catalog_item_id"},
		{"another contract than the catalog item's", body(items(e.fromCatalog(id(paper), "1", "")), fmt.Sprintf(`, "contract_id": %q`, id(other))),
			"procurement.catalog_item_contract", "items[0].catalog_item_id"},
	} {
		t.Run(c.name, func(t *testing.T) {
			refusal := e.in(t).Problem(422, c.code, "POST", "/v1/requisitions", c.body, "")
			if got := apitest.String(t, apitest.List(t, refusal, "errors")[0].(map[string]any), "field"); got != c.field {
				t.Errorf("refused field: got %s, want %s", got, c.field)
			}
		})
	}
	// A change that leaves the items is held to them too: another supplier
	// or contract is refused, and clearing one takes it from the catalog
	// items again.
	_, tag := e.Must(200, "GET", path, "", "")
	refusal := e.Problem(422, "procurement.catalog_item_supplier", "PATCH", path, fmt.Sprintf(`{"supplier_id": %q}`, e.Vendor), tag)
	if got := apitest.String(t, apitest.List(t, refusal, "errors")[0].(map[string]any), "field"); got != "items[0].catalog_item_id" {
		t.Errorf("refused field of the supplier change: got %s, want items[0].catalog_item_id", got)
	}
	refusal = e.Problem(422, "procurement.catalog_item_contract", "PATCH", path, fmt.Sprintf(`{"contract_id": %q}`, id(other)), tag)
	if got := apitest.String(t, apitest.List(t, refusal, "errors")[0].(map[string]any), "field"); got != "items[1].catalog_item_id" {
		t.Errorf("refused field of the contract change: got %s, want items[1].catalog_item_id", got)
	}
	cleared, tag := e.Must(200, "PATCH", path, `{"supplier_id": null, "contract_id": null, "needed_by": "2026-12-01"}`, tag)
	if cleared["contract_id"] != id(active) {
		t.Errorf("contract after clearing: got %v, want the catalog item's %v", cleared["contract_id"], id(active))
	}
	if cleared["supplier_id"] != e.Supplier.String() {
		t.Errorf("supplier after clearing: got %v, want the catalog items' %v", cleared["supplier_id"], e.Supplier)
	}
	if n := len(apitest.List(t, cleared, "items")); n != 2 {
		t.Errorf("items after clearing: got %d, want 2", n)
	}
	// The stored items still say whose it is once a catalog item is retired.
	e.act(t, "/v1/catalog-items/"+id(pens), "retire", "")
	e.Problem(422, "procurement.catalog_item_supplier", "PATCH", path, fmt.Sprintf(`{"supplier_id": %q}`, e.Vendor), tag)
	// New items replace them: without catalog items the requisition is the requester's again.
	upd, _ := e.Must(200, "PATCH", path, fmt.Sprintf(`{"items": %s, "supplier_id": %q, "contract_id": null}`, items(item(e.office, "1", "EA", "1.00", "EUR", "")), e.Vendor), tag)
	if upd["supplier_id"] != e.Vendor.String() {
		t.Errorf("supplier after replacing the items: got %v, want %v", upd["supplier_id"], e.Vendor)
	}
	if upd["contract_id"] != nil {
		t.Errorf("contract after replacing the items: got %v, want none", upd["contract_id"])
	}
	if got := firstItem(t, upd)["catalog_item_id"]; got != nil {
		t.Errorf("catalog item after replacing the items: got %v, want none", got)
	}
}

// TestRequisitionCatalogRefusals (T84b): an item from the catalog sends
// no price, price unit or description of its own and is in the catalog
// item's unit; the catalog item is an active one of the requisition's
// company in its currency; each refusal names the item.
func TestRequisitionCatalogRefusals(t *testing.T) {
	e := setup(t)
	chair := e.catalogItem(t, e.Supplier, "CHAIR-01", "Office chair", "450.00", "")
	retired := e.catalogItem(t, e.Supplier, "CHAIR-00", "Old chair", "400.00", "")
	e.act(t, "/v1/catalog-items/"+id(retired), "retire", "")
	usd, _ := e.Must(201, "POST", "/v1/catalog-items", strings.Replace(e.catalogBody(e.Supplier, "CHAIR-USD", "Chair", "500.00", ""), `"EUR"`, `"USD"`, 1), "")
	us, _ := e.Must(201, "POST", "/v1/catalog-items", strings.Replace(e.catalogBody(e.Supplier, "CHAIR-US", "Chair", "500.00", ""), e.DE.String(), e.US.String(), 1), "")
	body := func(its string) string {
		return fmt.Sprintf(`{"legal_entity_id": %q, "currency": "EUR", "items": %s}`, e.DE, its)
	}
	ok := item(e.office, "1", "EA", "1.00", "EUR", "")
	for _, c := range []struct{ name, items, code, field string }{
		{"a price sent with the catalog item", items(e.fromCatalog(id(chair), "1", `, "unit_price": {"amount": "1.00", "currency": "EUR"}`)),
			"procurement.catalog_item_override", "items[0].unit_price"},
		{"a price unit sent with it", items(e.fromCatalog(id(chair), "1", `, "price_unit": 1`)), "procurement.catalog_item_override", "items[0].price_unit"},
		{"a description sent with it", items(ok, e.fromCatalog(id(chair), "1", `, "description": "My chair"`)), "procurement.catalog_item_override", "items[1].description"},
		{"another unit than the catalog item's", items(strings.Replace(e.fromCatalog(id(chair), "1", ""), `"EA"`, `"KG"`, 1)), "procurement.catalog_item_override", "items[0].quantity.unit"},
		{"a retired catalog item", items(ok, e.fromCatalog(id(retired), "1", "")), "procurement.invalid_catalog_item", "items[1].catalog_item_id"},
		{"a catalog item in another currency", items(e.fromCatalog(id(usd), "1", "")), "procurement.invalid_catalog_item", "items[0].catalog_item_id"},
		{"another company's catalog item", items(e.fromCatalog(id(us), "1", "")), "procurement.invalid_catalog_item", "items[0].catalog_item_id"},
		{"no catalog item with the id", items(e.fromCatalog(uuid.New(), "1", "")), "procurement.invalid_catalog_item", "items[0].catalog_item_id"},
		{"a described item without a price", items(fmt.Sprintf(`{"description": "Paper", "quantity": {"value": "1", "unit": "EA"}, "account_id": %q}`, e.office)),
			"request.validation_failed", "items[0].unit_price"},
		{"a described item without a description", items(fmt.Sprintf(`{"quantity": {"value": "1", "unit": "EA"}, "unit_price": {"amount": "1.00", "currency": "EUR"}, "account_id": %q}`, e.office)),
			"request.validation_failed", "items[0].description"},
		{"a quantity of zero of a catalog item", items(e.fromCatalog(id(chair), "0", "")), "procurement.invalid_quantity", "items[0].quantity.value"},
	} {
		t.Run(c.name, func(t *testing.T) {
			refusal := e.in(t).Problem(422, c.code, "POST", "/v1/requisitions", body(c.items), "")
			if got := apitest.String(t, apitest.List(t, refusal, "errors")[0].(map[string]any), "field"); got != c.field {
				t.Errorf("refused field: got %s, want %s", got, c.field)
			}
		})
	}
	// The refusals name the catalog item by its key.
	refusal := e.Problem(422, "procurement.invalid_catalog_item", "POST", "/v1/requisitions", body(items(e.fromCatalog(id(retired), "1", ""))), "")
	if detail := apitest.String(t, refusal, "detail"); !strings.Contains(detail, "catalog item CHAIR-00 is retired") {
		t.Errorf("the refusal of a retired item: got %q, want it to say catalog item CHAIR-00 is retired", detail)
	}
	refusal = e.Problem(422, "procurement.invalid_catalog_item", "POST", "/v1/requisitions", body(items(e.fromCatalog(id(usd), "1", ""))), "")
	if detail := apitest.String(t, refusal, "detail"); !strings.Contains(detail, "catalog item CHAIR-USD is priced in USD") {
		t.Errorf("the refusal of an item in another currency: got %q, want it to say catalog item CHAIR-USD is priced in USD", detail)
	}
	// A draft changed to an item from the catalog is checked the same way.
	r := e.requisition(t, items(ok), "")
	path := "/v1/requisitions/" + id(r)
	e.Problem(422, "procurement.invalid_catalog_item", "PATCH", path, `{"items": `+items(e.fromCatalog(id(retired), "1", ""))+`}`, `"1"`)
	upd, _ := e.Must(200, "PATCH", path, `{"items": `+items(e.fromCatalog(id(chair), "3", ""))+`}`, `"1"`)
	if got := firstItem(t, upd)["item_key"]; got != "CHAIR-01" {
		t.Errorf("item key after the change to a catalog item: got %v, want CHAIR-01", got)
	}
	if upd["supplier_id"] != e.Supplier.String() {
		t.Errorf("supplier after the change to a catalog item: got %v, want %v", upd["supplier_id"], e.Supplier)
	}
	// Taking an item from the catalog reads the catalog: without the
	// permission it is refused, and nothing is created.
	var before, after int
	pgtest.Scan(t, `SELECT count(*) FROM procurement.requisition WHERE tenant_id = $1`, []any{e.Tenant.ID}, &before)
	pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = 'procurement.catalog.read'`, e.Tenant.ID)
	e.Problem(403, "identity.permission_missing", "POST", "/v1/requisitions", body(items(e.fromCatalog(id(chair), "1", ""))), "")
	e.Must(201, "POST", "/v1/requisitions", body(items(ok)), "")
	pgtest.Scan(t, `SELECT count(*) FROM procurement.requisition WHERE tenant_id = $1`, []any{e.Tenant.ID}, &after)
	if after != before+1 {
		t.Errorf("requisitions without the catalog permission: got %d more, want 1 (the described one)", after-before)
	}
}
