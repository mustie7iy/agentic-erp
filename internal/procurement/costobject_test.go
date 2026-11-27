package procurement_test

import (
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
)

// TestCostObjectFields: a purchase order's cost object custom fields name
// centers of the order's company valid on its order date, checked when the
// order is created, updated and changed (ADR-0046 As built T56k); the
// centers' locks hold back postings, not references.
func TestCostObjectFields(t *testing.T) {
	e := setupWith(t, fixture.CostObjectFields)
	c := e.CostCenters(t, fixture.GRIRAccounts)
	foreign := e.ForeignProfitCenter(t)
	order := func(t *testing.T, day, ext string) string {
		t.Helper()
		return fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "currency": "EUR", "order_date": %q, "extensions": %s, "items": %s}`,
			e.DE, e.Supplier, day, ext, items(item(e.office, "1", "EA", "1.00", "EUR", "")))
	}
	t.Run("named", func(t *testing.T) {
		e := e.in(t)
		ext := fmt.Sprintf(`{"cost_center": %q, "profit_center": %q}`, c.Locked, c.Profit)
		o, _ := e.Must(201, "POST", "/v1/purchase-orders", order(t, "2026-03-10", ext), "")
		got, _ := o["extensions"].(map[string]any)
		if got["cost_center"] != c.Locked.String() || got["profit_center"] != c.Profit.String() {
			t.Errorf("extensions: got %v, want %s", got, ext)
		}
	})
	t.Run("refused at creation", func(t *testing.T) {
		for name, x := range map[string]struct {
			day, ext, field, code string
		}{
			"an unknown cost center":            {"2026-03-10", fmt.Sprintf(`{"cost_center": %q}`, uuid.New()), "extensions.cost_center", "not_found"},
			"a profit center as a cost center":  {"2026-03-10", fmt.Sprintf(`{"cost_center": %q}`, c.Profit), "extensions.cost_center", "not_found"},
			"another company's profit center":   {"2026-03-10", fmt.Sprintf(`{"profit_center": %q}`, foreign), "extensions.profit_center", "not_in_company"},
			"before the cost center's validity": {"2025-12-31", fmt.Sprintf(`{"cost_center": %q}`, c.Admin), "extensions.cost_center", "not_valid_on_date"},
		} {
			t.Run(name, func(t *testing.T) {
				p := e.in(t).Problem(422, "request.invalid_extension", "POST", "/v1/purchase-orders", order(t, x.day, x.ext), "")
				wantField(t, p, x.field, x.code)
			})
		}
	})
	t.Run("an update on the order's new date", func(t *testing.T) {
		e := e.in(t)
		o, tag := e.Must(201, "POST", "/v1/purchase-orders", order(t, "2026-03-10", fmt.Sprintf(`{"cost_center": %q}`, c.Admin)), "")
		p := e.Problem(422, "request.invalid_extension", "PATCH", "/v1/purchase-orders/"+id(o), `{"order_date": "2025-12-31"}`, tag)
		wantField(t, p, "extensions.cost_center", "not_valid_on_date")
	})
	t.Run("a change of a released order", func(t *testing.T) {
		e := e.in(t)
		o := e.order(t, items(centered(e.office, "1.00", c.Admin, "")), fmt.Sprintf(`, "extensions": {"cost_center": %q}`, c.Admin))
		path := "/v1/purchase-orders/" + id(o)
		e.act(t, path, "release", "")
		p := e.Problem(422, "request.invalid_extension", "POST", path+"/changes", fmt.Sprintf(`{"reason": "r", "extensions": {"profit_center": %q}}`, foreign), "")
		wantField(t, p, "extensions.profit_center", "not_in_company")
		ch, tag := e.Must(201, "POST", path+"/changes", fmt.Sprintf(`{"reason": "r", "extensions": {"cost_center": %q}}`, c.Sales), "")
		if ch, _ = e.Must(200, "POST", path+"/changes/"+id(ch)+":submit", "", tag); ch["status"] != "applied" {
			t.Fatalf("change: got %v, want applied", ch["status"])
		}
		got, _ := e.Must(200, "GET", path, "", "")
		if ext, _ := got["extensions"].(map[string]any); ext["cost_center"] != c.Sales.String() {
			t.Errorf("extensions: got %v, want cost center %s", ext, c.Sales)
		}
	})
}
