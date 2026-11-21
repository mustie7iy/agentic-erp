package tenancy_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// TestOrgUnitRegion (product owner 2026-11-21; migration 000431, ADR-0082
// As built S2j, the plant required): a plant names its region, a
// subdivision of its company's country (DE-BY for a German company), on
// its creation and by an update, which null clears; another kind, or a
// region of another country, is refused (422 tenancy.invalid_region
// naming region). The directory reads it with the unit, and reads a
// company's only active plant.
func TestOrgUnitRegion(t *testing.T) {
	ten := org(t)
	c := newClient(t, ten, ten.Human)
	de := c.legalEntity("DE01")
	plant, _ := c.Must(201, "POST", "/v1/org-units", fmt.Sprintf(`{"legal_entity_id": %q, "kind": "plant", "code": "P1", "name": "Munich", "region": "DE-BY"}`,
		de["id"]), "")
	if plant["region"] != "DE-BY" {
		t.Errorf("the plant's region: got %v, want DE-BY", plant["region"])
	}
	for name, tc := range map[string]struct{ kind, region, code string }{
		"a storage location's":   {"storage_location", "DE-BY", "not_allowed"},
		"another country's":      {"plant", "AE-DU", "invalid"},
		"the country's own code": {"plant", "DE-DE", ""},
	} {
		t.Run(name, func(t *testing.T) {
			status, m, _ := c.in(t).Do("POST", "/v1/org-units", fmt.Sprintf(`{"legal_entity_id": %q, "kind": %q, "code": "X1", "name": "X", "region": %q}`,
				de["id"], tc.kind, tc.region), "")
			if tc.code == "" {
				if status != 201 {
					t.Errorf("got %d %v, want 201", status, m)
				}
				return
			}
			if status != 422 || m["code"] != "tenancy.invalid_region" || field(m) != "region" || code(m) != tc.code {
				t.Errorf("got %d %v %v %v, want 422 tenancy.invalid_region naming region, %s", status, m["code"], field(m), code(m), tc.code)
			}
		})
	}
	id := plant["id"].(string)
	status, m, _ := c.Do("PATCH", "/v1/org-units/"+id, `{"region": "FR-IDF"}`, `"1"`)
	if status != 422 || m["code"] != "tenancy.invalid_region" {
		t.Errorf("another country's region by an update: got %d %v, want 422 tenancy.invalid_region", status, m["code"])
	}
	updated, _ := c.Must(200, "PATCH", "/v1/org-units/"+id, `{"region": "DE-BE"}`, `"1"`)
	if updated["region"] != "DE-BE" {
		t.Errorf("the updated region: got %v, want DE-BE", updated["region"])
	}
	var changedFields string
	pgtest.Scan(t, `SELECT (detail -> 'changed_fields')::text FROM audit.audit_event WHERE tenant_id = $1 AND action = 'tenancy.org_unit.update' AND object_id = $2`,
		[]any{ten.ID, id}, &changedFields)
	if changedFields != `["region"]` {
		t.Errorf("the update's audit record: got %s, want [\"region\"]", changedFields)
	}
	cleared, _ := c.Must(200, "PATCH", "/v1/org-units/"+id, `{"region": null}`, `"2"`)
	if cleared["region"] != nil {
		t.Errorf("the cleared region: got %v, want none", cleared["region"])
	}
	c.Must(200, "PATCH", "/v1/org-units/"+id, `{"region": "DE-BY"}`, `"3"`)
	onlyPlant(t, ten, c, de, uuid.MustParse(id))
}

// onlyPlant: the directory reads a company's only active plant with its
// region, none while it has no active plant, and none once it has two.
func onlyPlant(t *testing.T, ten pgtest.Tenant, c client, de map[string]any, plant uuid.UUID) {
	t.Helper()
	le := uuid.MustParse(de["id"].(string))
	read := func(want bool, region string) {
		t.Helper()
		if err := apitest.InRequest(t, ten, ten.Human, func(ctx context.Context) error {
			u, ok, err := tenancy.Directory{}.OnlyPlant(ctx, le)
			if ok != want || (ok && (u.ID != plant || u.Region == nil || *u.Region != region)) {
				t.Errorf("the only plant: got %v %v, want found %v, %s in %s", u, ok, want, plant, region)
			}
			units, err2 := tenancy.Directory{}.OrgUnits(ctx, []uuid.UUID{plant})
			if r := units[plant].Region; r == nil || *r != region {
				t.Errorf("the plant's region read by id: got %v, want %s", r, region)
			}
			if err != nil {
				return err
			}
			return err2
		}); err != nil {
			t.Fatal(err)
		}
	}
	read(false, "DE-BY") // inactive, as is the plant of "the country's own code"
	c.Must(200, "POST", "/v1/org-units/"+plant.String()+":activate", "", `"4"`)
	read(true, "DE-BY")
	other, _ := c.Must(201, "POST", "/v1/org-units", fmt.Sprintf(`{"legal_entity_id": %q, "kind": "plant", "code": "P2", "name": "Berlin"}`, le), "")
	c.Must(200, "POST", "/v1/org-units/"+other["id"].(string)+":activate", "", `"1"`)
	read(false, "DE-BY")
}

// field and code are the problem's first field error's field and code.
func field(m map[string]any) any { return firstError(m)["field"] }
func code(m map[string]any) any  { return firstError(m)["code"] }

func firstError(m map[string]any) map[string]any {
	errs, _ := m["errors"].([]any)
	if len(errs) == 0 {
		return map[string]any{}
	}
	first, _ := errs[0].(map[string]any)
	return first
}
