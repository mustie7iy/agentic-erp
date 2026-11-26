package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// cycleScene is a tenant with two plants and a material, by id.
type cycleScene struct{ tenant, company, plant, other, material, indicator uuid.UUID }

const cycleIndicator = `INSERT INTO inventory.cycle_count_indicator (tenant_id, id, legal_entity_id, plant_id, code, interval_days, created_by, updated_by)
	VALUES ($1, $2, $3, $4, $5, $6, $1, $1)`

const cycleMaterial = `INSERT INTO inventory.cycle_count_material (tenant_id, id, legal_entity_id, plant_id, material_id, indicator_id, created_by, updated_by)
	VALUES ($1, $2, $3, $4, $5, $6, $1, $1)`

// code is an indicator of the plant with the code and interval.
func (s cycleScene) code(id uuid.UUID, plant uuid.UUID, code string, days int) stmt {
	return step(cycleIndicator, s.tenant, id, s.company, plant, code, days)
}

// assigned is the scene's indicator A of 90 days given to its material at its plant.
func (s cycleScene) assigned() []stmt {
	return []stmt{s.code(s.indicator, s.plant, "A", 90), step(cycleMaterial, s.tenant, id(), s.company, s.plant, s.material, s.indicator)}
}

// TestCycleCountRules (S2c2, migration 000475; ADR-0082 As built S2c2): an
// indicator's code is one to four capital letters or digits, once at a
// plant, and its interval 1 to 366 days; a material has one indicator at
// a plant, one of that plant. What an indicator is of (its plant, code
// and company) and what a material's indicator is of (its plant and
// material) do not change; its interval and indicator do. Nothing is
// deleted.
func TestCycleCountRules(t *testing.T) {
	need(t)
	cases := []struct {
		name string
		run  func(s cycleScene) []stmt
		want string // empty: allowed
	}{
		{"an indicator", func(s cycleScene) []stmt { return []stmt{s.code(id(), s.plant, "Z9", 30)} }, ""},
		{"a code of four", func(s cycleScene) []stmt { return []stmt{s.code(id(), s.plant, "ABCD", 30)} }, ""},
		{"a code of five", func(s cycleScene) []stmt { return []stmt{s.code(id(), s.plant, "ABCDE", 30)} }, "cycle_count_indicator_code_check"},
		{"an empty code", func(s cycleScene) []stmt { return []stmt{s.code(id(), s.plant, "", 30)} }, "cycle_count_indicator_code_check"},
		{"a small letter", func(s cycleScene) []stmt { return []stmt{s.code(id(), s.plant, "a", 30)} }, "cycle_count_indicator_code_check"},
		{"an interval of a day", func(s cycleScene) []stmt { return []stmt{s.code(id(), s.plant, "A", 1)} }, ""},
		{"an interval of 366 days", func(s cycleScene) []stmt { return []stmt{s.code(id(), s.plant, "A", 366)} }, ""},
		{"an interval of no day", func(s cycleScene) []stmt { return []stmt{s.code(id(), s.plant, "A", 0)} }, "cycle_count_indicator_interval_check"},
		{"an interval past a year", func(s cycleScene) []stmt { return []stmt{s.code(id(), s.plant, "A", 367)} }, "cycle_count_indicator_interval_check"},
		{"a code twice at a plant", func(s cycleScene) []stmt {
			return []stmt{s.code(id(), s.plant, "A", 90), s.code(id(), s.plant, "A", 30)}
		}, "cycle_count_indicator_code_once"},
		{"a code at two plants", func(s cycleScene) []stmt {
			return []stmt{s.code(id(), s.plant, "A", 90), s.code(id(), s.other, "A", 30)}
		}, ""},
		{"an interval changed", func(s cycleScene) []stmt {
			return append(s.assigned(), step(`UPDATE inventory.cycle_count_indicator SET interval_days = 30, version = version + 1`))
		}, ""},
		{"an interval changed past a year", func(s cycleScene) []stmt {
			return append(s.assigned(), step(`UPDATE inventory.cycle_count_indicator SET interval_days = 400, version = version + 1`))
		}, "cycle_count_indicator_interval_check"},
		{"an indicator's code changed", func(s cycleScene) []stmt {
			return append(s.assigned(), step(`UPDATE inventory.cycle_count_indicator SET code = 'B'`))
		}, "permission denied"},
		{"an indicator's plant changed", func(s cycleScene) []stmt {
			return append(s.assigned(), step(`UPDATE inventory.cycle_count_indicator SET plant_id = gen_random_uuid()`))
		}, "permission denied"},
		{"an indicator deleted", func(s cycleScene) []stmt {
			return []stmt{s.code(id(), s.plant, "A", 90), step(`DELETE FROM inventory.cycle_count_indicator`)}
		}, "permission denied"},
		{"an indicator at version 0", func(s cycleScene) []stmt {
			return []stmt{step(`INSERT INTO inventory.cycle_count_indicator (tenant_id, id, legal_entity_id, plant_id, code, interval_days, version,
				created_by, updated_by) VALUES ($1, $2, $3, $4, 'A', 90, 0, $1, $1)`, s.tenant, id(), s.company, s.plant)}
		}, "cycle_count_indicator_version_check"},
		{"a material's indicator", func(s cycleScene) []stmt { return s.assigned() }, ""},
		{"a material given two indicators at a plant", func(s cycleScene) []stmt {
			return append(s.assigned(), s.code(s.other, s.plant, "B", 180), step(cycleMaterial, s.tenant, id(), s.company, s.plant, s.material, s.other))
		}, "cycle_count_material_once"},
		{"a material given another plant's indicator", func(s cycleScene) []stmt {
			return []stmt{s.code(s.indicator, s.other, "A", 90), step(cycleMaterial, s.tenant, id(), s.company, s.plant, s.material, s.indicator)}
		}, "cycle_count_material_indicator_fkey"},
		{"a material given no indicator there is", func(s cycleScene) []stmt {
			return []stmt{step(cycleMaterial, s.tenant, id(), s.company, s.plant, s.material, s.indicator)}
		}, "cycle_count_material_indicator_fkey"},
		{"a material's indicator changed at its plant", func(s cycleScene) []stmt {
			return append(s.assigned(), s.code(s.other, s.plant, "B", 180),
				step(`UPDATE inventory.cycle_count_material SET indicator_id = $1, version = version + 1`, s.other))
		}, ""},
		{"a material's indicator changed to another plant's", func(s cycleScene) []stmt {
			return append(s.assigned(), s.code(s.other, s.other, "B", 180),
				step(`UPDATE inventory.cycle_count_material SET indicator_id = $1, version = version + 1`, s.other))
		}, "cycle_count_material_indicator_fkey"},
		{"a material's indicator's material changed", func(s cycleScene) []stmt {
			return append(s.assigned(), step(`UPDATE inventory.cycle_count_material SET material_id = gen_random_uuid()`))
		}, "permission denied"},
		{"a material's indicator's plant changed", func(s cycleScene) []stmt {
			return append(s.assigned(), step(`UPDATE inventory.cycle_count_material SET plant_id = $1`, s.other))
		}, "permission denied"},
		{"a material's indicator deleted", func(s cycleScene) []stmt {
			return append(s.assigned(), step(`DELETE FROM inventory.cycle_count_material`))
		}, "permission denied"},
		{"a material's indicator at version 0", func(s cycleScene) []stmt {
			return []stmt{s.code(s.indicator, s.plant, "A", 90), step(`INSERT INTO inventory.cycle_count_material (tenant_id, id, legal_entity_id,
				plant_id, material_id, indicator_id, version, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $6, 0, $1, $1)`,
				s.tenant, id(), s.company, s.plant, s.material, s.indicator)}
		}, "cycle_count_material_version_check"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := cycleScene{tenant: id(), company: id(), plant: id(), other: id(), material: id(), indicator: id()}
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

// TestCycleCountDownRefused (S2c2, migration 000475): the down refuses,
// rather than drops, cycle counting indicators (06 §6.4: a down
// migration loses no data), reading every tenant's rows: a migrating
// role held to row-level security fails it rather than finding none.
func TestCycleCountDownRefused(t *testing.T) {
	need(t)
	down := migrationNamed(t, "000475_cycle_count").Down
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	s := cycleScene{tenant: id(), company: id(), plant: id(), indicator: id()}
	if err := all(tx, step(`SELECT set_config('app.tenant_id', $1, true)`, s.tenant.String()), s.code(s.indicator, s.plant, "A", 90),
		step(`SELECT set_config('app.tenant_id', '', true)`), step(`SAVEPOINT migrator`)); err != nil {
		t.Fatal(err)
	}
	if err := asMigrator(tx); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, down), "row-level security")
	if err := exec(tx, `ROLLBACK TO SAVEPOINT migrator`); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, down), "cycle counting indicators exist")
}
