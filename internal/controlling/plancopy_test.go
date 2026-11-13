package controlling_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// copyBody is a copy of company 2000's 2026 plan into the target.
func (p plan) copyBody(target, percentage string, replace bool) string {
	return fmt.Sprintf(`{"target_plan_category_id": %q, "legal_entity_id": %q, "fiscal_year": 2026, "percentage": %q, "replace": %v}`,
		target, p.DE, percentage, replace)
}

// copyPlan copies the source's plan into the target and returns the batch.
func (p plan) copyPlan(t *testing.T, source, target, percentage string, replace bool) map[string]any {
	t.Helper()
	b, _ := p.Must(200, "POST", "/v1/plan-categories/"+source+":copy", p.copyBody(target, percentage, replace), `"1"`)
	return b
}

// planned are a category's current lines of company 2000 in 2026, sorted.
func (p plan) planned(t *testing.T, category string) []string {
	t.Helper()
	return sorted(p.lines(t, "&plan_category_id="+category+"&page_size=200"))
}

// lineID is the id of the category's current line on the center, account
// and period, named by their codes as lines names them.
func (p plan) lineID(t *testing.T, category, key string) string {
	t.Helper()
	page, _ := p.Must(200, "GET", "/v1/plan-lines?legal_entity_id="+p.DE.String()+"&fiscal_year=2026&page_size=200&plan_category_id="+category, "", "")
	for _, it := range page["items"].([]any) {
		l := it.(map[string]any)
		center, _ := l["cost_center_id"].(string)
		if pc, ok := l["profit_center_id"].(string); ok {
			center = pc
		}
		if fmt.Sprintf("%s/%s/%v", p.code(center), p.code(l["account_id"].(string)), l["fiscal_period"]) == key {
			return l["id"].(string)
		}
	}
	t.Fatalf("no current line %s in category %s", key, category)
	return ""
}

// budgeted is a tenant with a category PLAN, whose 2026 plan has lines on
// two cost centers and a profit center, one with a quantity, and an empty
// category FC.
type budgeted struct {
	plan
	budget, forecast string
}

func setupBudgeted(t *testing.T) budgeted {
	t.Helper()
	b := budgeted{plan: setup(t)}
	b.budget, b.forecast = b.category(t, "PLAN"), b.category(t, "FC")
	hours := fmt.Sprintf(`{"cost_center_id": %q, "account_id": %q, "fiscal_period": 4, "amount": {"amount": "200.00", "currency": "EUR"},
		"quantity": {"value": "1.5", "unit": "H"}}`, b.c.Admin, b.office)
	b.batch(t, b.budget, "budget", line(b.c.Admin, b.office, 1, "100.00"), line(b.c.Admin, b.office, 2, "0.10"), line(b.c.Sales, b.office, 2, "33.33"),
		pcLine(b.c.Profit, b.revenue, 3, "-0.10"), hours)
	return b
}

var budget = []string{"1000/520000/1 100.00", "1000/520000/2 0.10", "1000/520000/4 200.00 1.5 H", "2000/520000/2 33.33", "PC1/400000/3 -0.10"}

// TestPlanCopy: each line is copied to the same center, account and
// period with its quantity, its amount scaled and rounded half away from
// zero; the copy is one batch of the target naming its origin, audited
// and published; the source is not changed; the plan against actual
// report reads the target's figures.
func TestPlanCopy(t *testing.T) {
	b := setupBudgeted(t)
	got := b.copyPlan(t, b.budget, b.forecast, "105", false)
	// 0.10 at 105 percent is 0.105, a half: away from zero, on both signs;
	// 33.33 is 34.9965.
	wantList(t, "the forecast", b.planned(t, b.forecast),
		[]string{"1000/520000/1 105.00", "1000/520000/2 0.11", "1000/520000/4 210.00 1.5 H", "2000/520000/2 35.00", "PC1/400000/3 -0.11"})
	wantList(t, "the budget, unchanged", b.planned(t, b.budget), budget)
	for field, wanted := range map[string]any{"plan_category_id": b.forecast, "legal_entity_id": b.DE.String(), "fiscal_year": 2026.0, "line_count": 5.0,
		"replaced_count": 0.0, "replayed": false, "copied_from_plan_category_id": b.budget, "copied_from_plan_category_code": "PLAN",
		"copy_percentage": "105"} {
		want(t, "the batch's "+field, got[field], wanted)
	}
	want(t, "the batch's key, the copy's own", got["batch_key"], any("copy-"+got["id"].(string)))
	wantList(t, "the batch's lines", sorted(b.lines(t, "&plan_batch_id="+got["id"].(string))), b.planned(t, b.forecast))

	events := b.events(t, "plan_batch_applied")
	ev := events[len(events)-1]
	for field, wanted := range map[string]any{"plan_batch_id": got["id"], "plan_category_id": b.forecast, "copied_from_plan_category_id": b.budget,
		"copy_percentage": "105", "line_count": 5.0, "replaced_count": 0.0} {
		want(t, "the event's "+field, ev[field], wanted)
	}
	want(t, "the event's cost centers", fmt.Sprint(sortedIDs(ev["cost_center_ids"])), fmt.Sprint(sortedIDs([]any{b.c.Admin.String(), b.c.Sales.String()})))
	want(t, "the event's profit centers", fmt.Sprint(ev["profit_center_ids"]), fmt.Sprint([]any{b.c.Profit.String()}))
	if _, uploaded := events[0]["copied_from_plan_category_id"]; uploaded {
		t.Error("the uploaded batch's event names a category copied from, want none")
	}
	var detail string
	pgtest.Scan(t, `SELECT detail->>'copied_from_plan_category_id' || ' ' || (detail->>'copy_percentage') FROM audit.audit_event
		WHERE tenant_id = $1 AND action = 'controlling.plan_batch.apply' AND object_id = $2`, []any{b.Tenant.ID, got["id"]}, &detail)
	want(t, "the audit record's origin", detail, b.budget+" 105")

	page, _ := b.Must(200, "GET", "/v1/cost-center-plan-actual?plan_category_id="+b.forecast+"&legal_entity_id="+b.DE.String()+"&fiscal_year=2026", "", "")
	var rows []string
	for _, it := range page["items"].([]any) {
		r := it.(map[string]any)
		rows = append(rows, fmt.Sprintf("%s plan %s", b.code(r["cost_center_id"].(string)), r["plan"].(map[string]any)["amount"]))
	}
	wantList(t, "the forecast in plan against actual", sorted(rows), []string{"1000 plan 315.11", "2000 plan 35.00"})
}

func sortedIDs(v any) []string {
	var out []string
	for _, id := range v.([]any) {
		out = append(out, id.(string))
	}
	return sorted(out)
}

// TestPlanCopyReplaces: a target that has a plan of the company and year
// is refused unless the copy says to replace it, and then all of it is
// replaced, the centers the copy has no line of included; a repeat with
// the same key answers the same batch.
func TestPlanCopyReplaces(t *testing.T) {
	b := setupBudgeted(t)
	b.batch(t, b.forecast, "old", line(b.c.Locked, b.office, 1, "7.00"), line(b.c.Admin, b.office, 9, "9.00"))
	old := []string{"1000/520000/9 9.00", "3000/520000/1 7.00"}
	b.Problem(409, "controlling.plan_copy_target_planned", "POST", "/v1/plan-categories/"+b.budget+":copy", b.copyBody(b.forecast, "50", false), `"1"`)
	wantList(t, "the forecast after the refusal", b.planned(t, b.forecast), old)

	req := apitest.Req{Method: "POST", Path: "/v1/plan-categories/" + b.budget + ":copy", Body: b.copyBody(b.forecast, "50", true), Tenant: b.Tenant.ID,
		As: b.Tenant.Human, Key: apitest.NewKey(), IfMatch: `"1"`}
	first := b.API.Do(req)
	got := apitest.JSON(t, first)
	want(t, "status", first.Code, 200)
	want(t, "lines replaced", got["replaced_count"], any(2.0))
	halved := []string{"1000/520000/1 50.00", "1000/520000/2 0.05", "1000/520000/4 100.00 1.5 H", "2000/520000/2 16.67", "PC1/400000/3 -0.05"}
	wantList(t, "the forecast, the budget halved and nothing else", b.planned(t, b.forecast), halved)
	events := b.events(t, "plan_batch_applied")
	want(t, "the event's cost centers, the replaced plan's among them", fmt.Sprint(sortedIDs(events[len(events)-1]["cost_center_ids"])),
		fmt.Sprint(sortedIDs([]any{b.c.Admin.String(), b.c.Sales.String(), b.c.Locked.String()})))

	again := b.API.Do(req)
	want(t, "the repeat's status", again.Code, 200)
	want(t, "the repeat's batch", apitest.JSON(t, again)["id"], got["id"])
	want(t, "the repeat marked as one", again.Header().Get("Idempotent-Replayed"), "true")
	want(t, "batches applied", len(b.events(t, "plan_batch_applied")), len(events))

	t.Run("replacing a plan with itself scaled again", func(t *testing.T) {
		p := b.in(t)
		third := p.category(t, "THIRD")
		p.copyPlan(t, b.forecast, third, "100", false)
		wantList(t, "the copy at 100 percent", p.planned(t, third), halved)
		want(t, "lines replaced", p.copyPlan(t, b.budget, third, "1000", true)["replaced_count"], any(5.0))
		wantList(t, "the copy at 1000 percent", p.planned(t, third),
			[]string{"1000/520000/1 1000.00", "1000/520000/2 1.00", "1000/520000/4 2000.00 1.5 H", "2000/520000/2 333.30", "PC1/400000/3 -1.00"})
	})
}

// TestPlanCopyRefusals: what a copy refuses before it reads a plan, and
// who may not copy.
func TestPlanCopyRefusals(t *testing.T) {
	b := setupBudgeted(t)
	path := "/v1/plan-categories/" + b.budget + ":copy"
	locked := b.category(t, "LOCKED")
	b.Must(200, "POST", "/v1/plan-categories/"+locked+":lock", "", `"1"`)
	for name, c := range map[string]struct {
		body  string
		field string
	}{
		"into the category itself":           {b.copyBody(b.budget, "105", false), "target_plan_category_id same_category"},
		"a percentage of zero":               {b.copyBody(b.forecast, "0", false), "percentage invalid"},
		"a percentage above 1000":            {b.copyBody(b.forecast, "1000.0001", false), "percentage invalid"},
		"a year the category has no plan of": {strings.Replace(b.copyBody(b.forecast, "105", false), "2026", "2027", 1), "fiscal_year no_plan"},
	} {
		t.Run(name, func(t *testing.T) {
			got := b.in(t).Problem(422, "controlling.plan_copy_invalid", "POST", path, c.body, `"1"`)
			wantList(t, "fields", refusedFields(got), []string{c.field})
		})
	}
	b.Problem(404, "request.not_found", "POST", "/v1/plan-categories/"+uuid.NewString()+":copy", b.copyBody(b.forecast, "105", false), `"1"`)
	b.Problem(422, "request.reference_not_found", "POST", path, b.copyBody(uuid.NewString(), "105", false), `"1"`)
	b.Problem(422, "request.reference_not_found", "POST", path, strings.Replace(b.copyBody(b.forecast, "105", false), b.DE.String(), uuid.NewString(), 1), `"1"`)
	b.as(b.Reader(t, "us-only", b.US)).Problem(422, "request.reference_not_found", "POST", path, b.copyBody(b.forecast, "105", false), `"1"`)
	b.Problem(409, "controlling.plan_category_locked", "POST", path, b.copyBody(locked, "105", false), `"1"`)
	b.Problem(412, "request.version_mismatch", "POST", path, b.copyBody(b.forecast, "105", false), `"2"`)
	b.Problem(428, "request.precondition_required", "POST", path, b.copyBody(b.forecast, "105", false), "")
	b.Problem(400, "request.invalid_body", "POST", path, b.copyBody(b.forecast, "five", false), `"1"`)
	b.As(b.Tenant.Agent).Problem(403, "identity.agent_not_allowed", "POST", path, b.copyBody(b.forecast, "105", false), `"1"`)
	wantList(t, "the forecast after the refusals", b.planned(t, b.forecast), nil)

	h := endpoints(t)
	err := apitest.InRequest(t, b.Tenant, b.Tenant.Human, func(ctx context.Context) error {
		_, err := h.CopyPlanCategory(ctx, &gen.PlanCategoryCopy{TargetPlanCategoryID: uuid.MustParse(b.forecast), LegalEntityID: b.DE, FiscalYear: 2026,
			Percentage: "1.23456"}, gen.CopyPlanCategoryParams{ID: uuid.MustParse(b.budget), IfMatch: `"1"`})
		return err
	})
	want(t, "five decimals, which only a direct call can send", problem.From(err).Code, "controlling.plan_copy_invalid")

	pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = 'controlling.plan.write'`, b.Tenant.ID)
	b.Problem(403, "identity.permission_missing", "POST", path, b.copyBody(b.forecast, "105", false), `"1"`)
}

// seedLines writes n lines of a new batch of the category on cost center
// 1000 in period 1, each on an account of its own, of the amount.
func (p plan) seedLines(t *testing.T, category string, n int, amount int64, account *uuid.UUID) {
	t.Helper()
	batch := uuid.New()
	pgtest.Exec(t, `INSERT INTO controlling.plan_batch (tenant_id, id, category_id, legal_entity_id, fiscal_year, batch_key, digest, line_count, replaced_count,
		created_by) VALUES ($1, $2, $3, $4, 2026, $5, repeat('a', 64), $6, 0, $7)`, p.Tenant.ID, batch, category, p.DE, "seed-"+batch.String(), n, p.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO controlling.plan_line (tenant_id, id, batch_id, category_id, legal_entity_id, fiscal_year, fiscal_period, cost_center_id,
			account_id, amount, created_by)
		SELECT $1, gen_random_uuid(), $2, $3, $4, 2026, 1, $5, coalesce($8, gen_random_uuid()), $6, $7 FROM generate_series(1, $9::int)`,
		p.Tenant.ID, batch, category, p.DE, p.c.Admin, amount, p.Tenant.Human.ID, account, n)
}

// TestPlanCopyOfAPlanItCannotTake: a plan too large for one request, an
// amount that does not fit after scaling, and a line the ledger no longer
// lets a plan name, which is named by the source line's id.
func TestPlanCopyOfAPlanItCannotTake(t *testing.T) {
	p := setup(t)
	target := p.category(t, "FC")
	refusal := func(t *testing.T, source, code string) []string {
		t.Helper()
		got := p.in(t).Problem(422, code, "POST", "/v1/plan-categories/"+source+":copy", p.copyBody(target, "1000", false), `"1"`)
		return refusedFields(got)
	}
	t.Run("more than 50,000 lines", func(t *testing.T) {
		large := p.category(t, "LARGE")
		p.seedLines(t, large, 50_001, 100, nil)
		wantList(t, "fields", refusal(t, large, "controlling.plan_copy_invalid"), []string{"fiscal_year too_many_lines"})
	})
	t.Run("an amount that does not fit after scaling", func(t *testing.T) {
		huge := p.category(t, "HUGE")
		p.seedLines(t, huge, 1, 9_000_000_000_000_000_000, &p.office)
		wantList(t, "fields", refusal(t, huge, "controlling.plan_copy_invalid"), []string{"percentage overflow"})
	})
	t.Run("a line on an account a plan may not name", func(t *testing.T) {
		stale := p.category(t, "STALE")
		p.seedLines(t, stale, 1, 100, &p.bank)
		wantList(t, "fields", refusal(t, stale, "controlling.plan_batch_invalid"),
			[]string{"source_lines[" + p.lineID(t, stale, "1000/100000/1") + "].account_id invalid"})
	})
	wantList(t, "the target after the refusals", p.planned(t, target), nil)
}

// TestPlanCopyAndLockedCostCenters: a copy is refused as a batch is when
// it would change a cost center's plan in a period the center is locked
// for planning in: a line it copies there, the target's current line
// there of a center it copies lines of, named by the first of them, and
// the target's current line there of a center it copies none of, named
// by replace.
func TestPlanCopyAndLockedCostCenters(t *testing.T) {
	b := setupBudgeted(t)
	path := "/v1/plan-categories/" + b.budget + ":copy"
	b.batch(t, b.forecast, "old", line(b.c.Locked, b.office, 3, "7.00"), line(b.c.Sales, b.office, 3, "9.00"))
	// Admin is locked in February, where the budget plans it; Sales and
	// Locked in March, where only the forecast's current plan has them.
	b.Approve(t, b.requestPlanLock(t, b.c.Admin, "2026-02-01", "2026-03-01")["id"].(string))
	for _, center := range []uuid.UUID{b.c.Sales, b.c.Locked} {
		b.Approve(t, b.requestPlanLock(t, center, "2026-03-01", "2026-04-01")["id"].(string))
	}
	got := b.Problem(422, "controlling.cost_center_plan_locked", "POST", path, b.copyBody(b.forecast, "105", true), `"1"`)
	wantList(t, "fields", sorted(refusedFields(got)), sorted([]string{
		"source_lines[" + b.lineID(t, b.budget, "1000/520000/2") + "].cost_center_id plan_locked",
		"source_lines[" + b.lineID(t, b.budget, "2000/520000/2") + "].cost_center_id plan_locked",
		"replace plan_locked"}))
	wantList(t, "the forecast after the refusal", b.planned(t, b.forecast), []string{"2000/520000/3 9.00", "3000/520000/3 7.00"})
	t.Run("into a category without a plan in the locked periods", func(t *testing.T) {
		got := b.in(t).Problem(422, "controlling.cost_center_plan_locked", "POST", path, b.copyBody(b.category(t, "NEW"), "105", false), `"1"`)
		wantList(t, "fields", refusedFields(got), []string{"source_lines[" + b.lineID(t, b.budget, "1000/520000/2") + "].cost_center_id plan_locked"})
	})
}

// TestPlanLockWaitsForACopy: a copy holds its target, so a lock of the
// target waits for it and then locks the plan the copy wrote; the source
// is not held.
func TestPlanLockWaitsForACopy(t *testing.T) {
	b := setupBudgeted(t)
	h := endpoints(t)
	source, target := uuid.MustParse(b.budget), uuid.MustParse(b.forecast)
	copyErr, lockErr := apitest.Serialized(t, b.Tenant, b.Tenant.Human, func(ctx context.Context) error {
		if _, err := h.LockPlanCategory(ctx, gen.LockPlanCategoryParams{ID: source, IfMatch: `"1"`}); err != nil {
			return fmt.Errorf("lock the source: %w", err)
		}
		_, err := h.CopyPlanCategory(ctx, &gen.PlanCategoryCopy{TargetPlanCategoryID: target, LegalEntityID: b.DE, FiscalYear: 2026, Percentage: "105"},
			gen.CopyPlanCategoryParams{ID: source, IfMatch: `"2"`})
		return err
	}, func(ctx context.Context) error {
		_, err := h.LockPlanCategory(ctx, gen.LockPlanCategoryParams{ID: target, IfMatch: `"1"`})
		return err
	})
	if copyErr != nil || lockErr != nil {
		t.Fatalf("copy: %v, lock: %v, want both to succeed", copyErr, lockErr)
	}
	want(t, "lines the lock found written", len(b.planned(t, b.forecast)), 5)
	b.Problem(409, "controlling.plan_category_locked", "POST", "/v1/plan-categories/"+b.budget+":copy", b.copyBody(b.forecast, "105", true), `"2"`)
}
