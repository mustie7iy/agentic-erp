package mrp_test

import (
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestMaterialPlans (ADR-0084 §4): a material is planned at a plant once,
// an active stocked material extended to an active plant of the caller's
// companies, with a reorder point of zero or more in its base unit and a
// postable account of the company; it is read and listed by plant and
// material, and changed at its version, audited before and after.
func TestMaterialPlans(t *testing.T) {
	e := setup(t)
	p, tag := e.plan(t, e.bracket, "50.5")
	for f, want := range map[string]any{"plant_code": "P1", "plant_name": "Unit P1", "company_code": "2000", "material_description": "Machined bracket M10",
		"mrp_type": "reorder_point", "account_number": "150000", "version": float64(1)} {
		if p[f] != want {
			t.Errorf("%s: got %v, want %v", f, p[f], want)
		}
	}
	if rp := p["reorder_point"].(map[string]any); rp["value"] != "50.5" || rp["unit"] != "EA" {
		t.Errorf("reorder point: got %v, want 50.5 EA", rp)
	}
	id := p["id"].(string)
	e.Problem(409, "mrp.material_plan_exists", "POST", plans, mustJSON(t, map[string]any{"plant_id": e.p1, "material_id": e.bracket,
		"mrp_type": "no_planning", "reorder_point": "0", "account_id": e.stock}), "")
	t.Run("refused", func(t *testing.T) {
		for _, c := range []struct {
			name, code, field string
			body              map[string]any
		}{
			{"an unknown plant", "mrp.plant_invalid", "plant_id", map[string]any{"plant_id": uuid.New()}},
			{"a storage location", "mrp.plant_invalid", "plant_id", map[string]any{"plant_id": e.s1}},
			{"a plant the material is not kept at", "mrp.material_invalid", "material_id", map[string]any{"plant_id": e.p9}},
			{"a service", "mrp.material_invalid", "material_id", map[string]any{"material_id": e.service}},
			{"an unknown material", "mrp.material_invalid", "material_id", map[string]any{"material_id": uuid.New()}},
			{"an unknown MRP type", "mrp.invalid_mrp_type", "mrp_type", map[string]any{"mrp_type": "forecast"}},
			{"a reconciliation account", "mrp.account_not_postable", "account_id", map[string]any{"account_id": e.Account(t, "2000", "120000")}},
		} {
			t.Run(c.name, func(t *testing.T) {
				body := map[string]any{"plant_id": e.p1, "material_id": e.service, "mrp_type": "reorder_point", "reorder_point": "1", "account_id": e.stock}
				if c.code != "mrp.material_invalid" {
					body["material_id"] = e.material(t, "Bracket "+c.name, "raw_material")
				}
				for k, v := range c.body {
					body[k] = v
				}
				if got := field(e.Problem(422, c.code, "POST", plans, mustJSON(t, body), "")); got != c.field {
					t.Errorf("field: got %v, want %s", got, c.field)
				}
			})
		}
	})
	t.Run("read and listed", func(t *testing.T) {
		got, gotTag := e.Must(200, "GET", plans+"/"+id, "", "")
		if got["id"] != id || gotTag != tag {
			t.Errorf("get: got %v at %s, want the plan at %s", got["id"], gotTag, tag)
		}
		for q, want := range map[string]int{"?plant_id=" + e.p1.String(): 1, "?plant_id=" + e.p9.String(): 0, "?material_id=" + e.bracket: 1,
			"?material_id=" + e.service: 0, "": 1} {
			if page, _ := e.Must(200, "GET", plans+q, "", ""); len(rows(t, page, "items")) != want {
				t.Errorf("list %q: got %d, want %d", q, len(rows(t, page, "items")), want)
			}
		}
		e.Problem(404, "request.not_found", "GET", plans+"/"+uuid.NewString(), "", "")
	})
	t.Run("changed", func(t *testing.T) {
		e.Problem(422, "mrp.nothing_to_change", "PATCH", plans+"/"+id, `{"reorder_point": "50.50"}`, tag)
		e.Problem(412, "request.version_mismatch", "PATCH", plans+"/"+id, `{"reorder_point": "40"}`, `"9"`)
		got, _ := e.Must(200, "PATCH", plans+"/"+id, fmt.Sprintf(`{"reorder_point": "40", "mrp_type": "no_planning", "account_id": %q}`, e.stock), tag)
		if s := fmt.Sprint(got["mrp_type"], " ", got["reorder_point"].(map[string]any)["value"], " ", got["version"]); s != "no_planning 40 2" {
			t.Errorf("changed: got %s, want no_planning 40 2", s)
		}
		var detail string
		pgtest.Scan(t, `SELECT detail::text FROM audit.audit_event WHERE tenant_id = $1 AND action = 'mrp.material_plan.change'`, []any{e.Tenant.ID}, &detail)
		want := fmt.Sprintf(`{"after": {"mrp_type": "no_planning", "account_id": "%s", "reorder_point": "40"}, "before": {"mrp_type": "reorder_point", "account_id": "%s", "reorder_point": "50.5"}}`,
			e.stock, e.stock)
		if detail != want {
			t.Errorf("the audit: got %s, want %s", detail, want)
		}
	})
}

// TestLists (ADR-0084 §3, §4): plans and runs are listed a page at a
// time, filtered by plant and status, a run's lines by material number;
// a page token of another list is refused.
func TestLists(t *testing.T) {
	e := setup(t)
	other := e.material(t, "Angle bracket", "raw_material")
	e.plan(t, e.bracket, "250")
	e.plan(t, other, "0")
	first := e.run(t)
	lines := rows(t, first, "lines")
	if len(lines) != 2 {
		t.Fatalf("the run's lines: got %v, want two", lines)
	}
	if lines[0]["material_number"].(string) > lines[1]["material_number"].(string) {
		t.Errorf("the run's lines: got %v then %v, want them by material number", lines[0]["material_number"], lines[1]["material_number"])
	}
	e.run(t)
	for _, path := range []string{plans + "?plant_id=" + e.p1.String(), runs + "?status=completed&plant_id=" + e.p1.String()} {
		page, _ := e.Must(200, "GET", path+"&page_size=1", "", "")
		next, _ := page["next_page_token"].(string)
		if len(rows(t, page, "items")) != 1 || next == "" {
			t.Fatalf("%s: got %v, want one item and a next page", path, page)
		}
		if rest, _ := e.Must(200, "GET", path+"&page_size=1&page_token="+next, "", ""); len(rows(t, rest, "items")) != 1 || rest["next_page_token"] != nil {
			t.Errorf("%s: the next page got %v, want the last item", path, rest)
		}
		e.Problem(400, "request.invalid_page_token", "GET", path+"&page_token=x", "", "")
	}
	if page, _ := e.Must(200, "GET", runs+"?status=failed", "", ""); len(rows(t, page, "items")) != 0 {
		t.Errorf("failed runs: got %v, want none", rows(t, page, "items"))
	}
}
