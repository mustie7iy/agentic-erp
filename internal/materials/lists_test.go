package materials_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// lists are two material types and two material groups (ADR-0082 As
// built S1a4).
var lists = config.Materials{
	MaterialTypes:  []config.MaterialType{{Code: "trading_good", Name: "Trading goods"}, {Code: "raw_material", Name: "Raw materials"}},
	MaterialGroups: []config.MaterialGroup{{Code: "BRACKETS", Name: "Brackets"}, {Code: "WIRE", Name: "Wire"}},
}

// configure makes the tenant's active configuration a new version with the
// material types and groups, as an approved change would leave it.
func (e env) configure(t *testing.T, m config.Materials) {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	pgtest.Exec(t, `INSERT INTO config.config_version (tenant_id, id, number, document, hash, source, restored_from, activated_by)
		SELECT v.tenant_id, gen_random_uuid(), v.number + 1, v.document || jsonb_build_object('materials', $2::jsonb),
			v.hash, 'rollback', v.number, v.activated_by
		FROM config.config_version v JOIN config.active_version a ON a.tenant_id = v.tenant_id AND a.number = v.number WHERE v.tenant_id = $1`,
		e.Tenant.ID, string(b))
	pgtest.Exec(t, `UPDATE config.active_version SET number = number + 1, version = version + 1 WHERE tenant_id = $1`, e.Tenant.ID)
}

// TestListsRefuseOnCreate: once the configuration lists material types
// and groups, a material of a platform type or a group it does not list
// is refused naming the field and what the configuration allows, and
// one it lists is created.
func TestListsRefuseOnCreate(t *testing.T) {
	e := setup(t)
	e.configure(t, lists)
	for name, c := range map[string]struct {
		over          map[string]any
		code, field   string
		detailMention string
	}{
		"a type the lists leave out": {map[string]any{"material_type": "service"}, "materials.unknown_material_type", "material_type",
			`"service" is not one of the configuration's material types (materials.material_types): trading_good, raw_material.`},
		"no platform type": {map[string]any{"material_type": "gadget"}, "materials.unknown_material_type", "material_type",
			"trading_good, raw_material"},
		"a group the lists leave out": {map[string]any{"material_group": "WIDGETS"}, "materials.unknown_material_group", "material_group",
			"Material group WIDGETS is not one of the configuration's (materials.material_groups)."},
	} {
		t.Run(name, func(t *testing.T) {
			got := e.Problem(422, c.code, "POST", path, e.bracket(t, c.over), "")
			if field(got) != c.field {
				t.Errorf("field: got %v, want %s", field(got), c.field)
			}
			if detail := fmt.Sprint(got["errors"]); !strings.Contains(detail, c.detailMention) {
				t.Errorf("errors: got %s, want them to say %q", detail, c.detailMention)
			}
		})
	}
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM materials.material WHERE tenant_id = $1`, []any{e.Tenant.ID}, &n)
	if n != 0 {
		t.Errorf("materials after the refusals: got %d, want none", n)
	}
	m := e.create(t, map[string]any{"material_type": "raw_material", "material_group": "WIRE"})
	fields(t, m, map[string]any{"material_type": "raw_material", "material_group": "WIRE", "status": "pending"})
}

// TestListsWithoutConfiguration: a tenant whose configuration lists no
// type and no group keeps every platform type and any group, as before
// (ADR-0082 As built S1a4).
func TestListsWithoutConfiguration(t *testing.T) {
	e := setup(t)
	m := e.create(t, map[string]any{"material_type": "service", "material_group": "WIDGETS"})
	fields(t, m, map[string]any{"material_type": "service", "material_group": "WIDGETS"})
	got := e.Problem(422, "materials.unknown_material_type", "POST", path, e.bracket(t, map[string]any{"material_type": "gadget"}), "")
	if detail := fmt.Sprint(got["errors"]); !strings.Contains(detail, `"gadget" is not a material type; the types are raw_material, semi_finished`) {
		t.Errorf("errors: got %s, want them to list the platform's types", detail)
	}
}

// TestListsOnChange: a change that sets a group the configuration does
// not list is refused; one it lists waits for approval; a material created
// before the lists keeps its group through a change of another field.
func TestListsOnChange(t *testing.T) {
	e := setup(t)
	before := e.active(t, map[string]any{"material_type": "service", "material_group": "OLD"})["id"].(string)
	id := e.active(t, map[string]any{"description": "Copper wire", "material_group": "BRACKETS"})["id"].(string)
	e.configure(t, lists)
	got := e.Problem(422, "materials.unknown_material_group", "PATCH", path+"/"+id, `{"material_group": "WIDGETS"}`, `"2"`)
	if field(got) != "material_group" {
		t.Errorf("field: got %v, want material_group", field(got))
	}
	after := e.changed(t, id, `{"material_group": "WIRE"}`)
	fields(t, after, map[string]any{"material_group": "WIRE", "version": 3})
	kept := e.changed(t, before, `{"description": "Machined bracket M10, kept"}`)
	fields(t, kept, map[string]any{"material_type": "service", "material_group": "OLD", "description": "Machined bracket M10, kept"})
	e.Problem(422, "materials.unknown_material_group", "PATCH", path+"/"+before, `{"material_group": "OLDER"}`, `"3"`)
}
