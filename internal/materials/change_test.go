package materials_test

import (
	"fmt"
	"testing"

	"github.com/google/uuid"
)

// TestChange: a change at the material's version sets its description,
// group and status, replaces its plants and units (a storage location and
// a unit removed, another plant added, a factor changed), is audited with
// the values before and after and published; a stale version is refused
// and nothing changes.
func TestChange(t *testing.T) {
	e := setup(t)
	m := e.create(t, nil)
	id := m["id"].(string)
	body := mustJSON(t, map[string]any{"description": "Machined bracket M10, zinc", "material_group": "FASTENERS", "status": "blocked",
		"plants":            []any{map[string]any{"plant_id": e.p1, "storage_location_ids": []any{e.s2, e.s1}}, map[string]any{"plant_id": e.p2, "storage_location_ids": []any{e.s3}}},
		"alternative_units": []any{map[string]any{"unit": "BOX", "numerator": 24, "denominator": 1}, map[string]any{"unit": "KG", "numerator": 50, "denominator": 3}}})
	got, tag := e.Must(200, "PATCH", path+"/"+id, body, `"1"`)
	fields(t, got, map[string]any{"description": "Machined bracket M10, zinc", "material_group": "FASTENERS", "status": "blocked", "version": 2,
		"number": "MA2000-100000", "base_unit": "EA"})
	if tag != `"2"` {
		t.Errorf("ETag: got %s, want \"2\"", tag)
	}
	if got, want := plants(t, got), "P1:S1,S2 P2:S3"; got != want {
		t.Errorf("plants: got %q, want %q", got, want)
	}
	if got, want := units(t, got), "BOX 24/1 KG 50/3"; got != want {
		t.Errorf("units: got %q, want %q", got, want)
	}
	audits := e.audits(t, id, "materials.material.change")
	if len(audits) != 1 {
		t.Fatalf("audit: got %d records, want 1", len(audits))
	}
	fields(t, audits[0], map[string]any{"fields": "[description material_group status plants alternative_units]"})
	before, after := audits[0]["before"].(map[string]any), audits[0]["after"].(map[string]any)
	fields(t, before, map[string]any{"status": "active", "material_group": "BRACKETS", "alternative_units": "map[BOX:[20 1]]"})
	fields(t, after, map[string]any{"status": "blocked", "material_group": "FASTENERS", "alternative_units": "map[BOX:[24 1] KG:[50 3]]"})
	events := e.events(t, id)
	if len(events) != 2 {
		t.Fatalf("events: got %d, want 2", len(events))
	}
	fields(t, events[1], map[string]any{"change": "changed", "status": "blocked", "status_before": "active", "version": 2})
	e.Problem(412, "request.version_mismatch", "PATCH", path+"/"+id, `{"description": "Again"}`, `"1"`)
	again, _ := e.Must(200, "GET", path+"/"+id, "", "")
	fields(t, again, map[string]any{"description": "Machined bracket M10, zinc", "version": 2})
}

// TestChangeBack: a plant, storage location and unit removed are given
// back by a later change, and empty lists remove every plant and unit.
func TestChangeBack(t *testing.T) {
	e := setup(t)
	m := e.create(t, nil)
	id := m["id"].(string)
	e.Must(200, "PATCH", path+"/"+id, `{"plants": [], "alternative_units": []}`, `"1"`)
	cleared, _ := e.Must(200, "GET", path+"/"+id, "", "")
	if plants(t, cleared) != "" || units(t, cleared) != "" {
		t.Errorf("cleared: got plants %q, units %q, want none", plants(t, cleared), units(t, cleared))
	}
	back, _ := e.Must(200, "PATCH", path+"/"+id, e.bracket(t, map[string]any{"legal_entity_id": nil, "base_unit": nil, "material_type": nil,
		"description": nil, "material_group": nil}), `"2"`)
	if got, want := fmt.Sprint(plants(t, back), " | ", units(t, back)), "P1:S1 | BOX 20/1"; got != want {
		t.Errorf("given back: got %q, want %q", got, want)
	}
}

// TestChangeRefusals: a change that sets nothing new, an unknown status, a
// field that does not change and the creation's checks are refused.
func TestChangeRefusals(t *testing.T) {
	e := setup(t)
	id := e.create(t, nil)["id"].(string)
	for name, c := range map[string]struct {
		body   string
		status int
		code   string
		field  string
	}{
		"nothing new":     {`{"description": "Machined bracket M10", "status": "active"}`, 422, "materials.nothing_to_change", ""},
		"an empty patch":  {`{}`, 422, "materials.nothing_to_change", ""},
		"pending":         {`{"status": "pending"}`, 422, "materials.unknown_status", "status"},
		"a blank text":    {`{"description": " "}`, 422, "request.validation_failed", "description"},
		"the base unit":   {`{"base_unit": "PC"}`, 400, "request.invalid_body", ""},
		"the type":        {`{"material_type": "service"}`, 400, "request.invalid_body", ""},
		"a unit twice":    {`{"alternative_units": [{"unit": "KG", "numerator": 1, "denominator": 1}, {"unit": "KG", "numerator": 2, "denominator": 1}]}`, 422, "materials.duplicate_unit", "alternative_units[1].unit"},
		"an unknown unit": {`{"alternative_units": [{"unit": "XX", "numerator": 1, "denominator": 1}]}`, 422, "materials.unknown_unit", "alternative_units[0].unit"},
		"an inactive plant": {mustJSON(t, map[string]any{"plants": []any{map[string]any{"plant_id": e.p9}}}), 422, "materials.plant_invalid",
			"plants[0].plant_id"},
		"no such material": {`{"description": "X"}`, 404, "request.not_found", ""},
	} {
		t.Run(name, func(t *testing.T) {
			target := path + "/" + id
			if name == "no such material" {
				target = path + "/" + uuid.NewString()
			}
			status, got, _ := e.Do("PATCH", target, c.body, `"1"`)
			if status != c.status || got["code"] != c.code {
				t.Fatalf("got %d %v, want %d %s", status, got["code"], c.status, c.code)
			}
			if c.field != "" && field(got) != c.field {
				t.Errorf("field: got %v, want %s", field(got), c.field)
			}
		})
	}
	unchanged, _ := e.Must(200, "GET", path+"/"+id, "", "")
	fields(t, unchanged, map[string]any{"version": 1, "status": "active"})
}

// TestChangeKeepsOtherCompanies: a person who may act in company 2000
// alone sees and replaces its plants only; the plant of company 1000
// stays.
func TestChangeKeepsOtherCompanies(t *testing.T) {
	e := setup(t)
	id := e.create(t, map[string]any{"plants": []any{map[string]any{"plant_id": e.p1, "storage_location_ids": []any{e.s1}},
		map[string]any{"plant_id": e.p2, "storage_location_ids": []any{e.s3}}}})["id"].(string)
	de := e.As(e.Reader(t, "Dana", e.DE))
	seen, _ := de.Must(200, "GET", path+"/"+id, "", "")
	if got := plants(t, seen); got != "P1:S1" {
		t.Errorf("company 2000's view: got %q, want P1:S1", got)
	}
	de.Problem(422, "materials.nothing_to_change", "PATCH", path+"/"+id, mustJSON(t, map[string]any{"plants": []any{map[string]any{"plant_id": e.p1,
		"storage_location_ids": []any{e.s1}}}}), `"1"`)
	de.Must(200, "PATCH", path+"/"+id, `{"plants": []}`, `"1"`)
	all, _ := e.Must(200, "GET", path+"/"+id, "", "")
	if got := plants(t, all); got != "P2:S3" {
		t.Errorf("after company 2000's change: got %q, want company 1000's P2:S3 kept", got)
	}
}
