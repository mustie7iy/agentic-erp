package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// sopScene is a tenant with an S&OP plan version of a company, by id.
type sopScene struct{ tenant, plan, company uuid.UUID }

const (
	sopPlan = `INSERT INTO mrp.sop_plan (tenant_id, id, legal_entity_id, number, as_of, first_day, last_day, months, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5::date, $6::date, $7::date, $8, $1, $1)`
	sopLine = `INSERT INTO mrp.sop_plan_line (tenant_id, plan_id, material_group, uom_code, month, first_day, last_day, materials, forecast, orders,
		stock, purchase_orders, stock_transports, proposals, consensus) VALUES ($1, $2, $3, $4, $5::date, $6::date, $7::date, 1, $8::numeric, $9::numeric,
		10, 0, 5, 0, 3)`
	sopFix = `UPDATE mrp.sop_plan SET status = 'fixed', fixed_at = now(), fixed_by = $1, version = version + 1 WHERE id = $2`
)

// planned is the version numbered n, reviewed on 22 November 2026 for two
// months: the 23rd to the end of December.
func (s sopScene) planned(n int) stmt {
	return step(sopPlan, s.tenant, s.plan, s.company, n, "2026-11-22", "2026-11-23", "2026-12-31", 2)
}

// line is the plan's line of the group in the unit for November.
func (s sopScene) line(group, unit, forecast, orders string) stmt {
	return step(sopLine, s.tenant, s.plan, group, unit, "2026-11-01", "2026-11-23", "2026-11-30", forecast, orders)
}

func (s sopScene) fixed() stmt { return step(sopFix, s.tenant, s.plan) }

// TestSopPlanRules (S2n, migration 000445; ADR-0084 As built S2n): a
// version is numbered once per company, its horizon starts the day after
// its review and ends on the last day of its last month, of 1 to 12
// months; a fixed one names who fixed it and when, and changes no more,
// nor do its company, number and horizon; a line's days are of its month,
// its forecast never below its orders, its quantities never below zero;
// lines are added to a draft alone, and only a draft's consensus changes;
// nothing is deleted.
func TestSopPlanRules(t *testing.T) {
	need(t)
	cases := []struct {
		name string
		run  func(s sopScene) []stmt
		want string // empty: allowed
	}{
		{"a version with a line", func(s sopScene) []stmt { return []stmt{s.planned(1), s.line("FASTENERS", "EA", "50", "50")} }, ""},
		{"a version numbered twice", func(s sopScene) []stmt {
			return []stmt{s.planned(1), step(sopPlan, s.tenant, id(), s.company, 1, "2026-11-22", "2026-11-23", "2026-12-31", 2)}
		}, "sop_plan_number_once"},
		{"another company's version 1", func(s sopScene) []stmt {
			return []stmt{s.planned(1), step(sopPlan, s.tenant, id(), id(), 1, "2026-11-22", "2026-11-23", "2026-12-31", 2)}
		}, ""},
		{"number 0", func(s sopScene) []stmt { return []stmt{s.planned(0)} }, "sop_plan_number_check"},
		{"13 months", func(s sopScene) []stmt {
			return []stmt{step(sopPlan, s.tenant, s.plan, s.company, 1, "2026-11-22", "2026-11-23", "2027-11-30", 13)}
		}, "sop_plan_months_check"},
		{"a horizon starting on the review's day", func(s sopScene) []stmt {
			return []stmt{step(sopPlan, s.tenant, s.plan, s.company, 1, "2026-11-22", "2026-11-22", "2026-12-31", 2)}
		}, "sop_plan_horizon"},
		{"a horizon ending before its last month's end", func(s sopScene) []stmt {
			return []stmt{step(sopPlan, s.tenant, s.plan, s.company, 1, "2026-11-22", "2026-11-23", "2026-12-30", 2)}
		}, "sop_plan_horizon"},
		{"a horizon from the first of a month", func(s sopScene) []stmt {
			return []stmt{step(sopPlan, s.tenant, s.plan, s.company, 1, "2026-11-30", "2026-12-01", "2026-12-31", 1)}
		}, ""},
		{"fixed", func(s sopScene) []stmt { return []stmt{s.planned(1), s.fixed()} }, ""},
		{"fixed without who fixed it", func(s sopScene) []stmt {
			return []stmt{s.planned(1), step(`UPDATE mrp.sop_plan SET status = 'fixed', fixed_at = now(), version = version + 1`)}
		}, "sop_plan_fixed_by"},
		{"an unknown status", func(s sopScene) []stmt {
			return []stmt{s.planned(1), step(`UPDATE mrp.sop_plan SET status = 'approved', version = version + 1`)}
		}, "sop_plan_status_check"},
		{"a fixed version changed", func(s sopScene) []stmt {
			return []stmt{s.planned(1), s.fixed(), step(`UPDATE mrp.sop_plan SET version = version + 1`)}
		}, "so it changes no more"},
		{"a fixed version set draft again", func(s sopScene) []stmt {
			return []stmt{s.planned(1), s.fixed(), step(`UPDATE mrp.sop_plan SET status = 'draft', fixed_at = NULL, fixed_by = NULL, version = version + 1`)}
		}, "so it changes no more"},
		{"a draft's number changed", func(s sopScene) []stmt {
			return []stmt{s.planned(1), step(`UPDATE mrp.sop_plan SET number = 2, version = version + 1`)}
		}, "its company, number and horizon do not change"},
		{"a version deleted", func(s sopScene) []stmt { return []stmt{s.planned(1), step(`DELETE FROM mrp.sop_plan`)} }, "permission denied"},
		{"a line of no version", func(s sopScene) []stmt { return []stmt{s.line("FASTENERS", "EA", "1", "0")} }, "its plan version is not readable"},
		{"a line added to a fixed version", func(s sopScene) []stmt {
			return []stmt{s.planned(1), s.line("FASTENERS", "EA", "1", "0"), s.fixed(), s.line("BOLTS", "EA", "1", "0")}
		}, "its plan version is fixed"},
		{"negative stock transports", func(s sopScene) []stmt {
			return []stmt{s.planned(1), step(`INSERT INTO mrp.sop_plan_line (tenant_id, plan_id, material_group, uom_code, month, first_day, last_day, materials,
				forecast, orders, stock, purchase_orders, stock_transports, proposals, consensus)
				VALUES ($1, $2, 'BOLTS', 'EA', '2026-11-01', '2026-11-23', '2026-11-30', 1, 0, 0, 0, 0, -1, 0, 0)`, s.tenant, s.plan)}
		}, "sop_plan_line_stock_transports_check"},
		{"a line twice", func(s sopScene) []stmt {
			return []stmt{s.planned(1), s.line("FASTENERS", "EA", "1", "0"), s.line("FASTENERS", "EA", "2", "0")}
		}, "sop_plan_line_pkey"},
		{"a line in another unit", func(s sopScene) []stmt {
			return []stmt{s.planned(1), s.line("FASTENERS", "EA", "1", "0"), s.line("FASTENERS", "KG", "2", "0")}
		}, ""},
		{"an unknown unit", func(s sopScene) []stmt { return []stmt{s.planned(1), s.line("FASTENERS", "XX", "1", "0")} }, "sop_plan_line_uom_fkey"},
		{"a group of lower case", func(s sopScene) []stmt { return []stmt{s.planned(1), s.line("fasteners", "EA", "1", "0")} }, "sop_plan_line_material_group_check"},
		{"a forecast below its orders", func(s sopScene) []stmt { return []stmt{s.planned(1), s.line("FASTENERS", "EA", "1", "2")} }, "sop_plan_line_forecast_covers_orders"},
		{"a negative forecast", func(s sopScene) []stmt { return []stmt{s.planned(1), s.line("FASTENERS", "EA", "-1", "0")} }, "sop_plan_line_forecast_check"},
		{"a month not on its first day", func(s sopScene) []stmt {
			return []stmt{s.planned(1), step(sopLine, s.tenant, s.plan, "FASTENERS", "EA", "2026-11-02", "2026-11-23", "2026-11-30", "1", "0")}
		}, "sop_plan_line_days"},
		{"days past the month's end", func(s sopScene) []stmt {
			return []stmt{s.planned(1), step(sopLine, s.tenant, s.plan, "FASTENERS", "EA", "2026-11-01", "2026-11-23", "2026-12-01", "1", "0")}
		}, "sop_plan_line_days"},
		{"a negative consensus", func(s sopScene) []stmt {
			return []stmt{s.planned(1), s.line("FASTENERS", "EA", "1", "0"), step(`UPDATE mrp.sop_plan_line SET consensus = -1`)}
		}, "sop_plan_line_consensus_check"},
		{"a draft's consensus changed", func(s sopScene) []stmt {
			return []stmt{s.planned(1), s.line("FASTENERS", "EA", "1", "0"), step(`UPDATE mrp.sop_plan_line SET consensus = 7`)}
		}, ""},
		{"a fixed version's consensus changed", func(s sopScene) []stmt {
			return []stmt{s.planned(1), s.line("FASTENERS", "EA", "1", "0"), s.fixed(), step(`UPDATE mrp.sop_plan_line SET consensus = 7`)}
		}, "its plan version is fixed"},
		{"a line's forecast changed", func(s sopScene) []stmt {
			return []stmt{s.planned(1), s.line("FASTENERS", "EA", "1", "0"), step(`UPDATE mrp.sop_plan_line SET forecast = 7`)}
		}, "permission denied"},
		{"a line deleted", func(s sopScene) []stmt {
			return []stmt{s.planned(1), s.line("FASTENERS", "EA", "1", "0"), step(`DELETE FROM mrp.sop_plan_line`)}
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := sopScene{tenant: id(), plan: id(), company: id()}
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

// TestSopPlanTriggersAsOwner (S2n, migration 000445): past the grants, as
// the owner, a fixed version and its lines do not change nor gain a line,
// a draft's line changes only its consensus, and neither a version nor a
// line is deleted.
func TestSopPlanTriggersAsOwner(t *testing.T) {
	need(t)
	ctx := context.Background()
	for name, c := range map[string]struct {
		fix  bool
		st   func(s sopScene) stmt
		want string
	}{
		"a fixed version": {true, func(s sopScene) stmt {
			return step(`UPDATE mrp.sop_plan SET version = version + 1 WHERE id = $1`, s.plan)
		}, "so it changes no more"},
		"a draft's horizon": {false, func(s sopScene) stmt {
			return step(`UPDATE mrp.sop_plan SET months = 1, last_day = '2026-11-30' WHERE id = $1`, s.plan)
		}, "its company, number and horizon do not change"},
		"a version deleted": {false, func(s sopScene) stmt { return step(`DELETE FROM mrp.sop_plan WHERE id = $1`, s.plan) }, "never deleted"},
		"a fixed version's consensus": {true, func(s sopScene) stmt {
			return step(`UPDATE mrp.sop_plan_line SET consensus = 9 WHERE plan_id = $1`, s.plan)
		}, "its plan version is fixed"},
		"a draft's forecast": {false, func(s sopScene) stmt {
			return step(`UPDATE mrp.sop_plan_line SET forecast = 9 WHERE plan_id = $1`, s.plan)
		}, "only its consensus changes"},
		"a line deleted": {false, func(s sopScene) stmt { return step(`DELETE FROM mrp.sop_plan_line WHERE plan_id = $1`, s.plan) }, "never deleted"},
		"a line added to a fixed version": {true, func(s sopScene) stmt { return s.line("BOLTS", "EA", "1", "0") }, "its plan version is fixed"},
	} {
		t.Run(name, func(t *testing.T) {
			s := sopScene{tenant: id(), plan: id(), company: id()}
			tx, err := testDB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", s.tenant.String()); err != nil {
				t.Fatal(err)
			}
			setup := []stmt{s.planned(1), s.line("FASTENERS", "EA", "1", "0")}
			if c.fix {
				setup = append(setup, s.fixed())
			}
			if err := all(tx, setup...); err != nil {
				t.Fatal(err)
			}
			failsWith(t, all(tx, c.st(s)), c.want)
		})
	}
}
