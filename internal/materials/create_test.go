package materials_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestCreate: a material is numbered from its company's material range,
// created pending with its plant, storage location and alternative unit
// and its create change, read back with their codes and names, audited
// and published; approved, it is active.
func TestCreate(t *testing.T) {
	e := setup(t)
	got, tag := e.Must(201, "POST", path, e.bracket(t, map[string]any{"reason": "New bracket"}), "")
	fields(t, got, map[string]any{"number": "MA2000-100000", "company_code": "2000", "description": "Machined bracket M10", "base_unit": "EA",
		"base_unit_name": "Each", "material_type": "trading_good", "material_group": "BRACKETS", "status": "pending", "version": 1,
		"pending_change_number": "MC00000001"})
	if tag != `"1"` {
		t.Errorf("ETag: got %s, want \"1\"", tag)
	}
	if got, want := plants(t, got), "P1:S1"; got != want {
		t.Errorf("plants: got %q, want %q", got, want)
	}
	if got, want := units(t, got), "BOX 20/1"; got != want {
		t.Errorf("units: got %q, want %q", got, want)
	}
	p := rows(t, got, "plants")[0]
	fields(t, p, map[string]any{"plant_id": e.p1, "plant_name": "Unit P1", "legal_entity_id": e.DE, "company_code": "2000"})
	if name := rows(t, got, "alternative_units")[0]["unit_name"]; name != "Box" {
		t.Errorf("unit name: got %v, want Box", name)
	}
	id, change := got["id"].(string), got["pending_change_id"].(string)
	c, _ := e.Must(200, "GET", "/v1/material-changes/"+change, "", "")
	fields(t, c, map[string]any{"kind": "create", "status": "pending", "material_number": "MA2000-100000", "reason": "New bracket",
		"possible_duplicates": "[]"})
	if c["workflow_request_id"] != e.Pending(t, change)["id"] {
		t.Errorf("approval request: got %v, want the change's pending request", c["workflow_request_id"])
	}
	req := c["requested"].(map[string]any)
	fields(t, req, map[string]any{"description": "Machined bracket M10", "base_unit": "EA", "material_type": "trading_good"})
	if plant := rows(t, req, "plants")[0]; plant["plant_code"] != "P1" {
		t.Errorf("requested plant: got %v, want P1", plant)
	}
	e.Approve(t, change)
	read, _ := e.Must(200, "GET", path+"/"+id, "", "")
	fields(t, read, map[string]any{"status": "active", "version": 2, "pending_change_id": nil})
	events := e.events(t, "material_changed", id)
	if len(events) != 2 {
		t.Fatalf("events: got %d, want 2", len(events))
	}
	fields(t, events[0], map[string]any{"number": "MA2000-100000", "legal_entity_code": "2000", "change": "created", "status": "pending",
		"status_before": nil, "version": 1})
	fields(t, events[1], map[string]any{"change": "activated", "status": "active", "status_before": "pending", "version": 2})
	ep := events[0]["plants"].([]any)[0].(map[string]any)
	fields(t, ep, map[string]any{"plant_code": "P1", "legal_entity_code": "2000"})
	steps := e.events(t, "material_change_changed", id)
	if len(steps) != 2 {
		t.Fatalf("change events: got %d, want 2", len(steps))
	}
	fields(t, steps[0], map[string]any{"status": "pending", "status_before": nil, "kind": "create"})
	fields(t, steps[1], map[string]any{"status": "applied", "status_before": "pending"})
	audits := e.audits(t, id, "materials.material.create")
	if len(audits) != 1 {
		t.Fatalf("audit: got %d records, want 1", len(audits))
	}
	fields(t, audits[0], map[string]any{"number": "MA2000-100000", "alternative_units": "[BOX 20/1]", "material_change_id": change})
	second := e.create(t, map[string]any{"description": "Machined bracket M12", "plants": nil, "alternative_units": nil})
	fields(t, second, map[string]any{"number": "MA2000-100001", "plants": "[]", "alternative_units": "[]"})
}

// TestCreateRefusals: each refusal names the request's field and creates
// nothing.
func TestCreateRefusals(t *testing.T) {
	e := setup(t)
	other := e.orgUnit(t, e.DE, "plant", "P3", nil, true)
	elsewhere := e.orgUnit(t, e.DE, "storage_location", "S9", &other, true)
	inactive := e.orgUnit(t, e.DE, "storage_location", "S8", &e.p1, false)
	plant := func(p uuid.UUID, locs ...uuid.UUID) map[string]any {
		return map[string]any{"plant_id": p, "storage_location_ids": append([]uuid.UUID{}, locs...)}
	}
	unit := func(code string) map[string]any {
		return map[string]any{"unit": code, "numerator": 2, "denominator": 1}
	}
	for name, c := range map[string]struct {
		over   map[string]any
		status int
		code   string
		field  string
	}{
		"no company":                  {map[string]any{"legal_entity_id": uuid.New()}, 422, "request.reference_not_found", "legal_entity_id"},
		"a blank description":         {map[string]any{"description": "  "}, 422, "request.validation_failed", "description"},
		"an unknown type":             {map[string]any{"material_type": "gadget"}, 422, "materials.unknown_material_type", "material_type"},
		"an unknown base unit":        {map[string]any{"base_unit": "XX"}, 422, "materials.unknown_unit", "base_unit"},
		"an unknown unit":             {map[string]any{"alternative_units": []any{unit("XX")}}, 422, "materials.unknown_unit", "alternative_units[0].unit"},
		"the base unit converted":     {map[string]any{"alternative_units": []any{unit("EA")}}, 422, "materials.base_unit_conversion", "alternative_units[0].unit"},
		"a unit twice":                {map[string]any{"alternative_units": []any{unit("BOX"), unit("BOX")}}, 422, "materials.duplicate_unit", "alternative_units[1].unit"},
		"a factor of zero":            {map[string]any{"alternative_units": []any{map[string]any{"unit": "BOX", "numerator": 0, "denominator": 1}}}, 400, "request.invalid_body", ""},
		"a group in lower case":       {map[string]any{"material_group": "brackets"}, 400, "request.invalid_body", ""},
		"no plant":                    {map[string]any{"plants": []any{plant(uuid.New())}}, 422, "materials.plant_invalid", "plants[0].plant_id"},
		"an inactive plant":           {map[string]any{"plants": []any{plant(e.p9)}}, 422, "materials.plant_invalid", "plants[0].plant_id"},
		"a storage location as plant": {map[string]any{"plants": []any{plant(e.s1)}}, 422, "materials.plant_invalid", "plants[0].plant_id"},
		"a plant twice":               {map[string]any{"plants": []any{plant(e.p1), plant(e.p1)}}, 422, "materials.duplicate_plant", "plants[1].plant_id"},
		"another plant's location":    {map[string]any{"plants": []any{plant(e.p1, elsewhere)}}, 422, "materials.storage_location_invalid", "plants[0].storage_location_ids[0]"},
		"an inactive location":        {map[string]any{"plants": []any{plant(e.p1, inactive)}}, 422, "materials.storage_location_invalid", "plants[0].storage_location_ids[0]"},
		"a plant as location":         {map[string]any{"plants": []any{plant(e.p1, e.p2)}}, 422, "materials.storage_location_invalid", "plants[0].storage_location_ids[0]"},
		"a location twice":            {map[string]any{"plants": []any{plant(e.p1, e.s1, e.s1)}}, 422, "materials.duplicate_storage_location", "plants[0].storage_location_ids[1]"},
	} {
		t.Run(name, func(t *testing.T) {
			status, got, _ := e.Do("POST", path, e.bracket(t, c.over), "")
			if status != c.status || got["code"] != c.code {
				t.Fatalf("got %d %v, want %d %s", status, got["code"], c.status, c.code)
			}
			if c.field != "" && field(got) != c.field {
				t.Errorf("field: got %v, want %s", field(got), c.field)
			}
		})
	}
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM materials.material WHERE tenant_id = $1`, []any{e.Tenant.ID}, &n)
	if n != 0 {
		t.Errorf("materials: got %d, want none created", n)
	}
}

// TestCreateOutsideCompanies: a person who may act in company 2000 alone
// numbers no material from company 1000's range, nor extends one to its
// plants.
func TestCreateOutsideCompanies(t *testing.T) {
	e := setup(t)
	de := e.As(e.Reader(t, "Dana", e.DE))
	if got := de.Problem(422, "request.reference_not_found", "POST", path, e.bracket(t, map[string]any{"legal_entity_id": e.US}), ""); field(got) != "legal_entity_id" {
		t.Errorf("company 1000: got field %v, want legal_entity_id", field(got))
	}
	body := e.bracket(t, map[string]any{"plants": []any{map[string]any{"plant_id": e.p2}}})
	if got := de.Problem(422, "materials.plant_invalid", "POST", path, body, ""); field(got) != "plants[0].plant_id" {
		t.Errorf("plant of company 1000: got field %v, want plants[0].plant_id", field(got))
	}
}

// TestNumberTaken: a number another company's range issued is refused
// (409 materials.number_taken), and the material is not created.
func TestNumberTaken(t *testing.T) {
	e := setup(t)
	pgtest.Exec(t, `INSERT INTO materials.material (tenant_id, id, number, legal_entity_id, description, base_uom_code, material_type, material_group,
			status, created_by, updated_by) VALUES ($1, gen_random_uuid(), 'MA2000-100000', $2, 'Migrated', 'EA', 'service', 'B', 'active', $3, $3)`,
		e.Tenant.ID, e.US, e.Tenant.Human.ID)
	got := e.Problem(409, "materials.number_taken", "POST", path, e.bracket(t, nil), "")
	if detail := fmt.Sprint(got["detail"]); !strings.Contains(detail, "material ranges of two companies or years meet") {
		t.Errorf("detail: got %q, want it to say the ranges meet", detail)
	}
}
