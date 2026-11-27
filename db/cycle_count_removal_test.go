package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// removed is the scene's material's indicator taken off.
func (s cycleScene) removed() []stmt {
	return append(s.assigned(), step(`UPDATE inventory.cycle_count_material SET status = 'removed', version = version + 1 WHERE material_id = $1`, s.material))
}

// TestCycleCountRemovalRules (S2c3, migration 000477; ADR-0082 As built
// S2c3): a material's indicator is active or removed; a material has one
// active indicator at a plant and may be given one again once it is
// removed; a removed one never changes again, as the application role
// and as the owner, whom the trigger holds too.
func TestCycleCountRemovalRules(t *testing.T) {
	need(t)
	cases := []struct {
		name string
		run  func(s cycleScene) []stmt
		want string // empty: allowed
	}{
		{"an indicator removed", func(s cycleScene) []stmt { return s.removed() }, ""},
		{"an indicator given again once removed", func(s cycleScene) []stmt {
			return append(s.removed(), step(cycleMaterial, s.tenant, id(), s.company, s.plant, s.material, s.indicator))
		}, ""},
		{"two removed and one active", func(s cycleScene) []stmt {
			return append(s.removed(), step(cycleMaterial, s.tenant, s.other, s.company, s.plant, s.material, s.indicator),
				step(`UPDATE inventory.cycle_count_material SET status = 'removed', version = version + 1 WHERE id = $1`, s.other),
				step(cycleMaterial, s.tenant, id(), s.company, s.plant, s.material, s.indicator))
		}, ""},
		{"two active", func(s cycleScene) []stmt {
			return append(s.removed(), step(cycleMaterial, s.tenant, id(), s.company, s.plant, s.material, s.indicator),
				step(cycleMaterial, s.tenant, id(), s.company, s.plant, s.material, s.indicator))
		}, "cycle_count_material_once"},
		{"a status unknown", func(s cycleScene) []stmt {
			return append(s.assigned(), step(`UPDATE inventory.cycle_count_material SET status = 'ended', version = version + 1`))
		}, "cycle_count_material_status_check"},
		{"a removed indicator active again", func(s cycleScene) []stmt {
			return append(s.removed(), step(`UPDATE inventory.cycle_count_material SET status = 'active', version = version + 1`))
		}, "cycle_count_material_removed"},
		{"a removed indicator changed", func(s cycleScene) []stmt {
			return append(s.removed(), s.code(s.other, s.plant, "B", 180),
				step(`UPDATE inventory.cycle_count_material SET indicator_id = $1, version = version + 1`, s.other))
		}, "cycle_count_material_removed"},
		{"a removed indicator's version moved", func(s cycleScene) []stmt {
			return append(s.removed(), step(`UPDATE inventory.cycle_count_material SET version = version + 1`))
		}, "cycle_count_material_removed"},
		{"a removed indicator deleted", func(s cycleScene) []stmt {
			return append(s.removed(), step(`DELETE FROM inventory.cycle_count_material`))
		}, "permission denied"},
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
	t.Run("a removed indicator changed by the owner", func(t *testing.T) {
		s := cycleScene{tenant: id(), company: id(), plant: id(), other: id(), material: id(), indicator: id()}
		err := asApp(t, s.tenant, func(tx pgx.Tx) error {
			if err := all(tx, s.removed()...); err != nil {
				return err
			}
			return asOwner(func(tx pgx.Tx) error {
				return exec(tx, `UPDATE inventory.cycle_count_material SET status = 'active'`)
			})(tx)
		})
		failsWith(t, err, "cycle_count_material_removed")
	})
}

// TestCycleCountRemovalDownRefused (S2c3, migration 000477): the down
// refuses, rather than drops, removed indicators of materials, reading
// every tenant's rows: a migrating role held to row-level security fails
// it rather than finding none.
func TestCycleCountRemovalDownRefused(t *testing.T) {
	need(t)
	down := migrationNamed(t, "000477_cycle_count_removal").Down
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	s := cycleScene{tenant: id(), company: id(), plant: id(), material: id(), indicator: id()}
	if err := all(tx, append(append([]stmt{step(`SELECT set_config('app.tenant_id', $1, true)`, s.tenant.String())}, s.removed()...),
		step(`SELECT set_config('app.tenant_id', '', true)`), step(`SAVEPOINT migrator`))...); err != nil {
		t.Fatal(err)
	}
	if err := asMigrator(tx); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, down), "row-level security")
	if err := exec(tx, `ROLLBACK TO SAVEPOINT migrator`); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, down), "removed cycle counting indicators of materials exist")
}

const abcApplication = `INSERT INTO inventory.cycle_count_abc_application (tenant_id, id, legal_entity_id, plant_id, from_date, to_date, share_a,
	share_b, currency, total_consumption, preview_token, created_by) VALUES ($1, $2, $3, $4, $5::date, $6::date, $7, $8, 'EUR', $9, $10, $1)`

const abcChange = `INSERT INTO inventory.cycle_count_abc_change (tenant_id, application_id, line_no, material_id, consumption, class, indicator_id,
	before_indicator_id, cycle_count_material_id) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`

// abcToken is a well-formed preview token.
var abcToken = strings.Repeat("0a", 32)

// applied is the scene's material's indicator and an application of
// the scene's tenant, by app's id.
func (s cycleScene) applied(app uuid.UUID) []stmt {
	return append([]stmt{s.code(s.indicator, s.plant, "A", 90)}, step(cycleMaterial, s.tenant, s.other, s.company, s.plant, s.material, s.indicator),
		step(abcApplication, s.tenant, app, s.company, s.plant, "2026-01-01", "2026-12-31", 80, 15, 100, abcToken))
}

// TestABCApplicationRules (S2c3, migration 000477): an application's
// window ends where it starts or later and spans at most 366 days, its
// shares are each at least 1 and together at most 99, its total is not
// negative and its token 64 hex digits; a change is numbered 1 to 1,000,
// once per material, of class A, B or C, of a value not negative, names
// indicators and a material's indicator there are, and replaces another
// indicator or none. Both are insert-only, the owner held too.
func TestABCApplicationRules(t *testing.T) {
	need(t)
	app := func(from, to string, a, b, total int, token string) func(s cycleScene) []stmt {
		return func(s cycleScene) []stmt {
			return []stmt{step(abcApplication, s.tenant, id(), s.company, s.plant, from, to, a, b, total, token)}
		}
	}
	change := func(line, value int, class string, before func(s cycleScene) any) func(s cycleScene) []stmt {
		return func(s cycleScene) []stmt {
			a := id()
			return append(s.applied(a), step(abcChange, s.tenant, a, line, s.material, value, class, s.indicator, before(s), s.other))
		}
	}
	none := func(cycleScene) any { return nil }
	cases := []struct {
		name string
		run  func(s cycleScene) []stmt
		want string // empty: allowed
	}{
		{"an application", app("2026-01-01", "2026-12-31", 80, 15, 0, abcToken), ""},
		{"a window of a day", app("2026-05-01", "2026-05-01", 80, 15, 0, abcToken), ""},
		{"a window of 367 days", app("2025-12-31", "2027-01-01", 80, 15, 0, abcToken), "cycle_count_abc_application_window_check"},
		{"a window ending before it starts", app("2026-05-02", "2026-05-01", 80, 15, 0, abcToken), "cycle_count_abc_application_window_check"},
		{"no share A", app("2026-01-01", "2026-12-31", 0, 15, 0, abcToken), "cycle_count_abc_application_shares_check"},
		{"no share B", app("2026-01-01", "2026-12-31", 80, 0, 0, abcToken), "cycle_count_abc_application_shares_check"},
		{"no share C", app("2026-01-01", "2026-12-31", 80, 20, 0, abcToken), "cycle_count_abc_application_shares_check"},
		{"a negative total", app("2026-01-01", "2026-12-31", 80, 15, -1, abcToken), "cycle_count_abc_application_total_check"},
		{"a short token", app("2026-01-01", "2026-12-31", 80, 15, 0, abcToken[1:]), "cycle_count_abc_application_token_check"},
		{"a token in capitals", app("2026-01-01", "2026-12-31", 80, 15, 0, strings.ToUpper(abcToken)), "cycle_count_abc_application_token_check"},
		{"a change", change(1, 100, "A", none), ""},
		{"a change of line 1,000", change(1000, 0, "C", none), ""},
		{"a change of line 0", change(0, 0, "C", none), "cycle_count_abc_change_line_no_check"},
		{"a change of line 1,001", change(1001, 0, "C", none), "cycle_count_abc_change_line_no_check"},
		{"a change of a negative value", change(1, -1, "A", none), "cycle_count_abc_change_consumption_check"},
		{"a change of class D", change(1, 0, "D", none), "cycle_count_abc_change_class_check"},
		{"a change replacing its own indicator", change(1, 0, "A", func(s cycleScene) any { return s.indicator }), "cycle_count_abc_change_before_check"},
		{"a change replacing no indicator there is", change(1, 0, "A", func(cycleScene) any { return id() }), "cycle_count_abc_change_before_fkey"},
		{"a change of no application", func(s cycleScene) []stmt {
			return append(s.applied(id()), step(abcChange, s.tenant, id(), 1, s.material, 0, "A", s.indicator, nil, s.other))
		}, "cycle_count_abc_change_application_fkey"},
		{"a change to no indicator there is", func(s cycleScene) []stmt {
			a := id()
			return append(s.applied(a), step(abcChange, s.tenant, a, 1, s.material, 0, "A", id(), nil, s.other))
		}, "cycle_count_abc_change_indicator_fkey"},
		{"a change of no material's indicator", func(s cycleScene) []stmt {
			a := id()
			return append(s.applied(a), step(abcChange, s.tenant, a, 1, s.material, 0, "A", s.indicator, nil, id()))
		}, "cycle_count_abc_change_material_fkey"},
		{"a material changed twice", func(s cycleScene) []stmt {
			a := id()
			return append(s.applied(a), step(abcChange, s.tenant, a, 1, s.material, 0, "A", s.indicator, nil, s.other),
				step(abcChange, s.tenant, a, 2, s.material, 0, "A", s.indicator, nil, s.other))
		}, "cycle_count_abc_change_material_once"},
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
	for name, st := range map[string]string{
		"an application changed": `UPDATE inventory.cycle_count_abc_application SET total_consumption = 1`,
		"an application deleted": `DELETE FROM inventory.cycle_count_abc_application`,
		"a change changed":       `UPDATE inventory.cycle_count_abc_change SET consumption = 1`,
		"a change deleted":       `DELETE FROM inventory.cycle_count_abc_change`,
	} {
		t.Run(name+" by the owner", func(t *testing.T) {
			s := cycleScene{tenant: id(), company: id(), plant: id(), other: id(), material: id(), indicator: id()}
			a := id()
			err := asApp(t, s.tenant, func(tx pgx.Tx) error {
				if err := all(tx, append(s.applied(a), step(abcChange, s.tenant, a, 1, s.material, 0, "A", s.indicator, nil, s.other))...); err != nil {
					return err
				}
				return asOwner(func(tx pgx.Tx) error { return exec(tx, st+` WHERE tenant_id = $1`, s.tenant) })(tx)
			})
			failsWith(t, err, "insert-only")
		})
	}
}

// TestABCApplicationDownRefused (S2c3, migration 000477): with no removed
// indicator, the down still refuses while an ABC analysis applied is
// kept.
func TestABCApplicationDownRefused(t *testing.T) {
	need(t)
	down := migrationNamed(t, "000477_cycle_count_removal").Down
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	s := cycleScene{tenant: id(), company: id(), plant: id(), other: id(), material: id(), indicator: id()}
	if err := all(tx, append(append([]stmt{step(`SELECT set_config('app.tenant_id', $1, true)`, s.tenant.String())}, s.applied(id())...),
		step(`SELECT set_config('app.tenant_id', '', true)`),
		// Other tests' removed indicators would meet the first guard; in
		// this transaction alone, rolled back, there are none.
		step(`DELETE FROM inventory.cycle_count_material WHERE status = 'removed'`))...); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, down), "ABC analyses applied exist")
}
