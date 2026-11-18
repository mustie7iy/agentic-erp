package materials_test

import (
	"fmt"
	"testing"

	"github.com/google/uuid"
)

// TestChange: a change at the material's version opens a pending change,
// which leaves the material as it is until it is approved; approved, it
// sets the description, group and status and replaces the plants and
// units (a storage location and a unit removed, another plant added, a
// factor changed). The request is audited with the values before and
// after, each step published; a stale version is refused.
func TestChange(t *testing.T) {
	e := setup(t)
	m := e.active(t, nil)
	id := m["id"].(string)
	body := mustJSON(t, map[string]any{"description": "Machined bracket M10, zinc", "material_group": "FASTENERS", "reason": "Zinc plated now",
		"plants":            []any{map[string]any{"plant_id": e.p1, "storage_location_ids": []any{e.s2, e.s1}}, map[string]any{"plant_id": e.p2, "storage_location_ids": []any{e.s3}}},
		"alternative_units": []any{map[string]any{"unit": "BOX", "numerator": 24, "denominator": 1}, map[string]any{"unit": "KG", "numerator": 50, "denominator": 3}}})
	c := e.change(t, id, body)
	fields(t, c, map[string]any{"kind": "update", "status": "pending", "reason": "Zinc plated now", "material_description": "Machined bracket M10"})
	if got := rows(t, c["requested"].(map[string]any), "plants"); len(got) != 2 || got[1]["plant_code"] != "P2" {
		t.Errorf("requested plants: got %v, want P1 and P2", got)
	}
	waiting, _ := e.Must(200, "GET", path+"/"+id, "", "")
	fields(t, waiting, map[string]any{"description": "Machined bracket M10", "version": 2, "pending_change_id": c["id"]})
	audits := e.audits(t, c["id"].(string), "materials.change.request")
	if len(audits) != 1 {
		t.Fatalf("audit: got %d records, want 1", len(audits))
	}
	fields(t, audits[0], map[string]any{"fields": "[description material_group plants alternative_units]"})
	before, after := audits[0]["before"].(map[string]any), audits[0]["after"].(map[string]any)
	fields(t, before, map[string]any{"material_group": "BRACKETS", "alternative_units": "map[BOX:[20 1]]"})
	fields(t, after, map[string]any{"material_group": "FASTENERS", "alternative_units": "map[BOX:[24 1] KG:[50 3]]"})
	e.Approve(t, c["id"].(string))
	got, tag := e.Must(200, "GET", path+"/"+id, "", "")
	fields(t, got, map[string]any{"description": "Machined bracket M10, zinc", "material_group": "FASTENERS", "status": "active", "version": 3,
		"number": "MA2000-100000", "base_unit": "EA", "pending_change_id": nil})
	if tag != `"3"` {
		t.Errorf("ETag: got %s, want \"3\"", tag)
	}
	if got, want := plants(t, got), "P1:S1,S2 P2:S3"; got != want {
		t.Errorf("plants: got %q, want %q", got, want)
	}
	if got, want := units(t, got), "BOX 24/1 KG 50/3"; got != want {
		t.Errorf("units: got %q, want %q", got, want)
	}
	events := e.events(t, "material_changed", id)
	if n := len(events); n != 3 {
		t.Fatalf("events: got %d, want 3", n)
	}
	fields(t, events[2], map[string]any{"change": "changed", "status": "active", "status_before": "active", "version": 3})
	e.Problem(412, "request.version_mismatch", "PATCH", path+"/"+id, `{"description": "Again"}`, `"2"`)
}

// TestBlockAndUnblock: blocking applies at once, with an applied change of
// kind block; unblocking waits for its approval.
func TestBlockAndUnblock(t *testing.T) {
	e := setup(t)
	id := e.active(t, nil)["id"].(string)
	got, tag := e.Must(200, "PATCH", path+"/"+id, `{"status": "blocked", "reason": "Recalled"}`, `"2"`)
	fields(t, got, map[string]any{"status": "blocked", "version": 3, "pending_change_id": nil})
	if tag != `"3"` {
		t.Errorf("ETag: got %s, want \"3\"", tag)
	}
	page, _ := e.Must(200, "GET", "/v1/material-changes?material_id="+id+"&kind=block", "", "")
	if items := rows(t, page, "items"); len(items) != 1 || items[0]["status"] != "applied" || items[0]["applied_at"] == nil || items[0]["reason"] != "Recalled" {
		t.Errorf("block change: got %v, want one applied, with its reason", items)
	}
	events := e.events(t, "material_changed", id)
	fields(t, events[len(events)-1], map[string]any{"change": "blocked", "status": "blocked", "status_before": "active"})
	c := e.change(t, id, `{"status": "active"}`)
	fields(t, c, map[string]any{"kind": "unblock", "status": "pending"})
	e.Approve(t, c["id"].(string))
	again, _ := e.Must(200, "GET", path+"/"+id, "", "")
	fields(t, again, map[string]any{"status": "active", "version": 4})
	events = e.events(t, "material_changed", id)
	fields(t, events[len(events)-1], map[string]any{"change": "unblocked", "status": "active", "status_before": "blocked"})
}

// TestChangeRejected: a rejected change and a withdrawn one leave the
// material as it was, and free it for the next.
func TestChangeRejected(t *testing.T) {
	e := setup(t)
	id := e.active(t, nil)["id"].(string)
	c := e.change(t, id, `{"material_group": "FASTENERS"}`)
	r := e.Pending(t, c["id"].(string))
	e.As(e.Approver).Must(200, "POST", "/v1/approval-requests/"+r["id"].(string)+":reject", `{"comment": "Stays a bracket"}`, `"1"`)
	got, _ := e.Must(200, "GET", "/v1/material-changes/"+c["id"].(string), "", "")
	fields(t, got, map[string]any{"status": "rejected", "applied_at": nil})
	m, _ := e.Must(200, "GET", path+"/"+id, "", "")
	fields(t, m, map[string]any{"material_group": "BRACKETS", "version": 2, "pending_change_id": nil})
	w := e.change(t, id, `{"material_group": "FASTENERS"}`)
	r = e.Pending(t, w["id"].(string))
	e.Must(200, "POST", "/v1/approval-requests/"+r["id"].(string)+":cancel", `{"comment": "Asked too early"}`, `"1"`)
	withdrawn, _ := e.Must(200, "GET", "/v1/material-changes/"+w["id"].(string), "", "")
	fields(t, withdrawn, map[string]any{"status": "cancelled"})
	steps := e.events(t, "material_change_changed", id)
	fields(t, steps[len(steps)-1], map[string]any{"status": "cancelled", "status_before": "pending", "kind": "update"})
	m, _ = e.Must(200, "GET", path+"/"+id, "", "")
	fields(t, m, map[string]any{"material_group": "BRACKETS", "version": 2})
}

// TestChangeBack: a plant, storage location and unit removed are given
// back by a later change, and empty lists remove every plant and unit.
func TestChangeBack(t *testing.T) {
	e := setup(t)
	id := e.active(t, nil)["id"].(string)
	cleared := e.changed(t, id, `{"plants": [], "alternative_units": []}`)
	if plants(t, cleared) != "" || units(t, cleared) != "" {
		t.Errorf("cleared: got plants %q, units %q, want none", plants(t, cleared), units(t, cleared))
	}
	back := e.changed(t, id, e.bracket(t, map[string]any{"legal_entity_id": nil, "base_unit": nil, "material_type": nil, "description": nil,
		"material_group": nil}))
	if got, want := fmt.Sprint(plants(t, back), " | ", units(t, back)), "P1:S1 | BOX 20/1"; got != want {
		t.Errorf("given back: got %q, want %q", got, want)
	}
}

// TestChangeRefusals: a change that sets nothing new, an unknown status, a
// field that does not change and the creation's checks are refused, as are
// a change of a pending material and a second pending change.
func TestChangeRefusals(t *testing.T) {
	e := setup(t)
	id := e.active(t, nil)["id"].(string)
	pending := e.create(t, map[string]any{"description": "Pending bracket"})["id"].(string)
	for name, c := range map[string]struct {
		id     string
		body   string
		status int
		code   string
		field  string
	}{
		"nothing new":     {id, `{"description": "Machined bracket M10", "status": "active"}`, 422, "materials.nothing_to_change", ""},
		"an empty patch":  {id, `{}`, 422, "materials.nothing_to_change", ""},
		"pending":         {id, `{"status": "pending"}`, 422, "materials.unknown_status", "status"},
		"a blank text":    {id, `{"description": " "}`, 422, "request.validation_failed", "description"},
		"the base unit":   {id, `{"base_unit": "PC"}`, 400, "request.invalid_body", ""},
		"the type":        {id, `{"material_type": "service"}`, 400, "request.invalid_body", ""},
		"a unit twice":    {id, `{"alternative_units": [{"unit": "KG", "numerator": 1, "denominator": 1}, {"unit": "KG", "numerator": 2, "denominator": 1}]}`, 422, "materials.duplicate_unit", "alternative_units[1].unit"},
		"an unknown unit": {id, `{"alternative_units": [{"unit": "XX", "numerator": 1, "denominator": 1}]}`, 422, "materials.unknown_unit", "alternative_units[0].unit"},
		"an inactive plant": {id, mustJSON(t, map[string]any{"plants": []any{map[string]any{"plant_id": e.p9}}}), 422, "materials.plant_invalid",
			"plants[0].plant_id"},
		"no such material":   {uuid.NewString(), `{"description": "X"}`, 404, "request.not_found", ""},
		"a pending material": {pending, `{"description": "X"}`, 409, "materials.invalid_status", ""},
	} {
		t.Run(name, func(t *testing.T) {
			version := `"2"`
			if c.id == pending {
				version = `"1"`
			}
			status, got, _ := e.Do("PATCH", path+"/"+c.id, c.body, version)
			if status != c.status || got["code"] != c.code {
				t.Fatalf("got %d %v, want %d %s", status, got["code"], c.status, c.code)
			}
			if c.field != "" && field(got) != c.field {
				t.Errorf("field: got %v, want %s", field(got), c.field)
			}
		})
	}
	e.change(t, id, `{"description": "First"}`)
	e.Problem(409, "materials.change_pending", "PATCH", path+"/"+id, `{"description": "Second"}`, `"2"`)
	unchanged, _ := e.Must(200, "GET", path+"/"+id, "", "")
	fields(t, unchanged, map[string]any{"version": 2, "status": "active", "description": "Machined bracket M10"})
}

// TestChangeKeepsOtherCompanies: a person who may act in company 2000
// alone sees and replaces its plants only; approved by a person who may
// act in every company, the change keeps the plant of company 1000.
func TestChangeKeepsOtherCompanies(t *testing.T) {
	e := setup(t)
	id := e.active(t, map[string]any{"plants": []any{map[string]any{"plant_id": e.p1, "storage_location_ids": []any{e.s1}},
		map[string]any{"plant_id": e.p2, "storage_location_ids": []any{e.s3}}}})["id"].(string)
	de := e.As(e.Reader(t, "Dana", e.DE))
	seen, _ := de.Must(200, "GET", path+"/"+id, "", "")
	if got := plants(t, seen); got != "P1:S1" {
		t.Errorf("company 2000's view: got %q, want P1:S1", got)
	}
	de.Problem(422, "materials.nothing_to_change", "PATCH", path+"/"+id, mustJSON(t, map[string]any{"plants": []any{map[string]any{"plant_id": e.p1,
		"storage_location_ids": []any{e.s1}}}}), `"2"`)
	c, _ := de.Must(200, "PATCH", path+"/"+id, `{"plants": []}`, `"2"`)
	e.Approve(t, c["pending_change_id"].(string))
	all, _ := e.Must(200, "GET", path+"/"+id, "", "")
	if got := plants(t, all); got != "P2:S3" {
		t.Errorf("after company 2000's change: got %q, want company 1000's P2:S3 kept", got)
	}
}

// TestBlockWhileChangePending: a block applies while a change waits, and
// the change, approved later, keeps the block: it writes only the fields
// it changes (a status given as it was is not one of them).
func TestBlockWhileChangePending(t *testing.T) {
	e := setup(t)
	id := e.active(t, nil)["id"].(string)
	c := e.change(t, id, `{"description": "Machined bracket M10, zinc", "status": "active"}`)
	if req := c["requested"].(map[string]any); req["status"] != nil || req["description"] != "Machined bracket M10, zinc" {
		t.Errorf("requested: got %v, want the description alone", req)
	}
	blocked, _ := e.Must(200, "PATCH", path+"/"+id, `{"status": "blocked"}`, `"2"`)
	fields(t, blocked, map[string]any{"status": "blocked", "version": 3, "pending_change_id": c["id"]})
	e.Approve(t, c["id"].(string))
	after, _ := e.Must(200, "GET", path+"/"+id, "", "")
	fields(t, after, map[string]any{"status": "blocked", "description": "Machined bracket M10, zinc", "version": 4})
}

// TestChangeSeenByCompany: a person who may act in company 2000 alone sees
// a change's plants of that company only, each with its company and
// storage locations by code, and finds no material by another company's
// plant.
func TestChangeSeenByCompany(t *testing.T) {
	e := setup(t)
	m := e.create(t, map[string]any{"plants": []any{map[string]any{"plant_id": e.p1, "storage_location_ids": []any{e.s1}},
		map[string]any{"plant_id": e.p2, "storage_location_ids": []any{e.s3}}}})
	de := e.As(e.Reader(t, "Dana", e.DE))
	c, _ := de.Must(200, "GET", "/v1/material-changes/"+m["pending_change_id"].(string), "", "")
	got := rows(t, c["requested"].(map[string]any), "plants")
	if len(got) != 1 {
		t.Fatalf("plants: got %v, want company 2000's alone", got)
	}
	fields(t, got[0], map[string]any{"plant_code": "P1", "company_code": "2000"})
	if l := rows(t, got[0], "storage_locations"); len(l) != 1 || l[0]["code"] != "S1" || l[0]["name"] != "Unit S1" {
		t.Errorf("storage locations: got %v, want S1 by code and name", l)
	}
	page, _ := de.Must(200, "GET", path+"?plant_id="+e.p2.String(), "", "")
	if n := len(rows(t, page, "items")); n != 0 {
		t.Errorf("by company 1000's plant: got %d materials, want none", n)
	}
}
