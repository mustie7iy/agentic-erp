package controlling_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/controlling"
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
	var pcRoot uuid.UUID
	pgtest.Scan(t, `SELECT n.id FROM ledger.hierarchy h JOIN ledger.hierarchy_node n ON n.tenant_id = h.tenant_id AND n.hierarchy_id = h.id
		WHERE h.tenant_id = $1 AND h.kind = 'profit_center' AND h.is_standard AND n.parent_id IS NULL`, []any{c.Tenant.ID}, &pcRoot)
	byNode := gen.ListProfitCenterPlanActualParams{PlanCategoryID: cat, LegalEntityID: c.DE, FiscalYear: 2026, HierarchyNodeID: gen.NewOptUUID(pcRoot),
		PageSize: gen.NewOptInt32(1)}
	created := uuid.MustParse(c.order(t, "CREATED"))
	charged := uuid.MustParse(c.releasedOrder(t, "CHARGED"))
	c.charge(t, charged.String(), "2026-05-10", c.office, "10.00")
	c.charge(t, charged.String(), "2026-06-10", c.office, "20.00")
	budgeted := uuid.MustParse(c.order(t, "BUDGETED"))
	c.Must(200, "PATCH", "/v1/internal-orders/"+budgeted.String()+"/budget", budgetBody("5.00", "EUR"), `"0"`)
	ruled := uuid.MustParse(c.order(t, "RULED"))
	c.Must(200, "PATCH", "/v1/internal-orders/"+ruled.String()+"/settlement-rule", ruleBody(c.c.Sales), `"1"`)
	balances := gen.ListInternalOrderBalancesParams{InternalOrderID: charged, FiscalYear: 2026, PageSize: gen.NewOptInt32(1)}
	// An order with costs to settle, and one settled twice and ready to close.
	unsettledID, _ := c.ruledOrder(t, "UNSETTLED", c.c.Sales)
	c.charge(t, unsettledID, "2026-05-10", c.office, "10.00")
	settledID, settledPath := c.ruledOrder(t, "SETTLED", c.c.Sales)
	for _, m := range []int{5, 6} {
		c.charge(t, settledID, fmt.Sprintf("2026-%02d-10", m), c.office, "10.00")
		c.Must(200, "POST", settledPath+":settle", settleBody(2026, m), `"3"`)
	}
	unsettled, settled := uuid.MustParse(unsettledID), uuid.MustParse(settledID)
	settlements := gen.ListInternalOrderSettlementsParams{ID: settled, PageSize: gen.NewOptInt32(1)}
	calls := map[string]func(ctx context.Context) error{
		"create an order": func(ctx context.Context) error {
			_, err := h.CreateInternalOrder(ctx, &gen.InternalOrderCreate{LegalEntityID: c.DE, Code: "NEW", Name: "N", ResponsibleCostCenterID: c.c.Admin},
				gen.CreateInternalOrderParams{})
			return err
		},
		"get an order": func(ctx context.Context) error {
			_, err := h.GetInternalOrder(ctx, gen.GetInternalOrderParams{ID: created})
			return err
		},
		"list orders": func(ctx context.Context) error {
			_, err := h.ListInternalOrders(ctx, gen.ListInternalOrdersParams{})
			return err
		},
		"release an order": func(ctx context.Context) error {
			_, err := h.ReleaseInternalOrder(ctx, gen.ReleaseInternalOrderParams{ID: created, IfMatch: `"1"`})
			return err
		},
		"list an order's balances, a page after the first": func(ctx context.Context) error {
			res, err := h.ListInternalOrderBalances(ctx, balances)
			if err != nil {
				return err
			}
			next := balances
			next.PageToken = gen.NewOptString(res.(*gen.InternalOrderBalancePage).NextPageToken.Or("none"))
			_, err = h.ListInternalOrderBalances(ctx, next)
			return err
		},
		"get an order's budget": func(ctx context.Context) error {
			_, err := h.GetInternalOrderBudget(ctx, gen.GetInternalOrderBudgetParams{ID: budgeted})
			return err
		},
		"set an order's budget": func(ctx context.Context) error {
			_, err := h.UpdateInternalOrderBudget(ctx, &gen.InternalOrderBudgetUpdate{Budget: amount}, gen.UpdateInternalOrderBudgetParams{ID: budgeted, IfMatch: `"1"`})
			return err
		},
		"the ledger's balance of an order": func(ctx context.Context) error {
			_, err := ledger.AssignmentBalances{}.OrderBalance(ctx, c.DE, charged)
			return err
		},
		"get an order's settlement rule": func(ctx context.Context) error {
			_, err := h.GetInternalOrderSettlementRule(ctx, gen.GetInternalOrderSettlementRuleParams{ID: ruled})
			return err
		},
		"replace an order's settlement rule": func(ctx context.Context) error {
			_, err := h.UpdateInternalOrderSettlementRule(ctx, &gen.InternalOrderSettlementRuleUpdate{ReceiverCostCenterID: c.c.Admin},
				gen.UpdateInternalOrderSettlementRuleParams{ID: ruled, IfMatch: `"2"`})
			return err
		},
		"settle an order": func(ctx context.Context) error {
			_, err := h.SettleInternalOrder(ctx, &gen.InternalOrderSettle{FiscalYear: 2026, FiscalPeriod: 5}, gen.SettleInternalOrderParams{ID: unsettled, IfMatch: `"3"`})
			return err
		},
		"list an order's settlements, a page after the first": func(ctx context.Context) error {
			res, err := h.ListInternalOrderSettlements(ctx, settlements)
			if err != nil {
				return err
			}
			next := settlements
			next.PageToken = gen.NewOptString(res.(*gen.InternalOrderSettlementPage).NextPageToken.Or("none"))
			_, err = h.ListInternalOrderSettlements(ctx, next)
			return err
		},
		"close an order": func(ctx context.Context) error {
			_, err := h.CloseInternalOrder(ctx, gen.CloseInternalOrderParams{ID: settled, IfMatch: `"3"`})
			return err
		},
		"the ledger's balances of an order by account": func(ctx context.Context) error {
			_, err := ledger.AssignmentBalances{}.OrderAccountBalances(ctx, c.DE, unsettled, &ledger.PeriodKey{FiscalYear: 2026, Period: 5})
			return err
		},
		"the ledger's validity of a cost center": func(ctx context.Context) error {
			_, err := ledger.AssignmentBalances{}.CostCenterValidOn(ctx, c.c.Admin, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
			return err
		},
		"the orders a posting names": func(ctx context.Context) error {
			_, err := controlling.CostObjects{}.Orders(ctx, []uuid.UUID{charged}, true)
			return err
		},
		"the ledger's actuals of an order": func(ctx context.Context) error {
			_, err := ledger.AssignmentBalances{}.OrderActuals(ctx, ledger.OrderActualsQuery{LegalEntityID: c.DE, OrderID: charged, FiscalYear: 2026,
				PeriodFrom: 1, PeriodTo: 16, Limit: 1})
			return err
		},
		"the ledger's cost centers named": func(ctx context.Context) error {
			_, err := ledger.AssignmentBalances{}.CostCenterRefs(ctx, []uuid.UUID{c.c.Admin})
			return err
		},
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
		"list plan against actual by profit center": func(ctx context.Context) error {
			_, err := h.ListProfitCenterPlanActual(ctx, gen.ListProfitCenterPlanActualParams{PlanCategoryID: cat, LegalEntityID: c.DE, FiscalYear: 2026})
			return err
		},
		"list plan against actual by a profit center node, a page after the first": func(ctx context.Context) error {
			res, err := h.ListProfitCenterPlanActual(ctx, byNode)
			if err != nil {
				return err
			}
			next := byNode
			next.PageToken = gen.NewOptString(res.(*gen.ProfitCenterPlanActualPage).NextPageToken.Or("none"))
			_, err = h.ListProfitCenterPlanActual(ctx, next)
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
		"the ledger's profit centers of a node": func(ctx context.Context) error {
			_, _, err := ledger.AssignmentBalances{}.ProfitCenters(ctx, nil, &pcRoot)
			return err
		},
		"the ledger's profit center actuals": func(ctx context.Context) error {
			_, err := ledger.AssignmentBalances{}.ProfitCenterActuals(ctx, ledger.ActualsQuery{LegalEntityID: c.DE, FiscalYear: 2026, PeriodFrom: 1, PeriodTo: 16, Limit: 1})
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
		FROM controlling.plan_batch WHERE tenant_id = $1 AND batch_key = 'budget'`, c.Tenant.ID, c.c.Locked, c.office)
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
		"internal_order_code_once": "controlling.internal_order_exists",
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

// TestPlanBatchConcurrentReplay: the same batch sent twice at once is
// applied once: the second waits for the category the first holds, then
// finds the first's batch and answers it as replayed.
func TestPlanBatchConcurrentReplay(t *testing.T) {
	p := setup(t)
	h := endpoints(t)
	cat := uuid.MustParse(p.category(t, "PLAN"))
	var replayed []bool
	apply := func(ctx context.Context) error {
		res, err := h.ApplyPlanBatch(ctx, &gen.PlanBatchCreate{PlanCategoryID: cat, LegalEntityID: p.DE, FiscalYear: 2026, BatchKey: "b",
			Lines: []gen.PlanBatchLine{{CostCenterID: gen.NewOptUUID(p.c.Admin), AccountID: p.office, FiscalPeriod: 1,
				Amount: gen.Money{Amount: "1.00", Currency: "EUR"}}}}, gen.ApplyPlanBatchParams{})
		if err == nil {
			replayed = append(replayed, res.(*gen.PlanBatchHeaders).Response.Replayed)
		}
		return err
	}
	if err := errors.Join(apitest.Serialized(t, p.Tenant, p.Tenant.Human, apply, apply)); err != nil {
		t.Fatal(err)
	}
	want(t, "first and second replayed", fmt.Sprint(replayed), "[false true]")
	want(t, "events", len(p.events(t, "plan_batch_applied")), 1)
	var batches int
	pgtest.Scan(t, `SELECT count(*) FROM controlling.plan_batch WHERE tenant_id = $1`, []any{p.Tenant.ID}, &batches)
	want(t, "batches", batches, 1)
}
