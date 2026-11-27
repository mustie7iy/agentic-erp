package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// measureScene is a material kept in EA with BOX as its alternative unit,
// in its own tenant.
type measureScene struct{ tenant, material uuid.UUID }

func (s measureScene) material2() []stmt {
	return []stmt{
		step(`INSERT INTO materials.material (tenant_id, id, number, legal_entity_id, description, base_uom_code, material_type, material_group,
			status, created_by, updated_by) VALUES ($1, $2, 'MA1', $1, 'Bracket', 'EA', 'trading_good', 'B', 'active', $1, $1)`, s.tenant, s.material),
		step(`INSERT INTO materials.unit_conversion (tenant_id, material_id, uom_code, numerator, denominator, created_by)
			VALUES ($1, $2, 'BOX', 20, 1, $1)`, s.tenant, s.material),
	}
}

// measure writes a unit's measures given as column = value pairs.
func (s measureScene) measure(unit, columns, values string, args ...any) stmt {
	return step(`INSERT INTO materials.unit_measure (tenant_id, material_id, uom_code, created_by, `+columns+`) VALUES ($1, $2, $3, $1, `+values+`)`,
		append([]any{s.tenant, s.material, unit}, args...)...)
}

// TestUnitMeasureRules (S1a3, migration 000495; ADR-0082 As built S1a3):
// a unit's measures are of the material's base unit or a unit it has;
// every value is above zero, given with its unit and the unit only with a
// value, each unit of its kind; a net weight is no more than the gross;
// a GTIN has 8, 12, 13 or 14 digits and its check digit, and names one
// unit in the tenant, as GS1 compares them, until it is removed; a row
// says something; its values and mark are updated, its material and unit
// not, and none is deleted.
func TestUnitMeasureRules(t *testing.T) {
	need(t)
	with := func(more ...func(s measureScene) stmt) func(s measureScene) []stmt {
		return func(s measureScene) []stmt {
			out := s.material2()
			for _, m := range more {
				out = append(out, m(s))
			}
			return out
		}
	}
	m := func(unit, columns, values string, args ...any) func(s measureScene) stmt {
		return func(s measureScene) stmt { return s.measure(unit, columns, values, args...) }
	}
	set := func(sql string) func(s measureScene) stmt { return func(measureScene) stmt { return step(sql) } }
	box := m("BOX", "gross_weight, net_weight, weight_uom, volume, volume_uom, length, width, height, dimension_uom, gtin",
		"2.5, 2.4, 'KG', 0.012, 'M3', 0.3, 0.2, 0.2, 'M', '10036000291459'")
	cases := []struct {
		name string
		run  func(s measureScene) []stmt
		want string // empty: allowed
	}{
		{"every measure of an alternative unit", with(box), ""},
		{"a GTIN alone of the base unit", with(m("EA", "gtin", "'036000291452'")), ""},
		{"a gross weight alone", with(m("EA", "gross_weight, weight_uom", "0.001, 'G'")), ""},
		{"a GTIN-8", with(m("EA", "gtin", "'96385074'")), ""},
		{"a unit the material has not", with(m("PC", "gtin", "'036000291452'")), "measures are of the material's base unit"},
		{"a unit the material lost", with(set(`UPDATE materials.unit_conversion SET removed_at = now(), removed_by = tenant_id`), box),
			"measures are of the material's base unit"},
		{"a measure of no material", func(s measureScene) []stmt { return []stmt{s.measure("EA", "gtin", "'036000291452'")} },
			"measures are of the material's base unit"},
		{"a weight of zero", with(m("EA", "gross_weight, weight_uom", "0, 'KG'")), "unit_measure_gross_weight_check"},
		{"a net weight of zero", with(m("EA", "net_weight, weight_uom", "0, 'KG'")), "unit_measure_net_weight_check"},
		{"a volume of zero", with(m("EA", "volume, volume_uom", "0, 'L'")), "unit_measure_volume_check"},
		{"a length below zero", with(m("EA", "length, dimension_uom", "-1, 'M'")), "unit_measure_length_check"},
		{"a width of zero", with(m("EA", "width, dimension_uom", "0, 'M'")), "unit_measure_width_check"},
		{"a height of zero", with(m("EA", "height, dimension_uom", "0, 'M'")), "unit_measure_height_check"},
		{"a weight without its unit", with(m("EA", "gross_weight", "1")), "unit_measure_weight_uom_check"},
		{"a weight unit without a weight", with(m("EA", "gtin, weight_uom", "'036000291452', 'KG'")), "unit_measure_weight_uom_check"},
		{"a volume without its unit", with(m("EA", "volume", "1")), "unit_measure_volume_uom_check"},
		{"a volume unit without a volume", with(m("EA", "gtin, volume_uom", "'036000291452', 'L'")), "unit_measure_volume_uom_check"},
		{"a height without its unit", with(m("EA", "height", "1")), "unit_measure_dimension_uom_check"},
		{"a length unit without a length", with(m("EA", "gtin, dimension_uom", "'036000291452', 'M'")), "unit_measure_dimension_uom_check"},
		{"a weight in litres", with(m("EA", "gross_weight, weight_uom", "1, 'L'")), "a weight is in a unit of mass"},
		{"a volume in metres", with(m("EA", "volume, volume_uom", "1, 'M'")), "a weight is in a unit of mass"},
		{"a length in kilograms", with(m("EA", "length, dimension_uom", "1, 'KG'")), "a weight is in a unit of mass"},
		{"an unknown unit", with(m("EA", "gross_weight, weight_uom", "1, 'XX'")), "unit_measure_weight_uom_fkey"},
		{"a net weight above the gross", with(m("EA", "gross_weight, net_weight, weight_uom", "1, 1.001, 'KG'")), "unit_measure_net_weight_gross"},
		{"a net weight as the gross", with(m("EA", "gross_weight, net_weight, weight_uom", "1, 1, 'KG'")), ""},
		{"a GTIN's check digit wrong", with(m("EA", "gtin", "'036000291453'")), "unit_measure_gtin_check"},
		{"a GTIN of eleven digits", with(m("EA", "gtin", "'03600029145'")), "unit_measure_gtin_check"},
		{"a GTIN with a letter", with(m("EA", "gtin", "'03600029145A'")), "unit_measure_gtin_check"},
		{"a row that says nothing", with(m("EA", "removed_at", "NULL")), "unit_measure_something"},
		{"one GTIN on two units", with(m("EA", "gtin", "'036000291452'"), m("BOX", "gtin", "'036000291452'")), "unit_measure_gtin_once"},
		{"a GTIN-12 and its GTIN-13", with(m("EA", "gtin", "'036000291452'"), m("BOX", "gtin", "'0036000291452'")), "unit_measure_gtin_once"},
		{"a GTIN again once removed", with(m("EA", "gtin, removed_at, removed_by", "'036000291452', now(), $1"), m("BOX", "gtin", "'036000291452'")), ""},
		{"a removed row giving back a GTIN taken", with(m("EA", "gtin, removed_at, removed_by", "'036000291452', now(), $1"),
			m("BOX", "gtin", "'036000291452'"), set(`UPDATE materials.unit_measure SET removed_at = NULL, removed_by = NULL WHERE uom_code = 'EA'`)),
			"unit_measure_gtin_once"},
		{"a measure removed by no one", with(box, set(`UPDATE materials.unit_measure SET removed_at = now()`)), "unit_measure_removed_check"},
		{"values changed and the row removed", with(box, set(`UPDATE materials.unit_measure SET gross_weight = 3, removed_at = now(), removed_by = tenant_id`)), ""},
		{"a unit changed", with(box, set(`UPDATE materials.unit_measure SET uom_code = 'EA'`)), "permission denied"},
		{"a measure moved to another material", with(box, set(`UPDATE materials.unit_measure SET material_id = gen_random_uuid()`)), "permission denied"},
		{"a measure deleted", with(box, set(`DELETE FROM materials.unit_measure`)), "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := measureScene{tenant: id(), material: id()}
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
	t.Run("one GTIN in two tenants", func(t *testing.T) {
		for range 2 {
			s := measureScene{tenant: id(), material: id()}
			if err := asApp(t, s.tenant, func(tx pgx.Tx) error { return all(tx, with(m("EA", "gtin", "'4006381333931'"))(s)...) }); err != nil {
				t.Fatalf("got %v, want each tenant to have it", err)
			}
		}
	})
}

// TestUnitMeasureRemovedWithItsUnit (S1a3, migration 000495): an
// alternative unit taken off a material, by either version, takes its
// measures with it, by whoever took it off, so its GTIN is free again;
// the base unit's stay.
func TestUnitMeasureRemovedWithItsUnit(t *testing.T) {
	need(t)
	s := measureScene{tenant: id(), material: id()}
	remover := id()
	var removedBy *uuid.UUID
	var live int
	err := asApp(t, s.tenant, func(tx pgx.Tx) error {
		if err := all(tx, append(s.material2(), s.measure("BOX", "gtin", "'10036000291459'"), s.measure("EA", "gtin", "'036000291452'"),
			step(`UPDATE materials.unit_conversion SET removed_at = now(), removed_by = $1`, remover))...); err != nil {
			return err
		}
		ctx := context.Background()
		if err := tx.QueryRow(ctx, `SELECT removed_by FROM materials.unit_measure WHERE uom_code = 'BOX'`).Scan(&removedBy); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM materials.unit_measure WHERE removed_at IS NULL`).Scan(&live); err != nil {
			return err
		}
		return all(tx, step(`UPDATE materials.unit_measure SET gtin = '10036000291459' WHERE uom_code = 'EA'`))
	})
	if err != nil {
		t.Fatalf("got %v, want the box's GTIN free for the base unit", err)
	}
	if removedBy == nil || *removedBy != remover {
		t.Errorf("the box's measures removed by: got %v, want %v", removedBy, remover)
	}
	if live != 1 {
		t.Errorf("measures left: got %d, want the base unit's 1", live)
	}
}

// TestUnitMeasureTriggersAsOwner (S1a3, migration 000495): past the
// grants, as the owner, a unit's measures stay of a unit the material has
// and of units of their kind.
func TestUnitMeasureTriggersAsOwner(t *testing.T) {
	need(t)
	ctx := context.Background()
	tenant, material := id(), id()
	if _, err := testDB.Exec(ctx, `INSERT INTO materials.material (tenant_id, id, number, legal_entity_id, description, base_uom_code, material_type,
			material_group, status, created_by, updated_by) VALUES ($1, $2, $3, $1, 'Bracket', 'EA', 'trading_good', 'B', 'active', $1, $1)`,
		tenant, material, "MA-"+material.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := testDB.Exec(ctx, `INSERT INTO materials.unit_measure (tenant_id, material_id, uom_code, created_by, gross_weight, weight_uom)
		VALUES ($1, $2, 'EA', $1, 1, 'KG')`, tenant, material); err != nil {
		t.Fatalf("the base unit's weight: got %v, want it written", err)
	}
	for set, want := range map[string]string{
		"UPDATE materials.unit_measure SET uom_code = 'BOX' WHERE material_id = $1": "unit_measure_of_unit",
		"UPDATE materials.unit_measure SET weight_uom = 'L' WHERE material_id = $1": "unit_measure_dimension",
	} {
		t.Run(set, func(t *testing.T) {
			_, err := testDB.Exec(ctx, set, material)
			failsOn(t, err, want)
		})
	}
}

// TestUnitMeasureDownRefused (S1a3, migration 000495): the down refuses,
// rather than drops, a unit's measures (removed ones too: what a unit last
// carried before it was removed),
// which the previous version cannot hold (06 §6.4).
func TestUnitMeasureDownRefused(t *testing.T) {
	need(t)
	down := migrationNamed(t, "000495_material_measures").Down
	for name, removed := range map[string]string{"measures": "NULL, NULL", "removed measures": "now(), $1"} {
		t.Run(name, func(t *testing.T) {
			s := measureScene{tenant: id(), material: id()}
			downRefused(t, down, s.tenant, append(s.material2(), s.measure("EA", "gtin, removed_at, removed_by", "'036000291452', "+removed)),
				"units of measure of materials carry")
		})
	}
}
