package mrp_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/mrp"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
	"github.com/mustie7iy/agentic-erp/internal/sales"
)

// codeOf is the problem code of err, "" for none.
func codeOf(err error) string {
	var p *problem.Problem
	if errors.As(err, &p) {
		return p.Code
	}
	return ""
}

// TestPlanChecks (ADR-0084 §4): a reorder point the contract's pattern
// lets through only as a decimal is still checked by the context for its
// callers: a negative one, or seven decimals, is refused naming the
// field.
func TestPlanChecks(t *testing.T) {
	e := setup(t)
	h := endpoints(t)
	p, tag := e.plan(t, e.bracket, "5")
	id := uuid.MustParse(p["id"].(string))
	for _, q := range []string{"-1", "1.0000001", "x"} {
		t.Run(q, func(t *testing.T) {
			err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
				_, err := h.CreatePlan(ctx, mrp.PlanInput{PlantID: e.p1, MaterialID: uuid.MustParse(e.material(t, "Bracket "+q, "raw_material")),
					AccountID: e.stock, MRPType: mrp.ReorderPoint, ReorderPoint: q})
				return err
			})
			if codeOf(err) != "mrp.invalid_quantity" {
				t.Errorf("create: got %v, want mrp.invalid_quantity", err)
			}
			err = apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
				_, err := h.ChangePlan(ctx, id, etag.IfMatch(tag), mrp.PlanChange{ReorderPoint: &q})
				return err
			})
			if codeOf(err) != "mrp.invalid_quantity" {
				t.Errorf("change: got %v, want mrp.invalid_quantity", err)
			}
		})
	}
}

// TestRunTwice (ADR-0084 §3, ADR-0012): each step of a run done again
// after its commit does nothing more: a second plan of a completed run
// proposes nothing and a failure recorded twice is recorded once.
func TestRunTwice(t *testing.T) {
	e := setup(t)
	e.plan(t, e.bracket, "250")
	in := e.start(t)
	w := planner(t)
	for i := range 2 {
		if err := w.Mrps.PlanMrpRun(context.Background(), in); err != nil {
			t.Fatalf("plan %d: %v", i+1, err)
		}
	}
	if got := e.proposals(t); len(got) != 1 {
		t.Errorf("after two plans: got %v, want one proposal", got)
	}
	failed := e.start(t)
	for i := range 2 {
		if err := w.Mrps.FailMrpRun(context.Background(), failed, *problem.Conflict("mrp.test_stop", "Stopped.")); err != nil {
			t.Fatalf("fail %d: %v", i+1, err)
		}
	}
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'mrp.run.fail'`, []any{e.Tenant.ID}, &n)
	if n != 1 {
		t.Errorf("failures audited: got %d, want 1", n)
	}
	if err := w.Mrps.PlanMrpRun(context.Background(), failed); err != nil {
		t.Errorf("plan of a failed run: got %v, want it left alone", err)
	}
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error { return endpoints(t).PlanRun(ctx, uuid.New()) })
	if codeOf(err) != "request.not_found" {
		t.Errorf("an unknown run: got %v, want request.not_found", err)
	}
	queued := e.start(t)
	if n, err := w.RunOnce(context.Background()); err != nil || n != 1 {
		t.Errorf("the in-process runner: got %d, %v; want the queued run carried out", n, err)
	}
	if r, _ := e.Must(200, "GET", runs+"/"+queued.RunID.String(), "", ""); r["status"] != "completed" {
		t.Errorf("the queued run: got %v, want completed", r["status"])
	}
}

// changing is the mrp context's supply with a requisition changed by a
// person between the run's read and its withdrawal: Withdraw finds it no
// longer the run's own.
type changing struct{ mrp.Supply }

func (changing) Withdraw(context.Context, uuid.UUID, int32) (bool, error) { return false, nil }

// TestProposalChangedDuringTheRun: a proposal a person changes while the
// run settles it is taken over, not replaced, and counted as a receipt.
func TestProposalChangedDuringTheRun(t *testing.T) {
	e := setup(t)
	e.plan(t, e.bracket, "250")
	e.run(t)
	h := endpoints(t)
	h.Supply = changing{h.Supply}
	in := e.start(t)
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error { return h.PlanRun(ctx, in.RunID) }); err != nil {
		t.Fatal(err)
	}
	if got := e.proposals(t); got["PR100000"] != "taken_over" {
		t.Errorf("the first proposal: got %v, want it taken over", got)
	}
}

// TestDatabaseFailures calls every handler and step outside a unit of
// work and with each of its database statements failing in turn.
func TestDatabaseFailures(t *testing.T) {
	e := setup(t)
	h := endpoints(t)
	p, tag := e.plan(t, e.bracket, "250")
	plan := uuid.MustParse(p["id"].(string))
	e.confirmed(t, "1 BOX")
	e.ordered(t, "PO1", "released", "5")
	done := e.run(t)
	taken := rows(t, e.run(t), "lines")[0]["requisition_id"].(string) // the second run replaced the first's proposal
	_, rtag := e.requisition(t, taken)
	e.Must(200, "PATCH", "/v1/requisitions/"+taken, `{"needed_by": "2026-12-31"}`, rtag)
	e.confirmed(t, "1 BOX")
	e.run(t)             // proposes 20 more, beside the proposal taken over
	queued := e.start(t) // replaces that proposal, counts the one taken over and proposes again
	other := uuid.MustParse(e.material(t, "Other bracket", "raw_material"))
	q := "60"
	calls := map[string]func(ctx context.Context) error{
		"create": func(ctx context.Context) error {
			_, err := h.CreateMaterialPlan(ctx, &gen.MaterialPlanCreate{PlantID: e.p1, MaterialID: other, MrpType: mrp.ReorderPoint, ReorderPoint: "1",
				AccountID: e.stock}, gen.CreateMaterialPlanParams{})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetMaterialPlan(ctx, gen.GetMaterialPlanParams{ID: plan})
			return err
		},
		"change": func(ctx context.Context) error {
			_, err := h.ChangePlan(ctx, plan, etag.IfMatch(tag), mrp.PlanChange{ReorderPoint: &q})
			return err
		},
		"update": func(ctx context.Context) error {
			_, err := h.UpdateMaterialPlan(ctx, &gen.MaterialPlanUpdate{ReorderPoint: gen.NewOptString(q)}, gen.UpdateMaterialPlanParams{ID: plan, IfMatch: tag})
			return err
		},
		"list plans": func(ctx context.Context) error {
			_, err := h.ListMaterialPlans(ctx, gen.ListMaterialPlansParams{})
			return err
		},
		"start": func(ctx context.Context) error {
			_, err := h.StartMrpRun(ctx, &gen.MrpRunStart{PlantID: e.p9}, gen.StartMrpRunParams{})
			return err
		},
		"get a run": func(ctx context.Context) error {
			_, err := h.GetMrpRun(ctx, gen.GetMrpRunParams{ID: uuid.MustParse(done["id"].(string))})
			return err
		},
		"list runs": func(ctx context.Context) error {
			_, err := h.ListMrpRuns(ctx, gen.ListMrpRunsParams{})
			return err
		},
		"plan": func(ctx context.Context) error { return h.PlanRun(ctx, queued.RunID) },
		"fail": func(ctx context.Context) error {
			return h.FailRun(ctx, queued.RunID, problem.Conflict("mrp.test_stop", "Stopped."))
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, call)
		})
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "unfinished", func(ctx context.Context) error {
		tx, _ := reqctx.TxFrom(ctx) // EveryFailure's request always has one
		_, err := h.UnfinishedRuns(ctx, tx)
		return err
	})
}

// TestWithdrawAChangedProposal: procurement withdraws a proposal only at
// the version the run created it in, a draft; one a person changed is
// left alone.
func TestWithdrawAChangedProposal(t *testing.T) {
	e := setup(t)
	e.plan(t, e.bracket, "250")
	r1 := uuid.MustParse(rows(t, e.run(t), "lines")[0]["requisition_id"].(string))
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		withdrawn, err := app.Procurement(app.Config{}).WithdrawProposal(ctx, r1, 2)
		if err == nil && withdrawn {
			t.Error("at another version: got withdrawn, want it left alone")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestRunInProgressConstraint: a run started at the same time as another
// of the plant, whose insert meets the other's queued row, is refused as
// the check before it refuses it.
func TestRunInProgressConstraint(t *testing.T) {
	if p := problem.From(&pgconn.PgError{Code: "23505", ConstraintName: "run_one_at_a_time"}); p.Status != 409 || p.Code != "mrp.run_in_progress" {
		t.Errorf("got %d %s, want 409 mrp.run_in_progress", p.Status, p.Code)
	}
}

// TestPublishedReadsOutsideARequest: the reads and writes other contexts
// publish for planning need a unit of work.
func TestPublishedReadsOutsideARequest(t *testing.T) {
	ctx, ids := context.Background(), []uuid.UUID{uuid.New()}
	inv, pr := app.Inventory(app.Config{}), app.Procurement(app.Config{})
	for name, call := range map[string]func() error{
		"demand":       func() error { _, err := sales.Orders{}.PlantRequirements(ctx, ids[0], ids); return err },
		"prices":       func() error { _, err := inv.PlantPrices(ctx, ids[0], ids); return err },
		"receipts":     func() error { _, err := procurement.Orders{}.OpenReceipts(ctx, ids[0], ids); return err },
		"requisitions": func() error { _, err := procurement.Orders{}.Requisitions(ctx, ids); return err },
		"withdraw":     func() error { _, err := pr.WithdrawProposal(ctx, ids[0], 1); return err },
	} {
		if err := call(); err == nil {
			t.Errorf("%s: got nil error, want an error", name)
		}
	}
}
