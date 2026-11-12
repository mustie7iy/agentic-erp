package procurement_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// catalogBody is a catalog item's create request: of company 2000 and the
// fixture's supplier, priced in EUR per EA, with what extra adds.
func (e env) catalogBody(supplier uuid.UUID, key, description, price, extra string) string {
	return fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "item_key": %q, "description": %q, "unit": "EA",
		"unit_price": {"amount": %q, "currency": "EUR"}%s}`, e.DE, supplier, key, description, price, extra)
}

func (e env) catalogItem(t *testing.T, supplier uuid.UUID, key, description, price, extra string) map[string]any {
	t.Helper()
	c, _ := e.Must(201, "POST", "/v1/catalog-items", e.catalogBody(supplier, key, description, price, extra), "")
	return c
}

// search is the catalog items a search of company 2000 returns, as "key
// status" in the order given, and its next page token.
func (e env) search(t *testing.T, query string) (string, string) {
	t.Helper()
	page, _ := e.Must(200, "GET", fmt.Sprintf("/v1/catalog-items?legal_entity_id=%s%s", e.DE, query), "", "")
	var got []string
	for _, it := range apitest.List(t, page, "items") {
		m := it.(map[string]any)
		got = append(got, fmt.Sprintf("%v %v", m["item_key"], m["status"]))
	}
	token, _ := page["next_page_token"].(string)
	return strings.Join(got, ", "), token
}

// TestCatalogItems (T84a, ADR-0071): an item is loaded active with its
// supplier's number and name, read back, and retired once; its event
// names the supplier by id and number, never by name.
func TestCatalogItems(t *testing.T) {
	e := setup(t)
	pgtest.Exec(t, `UPDATE partner.business_partner SET name = 'Tyrell Office Supply', version = version + 1 WHERE id = $1`, e.Supplier)
	c := e.catalogItem(t, e.Supplier, "CHAIR-01", "Office chair, ergonomic", "450.00", "")
	path := "/v1/catalog-items/" + id(c)
	for field, want := range map[string]any{"status": "active", "item_key": "CHAIR-01", "description": "Office chair, ergonomic", "unit": "EA",
		"price_unit": float64(1), "supplier_id": e.Supplier.String(), "supplier_number": "BP90000001", "supplier_name": "Tyrell Office Supply",
		"legal_entity_id": e.DE.String(), "version": float64(1), "contract_id": nil, "contract_number": nil, "contract_item_no": nil} {
		if c[field] != want {
			t.Errorf("created %s: got %v, want %v", field, c[field], want)
		}
	}
	if got := fmt.Sprint(c["unit_price"]); got != "map[amount:450.00 currency:EUR]" {
		t.Errorf("created unit price: got %s, want 450.00 EUR", got)
	}
	got, tag := e.Must(200, "GET", path, "", "")
	if tag != `"1"` {
		t.Errorf("etag: got %s, want \"1\"", tag)
	}
	if got["item_key"] != "CHAIR-01" || got["supplier_name"] != "Tyrell Office Supply" {
		t.Errorf("read back: got %v %v, want CHAIR-01 Tyrell Office Supply", got["item_key"], got["supplier_name"])
	}
	// The key is the supplier's once among the company's active items;
	// another supplier's item may have it.
	e.Problem(409, "procurement.catalog_item_exists", "POST", "/v1/catalog-items", e.catalogBody(e.Supplier, "CHAIR-01", "Another chair", "1.00", ""), "")
	e.catalogItem(t, e.Vendor, "CHAIR-01", "Office chair, plain", "300.00", `, "price_unit": 2`)
	// Retired at the version read, once.
	e.Problem(412, "request.version_mismatch", "POST", path+":retire", "", `"7"`)
	retired, tag := e.Must(200, "POST", path+":retire", "", tag)
	if retired["status"] != "retired" || tag != `"2"` {
		t.Errorf("retired: got status %v and etag %s, want retired and \"2\"", retired["status"], tag)
	}
	e.Problem(409, "procurement.invalid_status", "POST", path+":retire", "", tag)
	if got, _ := e.Must(200, "GET", path, "", ""); got["status"] != "retired" {
		t.Errorf("read after retiring: got %v, want retired", got["status"])
	}
	// A retired item frees its key.
	again := e.catalogItem(t, e.Supplier, "CHAIR-01", "Office chair, ergonomic, new price", "470.00", "")
	evs := events(t, e, "catalog_item_changed")
	if len(evs) != 4 {
		t.Fatalf("catalog_item_changed events: got %d, want 4 (three loaded, one retired)", len(evs))
	}
	first, last := evs[0], evs[2]
	for field, want := range map[string]any{"catalog_item_id": id(c), "supplier_id": e.Supplier.String(), "supplier_number": "BP90000001",
		"item_key": "CHAIR-01", "unit": "EA", "status": "active", "status_before": nil, "version": float64(1), "legal_entity_id": e.DE.String()} {
		if first[field] != want {
			t.Errorf("loaded event %s: got %v, want %v", field, first[field], want)
		}
	}
	if got := fmt.Sprint(first["unit_price"]); got != "map[amount:450.00 currency:EUR]" {
		t.Errorf("loaded event unit price: got %s, want 450.00 EUR", got)
	}
	if last["status"] != "retired" || last["status_before"] != "active" || last["version"] != float64(2) {
		t.Errorf("retired event: got %v from %v at version %v, want retired from active at version 2", last["status"], last["status_before"], last["version"])
	}
	apitest.NeverPublished(t, e.Tenant, "procurement", "Tyrell Office Supply")
	var audits int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND object_type = 'procurement.catalog_item'
		AND action IN ('procurement.catalog_item.create', 'procurement.catalog_item.retire')`, []any{e.Tenant.ID}, &audits)
	if audits != 4 {
		t.Errorf("audit records: got %d, want 4", audits)
	}
	// Someone who may not act in the company finds neither the item nor
	// the company's catalog.
	other := e.As(e.Reader(t, "us only", e.US))
	other.Problem(404, "request.not_found", "GET", "/v1/catalog-items/"+id(again), "", "")
	other.Problem(404, "request.not_found", "POST", "/v1/catalog-items/"+id(again)+":retire", "", `"1"`)
	page, _ := other.Must(200, "GET", fmt.Sprintf("/v1/catalog-items?legal_entity_id=%s", e.DE), "", "")
	if n := len(apitest.List(t, page, "items")); n != 0 {
		t.Errorf("another company's catalog: got %d items, want none", n)
	}
}

// TestCatalogSearch (T84a): the search is one company's, by text in the
// key or the description whatever the case, by supplier and by status,
// active unless asked; its pages are stable.
func TestCatalogSearch(t *testing.T) {
	e := setup(t)
	e.catalogItem(t, e.Supplier, "CHAIR-01", "Office chair, ergonomic", "450.00", "")
	e.catalogItem(t, e.Supplier, "DESK-01", "Standing desk", "900.00", "")
	e.catalogItem(t, e.Vendor, "CHAIR-01", "Visitor Chair", "120.00", "")
	e.catalogItem(t, e.Vendor, "LAMP-01", "Desk lamp", "40.00", "")
	old := e.catalogItem(t, e.Vendor, "CHAIR-00", "Chair, discontinued", "99.00", "")
	e.act(t, "/v1/catalog-items/"+id(old), "retire", "")
	// Company 1000's catalog is its own.
	e.Must(201, "POST", "/v1/catalog-items", strings.Replace(e.catalogBody(e.Supplier, "CHAIR-US", "Chair", "10.00", ""), e.DE.String(), e.US.String(), 1), "")
	for _, c := range []struct{ name, query, want string }{
		{"every active item, by key", "", "CHAIR-01 active, CHAIR-01 active, DESK-01 active, LAMP-01 active"},
		{"a word of the description, whatever the case", "&text=CHAIR", "CHAIR-01 active, CHAIR-01 active"},
		{"a word found in descriptions and keys", "&text=desk", "DESK-01 active, LAMP-01 active"},
		{"part of a key", "&text=amp-0", "LAMP-01 active"},
		{"a text nothing contains", "&text=sofa", ""},
		{"a text with LIKE's wildcards is plain text", "&text=%25", ""},
		{"one supplier's items", "&supplier_id=" + e.Vendor.String(), "CHAIR-01 active, LAMP-01 active"},
		{"text and supplier", "&text=chair&supplier_id=" + e.Supplier.String(), "CHAIR-01 active"},
		{"retired items when asked", "&status=retired", "CHAIR-00 retired"},
		{"retired items by text", "&status=retired&text=chair", "CHAIR-00 retired"},
		{"active items when asked", "&status=active&text=lamp", "LAMP-01 active"},
		{"by key, descending", "&order_by=item_key%20desc", "LAMP-01 active, DESK-01 active, CHAIR-01 active, CHAIR-01 active"},
		{"by when they were loaded", "&order_by=created_at", "CHAIR-01 active, DESK-01 active, CHAIR-01 active, LAMP-01 active"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got, _ := e.in(t).search(t, c.query); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
	for _, order := range []string{"item_key", "created_at"} {
		t.Run("pages by "+order, func(t *testing.T) {
			e := e.in(t)
			whole, _ := e.search(t, "&order_by="+order)
			var pages []string
			token := ""
			for {
				got, next := e.search(t, "&page_size=3&order_by="+order+token)
				pages = append(pages, got)
				if next == "" {
					break
				}
				token = "&page_token=" + next
			}
			if got := strings.Join(pages, ", "); got != whole || len(pages) != 2 {
				t.Errorf("got %q in %d pages, want %q in 2", got, len(pages), whole)
			}
		})
	}
	// The suppliers' numbers are read for the page, each beside its id.
	page, _ := e.Must(200, "GET", fmt.Sprintf("/v1/catalog-items?legal_entity_id=%s&text=chair", e.DE), "", "")
	var numbers []string
	for _, it := range apitest.List(t, page, "items") {
		numbers = append(numbers, apitest.String(t, it.(map[string]any), "supplier_number"))
	}
	if got := strings.Join(numbers, " "); got != "BP90000001 BP90000002" {
		t.Errorf("suppliers of the chairs: got %s, want BP90000001 BP90000002", got)
	}
	e.Problem(400, "request.invalid_order_by", "GET", fmt.Sprintf("/v1/catalog-items?legal_entity_id=%s&order_by=description", e.DE), "", "")
	e.Problem(400, "request.invalid_parameter", "GET", "/v1/catalog-items", "", "")
}

// TestCatalogItemContract (T84a): a catalog item may name the contract
// item that prices an order of it: an item of an active contract of the
// company and the supplier, in the item's unit and currency and at its
// price (product owner, 2026-11-12): another amount or price unit is
// refused when the item is loaded, naming the item.
func TestCatalogItemContract(t *testing.T) {
	e := setupIn2026(t)
	draft := e.contract(t, e.Supplier.String(), "")
	active := e.contract(t, e.Supplier.String(), "")
	e.act(t, "/v1/contracts/"+id(active), "release", "")
	theirs := e.contract(t, e.Vendor.String(), "")
	e.act(t, "/v1/contracts/"+id(theirs), "release", "")
	named := func(contract any, item int) string {
		return fmt.Sprintf(`, "contract_id": %q, "contract_item_no": %d`, contract, item)
	}
	c := e.catalogItem(t, e.Supplier, "PAPER", "Paper", "5.00", named(id(active), 1))
	if c["contract_id"] != id(active) || c["contract_number"] != active["document_number"] || c["contract_item_no"] != float64(1) {
		t.Errorf("the contract item: got %v %v item %v, want %v %v item 1", c["contract_id"], c["contract_number"], c["contract_item_no"],
			id(active), active["document_number"])
	}
	if got := fmt.Sprint(c["unit_price"], c["price_unit"]); got != "map[amount:5.00 currency:EUR] 1" {
		t.Errorf("the catalog item's price: got %s, want the contract item's 5.00 EUR per 1", got)
	}
	// The contract item's price per its price unit, 30.00 per 2 BOX.
	toner, _ := e.Must(201, "POST", "/v1/catalog-items",
		strings.Replace(e.catalogBody(e.Supplier, "TONER", "Toner", "30.00", `, "price_unit": 2`+named(id(active), 2)), `"EA"`, `"BOX"`, 1), "")
	if got := fmt.Sprint(toner["unit_price"], toner["price_unit"]); got != "map[amount:30.00 currency:EUR] 2" {
		t.Errorf("the catalog item's price per two: got %s, want the contract item's 30.00 EUR per 2", got)
	}
	if ev := events(t, e, "catalog_item_changed"); len(ev) != 2 || ev[0]["contract_id"] != id(active) || ev[0]["contract_item_no"] != float64(1) {
		t.Errorf("the event's contract item: got %v, want contract %s item 1", ev, id(active))
	}
	usd := strings.Replace(e.catalogBody(e.Supplier, "PAPER-USD", "Paper", "5.00", named(id(active), 1)), `"EUR"`, `"USD"`, 1)
	for _, r := range []struct{ name, body, code, field string }{
		{"a contract without its item", e.catalogBody(e.Supplier, "K", "D", "1.00", fmt.Sprintf(`, "contract_id": %q`, id(active))), "procurement.invalid_contract_item", "contract_item_no"},
		{"an item without its contract", e.catalogBody(e.Supplier, "K", "D", "1.00", `, "contract_item_no": 1`), "procurement.invalid_contract_item", "contract_item_no"},
		{"an unknown contract", e.catalogBody(e.Supplier, "K", "D", "1.00", named(uuid.New(), 1)), "procurement.invalid_contract", "contract_id"},
		{"a draft contract", e.catalogBody(e.Supplier, "K", "D", "1.00", named(id(draft), 1)), "procurement.invalid_contract", "contract_id"},
		{"another supplier's contract", e.catalogBody(e.Supplier, "K", "D", "1.00", named(id(theirs), 1)), "procurement.invalid_contract", "contract_id"},
		{"a contract in another currency", usd, "procurement.invalid_contract", "contract_id"},
		{"a contract item in another unit", e.catalogBody(e.Supplier, "K", "D", "1.00", named(id(active), 2)), "procurement.invalid_contract_item", "contract_item_no"},
		{"an item the contract does not have", e.catalogBody(e.Supplier, "K", "D", "1.00", named(id(active), 9)), "procurement.invalid_contract_item", "contract_item_no"},
		{"another amount than the contract item's", e.catalogBody(e.Supplier, "K", "D", "4.50", named(id(active), 1)), "procurement.catalog_item_price", "unit_price.amount"},
		{"another price unit than the contract item's", e.catalogBody(e.Supplier, "K", "D", "5.00", `, "price_unit": 2`+named(id(active), 1)), "procurement.catalog_item_price", "price_unit"},
		{"the same ratio at another price unit", e.catalogBody(e.Supplier, "K", "D", "10.00", `, "price_unit": 2`+named(id(active), 1)), "procurement.catalog_item_price", "unit_price.amount"},
	} {
		t.Run(r.name, func(t *testing.T) {
			refusal := e.in(t).Problem(422, r.code, "POST", "/v1/catalog-items", r.body, "")
			if got := apitest.String(t, apitest.List(t, refusal, "errors")[0].(map[string]any), "field"); got != r.field {
				t.Errorf("refused field: got %s, want %s", got, r.field)
			}
		})
	}
	// The refusal of a price names the catalog item, the contract item and both prices.
	refusal := e.Problem(422, "procurement.catalog_item_price", "POST", "/v1/catalog-items", e.catalogBody(e.Supplier, "PAPER-2", "Paper", "4.50", named(id(active), 1)), "")
	want := fmt.Sprintf("catalog item PAPER-2 is priced at 4.50 EUR per 1, but item 1 of contract %v prices it at 5.00 EUR per 1", active["document_number"])
	if detail := apitest.String(t, refusal, "detail"); detail != want {
		t.Errorf("the refusal of a price: got %q, want %q", detail, want)
	}
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM procurement.catalog_item WHERE tenant_id = $1`, []any{e.Tenant.ID}, &n)
	if n != 2 {
		t.Errorf("catalog items after the refusals: got %d, want 2 (the two at their contract items' prices)", n)
	}
	// A contract of another company is not one of this company's.
	other := strings.Replace(e.catalogBody(e.Supplier, "K", "D", "1.00", named(id(active), 1)), e.DE.String(), e.US.String(), 1)
	e.Problem(422, "procurement.invalid_contract", "POST", "/v1/catalog-items", other, "")
}

// TestCatalogItemRefusals (T84a): what is loaded is checked as an order's
// is: the company, the supplier, the unit, the price and the texts.
func TestCatalogItemRefusals(t *testing.T) {
	e := setup(t)
	blocked := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by)
		VALUES ($1, $2, '3000', 'Blocked', 'DE', 'EUR', gen_random_uuid(), 'blocked', $3, $3)`, e.Tenant.ID, blocked, e.Tenant.Human.ID)
	ok := e.catalogBody(e.Supplier, "K", "D", "1.00", "")
	for _, r := range []struct {
		name, body string
		status     int
		code       string
	}{
		{"a company that is not active", strings.Replace(ok, e.DE.String(), blocked.String(), 1), 422, "procurement.legal_entity_not_active"},
		{"no supplier with the id", e.catalogBody(uuid.New(), "K", "D", "1.00", ""), 422, "procurement.supplier_not_usable"},
		{"a key with white space around it", e.catalogBody(e.Supplier, " K", "D", "1.00", ""), 422, "procurement.invalid_item_key"},
		{"a description of white space", e.catalogBody(e.Supplier, "K", "  ", "1.00", ""), 422, "request.validation_failed"},
		{"a price of zero", e.catalogBody(e.Supplier, "K", "D", "0.00", ""), 422, "procurement.invalid_amount"},
		{"a price with the wrong decimals", e.catalogBody(e.Supplier, "K", "D", "1.000", ""), 400, "request.invalid_amount"},
		{"an unknown currency", strings.Replace(ok, `"EUR"`, `"XXA"`, 1), 422, "procurement.unknown_currency"},
		{"an unknown unit", strings.Replace(ok, `"EA"`, `"ZZZ"`, 1), 422, "procurement.unknown_unit"},
		{"a price unit of zero", e.catalogBody(e.Supplier, "K", "D", "1.00", `, "price_unit": 0`), 400, "request.invalid_body"},
		{"a field the contract does not have", e.catalogBody(e.Supplier, "K", "D", "1.00", `, "status": "retired"`), 400, "request.invalid_body"},
	} {
		t.Run(r.name, func(t *testing.T) {
			e.in(t).Problem(r.status, r.code, "POST", "/v1/catalog-items", r.body, "")
		})
	}
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM procurement.catalog_item WHERE tenant_id = $1`, []any{e.Tenant.ID}, &n)
	if n != 0 {
		t.Errorf("catalog items after the refusals: got %d, want none", n)
	}
	// Loading and retiring need the manage permission; searching the read one.
	c := e.catalogItem(t, e.Supplier, "K", "D", "1.00", "")
	pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = 'procurement.catalog.manage'`, e.Tenant.ID)
	e.Problem(403, "identity.permission_missing", "POST", "/v1/catalog-items", ok, "")
	e.Problem(403, "identity.permission_missing", "POST", "/v1/catalog-items/"+id(c)+":retire", "", `"1"`)
	e.Must(200, "GET", "/v1/catalog-items/"+id(c), "", "")
	pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = 'procurement.catalog.read'`, e.Tenant.ID)
	e.Problem(403, "identity.permission_missing", "GET", "/v1/catalog-items/"+id(c), "", "")
	e.Problem(403, "identity.permission_missing", "GET", fmt.Sprintf("/v1/catalog-items?legal_entity_id=%s", e.DE), "", "")
}
