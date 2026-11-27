package db

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// mrpLot is a lot of the scene's line: its number, quantity and day.
const mrpLot = `INSERT INTO mrp.run_line_lot (tenant_id, run_id, material_id, lot_no, quantity, needed_by) VALUES ($1, $2, $3, $4, $5::numeric, $6::date)`

func (s mrpScene) lot(no int, quantity, day string) stmt {
	return step(mrpLot, s.tenant, s.run, s.material, no, quantity, day)
}

// TestMRPPeriodLots (migration 000497; ADR-0084 As built, period lot
// sizes): a plan and a run line may name the daily, weekly and monthly
// procedures, with the bounds and rounding lot for lot takes and not the
// fixed lot size or the maximum stock level; a period-sized line's lots
// are numbered 1 to 100 within its lots, each of a positive quantity, and
// only such a line has them; as the application they are neither changed
// nor deleted.
func TestMRPPeriodLots(t *testing.T) {
	need(t)
	weekly := func(s mrpScene) []stmt { return []stmt{s.queued(), s.lotLine("30", "60", 2, "weekly")} }
	cases := []struct {
		name string
		run  func(s mrpScene) []stmt
		want string // empty: allowed
	}{
		{"a daily plan", func(s mrpScene) []stmt { return []stmt{s.lotPlan("daily", nil, nil, nil, nil, nil)} }, ""},
		{"a weekly plan, bounded and rounded", func(s mrpScene) []stmt { return []stmt{s.lotPlan("weekly", nil, nil, "20", "100", "10")} }, ""},
		{"a monthly plan", func(s mrpScene) []stmt { return []stmt{s.lotPlan("monthly", nil, nil, nil, nil, nil)} }, ""},
		{"a weekly plan with a fixed lot", func(s mrpScene) []stmt { return []stmt{s.lotPlan("weekly", "70", nil, nil, nil, nil)} },
			"material_plan_lot_size_fields"},
		{"a monthly plan with a level", func(s mrpScene) []stmt { return []stmt{s.lotPlan("monthly", nil, "90", nil, nil, nil)} },
			"material_plan_lot_size_fields"},
		{"a quarterly plan", func(s mrpScene) []stmt { return []stmt{s.lotPlan("quarterly", nil, nil, nil, nil, nil)} }, "material_plan_lot_size_check"},
		{"a weekly line's two lots", func(s mrpScene) []stmt {
			return append(weekly(s), s.lot(1, "30", "2026-11-30"), s.lot(2, "30", "2026-12-07"))
		}, ""},
		{"a daily line", func(s mrpScene) []stmt {
			return []stmt{s.queued(), s.lotLine("30", "30", 1, "daily"), s.lot(1, "30", "2026-11-30")}
		}, ""},
		{"a monthly line", func(s mrpScene) []stmt {
			return []stmt{s.queued(), s.lotLine("30", "30", 1, "monthly"), s.lot(1, "30", "2026-12-01")}
		}, ""},
		{"a quarterly line", func(s mrpScene) []stmt { return []stmt{s.queued(), s.lotLine("30", "30", 1, "quarterly")} }, "run_line_lot_size_check"},
		{"a lot beyond the line's lots", func(s mrpScene) []stmt { return append(weekly(s), s.lot(3, "30", "2026-12-14")) }, "period-sized line's lots"},
		{"a lot of a fixed line", func(s mrpScene) []stmt {
			return []stmt{s.queued(), s.lotLine("30", "70", 1, "fixed"), s.lot(1, "70", "2026-11-30")}
		}, "period-sized line's lots"},
		{"a lot of a line proposing nothing", func(s mrpScene) []stmt {
			return []stmt{s.queued(), s.lotLine("0", "0", nil, "weekly"), s.lot(1, "30", "2026-11-30")}
		}, "period-sized line's lots"},
		{"a lot of no line", func(s mrpScene) []stmt { return []stmt{s.queued(), s.lot(1, "30", "2026-11-30")} }, "period-sized line's lots"},
		{"a lot numbered 0", func(s mrpScene) []stmt { return append(weekly(s), s.lot(0, "30", "2026-11-30")) }, "run_line_lot_no_check"},
		{"a lot numbered 101", func(s mrpScene) []stmt {
			return []stmt{s.queued(), s.lotLine("30", "101", 101, "weekly"), s.lot(101, "1", "2026-11-30")}
		}, "run_line_lot_no_check"},
		{"a lot of nothing", func(s mrpScene) []stmt { return append(weekly(s), s.lot(1, "0", "2026-11-30")) }, "run_line_lot_quantity_check"},
		{"a lot twice", func(s mrpScene) []stmt {
			return append(weekly(s), s.lot(1, "30", "2026-11-30"), s.lot(1, "30", "2026-11-30"))
		},
			"run_line_lot_pkey"},
		{"a lot changed", func(s mrpScene) []stmt {
			return append(weekly(s), s.lot(1, "30", "2026-11-30"), step(`UPDATE mrp.run_line_lot SET quantity = 40`))
		}, "permission denied"},
		{"a lot deleted", func(s mrpScene) []stmt {
			return append(weekly(s), s.lot(1, "30", "2026-11-30"), step(`DELETE FROM mrp.run_line_lot`))
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := mrpScene{tenant: id(), plan: id(), plant: id(), material: id(), run: id(), requisition: id()}
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

// TestMRPPeriodLotsAsOwner (migration 000497): past the grants, as the
// owner, a lot is neither changed nor deleted.
func TestMRPPeriodLotsAsOwner(t *testing.T) {
	need(t)
	ctx := context.Background()
	s := mrpScene{tenant: id(), plan: id(), plant: id(), material: id(), run: id(), requisition: id()}
	for name, st := range map[string]stmt{
		"a lot changed": step(`UPDATE mrp.run_line_lot SET quantity = 40 WHERE run_id = $1`, s.run),
		"a lot deleted": step(`DELETE FROM mrp.run_line_lot WHERE run_id = $1`, s.run),
	} {
		t.Run(name, func(t *testing.T) {
			tx, err := testDB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", s.tenant.String()); err != nil {
				t.Fatal(err)
			}
			if err := all(tx, s.queued(), s.lotLine("30", "30", 1, "weekly"), s.lot(1, "30", "2026-11-30")); err != nil {
				t.Fatal(err)
			}
			failsWith(t, all(tx, st), "insert-only")
		})
	}
}

// TestMRPPeriodLotsDownRefused (migration 000497): the down refuses,
// rather than drops, period lot sizing the previous version cannot hold:
// a plan or a run line sized by period, and a lot (06 §6.4).
func TestMRPPeriodLotsDownRefused(t *testing.T) {
	need(t)
	down := migrationNamed(t, "000497_mrp_period_lots").Down
	for name, rows := range map[string]func(s mrpScene) []stmt{
		"a daily plan":   func(s mrpScene) []stmt { return []stmt{s.lotPlan("daily", nil, nil, nil, nil, nil)} },
		"a weekly plan":  func(s mrpScene) []stmt { return []stmt{s.lotPlan("weekly", nil, nil, nil, nil, nil)} },
		"a monthly plan": func(s mrpScene) []stmt { return []stmt{s.lotPlan("monthly", nil, nil, nil, nil, nil)} },
		"a weekly line proposing nothing": func(s mrpScene) []stmt {
			return []stmt{s.queued(), s.lotLine("0", "0", nil, "weekly")}
		},
		"a weekly line's lot": func(s mrpScene) []stmt {
			return []stmt{s.queued(), s.lotLine("30", "30", 1, "weekly"), s.lot(1, "30", "2026-11-30")}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := mrpScene{tenant: id(), plan: id(), plant: id(), material: id(), run: id(), requisition: id()}
			downRefused(t, down, s.tenant, rows(s), "period lot sizing of material plans or run lines exists")
		})
	}
}
