package materials_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var perms = []string{"materials.material.read", "materials.material.write", "tenancy.org.manage"}

const path = "/v1/materials"

// env is a configured tenant with a plant in each company: P1 of company
// 2000 with storage locations S1 and S2, P2 of company 1000 with S3, and
// an inactive plant P9 of company 2000.
type env struct {
	fixture.Env
	p1, p2, p9 uuid.UUID
	s1, s2, s3 uuid.UUID
}

func setup(t *testing.T) env {
	t.Helper()
	e := env{Env: fixture.New(t, perms...)}
	e.p1 = e.orgUnit(t, e.DE, "plant", "P1", nil, true)
	e.s1 = e.orgUnit(t, e.DE, "storage_location", "S1", &e.p1, true)
	e.s2 = e.orgUnit(t, e.DE, "storage_location", "S2", &e.p1, true)
	e.p2 = e.orgUnit(t, e.US, "plant", "P2", nil, true)
	e.s3 = e.orgUnit(t, e.US, "storage_location", "S3", &e.p2, true)
	e.p9 = e.orgUnit(t, e.DE, "plant", "P9", nil, false)
	return e
}

// orgUnit creates an org unit of the company through the API, active or
// left inactive.
func (e env) orgUnit(t *testing.T, le uuid.UUID, kind, code string, parent *uuid.UUID, active bool) uuid.UUID {
	t.Helper()
	body := map[string]any{"legal_entity_id": le, "kind": kind, "code": code, "name": "Unit " + code}
	if parent != nil {
		body["parent_id"] = *parent
	}
	got, _ := e.Must(201, "POST", "/v1/org-units", mustJSON(t, body), "")
	id := got["id"].(string)
	if active {
		e.Must(200, "POST", "/v1/org-units/"+id+":activate", "", `"1"`)
	}
	return uuid.MustParse(id)
}

// bracket is a material of company 2000 at plant P1 in S1, with a box of
// 20 as an alternative unit, with fields replaced or added by over.
func (e env) bracket(t *testing.T, over map[string]any) string {
	t.Helper()
	m := map[string]any{"legal_entity_id": e.DE, "description": "Machined bracket M10", "base_unit": "EA", "material_type": "trading_good",
		"material_group": "BRACKETS", "plants": []any{map[string]any{"plant_id": e.p1, "storage_location_ids": []any{e.s1}}},
		"alternative_units": []any{map[string]any{"unit": "BOX", "numerator": 20, "denominator": 1}}}
	for k, v := range over {
		if v == nil {
			delete(m, k)
			continue
		}
		m[k] = v
	}
	return mustJSON(t, m)
}

// create creates the material and returns it.
func (e env) create(t *testing.T, over map[string]any) map[string]any {
	t.Helper()
	got, _ := e.Must(201, "POST", path, e.bracket(t, over), "")
	return got
}

// rows are the objects of the array field of m.
func rows(t *testing.T, m map[string]any, field string) []map[string]any {
	t.Helper()
	raw, _ := m[field].([]any)
	out := make([]map[string]any, len(raw))
	for i, r := range raw {
		out[i], _ = r.(map[string]any)
	}
	return out
}

// plants are a material's plants as "code:location,location" each.
func plants(t *testing.T, m map[string]any) string {
	t.Helper()
	var out []string
	for _, p := range rows(t, m, "plants") {
		var locs []string
		for _, l := range rows(t, p, "storage_locations") {
			locs = append(locs, fmt.Sprint(l["code"]))
		}
		out = append(out, fmt.Sprintf("%v:%s", p["plant_code"], strings.Join(locs, ",")))
	}
	return strings.Join(out, " ")
}

// units are a material's alternative units as "unit num/den" each.
func units(t *testing.T, m map[string]any) string {
	t.Helper()
	var out []string
	for _, u := range rows(t, m, "alternative_units") {
		out = append(out, fmt.Sprintf("%v %v/%v", u["unit"], u["numerator"], u["denominator"]))
	}
	return strings.Join(out, " ")
}

// field is the field the problem's first field error names.
func field(m map[string]any) any {
	errs, _ := m["errors"].([]any)
	if len(errs) == 0 {
		return nil
	}
	e, _ := errs[0].(map[string]any)
	return e["field"]
}

// fields checks each named field of m, one message per field.
func fields(t *testing.T, m, want map[string]any) {
	t.Helper()
	for f, w := range want {
		if fmt.Sprint(m[f]) != fmt.Sprint(w) {
			t.Errorf("%s: got %v, want %v", f, m[f], w)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// endpoints are the context's endpoints as the API wires them.
func endpoints(t *testing.T) *materials.Endpoints {
	t.Helper()
	pages, err := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	return app.Materials(app.Config{Pages: pages, Now: time.Now})
}

// events are the payloads of MaterialChanged for the material, in the
// order they were written, each conforming to the event contract.
func (e env) events(t *testing.T, id string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, p := range apitest.Published(t, e.Tenant, outbox.Event{Context: "materials", Name: "material_changed"}) {
		if p["material_id"] == id {
			out = append(out, p)
		}
	}
	return out
}

// audits are the details of the material's audit records of the action,
// oldest first.
func (e env) audits(t *testing.T, id, action string) []map[string]any {
	t.Helper()
	var raw string
	pgtest.Scan(t, `SELECT coalesce(json_agg(detail ORDER BY seq), '[]')::text FROM audit.audit_event
		WHERE tenant_id = $1 AND object_id = $2 AND action = $3`, []any{e.Tenant.ID, id, action}, &raw)
	var out []map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (e env) inRequest(t *testing.T, fn func(ctx context.Context) error) error {
	t.Helper()
	return apitest.InRequest(t, e.Tenant, e.Tenant.Human, fn)
}
