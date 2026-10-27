package controlling_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestPlanDatabaseFailures calls every handler and the ledger's port
// outside a unit of work and with each of its database statements failing
// in turn.
func TestPlanDatabaseFailures(t *testing.T) {
	c := setupCompared(t)
	h := endpoints(t)
	cat := uuid.MustParse(c.budget)
	open := uuid.MustParse(c.category(t, "OPEN"))
	locked := uuid.MustParse(c.category(t, "LOCKED"))
	c.Must(200, "POST", "/v1/plan-categories/"+locked.String()+":lock", "", `"1"`)
	amount := gen.Money{Amount: "1.00", Currency: "EUR"}
	newBatch := func(key string) *gen.PlanBatchCreate {
		return &gen.PlanBatchCreate{PlanCategoryID: open, LegalEntityID: c.DE, FiscalYear: 2026, BatchKey: key, Lines: []gen.PlanBatchLine{
			{CostCenterID: gen.NewOptUUID(c.c.Admin), AccountID: c.office, FiscalPeriod: 1, Amount: amount,
				Quantity: gen.NewOptQuantity(gen.Quantity{Value: "1", Unit: "H"})}}}
	}
	lock := func(id uuid.UUID) gen.LockPlanCategoryParams {
		return gen.LockPlanCategoryParams{ID: id, IfMatch: `"1"`}
	}
	byCenter := gen.ListCostCenterPlanActualParams{PlanCategoryID: cat, LegalEntityID: c.DE, FiscalYear: 2026, CostCenterID: gen.NewOptUUID(c.c.Sales),
		PageSize: gen.NewOptInt32(1)}
	calls := map[string]func(ctx context.Context) error{
		"create a category": func(ctx context.Context) error {
			_, err := h.CreatePlanCategory(ctx, &gen.PlanCategoryCreate{Code: "NEW", Name: "N"}, gen.CreatePlanCategoryParams{})
			return err
		},
		"get a category": func(ctx context.Context) error {
			_, err := h.GetPlanCategory(ctx, gen.GetPlanCategoryParams{ID: cat})
			return err
		},
		"list categories": func(ctx context.Context) error {
			_, err := h.ListPlanCategories(ctx, gen.ListPlanCategoriesParams{})
			return err
		},
		"lock a category": func(ctx context.Context) error {
			_, err := h.LockPlanCategory(ctx, lock(open))
			return err
		},
		"unlock a category": func(ctx context.Context) error {
			_, err := h.UnlockPlanCategory(ctx, gen.UnlockPlanCategoryParams{ID: locked, IfMatch: `"2"`})
			return err
		},
		"apply a batch": func(ctx context.Context) error {
			_, err := h.ApplyPlanBatch(ctx, newBatch("new"), gen.ApplyPlanBatchParams{})
			return err
		},
		"replay a batch": func(ctx context.Context) error {
			b := newBatch("budget")
			b.PlanCategoryID = cat
			b.Lines = []gen.PlanBatchLine{{CostCenterID: gen.NewOptUUID(c.c.Admin), AccountID: c.office, FiscalPeriod: 5, Amount: gen.Money{Amount: "100.00", Currency: "EUR"}},
				{CostCenterID: gen.NewOptUUID(c.c.Admin), AccountID: c.office, FiscalPeriod: 6, Amount: gen.Money{Amount: "50.00", Currency: "EUR"}},
				{CostCenterID: gen.NewOptUUID(c.c.Sales), AccountID: c.office, FiscalPeriod: 6, Amount: gen.Money{Amount: "20.00", Currency: "EUR"}}}
			_, err := h.ApplyPlanBatch(ctx, b, gen.ApplyPlanBatchParams{})
			return err
		},
		"list plan lines": func(ctx context.Context) error {
			_, err := h.ListPlanLines(ctx, gen.ListPlanLinesParams{LegalEntityID: c.DE, FiscalYear: 2026})
			return err
		},
		"list plan against actual": func(ctx context.Context) error {
			_, err := h.ListCostCenterPlanActual(ctx, gen.ListCostCenterPlanActualParams{PlanCategoryID: cat, LegalEntityID: c.DE, FiscalYear: 2026})
			return err
		},
		"list plan against actual by center, a page after the first": func(ctx context.Context) error {
			res, err := h.ListCostCenterPlanActual(ctx, byCenter)
			if err != nil {
				return err
			}
			next := byCenter
			next.PageToken = gen.NewOptString(res.(*gen.CostCenterPlanActualPage).NextPageToken.Or("none"))
			_, err = h.ListCostCenterPlanActual(ctx, next)
			return err
		},
		"the ledger's plan objects": func(ctx context.Context) error {
			_, err := ledger.AssignmentBalances{}.PlanObjects(ctx, c.DE, 2026, []uuid.UUID{c.c.Admin}, nil, []uuid.UUID{c.office})
			return err
		},
		"the ledger's cost centers of a node": func(ctx context.Context) error {
			_, _, err := ledger.AssignmentBalances{}.CostCenters(ctx, nil, &c.c.Admin)
			return err
		},
		"the ledger's actuals": func(ctx context.Context) error {
			_, err := ledger.AssignmentBalances{}.CostCenterActuals(ctx, ledger.ActualsQuery{LegalEntityID: c.DE, FiscalYear: 2026, PeriodFrom: 1, PeriodTo: 16, Limit: 1})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got no error, want one")
			}
			apitest.EveryFailure(t, c.Tenant, c.Tenant.Human, name, call)
		})
	}
}

// TestPlanActualRefusals: what only a direct call can send (the contract
// refuses it first) and a variance beyond what an amount can hold.
func TestPlanActualRefusals(t *testing.T) {
	c := setupCompared(t)
	h := endpoints(t)
	in := func(fn func(ctx context.Context) error) error {
		return apitest.InRequest(t, c.Tenant, c.Tenant.Human, fn)
	}
	params := gen.ListCostCenterPlanActualParams{PlanCategoryID: uuid.MustParse(c.budget), LegalEntityID: c.DE, FiscalYear: 2026}
	err := in(func(ctx context.Context) error {
		p := params
		p.PageSize = gen.NewOptInt32(0)
		_, err := h.ListCostCenterPlanActual(ctx, p)
		return err
	})
	want(t, "page size 0", problem.From(err).Code, "request.invalid_page_size")
	// A plan line of the most negative amount: its variance from an actual
	// of zero does not fit in an amount.
	pgtest.Exec(t, `INSERT INTO controlling.plan_line (tenant_id, id, batch_id, category_id, legal_entity_id, fiscal_year, fiscal_period, cost_center_id,
			account_id, amount, created_by)
		SELECT tenant_id, gen_random_uuid(), id, category_id, legal_entity_id, fiscal_year, 1, $2, $3, -9223372036854775808, created_by
		FROM controlling.plan_batch WHERE tenant_id = $1`, c.Tenant.ID, c.c.Locked, c.office)
	err = in(func(ctx context.Context) error {
		_, err := h.ListCostCenterPlanActual(ctx, params)
		return err
	})
	if err == nil || problem.From(err).Status != 500 {
		t.Errorf("a variance that does not fit: got %v, want an internal error", err)
	}
}

// TestPlanConstraintProblems: the database's rules a request meets only
// in a race its checks lose are the problems the checks give.
func TestPlanConstraintProblems(t *testing.T) {
	for constraint, code := range map[string]string{
		"plan_category_code_once": "controlling.plan_category_exists", "plan_category_open": "controlling.plan_category_locked",
	} {
		t.Run(constraint, func(t *testing.T) {
			if got := problem.From(&pgconn.PgError{Code: "23514", ConstraintName: constraint}).Code; got != code {
				t.Errorf("problem: got %s, want %s", got, code)
			}
		})
	}
}

// TestPlanLockWaitsForABatch: a lock waits for a batch under way, which
// holds the category, and then locks the plan the batch wrote.
func TestPlanLockWaitsForABatch(t *testing.T) {
	p := setup(t)
	h := endpoints(t)
	cat := uuid.MustParse(p.category(t, "PLAN"))
	batch, lockErr := apitest.Serialized(t, p.Tenant, p.Tenant.Human, func(ctx context.Context) error {
		_, err := h.ApplyPlanBatch(ctx, &gen.PlanBatchCreate{PlanCategoryID: cat, LegalEntityID: p.DE, FiscalYear: 2026, BatchKey: "b",
			Lines: []gen.PlanBatchLine{{CostCenterID: gen.NewOptUUID(p.c.Admin), AccountID: p.office, FiscalPeriod: 1,
				Amount: gen.Money{Amount: "1.00", Currency: "EUR"}}}}, gen.ApplyPlanBatchParams{})
		return err
	}, func(ctx context.Context) error {
		_, err := h.LockPlanCategory(ctx, gen.LockPlanCategoryParams{ID: cat, IfMatch: `"1"`})
		return err
	})
	if err := errors.Join(batch, lockErr); err != nil {
		t.Fatal(err)
	}
	wantList(t, "plan locked after the batch", p.lines(t, ""), []string{"1000/520000/1 1.00"})
	p.Problem(409, "controlling.plan_category_locked", "POST", "/v1/plan-batches", p.batchBody(cat.String(), "c", line(p.c.Admin, p.office, 1, "2.00")), "")
}
