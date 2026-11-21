package sales_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// emirates adds the output code U5, which levies the emirate each line
// names (AE-DU, AE-SH and AE-AZ), as the UAE pack's S5 does, to company
// 2000; with an establishment, the company's postal address is in that
// emirate.
func emirates(establishment string) func(*config.Config) {
	return func(doc *config.Config) {
		outputCodes(doc)
		de := doc.Company("2000")
		de.TaxCodes = append(de.TaxCodes, config.TaxCode{Code: "U5", Description: "Output VAT by emirate", Direction: config.TaxOutput,
			Components: []config.TaxComponent{{Key: "vat", AccountKey: config.KeyOutput, Account: "230000", Jurisdiction: "AE", JurisdictionOnLine: true,
				Subdivisions: []string{"AE-DU", "AE-SH", "AE-AZ"}, Rates: []config.TaxRate{{RatePercent: "5", ValidFrom: "2026-01-01"}}}}})
		if establishment != "" {
			de.PostalAddress = &config.PostalAddress{Line1: "Gulf Tower", City: "Dubai", Region: establishment, CountryCode: "AE"}
		}
	}
}

// plant inserts an active plant of company 2000 with the code and region
// ("" for none) and returns its id. It is written directly: tenancy holds
// a plant's region to its company's country (TestOrgUnitRegion), and the
// test company is German.
func (e env) plant(t *testing.T, code, region string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	var r *string
	if region != "" {
		r = &region
	}
	pgtest.Exec(t, `INSERT INTO tenancy.org_unit (tenant_id, id, legal_entity_id, kind, code, name, region, status, created_by, updated_by)
		VALUES ($1, $2, $3, 'plant', $4, $4, $5, 'active', $6, $6)`, e.Tenant.ID, id, e.DE, code, r, e.Tenant.Human.ID)
	return id
}

// stocked inserts an active material kept in stock (a trading good) of
// the base unit EA and returns its id.
func (e env) stocked(t *testing.T, number string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO materials.material (tenant_id, id, number, legal_entity_id, description, base_uom_code, material_type, material_group,
		status, created_by, updated_by) VALUES ($1, $2, $3, $4, 'Hinge H20', 'EA', 'trading_good', 'FASTENERS', 'active', $5, $5)`,
		e.Tenant.ID, id, number, e.DE, e.Tenant.Human.ID)
	return id
}

// auditDetail is the field of the audit record of the action on the
// object, as JSON text ("" for none).
func (e env) auditDetail(t *testing.T, action, object, field string) string {
	t.Helper()
	var out string
	pgtest.Scan(t, `SELECT coalesce((detail -> $4)::text, '') FROM audit.audit_event WHERE tenant_id = $1 AND action = $2 AND object_id = $3`,
		[]any{e.Tenant.ID, action, object, field}, &out)
	return out
}

// TestItemJurisdictionDefaults (product owner 2026-11-21; ADR-0063 As
// built, the emirate's default on a sales item): a UAE item that names
// no emirate takes the seller's, its order's plant's for an item of a
// material kept in stock, else the company's establishment; one named is
// kept and audited as differing from its default; another code's item
// takes none. The order delivers from the company's only plant.
func TestItemJurisdictionDefaults(t *testing.T) {
	e := setupWith(t, emirates("AE-DU"))
	plant := e.plant(t, "P1", "AE-SH")
	hinge := e.stocked(t, "100000020")
	items := []string{e.ofMaterial(hinge, "2 EA", `, "unit_price": {"amount": "3.00", "currency": "EUR"}, "tax_code": "U5"`),
		e.item(line{code: "U5"}), e.item(line{code: "U5", jurisdiction: "AE-AZ"}), e.item(line{code: "A1"})}
	order, _ := e.Must(201, "POST", "/v1/sales-orders", e.direct(e.customer, "", items...), "")
	if j := jurisdictions(t, order); j != "AE-SH AE-DU AE-AZ -" {
		t.Errorf("the order's items: got %s, want AE-SH AE-DU AE-AZ -: the stock item the plant's, the others the company's, the named one kept", j)
	}
	if order["plant_id"] != plant.String() {
		t.Errorf("the order's plant: got %v, want the company's only plant %s", order["plant_id"], plant)
	}
	id := order["id"].(string)
	want := `[{"default": "AE-SH", "item_no": 1, "overridden": false, "tax_jurisdiction": "AE-SH"}, ` +
		`{"default": "AE-DU", "item_no": 2, "overridden": false, "tax_jurisdiction": "AE-DU"}, ` +
		`{"default": "AE-DU", "item_no": 3, "overridden": true, "tax_jurisdiction": "AE-AZ"}]`
	if got := e.auditDetail(t, "sales.sales_order.create", id, "tax_jurisdiction_defaults"); got != want {
		t.Errorf("the creation's defaults: got %s, want %s", got, want)
	}
	if got := e.auditDetail(t, "sales.sales_order.create", id, "plant_defaulted"); got != "true" {
		t.Errorf("the plant defaulted: got %s, want true", got)
	}
	t.Run("an item an order change adds", func(t *testing.T) {
		added := e.ofMaterial(hinge, "1 EA", `, "unit_price": {"amount": "3.00", "currency": "EUR"}, "tax_code": "U5"`)
		changed, _ := e.Must(200, "POST", changing(id), fmt.Sprintf(`{"new_items": [%s]}`, added), e.version(t, id))
		if j := jurisdictions(t, changed); j != "AE-SH AE-DU AE-AZ - AE-SH" {
			t.Errorf("the order's items: got %s, want the added stock item in the plant's AE-SH", j)
		}
		if got := e.auditDetail(t, "sales.sales_order.change", id, "tax_jurisdiction_defaults"); got !=
			`[{"default": "AE-SH", "item_no": 5, "overridden": false, "tax_jurisdiction": "AE-SH"}]` {
			t.Errorf("the change's defaults: got %s, want item 5's", got)
		}
	})
	t.Run("a quotation, which has no plant", func(t *testing.T) {
		quoted, _ := e.Must(201, "POST", "/v1/quotations", e.body(e.customer, far, items[0]), "")
		if j := jurisdictions(t, quoted); j != "AE-DU" {
			t.Errorf("the quotation's stock item: got %s, want the company's AE-DU", j)
		}
		if got := e.auditDetail(t, "sales.quotation.create", quoted["id"].(string), "tax_jurisdiction_defaults"); got !=
			`[{"default": "AE-DU", "item_no": 1, "overridden": false, "tax_jurisdiction": "AE-DU"}]` {
			t.Errorf("the quotation's defaults: got %s, want item 1's", got)
		}
	})
	t.Run("a plant's region the code does not levy", func(t *testing.T) {
		other := e.plant(t, "P2", "AE-FU")
		p := e.Problem(422, "sales.tax_jurisdiction_required", "POST", "/v1/sales-orders",
			e.direct(e.customer, fmt.Sprintf(`, "plant_id": %q`, other), items[0]), "")
		if f := field(p); f != "items[0].tax_jurisdiction" {
			t.Errorf("the field: got %v, want items[0].tax_jurisdiction", f)
		}
	})
}

// TestItemJurisdictionWithoutEstablishment: a company without a postal
// address's region gives no default, so a UAE item without a plant's
// names its emirate (422 sales.tax_jurisdiction_required).
func TestItemJurisdictionWithoutEstablishment(t *testing.T) {
	e := setupWith(t, emirates(""))
	p := e.Problem(422, "sales.tax_jurisdiction_required", "POST", "/v1/sales-orders", e.direct(e.customer, "", e.item(line{code: "U5"})), "")
	if f := field(p); f != "items[0].tax_jurisdiction" {
		t.Errorf("the field: got %v, want items[0].tax_jurisdiction", f)
	}
}

// TestPlantRequired (product owner 2026-11-21; ADR-0082 As built S2j, the
// plant required): an order of a company with several plants names none
// unless it is given one, and is not confirmed while it has an item of a
// material kept in stock (422 sales.plant_required naming plant_id); an
// order of services alone is.
func TestPlantRequired(t *testing.T) {
	e := setup(t)
	e.plant(t, "P1", "")
	e.plant(t, "P2", "")
	hinge := e.stocked(t, "100000021")
	order, _ := e.Must(201, "POST", "/v1/sales-orders", e.direct(e.customer, "",
		e.item(line{}), e.ofMaterial(hinge, "2 EA", `, "unit_price": {"amount": "3.00", "currency": "EUR"}`)), "")
	if order["plant_id"] != nil {
		t.Errorf("the order's plant: got %v, want none of a company with two", order["plant_id"])
	}
	id := order["id"].(string)
	p := e.Problem(422, "sales.plant_required", "POST", "/v1/sales-orders/"+id+":confirm", "", `"1"`)
	if f := field(p); f != "plant_id" {
		t.Errorf("the field: got %v, want plant_id", f)
	}
	if got := e.auditDetail(t, "sales.sales_order.create", id, "plant_defaulted"); got != "false" {
		t.Errorf("the plant defaulted: got %s, want false", got)
	}
	t.Run("an order of services", func(t *testing.T) {
		confirmed, _ := e.confirmed(t, e.item(line{}))
		got, _ := e.Must(200, "GET", "/v1/sales-orders/"+confirmed, "", "")
		if got["status"] != "confirmed" {
			t.Errorf("the order of services: got %v, want confirmed", got["status"])
		}
		stock := e.ofMaterial(hinge, "1 EA", `, "unit_price": {"amount": "3.00", "currency": "EUR"}`)
		p := e.Problem(422, "sales.plant_required", "POST", changing(confirmed), fmt.Sprintf(`{"new_items": [%s]}`, stock), e.version(t, confirmed))
		if f := field(p); f != "plant_id" {
			t.Errorf("a stock item added to it: got field %v, want plant_id", f)
		}
		added, _ := e.Must(200, "POST", changing(confirmed), fmt.Sprintf(`{"new_items": [%s]}`, e.item(line{})), e.version(t, confirmed))
		if n := len(items(t, added)); n != 2 {
			t.Errorf("a service added to it: got %d items, want 2", n)
		}
	})
}

// TestChangeReadsThePlantInARequest: a change adding an item to an order
// with a plant reads the plant, its added items' jurisdictions defaulting
// from it, in the request's transaction, and fails as it does.
func TestChangeReadsThePlantInARequest(t *testing.T) {
	e := setupWith(t, emirates("AE-DU"))
	e.plant(t, "P1", "AE-SH")
	order, _ := e.Must(201, "POST", "/v1/sales-orders", e.direct(e.customer, "", e.item(line{})), "")
	id := uuid.MustParse(order["id"].(string))
	h := endpoints(t, time.Now())
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "add an item to an order with a plant", func(ctx context.Context) error {
		_, err := h.ChangeSalesOrder(ctx, &gen.SalesOrderChangeRequest{NewItems: []gen.SalesItemCreate{{Description: gen.NewOptString("Freight"),
			Quantity: gen.Quantity{Value: "1", Unit: "EA"}, UnitPrice: gen.NewOptMoney(gen.Money{Amount: "5.00", Currency: "EUR"}), AccountID: e.revenue,
			TaxCode: gen.NewOptString("U5")}}}, gen.ChangeSalesOrderParams{ID: id, IfMatch: `"1"`})
		return err
	})
}
