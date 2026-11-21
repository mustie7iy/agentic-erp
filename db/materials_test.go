package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestMaterialPermissions (S1a, migration 000373; ADR-0082 §4): the
// permissions of materials are seeded and are no approval permissions.
func TestMaterialPermissions(t *testing.T) {
	need(t)
	rows, err := testDB.Query(context.Background(), `SELECT code FROM ref.permission WHERE code LIKE 'materials.%' AND NOT is_approval ORDER BY code`)
	if err != nil {
		t.Fatal(err)
	}
	codes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(codes, " "), "materials.material.read materials.material.write"; got != want {
		t.Errorf("got %v, want %s", codes, want)
	}
	var approval bool
	if err := testDB.QueryRow(context.Background(), `SELECT is_approval FROM ref.permission WHERE code = 'materials.change.approve'`).Scan(&approval); err != nil || !approval {
		t.Errorf("materials.change.approve: got approval %v (%v), want an approval permission (S1b, 000374)", approval, err)
	}
}

// TestMaterialRules (S1a, migration 000373; ADR-0082 §4-§6): a material
// has a number once in the tenant, a description of 1 to 200 characters
// that is not blank, a known base unit, type and status and a group of 1
// to 9 capitals, digits, - or _; its number, company, base unit and type
// do not change, and it is not deleted. A plant, storage location or unit
// is of a material that exists, a storage location of one of its plants;
// a unit's factors are 1 to 1,000,000 and the base unit has none; each is
// marked removed with who removed it, or neither, and none is deleted.
func TestMaterialRules(t *testing.T) {
	need(t)
	type scene struct{ tenant, material, plant uuid.UUID }
	const material = `INSERT INTO materials.material (tenant_id, id, number, legal_entity_id, description, base_uom_code, material_type, material_group,
			status, created_by, updated_by) VALUES ($1, $2, $3, $1, $4, $5, $6, $7, $8, $1, $1)`
	with := func(s scene, number, description, unit, kind, group, status string) stmt {
		return step(material, s.tenant, s.material, number, description, unit, kind, group, status)
	}
	create := func(s scene) stmt { return with(s, "MA1", "Bracket", "EA", "trading_good", "BRACKETS", "active") }
	plant := func(s scene) stmt {
		return step(`INSERT INTO materials.plant_data (tenant_id, material_id, plant_id, legal_entity_id, created_by) VALUES ($1, $2, $3, $1, $1)`,
			s.tenant, s.material, s.plant)
	}
	location := func(s scene, plant uuid.UUID) stmt {
		return step(`INSERT INTO materials.storage_location_data (tenant_id, material_id, plant_id, storage_location_id, created_by)
			VALUES ($1, $2, $3, $1, $1)`, s.tenant, s.material, plant)
	}
	unit := func(s scene, code string, num, den int) stmt {
		return step(`INSERT INTO materials.unit_conversion (tenant_id, material_id, uom_code, numerator, denominator, created_by)
			VALUES ($1, $2, $3, $4, $5, $1)`, s.tenant, s.material, code, num, den)
	}
	update := func(set string) stmt { return step(`UPDATE materials.material SET ` + set + `, version = version + 1`) }
	cases := []struct {
		name string
		run  func(s scene) []stmt
		want string // empty: allowed
	}{
		{"a material with a plant, a storage location and a unit", func(s scene) []stmt {
			return []stmt{create(s), plant(s), location(s, s.plant), unit(s, "BOX", 20, 1)}
		}, ""},
		{"an empty number", func(s scene) []stmt { return []stmt{with(s, "", "Bracket", "EA", "trading_good", "B", "active")} }, "material_number_check"},
		{"a number of 41 characters", func(s scene) []stmt {
			return []stmt{with(s, strings.Repeat("M", 41), "Bracket", "EA", "trading_good", "B", "active")}
		}, "material_number_check"},
		{"a number taken in the tenant", func(s scene) []stmt {
			return []stmt{create(s), step(material, s.tenant, id(), "MA1", "Other", "EA", "trading_good", "B", "active")}
		}, "material_number_once"},
		{"a blank description", func(s scene) []stmt { return []stmt{with(s, "MA1", "  ", "EA", "trading_good", "B", "active")} }, "material_description_check"},
		{"a description of 201 characters", func(s scene) []stmt {
			return []stmt{with(s, "MA1", strings.Repeat("d", 201), "EA", "trading_good", "B", "active")}
		}, "material_description_check"},
		{"an unknown base unit", func(s scene) []stmt { return []stmt{with(s, "MA1", "Bracket", "XX", "trading_good", "B", "active")} }, "material_base_uom_code_fkey"},
		{"an unknown type", func(s scene) []stmt { return []stmt{with(s, "MA1", "Bracket", "EA", "gadget", "B", "active")} }, "material_type_check"},
		{"a group in lower case", func(s scene) []stmt { return []stmt{with(s, "MA1", "Bracket", "EA", "trading_good", "b", "active")} }, "material_group_check"},
		{"a group of 10 characters", func(s scene) []stmt {
			return []stmt{with(s, "MA1", "Bracket", "EA", "trading_good", "ABCDEFGHIJ", "active")}
		}, "material_group_check"},
		{"an unknown status", func(s scene) []stmt { return []stmt{with(s, "MA1", "Bracket", "EA", "trading_good", "B", "retired")} }, "material_status_check"},
		{"pending", func(s scene) []stmt { return []stmt{with(s, "MA1", "Bracket", "EA", "trading_good", "B", "pending")} }, ""},
		{"created at version 0", func(s scene) []stmt {
			return []stmt{step(`INSERT INTO materials.material (tenant_id, id, number, legal_entity_id, description, base_uom_code, material_type,
					material_group, status, version, created_by, updated_by) VALUES ($1, $2, 'MA1', $1, 'B', 'EA', 'service', 'B', 'active', 0, $1, $1)`,
				s.tenant, s.material)}
		}, "material_version_check"},
		{"its description, group and status changed", func(s scene) []stmt {
			return []stmt{create(s), update(`description = 'Bracket M10', material_group = 'FASTENERS', status = 'blocked'`)}
		}, ""},
		{"its number changed", func(s scene) []stmt { return []stmt{create(s), update(`number = 'MA2'`)} }, "number, company, base unit, type and batch management do not change"},
		{"its company changed", func(s scene) []stmt { return []stmt{create(s), update(`legal_entity_id = gen_random_uuid()`)} }, "number, company, base unit, type and batch management do not change"},
		{"its base unit changed", func(s scene) []stmt { return []stmt{create(s), update(`base_uom_code = 'PC'`)} }, "number, company, base unit, type and batch management do not change"},
		{"its type changed", func(s scene) []stmt { return []stmt{create(s), update(`material_type = 'service'`)} }, "number, company, base unit, type and batch management do not change"},
		{"its batch management changed", func(s scene) []stmt { return []stmt{create(s), update(`batch_managed = true`)} },
			"number, company, base unit, type and batch management do not change"},
		{"a material deleted", func(s scene) []stmt { return []stmt{create(s), step(`DELETE FROM materials.material`)} }, "permission denied"},
		{"a plant of no material", func(s scene) []stmt { return []stmt{plant(s)} }, "plant_data_material_fkey"},
		{"a plant twice", func(s scene) []stmt { return []stmt{create(s), plant(s), plant(s)} }, "plant_data_pkey"},
		{"a plant removed", func(s scene) []stmt {
			return []stmt{create(s), plant(s), step(`UPDATE materials.plant_data SET removed_at = now(), removed_by = tenant_id`)}
		}, ""},
		{"a plant removed by no one", func(s scene) []stmt {
			return []stmt{create(s), plant(s), step(`UPDATE materials.plant_data SET removed_at = now()`)}
		}, "plant_data_removed_check"},
		{"a plant moved to another company", func(s scene) []stmt {
			return []stmt{create(s), plant(s), step(`UPDATE materials.plant_data SET legal_entity_id = gen_random_uuid()`)}
		}, "permission denied"},
		{"a plant deleted", func(s scene) []stmt { return []stmt{create(s), plant(s), step(`DELETE FROM materials.plant_data`)} }, "permission denied"},
		{"a storage location of no plant of the material", func(s scene) []stmt {
			return []stmt{create(s), plant(s), location(s, id())}
		}, "storage_location_data_plant_fkey"},
		{"a storage location twice", func(s scene) []stmt {
			return []stmt{create(s), plant(s), location(s, s.plant), location(s, s.plant)}
		}, "storage_location_data_pkey"},
		{"a storage location removed by no one", func(s scene) []stmt {
			return []stmt{create(s), plant(s), location(s, s.plant), step(`UPDATE materials.storage_location_data SET removed_by = tenant_id`)}
		}, "storage_location_data_removed_check"},
		{"a storage location moved to another plant", func(s scene) []stmt {
			return []stmt{create(s), plant(s), location(s, s.plant), step(`UPDATE materials.storage_location_data SET plant_id = gen_random_uuid()`)}
		}, "permission denied"},
		{"a storage location deleted", func(s scene) []stmt {
			return []stmt{create(s), plant(s), location(s, s.plant), step(`DELETE FROM materials.storage_location_data`)}
		}, "permission denied"},
		{"a unit of no material", func(s scene) []stmt { return []stmt{unit(s, "BOX", 20, 1)} }, "unit_conversion_material_fkey"},
		{"an unknown unit", func(s scene) []stmt { return []stmt{create(s), unit(s, "XX", 20, 1)} }, "unit_conversion_uom_code_fkey"},
		{"the base unit converted", func(s scene) []stmt { return []stmt{create(s), unit(s, "EA", 1, 1)} }, "the base unit has no conversion"},
		{"a unit changed to the base unit", func(s scene) []stmt {
			return []stmt{create(s), unit(s, "BOX", 20, 1), step(`UPDATE materials.unit_conversion SET uom_code = 'EA'`)}
		}, "permission denied"},
		{"a unit twice", func(s scene) []stmt { return []stmt{create(s), unit(s, "BOX", 20, 1), unit(s, "BOX", 10, 1)} }, "unit_conversion_pkey"},
		{"a numerator of 0", func(s scene) []stmt { return []stmt{create(s), unit(s, "BOX", 0, 1)} }, "unit_conversion_numerator_check"},
		{"a numerator above 1000000", func(s scene) []stmt { return []stmt{create(s), unit(s, "BOX", 1000001, 1)} }, "unit_conversion_numerator_check"},
		{"a denominator of 0", func(s scene) []stmt { return []stmt{create(s), unit(s, "BOX", 1, 0)} }, "unit_conversion_denominator_check"},
		{"a denominator above 1000000", func(s scene) []stmt { return []stmt{create(s), unit(s, "BOX", 1, 1000001)} }, "unit_conversion_denominator_check"},
		{"factors of 1000000", func(s scene) []stmt { return []stmt{create(s), unit(s, "BOX", 1000000, 1000000)} }, ""},
		{"a unit's factors changed and the unit removed", func(s scene) []stmt {
			return []stmt{create(s), unit(s, "BOX", 20, 1),
				step(`UPDATE materials.unit_conversion SET numerator = 24, denominator = 1, removed_at = now(), removed_by = tenant_id`)}
		}, ""},
		{"a unit removed by no one", func(s scene) []stmt {
			return []stmt{create(s), unit(s, "BOX", 20, 1), step(`UPDATE materials.unit_conversion SET removed_at = now()`)}
		}, "unit_conversion_removed_check"},
		{"a unit deleted", func(s scene) []stmt {
			return []stmt{create(s), unit(s, "BOX", 20, 1), step(`DELETE FROM materials.unit_conversion`)}
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), material: id(), plant: id()}
			err := asApp(t, s.tenant, func(tx pgx.Tx) error { return all(tx, c.run(s)...) })
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
		})
	}
}

// TestMaterialTriggersAsOwner (S1a, migration 000373): past the grants, as
// the owner, a material's number, company, base unit, type and batch
// management (S2f, migration 000428) stay fixed and the base unit takes no
// conversion.
func TestMaterialTriggersAsOwner(t *testing.T) {
	need(t)
	ctx := context.Background()
	tenant, material := id(), id()
	if _, err := testDB.Exec(ctx, `INSERT INTO materials.material (tenant_id, id, number, legal_entity_id, description, base_uom_code, material_type,
			material_group, status, created_by, updated_by) VALUES ($1, $2, $3, $1, 'Bracket', 'EA', 'trading_good', 'B', 'active', $1, $1)`,
		tenant, material, "MA-"+material.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := testDB.Exec(ctx, `INSERT INTO materials.unit_conversion (tenant_id, material_id, uom_code, numerator, denominator, created_by)
		VALUES ($1, $2, 'BOX', 20, 1, $1)`, tenant, material); err != nil {
		t.Fatal(err)
	}
	for set, want := range map[string]string{
		"UPDATE materials.material SET base_uom_code = 'PC' WHERE id = $1":            "material_fixed",
		"UPDATE materials.material SET material_type = 'service' WHERE id = $1":       "material_fixed",
		"UPDATE materials.material SET batch_managed = true WHERE id = $1":            "material_fixed",
		"UPDATE materials.unit_conversion SET uom_code = 'EA' WHERE material_id = $1": "unit_conversion_not_base",
	} {
		t.Run(set, func(t *testing.T) {
			_, err := testDB.Exec(ctx, set, material)
			failsOn(t, err, want)
		})
	}
}

// TestMaterialChangeRules (S1b, migrations 000374 and 000375; ADR-0082
// §9): a change is numbered MC and eight digits once in the tenant, of a
// material that exists, of a known kind and status, its payload an object
// and its duplicates an array, its reason at most 500 characters; applied
// exactly when it has applied_at; one pending change per material; none
// is deleted.
func TestMaterialChangeRules(t *testing.T) {
	need(t)
	type scene struct{ tenant, material, change uuid.UUID }
	const material = `INSERT INTO materials.material (tenant_id, id, number, legal_entity_id, description, base_uom_code, material_type, material_group,
			status, created_by, updated_by) VALUES ($1, $2, 'MA1', $1, 'Bracket', 'EA', 'trading_good', 'B', 'pending', $1, $1)`
	const change = `INSERT INTO materials.material_change (tenant_id, id, number, kind, material_id, payload, reason, possible_duplicates, status,
			requested_by, applied_at, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $1, $10, $1, $1)`
	with := func(s scene, number, kind, payload, reason, dups, status string, applied any) stmt {
		return step(change, s.tenant, s.change, number, kind, s.material, payload, reason, dups, status, applied)
	}
	create := func(s scene) []stmt {
		return []stmt{step(material, s.tenant, s.material), with(s, "MC00000001", "create", "{}", "", "[]", "pending", nil)}
	}
	cases := []struct {
		name string
		run  func(s scene) []stmt
		want string // empty: allowed
	}{
		{"a change", create, ""},
		{"a number not MC and eight digits", func(s scene) []stmt {
			return []stmt{step(material, s.tenant, s.material), with(s, "MC1", "create", "{}", "", "[]", "pending", nil)}
		}, "material_change_number_check"},
		{"a number taken", func(s scene) []stmt {
			return append(create(s), step(change, s.tenant, id(), "MC00000001", "update", s.material, "{}", "", "[]", "rejected", nil))
		}, "material_change_number_once"},
		{"an unknown kind", func(s scene) []stmt {
			return []stmt{step(material, s.tenant, s.material), with(s, "MC00000001", "delete", "{}", "", "[]", "pending", nil)}
		}, "material_change_kind_check"},
		{"an unknown status", func(s scene) []stmt {
			return []stmt{step(material, s.tenant, s.material), with(s, "MC00000001", "create", "{}", "", "[]", "approved", nil)}
		}, "material_change_status_check"},
		{"a payload not an object", func(s scene) []stmt {
			return []stmt{step(material, s.tenant, s.material), with(s, "MC00000001", "create", "[]", "", "[]", "pending", nil)}
		}, "material_change_payload_object"},
		{"duplicates not an array", func(s scene) []stmt {
			return []stmt{step(material, s.tenant, s.material), with(s, "MC00000001", "create", "{}", "", "{}", "pending", nil)}
		}, "material_change_possible_duplicates_array"},
		{"a reason of 501 characters", func(s scene) []stmt {
			return []stmt{step(material, s.tenant, s.material), with(s, "MC00000001", "create", "{}", strings.Repeat("r", 501), "[]", "pending", nil)}
		}, "material_change_reason_check"},
		{"applied without applied_at", func(s scene) []stmt {
			return []stmt{step(material, s.tenant, s.material), with(s, "MC00000001", "block", "{}", "", "[]", "applied", nil)}
		}, "material_change_applied_check"},
		{"pending with applied_at", func(s scene) []stmt {
			return []stmt{step(material, s.tenant, s.material), with(s, "MC00000001", "block", "{}", "", "[]", "pending", "2026-11-17T10:00:00Z")}
		}, "material_change_applied_check"},
		{"a change of no material", func(s scene) []stmt {
			return []stmt{with(s, "MC00000001", "create", "{}", "", "[]", "pending", nil)}
		}, "material_change_material_fkey"},
		{"two pending changes", func(s scene) []stmt {
			return append(create(s), step(change, s.tenant, id(), "MC00000002", "update", s.material, "{}", "", "[]", "pending", nil))
		}, "material_change_one_pending"},
		{"a pending change and a decided one", func(s scene) []stmt {
			return append(create(s), step(change, s.tenant, id(), "MC00000002", "update", s.material, "{}", "", "[]", "cancelled", nil))
		}, ""},
		{"applied", func(s scene) []stmt {
			return append(create(s), step(`UPDATE materials.material_change SET status = 'applied', applied_at = now(), version = version + 1`))
		}, ""},
		{"created at version 0", func(s scene) []stmt {
			return []stmt{step(material, s.tenant, s.material), step(`INSERT INTO materials.material_change (tenant_id, id, number, kind, material_id,
					payload, status, requested_by, version, created_by, updated_by) VALUES ($1, $2, 'MC00000001', 'create', $3, '{}', 'pending', $1, 0, $1, $1)`,
				s.tenant, s.change, s.material)}
		}, "material_change_version_check"},
		{"a change deleted", func(s scene) []stmt { return append(create(s), step(`DELETE FROM materials.material_change`)) }, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), material: id(), change: id()}
			err := asApp(t, s.tenant, func(tx pgx.Tx) error { return all(tx, c.run(s)...) })
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
		})
	}
}

// TestNormalizedText (S1b, migration 000374): platform.normalized_text,
// by which materials' descriptions are matched, is partner.normalized_name's
// rule.
func TestNormalizedText(t *testing.T) {
	need(t)
	for _, text := range []string{"Muster Industrie GmbH", "  muster \t industrie\ngmbh  ", "Café Noir", "straße ǆ", "　x y\u0085", ""} {
		var got, want string
		if err := testDB.QueryRow(context.Background(), `SELECT platform.normalized_text($1), partner.normalized_name($1)`, text).Scan(&got, &want); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%q: got %q, want %q as partner.normalized_name gives it", text, got, want)
		}
	}
}
