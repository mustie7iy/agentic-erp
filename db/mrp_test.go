package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestMRPPermissions (S2k, migration 000431; ADR-0084 §5): the
// permissions of material requirements planning are seeded and are no
// approval permissions.
func TestMRPPermissions(t *testing.T) {
	need(t)
	rows, err := testDB.Query(context.Background(), `SELECT code FROM ref.permission WHERE code LIKE 'mrp.%' AND NOT is_approval ORDER BY code`)
	if err != nil {
		t.Fatal(err)
	}
	codes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(codes, " "), "mrp.plan.read mrp.plan.write mrp.run.start"; got != want {
		t.Errorf("got %v, want %s", codes, want)
	}
}

// mrpScene is a tenant with a material plan, a run and a proposal, by id.
type mrpScene struct{ tenant, plan, plant, material, run, requisition uuid.UUID }

const (
	mrpPlan = `INSERT INTO mrp.material_plan (tenant_id, id, legal_entity_id, plant_id, material_id, mrp_type, reorder_point, account_id, created_by, updated_by)
		VALUES ($1, $2, gen_random_uuid(), $3, $4, $5, $6::numeric, gen_random_uuid(), $1, $1)`
	mrpRun = `INSERT INTO mrp.run (tenant_id, id, legal_entity_id, plant_id, operation_id, created_by, updated_by)
		VALUES ($1, $2, gen_random_uuid(), $3, gen_random_uuid(), $1, $1)`
	mrpComplete = `UPDATE mrp.run SET status = 'completed', run_date = '2026-11-21', materials_planned = 1, proposals_created = 1, proposals_replaced = 0,
		version = version + 1 WHERE id = $1`
	mrpLine = `INSERT INTO mrp.run_line (tenant_id, run_id, material_id, uom_code, reorder_point, demand, stock, receipts, shortage, requisition_id,
		requisition_number) VALUES ($1, $2, $3, $4, 50, $5::numeric, 10, 0, $6::numeric, $7, $8)`
	mrpProposal = `INSERT INTO mrp.proposal (tenant_id, requisition_id, requisition_number, requisition_version, run_id, plant_id, material_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`
)

func (s mrpScene) planned(mrpType, rop string) stmt {
	return step(mrpPlan, s.tenant, s.plan, s.plant, s.material, mrpType, rop)
}

func (s mrpScene) queued() stmt { return step(mrpRun, s.tenant, s.run, s.plant) }

func (s mrpScene) line(unit, demand, shortage string, req any, number any) stmt {
	return step(mrpLine, s.tenant, s.run, s.material, unit, demand, shortage, req, number)
}

func (s mrpScene) proposed(number string, version int) stmt {
	return step(mrpProposal, s.tenant, s.requisition, number, version, s.run, s.plant, s.material)
}

// TestMRPRules (S2k, migration 000431; ADR-0084 §4): a plan has a known
// MRP type and a reorder point of zero or more, once per plant and
// material, and its company, plant and material do not change; one run
// of a plant is queued at a time, a completed run names its day and
// counts and a failed one its problem, and an ended run changes no more;
// a run's line counts nothing below zero and names a requisition exactly
// when it has a shortage, and does not change; a proposal moves from open
// to replaced (naming the run), taken over or closed, and from taken over
// to closed, and no further. Nothing is deleted.
func TestMRPRules(t *testing.T) {
	need(t)
	cases := []struct {
		name string
		run  func(s mrpScene) []stmt
		want string // empty: allowed
	}{
		{"a plan", func(s mrpScene) []stmt { return []stmt{s.planned("reorder_point", "0")} }, ""},
		{"an unknown MRP type", func(s mrpScene) []stmt { return []stmt{s.planned("forecast", "1")} }, "material_plan_mrp_type_check"},
		{"a reorder point below zero", func(s mrpScene) []stmt { return []stmt{s.planned("reorder_point", "-1")} }, "material_plan_reorder_point_check"},
		{"a material planned twice at a plant", func(s mrpScene) []stmt {
			return []stmt{s.planned("reorder_point", "1"), step(mrpPlan, s.tenant, id(), s.plant, s.material, "no_planning", "0")}
		}, "material_plan_once"},
		{"a plan's material changed", func(s mrpScene) []stmt {
			return []stmt{s.planned("reorder_point", "1"), step(`UPDATE mrp.material_plan SET material_id = gen_random_uuid(), version = version + 1`)}
		}, "company, plant and material do not change"},
		{"a plan's reorder point changed", func(s mrpScene) []stmt {
			return []stmt{s.planned("reorder_point", "1"), step(`UPDATE mrp.material_plan SET reorder_point = 2, version = version + 1`)}
		}, ""},
		{"a plan deleted", func(s mrpScene) []stmt {
			return []stmt{s.planned("reorder_point", "1"), step(`DELETE FROM mrp.material_plan`)}
		}, "permission denied"},
		{"two runs of a plant queued", func(s mrpScene) []stmt { return []stmt{s.queued(), step(mrpRun, s.tenant, id(), s.plant)} }, "run_one_at_a_time"},
		{"a run queued after one completed", func(s mrpScene) []stmt {
			return []stmt{s.queued(), step(mrpComplete, s.run), step(mrpRun, s.tenant, id(), s.plant)}
		}, ""},
		{"completed without its counts", func(s mrpScene) []stmt {
			return []stmt{s.queued(), step(`UPDATE mrp.run SET status = 'completed', run_date = '2026-11-21', version = version + 1`)}
		}, "run_outcome"},
		{"failed without its problem", func(s mrpScene) []stmt {
			return []stmt{s.queued(), step(`UPDATE mrp.run SET status = 'failed', version = version + 1`)}
		}, "run_outcome"},
		{"an unknown status", func(s mrpScene) []stmt {
			return []stmt{s.queued(), step(`UPDATE mrp.run SET status = 'running', version = version + 1`)}
		}, "run_status_check"},
		{"a negative count", func(s mrpScene) []stmt {
			return []stmt{s.queued(), step(`UPDATE mrp.run SET status = 'completed', run_date = '2026-11-21', materials_planned = -1, proposals_created = 0,
				proposals_replaced = 0, version = version + 1`)}
		}, "run_materials_planned_check"},
		{"a completed run changed", func(s mrpScene) []stmt {
			return []stmt{s.queued(), step(mrpComplete, s.run), step(`UPDATE mrp.run SET version = version + 1`)}
		}, "so it changes no more"},
		{"a queued run's plant changed", func(s mrpScene) []stmt {
			return []stmt{s.queued(), step(`UPDATE mrp.run SET plant_id = gen_random_uuid(), version = version + 1`)}
		}, "its company, plant and operation do not change"},
		{"a run deleted", func(s mrpScene) []stmt { return []stmt{s.queued(), step(`DELETE FROM mrp.run`)} }, "permission denied"},
		{"a line with its proposal", func(s mrpScene) []stmt {
			return []stmt{s.queued(), s.line("EA", "60", "100", s.requisition, "PR1")}
		}, ""},
		{"a shortage without a requisition", func(s mrpScene) []stmt { return []stmt{s.queued(), s.line("EA", "60", "100", nil, nil)} }, "run_line_proposed"},
		{"a requisition without a shortage", func(s mrpScene) []stmt {
			return []stmt{s.queued(), s.line("EA", "0", "0", s.requisition, "PR1")}
		}, "run_line_proposed"},
		{"a requisition without its number", func(s mrpScene) []stmt {
			return []stmt{s.queued(), s.line("EA", "60", "100", s.requisition, nil)}
		}, "run_line_proposed"},
		{"a negative demand", func(s mrpScene) []stmt { return []stmt{s.queued(), s.line("EA", "-1", "0", nil, nil)} }, "run_line_demand_check"},
		{"an unknown unit", func(s mrpScene) []stmt { return []stmt{s.queued(), s.line("XX", "0", "0", nil, nil)} }, "run_line_uom_fkey"},
		{"a line of no run", func(s mrpScene) []stmt { return []stmt{s.line("EA", "0", "0", nil, nil)} }, "run_line_run_fkey"},
		{"a line changed", func(s mrpScene) []stmt {
			return []stmt{s.queued(), s.line("EA", "0", "0", nil, nil), step(`UPDATE mrp.run_line SET demand = 1`)}
		}, "permission denied"},
		{"a proposal", func(s mrpScene) []stmt { return []stmt{s.queued(), s.proposed("PR1", 1)} }, ""},
		{"a proposal at version 0", func(s mrpScene) []stmt { return []stmt{s.queued(), s.proposed("PR1", 0)} }, "proposal_requisition_version_check"},
		{"a proposal of no run", func(s mrpScene) []stmt { return []stmt{s.proposed("PR1", 1)} }, "proposal_run_fkey"},
		{"replaced without the run", func(s mrpScene) []stmt {
			return []stmt{s.queued(), s.proposed("PR1", 1), step(`UPDATE mrp.proposal SET status = 'replaced'`)}
		}, "proposal_replaced"},
		{"replaced by a run", func(s mrpScene) []stmt {
			return []stmt{s.queued(), s.proposed("PR1", 1), step(`UPDATE mrp.proposal SET status = 'replaced', replaced_by_run_id = $1`, s.run)}
		}, ""},
		{"replaced, then open again", func(s mrpScene) []stmt {
			return []stmt{s.queued(), s.proposed("PR1", 1), step(`UPDATE mrp.proposal SET status = 'replaced', replaced_by_run_id = $1`, s.run),
				step(`UPDATE mrp.proposal SET status = 'open', replaced_by_run_id = NULL`)}
		}, "it moves only from open"},
		{"taken over, then closed", func(s mrpScene) []stmt {
			return []stmt{s.queued(), s.proposed("PR1", 1), step(`UPDATE mrp.proposal SET status = 'taken_over'`), step(`UPDATE mrp.proposal SET status = 'closed'`)}
		}, ""},
		{"taken over, then open again", func(s mrpScene) []stmt {
			return []stmt{s.queued(), s.proposed("PR1", 1), step(`UPDATE mrp.proposal SET status = 'taken_over'`), step(`UPDATE mrp.proposal SET status = 'open'`)}
		}, "it moves only from open"},
		{"closed, then taken over", func(s mrpScene) []stmt {
			return []stmt{s.queued(), s.proposed("PR1", 1), step(`UPDATE mrp.proposal SET status = 'closed'`), step(`UPDATE mrp.proposal SET status = 'taken_over'`)}
		}, "it moves only from open"},
		{"an unknown proposal status", func(s mrpScene) []stmt {
			return []stmt{s.queued(), s.proposed("PR1", 1), step(`UPDATE mrp.proposal SET status = 'ordered'`)}
		}, "proposal_status_check"},
		{"a proposal's number changed", func(s mrpScene) []stmt {
			return []stmt{s.queued(), s.proposed("PR1", 1), step(`UPDATE mrp.proposal SET requisition_number = 'PR2'`)}
		}, "permission denied"},
		{"a proposal deleted", func(s mrpScene) []stmt {
			return []stmt{s.queued(), s.proposed("PR1", 1), step(`DELETE FROM mrp.proposal`)}
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

// TestMRPTriggersAsOwner (S2k, migration 000431): past the grants, as the
// owner, a plan's material, a proposal's number and an ended run do not
// change, and lines and proposals are not deleted.
func TestMRPTriggersAsOwner(t *testing.T) {
	need(t)
	ctx := context.Background()
	s := mrpScene{tenant: id(), plan: id(), plant: id(), material: id(), run: id(), requisition: id()}
	for name, c := range map[string]struct {
		st   stmt
		want string
	}{
		"a plan's plant":      {step(`UPDATE mrp.material_plan SET plant_id = gen_random_uuid(), version = version + 1 WHERE id = $1`, s.plan), "company, plant and material do not change"},
		"a completed run":     {step(`UPDATE mrp.run SET version = version + 1 WHERE id = $1`, s.run), "so it changes no more"},
		"a line deleted":      {step(`DELETE FROM mrp.run_line WHERE run_id = $1`, s.run), "insert-only"},
		"a proposal.s number": {step(`UPDATE mrp.proposal SET requisition_number = 'PR9' WHERE requisition_id = $1`, s.requisition), "it moves only from open"},
		"a proposal deleted":  {step(`DELETE FROM mrp.proposal WHERE requisition_id = $1`, s.requisition), "insert-only"},
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
			if err := all(tx, s.planned("reorder_point", "1"), s.queued(), s.line("EA", "60", "100", s.requisition, "PR1"), s.proposed("PR1", 1),
				step(mrpComplete, s.run)); err != nil {
				t.Fatal(err)
			}
			failsWith(t, all(tx, c.st), c.want)
		})
	}
}
