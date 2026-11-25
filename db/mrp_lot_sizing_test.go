package db

import (
	"testing"

	"github.com/jackc/pgx/v5"
)

// mrpLotPlan is a plan of the scene with its lot sizing: the procedure,
// then the fixed lot size, maximum stock level, minimum and maximum lot
// sizes and rounding value, at reorder point 50.
const mrpLotPlan = `INSERT INTO mrp.material_plan (tenant_id, id, legal_entity_id, plant_id, material_id, mrp_type, reorder_point, account_id, lot_size,
		fixed_lot_size, maximum_stock_level, minimum_lot_size, maximum_lot_size, rounding_value, created_by, updated_by)
	VALUES ($1, $2, gen_random_uuid(), $3, $4, 'reorder_point', 50, gen_random_uuid(), $5, $6::numeric, $7::numeric, $8::numeric, $9::numeric,
		$10::numeric, $1, $1)`

// mrpLotLine is a line of the scene's run with its shortage, requisition,
// lot sizing procedure, proposed quantity and lots.
const mrpLotLine = `INSERT INTO mrp.run_line (tenant_id, run_id, material_id, uom_code, reorder_point, demand, stock, receipts, shortage, requisition_id,
		requisition_number, lot_size, proposed, lots)
	VALUES ($1, $2, $3, 'EA', 50, 40, 10, 0, $4::numeric, $5, $6, $7, $8::numeric, $9)`

func (s mrpScene) lotPlan(procedure string, fixed, level, minimum, maximum, rounding any) stmt {
	return step(mrpLotPlan, s.tenant, s.plan, s.plant, s.material, procedure, fixed, level, minimum, maximum, rounding)
}

func (s mrpScene) lotLine(shortage string, proposed, lots any, procedure string) stmt {
	var req, number any
	if shortage != "0" {
		req, number = s.requisition, "PR1"
	}
	return step(mrpLotLine, s.tenant, s.run, s.material, shortage, req, number, procedure, proposed, lots)
}

// TestMRPLotSizing (migration 000482; ADR-0084 As built, lot sizing): a
// plan's lot sizing is a known procedure; its fixed lot size and maximum
// stock level are given exactly with their procedures, the level not
// below the reorder point; a fixed lot is not bounded or rounded; every
// quantity is positive, the minimum not above the maximum and both
// multiples of the rounding value. A plan of a server before lot sizing
// is lot for lot (the column's default). A run line's proposed quantity covers its shortage, is
// zero exactly without one, in one lot or more exactly with one; a line
// of a server before names neither.
func TestMRPLotSizing(t *testing.T) {
	need(t)
	cases := []struct {
		name string
		run  func(s mrpScene) []stmt
		want string // empty: allowed
	}{
		{"a plan naming no lot sizing, as a server before writes it", func(s mrpScene) []stmt { return []stmt{s.planned("reorder_point", "1")} }, ""},
		{"lot for lot, bounded and rounded", func(s mrpScene) []stmt { return []stmt{s.lotPlan("lot_for_lot", nil, nil, "24", "480", "12")} }, ""},
		{"fixed", func(s mrpScene) []stmt { return []stmt{s.lotPlan("fixed", "70", nil, nil, nil, nil)} }, ""},
		{"to the maximum", func(s mrpScene) []stmt { return []stmt{s.lotPlan("replenish_to_maximum", nil, "50", "10", nil, "5")} }, ""},
		{"an unknown procedure", func(s mrpScene) []stmt { return []stmt{s.lotPlan("period", nil, nil, nil, nil, nil)} }, "material_plan_lot_size_check"},
		{"fixed without its lot", func(s mrpScene) []stmt { return []stmt{s.lotPlan("fixed", nil, nil, nil, nil, nil)} }, "material_plan_lot_size_fields"},
		{"a fixed lot with lot for lot", func(s mrpScene) []stmt { return []stmt{s.lotPlan("lot_for_lot", "70", nil, nil, nil, nil)} },
			"material_plan_lot_size_fields"},
		{"to the maximum without its level", func(s mrpScene) []stmt { return []stmt{s.lotPlan("replenish_to_maximum", nil, nil, nil, nil, nil)} },
			"material_plan_lot_size_fields"},
		{"a level with a fixed lot", func(s mrpScene) []stmt { return []stmt{s.lotPlan("fixed", "70", "90", nil, nil, nil)} },
			"material_plan_lot_size_fields"},
		{"a fixed lot rounded", func(s mrpScene) []stmt { return []stmt{s.lotPlan("fixed", "70", nil, nil, nil, "10")} }, "material_plan_lot_size_fields"},
		{"a fixed lot with a minimum", func(s mrpScene) []stmt { return []stmt{s.lotPlan("fixed", "70", nil, "10", nil, nil)} },
			"material_plan_lot_size_fields"},
		{"a fixed lot with a maximum", func(s mrpScene) []stmt { return []stmt{s.lotPlan("fixed", "70", nil, nil, "100", nil)} },
			"material_plan_lot_size_fields"},
		{"a level below the reorder point", func(s mrpScene) []stmt { return []stmt{s.lotPlan("replenish_to_maximum", nil, "49", nil, nil, nil)} },
			"material_plan_maximum_level"},
		{"the reorder point raised above the level", func(s mrpScene) []stmt {
			return []stmt{s.lotPlan("replenish_to_maximum", nil, "50", nil, nil, nil), step(`UPDATE mrp.material_plan SET reorder_point = 51, version = version + 1`)}
		}, "material_plan_maximum_level"},
		{"a zero fixed lot", func(s mrpScene) []stmt { return []stmt{s.lotPlan("fixed", "0", nil, nil, nil, nil)} }, "material_plan_fixed_lot_size_check"},
		{"a zero level", func(s mrpScene) []stmt {
			return []stmt{s.planned("reorder_point", "0"), step(`UPDATE mrp.material_plan SET lot_size = 'replenish_to_maximum', maximum_stock_level = 0,
				version = version + 1`)}
		}, "material_plan_maximum_stock_level_check"},
		{"a zero minimum", func(s mrpScene) []stmt { return []stmt{s.lotPlan("lot_for_lot", nil, nil, "0", nil, nil)} }, "material_plan_minimum_lot_size_check"},
		{"a zero maximum", func(s mrpScene) []stmt { return []stmt{s.lotPlan("lot_for_lot", nil, nil, nil, "0", nil)} }, "material_plan_maximum_lot_size_check"},
		{"a zero rounding value", func(s mrpScene) []stmt { return []stmt{s.lotPlan("lot_for_lot", nil, nil, nil, nil, "0")} },
			"material_plan_rounding_value_check"},
		{"a minimum above the maximum", func(s mrpScene) []stmt { return []stmt{s.lotPlan("lot_for_lot", nil, nil, "60", "50", nil)} },
			"material_plan_lot_bounds"},
		{"a minimum not a multiple of the rounding", func(s mrpScene) []stmt { return []stmt{s.lotPlan("lot_for_lot", nil, nil, "25", nil, "12")} },
			"material_plan_rounding"},
		{"a maximum not a multiple of the rounding", func(s mrpScene) []stmt { return []stmt{s.lotPlan("lot_for_lot", nil, nil, nil, "50", "0.3")} },
			"material_plan_rounding"},
		{"a line proposing lots", func(s mrpScene) []stmt { return []stmt{s.queued(), s.lotLine("30", "70", 1, "fixed")} }, ""},
		{"a line proposing nothing", func(s mrpScene) []stmt { return []stmt{s.queued(), s.lotLine("0", "0", nil, "fixed")} }, ""},
		{"a line of a server before", func(s mrpScene) []stmt { return []stmt{s.queued(), s.lotLine("30", nil, nil, "lot_for_lot")} }, ""},
		{"an unknown procedure on a line", func(s mrpScene) []stmt { return []stmt{s.queued(), s.lotLine("30", "30", 1, "period")} }, "run_line_lot_size_check"},
		{"less proposed than the shortage", func(s mrpScene) []stmt { return []stmt{s.queued(), s.lotLine("30", "29", 1, "lot_for_lot")} }, "run_line_lots"},
		{"proposed without a shortage", func(s mrpScene) []stmt { return []stmt{s.queued(), s.lotLine("0", "10", nil, "lot_for_lot")} }, "run_line_lots"},
		{"lots without a proposal", func(s mrpScene) []stmt { return []stmt{s.queued(), s.lotLine("0", "0", 1, "lot_for_lot")} }, "run_line_lots"},
		{"a proposal without its lots", func(s mrpScene) []stmt { return []stmt{s.queued(), s.lotLine("30", "30", nil, "lot_for_lot")} }, "run_line_lots"},
		{"no lots", func(s mrpScene) []stmt { return []stmt{s.queued(), s.lotLine("30", "30", 0, "lot_for_lot")} }, "run_line_lots"},
		{"lots without the proposed quantity", func(s mrpScene) []stmt { return []stmt{s.queued(), s.lotLine("30", nil, 1, "lot_for_lot")} }, "run_line_lots"},
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
