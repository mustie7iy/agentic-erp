package controlling_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// requestPlanLock requests a plan lock of the cost center over the dates
// [from, to) and returns the center change, pending its approval.
func (p plan) requestPlanLock(t *testing.T, center uuid.UUID, from, to string) map[string]any {
	t.Helper()
	path := "/v1/cost-centers/" + center.String()
	_, tag := p.Must(200, "GET", path, "", "")
	ch, _ := p.Must(200, "POST", path+":lock", fmt.Sprintf(`{"valid_from": %q, "valid_to": %q, "lock_plan": true, "reason": "Budget approved"}`, from, to), tag)
	return ch
}

// refusedFields are the fields a refusal names, in order.
func refusedFields(got map[string]any) []string {
	var out []string
	for _, e := range got["errors"].([]any) {
		f := e.(map[string]any)
		out = append(out, fmt.Sprint(f["field"], " ", f["code"]))
	}
	return out
}

// TestPlanLockedCostCenter (T56g2): a batch planning a cost center in a
// period sharing a day with a period of the center locked for planning is
// refused, naming each such line, and writes nothing; the center's other
// periods, other centers and profit centers are planned; a batch applied
// before the lock is still answered as it was.
func TestPlanLockedCostCenter(t *testing.T) {
	p := setup(t)
	cat := p.category(t, "PLAN")
	before := []string{line(p.c.Admin, p.office, 3, "30.00")}
	p.batch(t, cat, "before", before...)
	// From 15 March: March shares days with the lock, so it is locked too.
	p.Approve(t, p.requestPlanLock(t, p.c.Admin, "2026-03-15", "2026-05-01")["id"].(string))

	lines := []string{line(p.c.Admin, p.office, 2, "20.00"), line(p.c.Admin, p.office, 3, "31.00"), line(p.c.Admin, p.office, 4, "40.00"),
		line(p.c.Admin, p.office, 5, "50.00"), line(p.c.Sales, p.office, 3, "60.00"), pcLine(p.c.Profit, p.revenue, 3, "-70.00")}
	got := p.Problem(422, "controlling.cost_center_plan_locked", "POST", "/v1/plan-batches", p.batchBody(cat, "locked", lines...), "")
	wantList(t, "lines refused", refusedFields(got), []string{"lines[1].cost_center_id plan_locked", "lines[2].cost_center_id plan_locked"})
	wantList(t, "plan after the refusal", p.lines(t, ""), []string{"1000/520000/3 30.00"})

	t.Run("an invalid line is named first", func(t *testing.T) {
		got := p.in(t).Problem(422, "controlling.plan_batch_invalid", "POST", "/v1/plan-batches",
			p.batchBody(cat, "invalid", lines[1], line(p.c.Admin, p.bank, 2, "1.00")), "")
		wantList(t, "fields", refusedFields(got), []string{"lines[1].account_id invalid"})
	})
	t.Run("the batch applied before the lock", func(t *testing.T) {
		want(t, "replayed", p.in(t).batch(t, cat, "before", before...)["replayed"], any(true))
	})
	t.Run("the same lines under a new key", func(t *testing.T) {
		p.in(t).Problem(422, "controlling.cost_center_plan_locked", "POST", "/v1/plan-batches", p.batchBody(cat, "again", before...), "")
	})

	open := []string{lines[0], lines[3], lines[4], lines[5]}
	want(t, "the unlocked periods and centers", p.batch(t, cat, "open", open...)["line_count"], any(float64(4)))
	wantList(t, "plan", sorted(p.lines(t, "")), []string{"1000/520000/2 20.00", "1000/520000/5 50.00", "2000/520000/3 60.00", "PC1/400000/3 -70.00"})
	want(t, "events", len(p.events(t, "plan_batch_applied")), 2)

	t.Run("at most a hundred errors", func(t *testing.T) {
		p.Approve(t, p.in(t).requestPlanLock(t, p.c.Sales, "2026-01-01", "2027-01-01")["id"].(string))
		var many []string
		for _, account := range []uuid.UUID{p.office, p.revenue} {
			for period := 1; period <= 12; period++ {
				many = append(many, line(p.c.Sales, account, period, "1.00"))
			}
		}
		got := p.in(t).Problem(422, "controlling.cost_center_plan_locked", "POST", "/v1/plan-batches", p.batchBody(cat, "many", many...), "")
		want(t, "errors", len(got["errors"].([]any)), len(many))
	})
}

// TestPlanLockAndBatchSerialize: a cost center's plan lock and a batch
// planning it queue on the center, which the batch holds FOR KEY SHARE
// and the lock's approval FOR UPDATE. A batch meeting a lock under way
// waits for it and is refused; a lock meeting a batch under way waits for
// it, and the batch's lines stay, written before the lock.
func TestPlanLockAndBatchSerialize(t *testing.T) {
	p := setup(t)
	h := endpoints(t)
	cat := uuid.MustParse(p.category(t, "PLAN"))
	apply := func(key string, period int32) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := h.ApplyPlanBatch(ctx, &gen.PlanBatchCreate{PlanCategoryID: cat, LegalEntityID: p.DE, FiscalYear: 2026, BatchKey: key,
				Lines: []gen.PlanBatchLine{{CostCenterID: gen.NewOptUUID(p.c.Admin), AccountID: p.office, FiscalPeriod: period,
					Amount: gen.Money{Amount: fmt.Sprintf("%d.00", period), Currency: "EUR"}}}}, gen.ApplyPlanBatchParams{})
			return err
		}
	}
	approve := func(ch map[string]any) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			return ledger.CenterChanges{}.Approved(ctx, uuid.MustParse(ch["id"].(string)), uuid.MustParse(ch["workflow_request_id"].(string)))
		}
	}

	t.Run("the lock first", func(t *testing.T) {
		lock := p.in(t).requestPlanLock(t, p.c.Admin, "2026-06-01", "2026-07-01")
		lockErr, batchErr := apitest.Serialized(t, p.Tenant, p.Tenant.Human, approve(lock), apply("after the lock", 6))
		if lockErr != nil {
			t.Fatalf("lock: got %v, want it applied", lockErr)
		}
		want(t, "batch after the lock", problem.From(batchErr).Code, "controlling.cost_center_plan_locked")
		want(t, "plan lines", len(p.in(t).lines(t, "")), 0)
	})
	t.Run("the batch first", func(t *testing.T) {
		lock := p.in(t).requestPlanLock(t, p.c.Admin, "2026-07-01", "2026-08-01")
		batchErr, lockErr := apitest.Serialized(t, p.Tenant, p.Tenant.Human, apply("before the lock", 7), approve(lock))
		if batchErr != nil || lockErr != nil {
			t.Fatalf("batch, then lock: got %v and %v, want both applied", batchErr, lockErr)
		}
		wantList(t, "plan written before the lock", p.in(t).lines(t, ""), []string{"1000/520000/7 7.00"})
		p.in(t).Problem(422, "controlling.cost_center_plan_locked", "POST", "/v1/plan-batches",
			p.batchBody(cat.String(), "after", line(p.c.Admin, p.office, 7, "8.00")), "")
	})
}
