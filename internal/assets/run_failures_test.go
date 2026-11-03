package assets_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/assets"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// TestDepreciationRunDatabaseFailures: every database statement of a
// run's start, reads and steps failing is reported, never hidden: the
// start of July's run after June's posted, the run's and the list's
// reads, the plan of a queued run, the post of its pending batch, the
// finish and the failure of a run whose batches are posted, and the
// in-process runner's read of unfinished runs.
func TestDepreciationRunDatabaseFailures(t *testing.T) {
	e := newRuns(t)
	pages, err := pagination.NewCodec([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	h := app.Assets(app.Config{Pages: pages})
	e.depreciable(t, "Mill")
	e.depreciable(t, "Lathe")
	_, june := e.start(t, e.l0, 6)
	e.carry(t, june)
	start := &gen.DepreciationRunStart{LegalEntityID: e.DE, FiscalYear: 2026, Period: 7}
	every := func(name string, fn func(ctx context.Context) error) {
		t.Run(name, func(t *testing.T) { apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, fn) })
	}
	every("the start", func(ctx context.Context) error {
		_, err := h.StartDepreciationRun(ctx, start, gen.StartDepreciationRunParams{})
		return err
	})
	every("the run", func(ctx context.Context) error {
		_, err := h.GetDepreciationRun(ctx, gen.GetDepreciationRunParams{ID: june})
		return err
	})
	every("the list", func(ctx context.Context) error {
		_, err := h.ListDepreciationRuns(ctx, gen.ListDepreciationRunsParams{})
		return err
	})
	_, july := e.start(t, e.l0, 7)
	every("the plan", func(ctx context.Context) error {
		_, err := h.PlanDepreciationRun(ctx, july)
		return err
	})
	if err := e.step(t, func(ctx context.Context, h *assets.Endpoints) error {
		_, err := h.PlanDepreciationRun(ctx, july)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	every("the batch", func(ctx context.Context) error { return h.PostDepreciationBatch(ctx, july, 1) })
	if err := e.step(t, func(ctx context.Context, h *assets.Endpoints) error { return h.PostDepreciationBatch(ctx, july, 1) }); err != nil {
		t.Fatal(err)
	}
	every("the finish", func(ctx context.Context) error { return h.FinishDepreciationRun(ctx, july) })
	every("the failure", func(ctx context.Context) error {
		return h.FailDepreciationRun(ctx, july, problem.Conflict("ledger.period_not_open", "Closed."))
	})
	every("the unfinished runs", func(ctx context.Context) error {
		tx, _ := reqctx.TxFrom(ctx) // EveryFailure runs it in a unit of work
		_, err := h.UnfinishedDepreciationRuns(ctx, tx)
		return err
	})
	for name, fn := range map[string]func(ctx context.Context) error{
		"the start": func(ctx context.Context) error {
			_, err := h.StartDepreciationRun(ctx, start, gen.StartDepreciationRunParams{})
			return err
		},
		"the run": func(ctx context.Context) error {
			_, err := h.GetDepreciationRun(ctx, gen.GetDepreciationRunParams{ID: june})
			return err
		},
		"the list": func(ctx context.Context) error {
			_, err := h.ListDepreciationRuns(ctx, gen.ListDepreciationRunsParams{})
			return err
		},
	} {
		t.Run(name+" outside a unit of work", func(t *testing.T) {
			if err := fn(context.Background()); !errors.Is(err, reqctx.ErrOutsideRequest) {
				t.Errorf("got %v, want reqctx.ErrOutsideRequest", err)
			}
		})
	}
}

// failingRuns is the operations port with each method failing.
type failingRuns struct{ assets.Runs }

var errRuns = errors.New("operations unavailable")

func (failingRuns) Start(context.Context, string, any) (gen.Operation, error) {
	return gen.Operation{}, errRuns
}
func (failingRuns) Report(context.Context, uuid.UUID, int64, int64) error    { return errRuns }
func (failingRuns) Cancelled(context.Context, uuid.UUID, bool) (bool, error) { return false, nil }
func (failingRuns) Succeed(context.Context, uuid.UUID, string, any) error    { return errRuns }
func (failingRuns) Fail(context.Context, uuid.UUID, *problem.Problem) error  { return errRuns }

// TestDepreciationRunPortsFail: a run's step whose port fails is refused
// with the cause: the Operation not written or not reported, the
// configuration lost or without the asset's class, the group currency's
// rate missing (422 ledger.no_exchange_rate, a business problem that
// fails the run), the Operation's cancellation unread; an Operation
// cancelled meanwhile stops the run at its next step, failing it with
// assets.run_cancelled, the batch posted staying posted and the Operation
// cancelled.
func TestDepreciationRunPortsFail(t *testing.T) {
	e := newRuns(t)
	e.depreciable(t, "Mill")
	in := func(h *assets.Endpoints, fn func(ctx context.Context) error) error {
		return apitest.InRequest(t, e.Tenant, e.Tenant.Human, fn)
	}
	broken := app.Assets(app.Config{})
	broken.Runs = failingRuns{}
	start := &gen.DepreciationRunStart{LegalEntityID: e.DE, FiscalYear: 2026, Period: 6}
	if err := in(broken, func(ctx context.Context) error {
		_, err := broken.StartDepreciationRun(ctx, start, gen.StartDepreciationRunParams{})
		return err
	}); !errors.Is(err, errRuns) {
		t.Errorf("the start: got %v, want the operation's failure", err)
	}
	op, run := e.start(t, e.l0, 6)
	for name, fn := range map[string]func(ctx context.Context) error{
		"the plan's report": func(ctx context.Context) error { _, err := broken.PlanDepreciationRun(ctx, run); return err },
	} {
		if err := in(broken, fn); !errors.Is(err, errRuns) {
			t.Errorf("%s: got %v, want the operation's failure", name, err)
		}
	}
	if err := e.step(t, func(ctx context.Context, h *assets.Endpoints) error {
		_, err := h.PlanDepreciationRun(ctx, run)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		settings assets.Settings
		want     string
	}{
		"a class lost":     {classGone{configuration.Settings{}}, "assets.class_unknown"},
		"no configuration": {settingsLost{}, "configuration lost"},
		"the report":       {nil, errRuns.Error()},
	} {
		t.Run(name, func(t *testing.T) {
			h := app.Assets(app.Config{})
			if c.settings != nil {
				h.Settings = c.settings
			} else {
				h.Runs = failingRuns{}
			}
			err := in(h, func(ctx context.Context) error { return h.PostDepreciationBatch(ctx, run, 1) })
			if err == nil || !strings.Contains(fmt.Sprint(err, problemCode(err)), c.want) {
				t.Errorf("got %v, want %s", err, c.want)
			}
		})
	}
	if err := e.step(t, func(ctx context.Context, h *assets.Endpoints) error { return h.PostDepreciationBatch(ctx, run, 1) }); err != nil {
		t.Fatal(err)
	}
	if err := in(broken, func(ctx context.Context) error { return broken.FinishDepreciationRun(ctx, run) }); !errors.Is(err, errRuns) {
		t.Errorf("the finish: got %v, want the operation's failure", err)
	}
	if err := in(broken, func(ctx context.Context) error {
		return broken.FailDepreciationRun(ctx, run, problem.Conflict("ledger.period_not_open", "Closed."))
	}); !errors.Is(err, errRuns) {
		t.Errorf("the failure: got %v, want the operation's failure", err)
	}
	unread := app.Assets(app.Config{})
	unread.Runs = cancelUnread{unread.Runs}
	for name, fn := range map[string]func(ctx context.Context) error{
		"a step": func(ctx context.Context) error { return unread.FinishDepreciationRun(ctx, run) },
		"the failure": func(ctx context.Context) error {
			return unread.FailDepreciationRun(ctx, run, problem.Conflict("ledger.period_not_open", "Closed."))
		},
	} {
		if err := in(unread, fn); !errors.Is(err, errRuns) {
			t.Errorf("the cancellation unread by %s: got %v, want the operation's failure", name, err)
		}
	}
	pgtest.Exec(t, `UPDATE platform.operation SET status = 'cancelled', version = version + 1 WHERE id = $1`, op["id"])
	r := e.carry(t, run)
	want(t, "a run whose operation was cancelled", runOf(r), "failed 1 1 0 10.00; 1 posted 1 1 0 10.00")
	want(t, "its problem", r["error"].(map[string]any)["code"], "assets.run_cancelled")
	var status string
	pgtest.Scan(t, `SELECT status FROM platform.operation WHERE id = $1`, []any{op["id"]}, &status)
	want(t, "its operation", status, "cancelled")

	pgtest.Exec(t, `UPDATE tenancy.tenant SET group_currency = 'GBP', version = version + 1 WHERE id = $1`, e.Tenant.ID)
	_, july := e.start(t, e.l0, 7)
	r = e.carry(t, july)
	want(t, "a run without a rate into the group currency", fmt.Sprint(r["status"], " ", r["error"].(map[string]any)["code"]), "failed ledger.no_exchange_rate")
}

// cancelUnread is the operations port whose read of a cancellation fails.
type cancelUnread struct{ assets.Runs }

func (cancelUnread) Cancelled(context.Context, uuid.UUID, bool) (bool, error) { return false, errRuns }

// TestRunInProgressConstraint: a second run of a company and ledger
// queued at the same time as the first meets the database's rule, which
// the context reports as 409 assets.run_in_progress.
func TestRunInProgressConstraint(t *testing.T) {
	e := newRuns(t)
	_, run := e.start(t, e.l0, 6)
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		tx, _ := reqctx.TxFrom(ctx) // InRequest runs it in a unit of work
		_, err := tx.Exec(ctx, `INSERT INTO assets.depreciation_run (tenant_id, id, legal_entity_id, ledger_id, fiscal_year, fiscal_period, posting_date,
				currency, status, operation_id, created_by, updated_by)
			SELECT tenant_id, gen_random_uuid(), legal_entity_id, ledger_id, 2026, 7, '2026-07-31', currency, 'queued', gen_random_uuid(), created_by, created_by
			FROM assets.depreciation_run WHERE id = $1`, run)
		return err
	})
	if p := problem.From(err); p.Status != 409 || p.Code != "assets.run_in_progress" {
		t.Errorf("got %v, want 409 assets.run_in_progress", err)
	}
}
