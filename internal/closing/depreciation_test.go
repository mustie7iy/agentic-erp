package closing_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/closing"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// depreciationPermissions are Carla's, who also creates assets, posts
// their acquisitions and runs their depreciation in these tests.
var depreciationPermissions = append(append([]string{}, closePermissions...), "assets.asset.read", "assets.asset.change", "assets.asset.post",
	"assets.depreciation.run", "ledger.entry.post")

// withDepreciation is withTemplate (company 2000 keeping 0L and 2L) with
// the asset classes of fixture.AssetClasses, machines depreciated in 2L
// too, and a check task: the depreciation of the period posted.
func withDepreciation(doc *config.Config) {
	withTemplate(doc)
	fixture.AssetClasses(doc)
	for i := range doc.AssetClasses {
		if doc.AssetClasses[i].Code == "MACHINES" {
			doc.AssetClasses[i].Terms = append(doc.AssetClasses[i].Terms, config.AssetTerms{Ledger: "2L", Method: config.StraightLine, UsefulLifeMonths: 60})
		}
	}
	doc.Close.Tasks = append(doc.Close.Tasks,
		config.CloseTask{Code: "DEPRECIATED", Name: "Depreciation of the period posted", Kind: config.CloseCheck, Check: "depreciation_posted", DueDay: 1})
}

// machine creates a machine of company 2000 and capitalises EUR 1,200.00
// onto it against the bank account on 10 January 2026, so it depreciates
// from January in each ledger.
func machine(t *testing.T, e fixture.Env) {
	t.Helper()
	a, _ := e.Must(201, "POST", "/v1/assets", fmt.Sprintf(`{"legal_entity_id": %q, "class_code": "MACHINES", "description": "Press"}`, e.DE), "")
	e.Must(201, "POST", "/v1/asset-acquisitions", fmt.Sprintf(`{"asset_id": %q, "offset_account_id": %q, "amount": {"amount": "1200.00", "currency": "EUR"},
		"posting_date": "2026-01-10"}`, a["id"], e.Account(t, "2000", "100000")), "")
}

// depreciate runs January's depreciation of company 2000 in the ledger as
// the workflow carries it out, failing it on a business problem, and
// returns the run's id and status.
func depreciate(t *testing.T, e fixture.Env, ledger uuid.UUID, fail bool) (string, string) {
	t.Helper()
	return depreciateIn(t, e, ledger, 1, fail)
}

// depreciateIn is depreciate for 2026's period.
func depreciateIn(t *testing.T, e fixture.Env, ledger uuid.UUID, period int, fail bool) (string, string) {
	t.Helper()
	op, _ := e.Must(202, "POST", "/v1/depreciation-runs", fmt.Sprintf(`{"legal_entity_id": %q, "ledger_id": %q, "fiscal_year": 2026, "period": %d}`,
		e.DE, ledger, period), "")
	run := uuid.MustParse(strings.TrimPrefix(op["resource"].(string), "/v1/depreciation-runs/"))
	h := app.Assets(app.Config{})
	step := func(fn func(ctx context.Context) error) {
		if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, fn); err != nil {
			t.Fatalf("run %s: %v", run, err)
		}
	}
	if fail {
		step(func(ctx context.Context) error {
			return h.FailDepreciationRun(ctx, run, problem.Conflict("ledger.period_not_open", "closed"))
		})
	} else {
		var batches []int32
		step(func(ctx context.Context) error {
			var err error
			batches, err = h.PlanDepreciationRun(ctx, run)
			return err
		})
		for _, b := range batches {
			step(func(ctx context.Context) error { return h.PostDepreciationBatch(ctx, run, b) })
		}
		step(func(ctx context.Context) error { return h.FinishDepreciationRun(ctx, run) })
	}
	r, _ := e.Must(200, "GET", "/v1/depreciation-runs/"+run.String(), "", "")
	return run.String(), r["status"].(string)
}

// depreciationLines are a depreciation check's result: whether it passed
// and its summary, then each covered ledger with its run or the assets a
// run would select.
func depreciationLines(t *testing.T, task map[string]any) []string {
	t.Helper()
	r := task["check_result"].(map[string]any)
	out := []string{fmt.Sprintf("%v %v", r["passed"], r["summary"])}
	for _, x := range apitest.List(t, r, "ledgers") {
		l := x.(map[string]any)
		out = append(out, fmt.Sprintf("%v %v %v posted=%v run=%v to depreciate=%v", l["ledger_code"], l["ledger_name"], l["key_date"], l["posted"],
			l["depreciation_run_id"], l["assets_to_depreciate"]))
		if later, ok := l["later_depreciation_run_id"]; ok {
			out[len(out)-1] += fmt.Sprintf(" later=%v", later)
		}
	}
	out = append(out, fmt.Sprintf("%v warnings", r["warnings_count"]))
	for _, x := range apitest.List(t, r, "warnings") {
		w := x.(map[string]any)
		out = append(out, fmt.Sprintf("%v %v %v %v from %v/%v through %v/%v run=%v catch-up %v: %v", w["code"], w["ledger_code"], w["asset_number"],
			w["asset_description"], w["missing_from_year"], w["missing_from_period"], w["missing_through_year"], w["missing_through_period"],
			w["later_depreciation_run_id"], moneyText(w["catch_up"]), w["message"]))
	}
	return out
}

// moneyText is a Money as "amount currency".
func moneyText(m any) string {
	v := m.(map[string]any)
	return fmt.Sprint(v["amount"], " ", v["currency"])
}

// TestDepreciationPosted: the depreciation check passes for a company
// with nothing to depreciate, in each ledger it keeps; once a machine is
// capitalised it fails naming each covered ledger without a posted run of
// the period and how many assets a run would select; a failed run does
// not count; it passes once each covered ledger has its run, naming them.
func TestDepreciationPosted(t *testing.T) {
	e := fixture.NewWith(t, nil, withDepreciation, depreciationPermissions...)
	us, tag := open(t, e.Client, e.US, 1, "")
	task, _ := e.Must(200, "POST", "/v1/period-closes/"+apitest.String(t, us, "id")+"/tasks/DEPRECIATED:check", "", tag)
	wantLines(t, "company 1000, nothing to depreciate", depreciationLines(t, task), []string{
		"true Every covered ledger has a posted depreciation run of 2026/1 or nothing to depreciate.",
		"0L Leading ledger 2026-01-31 posted=false run=<nil> to depreciate=0", "0 warnings"})
	wantFields(t, "nothing to depreciate", task, map[string]any{"status": "done"})

	de, tag := open(t, e.Client, e.DE, 1, "")
	base := "/v1/period-closes/" + apitest.String(t, de, "id") + "/tasks/DEPRECIATED:check"
	machine(t, e)
	machine(t, e)
	task, tag = e.Must(200, "POST", base, "", tag)
	wantLines(t, "before any run", depreciationLines(t, task), []string{
		"false No posted depreciation run of 2026/1 in ledger 0L (2 assets to depreciate), 2L (2 assets to depreciate); run the depreciation first.",
		"0L Leading ledger 2026-01-31 posted=false run=<nil> to depreciate=2", "2L HGB 2026-01-31 posted=false run=<nil> to depreciate=2", "0 warnings"})
	wantFields(t, "before any run", task, map[string]any{"status": "open"})

	leading, local := e.Ledger(t, config.LeadingLedger), e.Ledger(t, "2L")
	run0, status := depreciate(t, e, leading, false)
	if status != "posted" {
		t.Fatalf("0L's run: got %s, want posted", status)
	}
	if _, status := depreciate(t, e, local, true); status != "failed" {
		t.Fatalf("2L's failed run: got %s, want failed", status)
	}
	task, tag = e.Must(200, "POST", base, "", tag)
	wantLines(t, "0L's run, 2L's failed", depreciationLines(t, task), []string{
		"false No posted depreciation run of 2026/1 in ledger 2L (2 assets to depreciate); run the depreciation first.",
		"0L Leading ledger 2026-01-31 posted=true run=" + run0 + " to depreciate=<nil>", "2L HGB 2026-01-31 posted=false run=<nil> to depreciate=2", "0 warnings"})

	run2, _ := depreciate(t, e, local, false)
	task, _ = e.Must(200, "POST", base, "", tag)
	wantLines(t, "every ledger's run", depreciationLines(t, task), []string{
		"true Every covered ledger has a posted depreciation run of 2026/1 or nothing to depreciate.",
		"0L Leading ledger 2026-01-31 posted=true run=" + run0 + " to depreciate=<nil>", "2L HGB 2026-01-31 posted=true run=" + run2 + " to depreciate=<nil>", "0 warnings"})
	wantFields(t, "every ledger's run", task, map[string]any{"status": "done"})
}

// TestDepreciationPostedLateAsset: a machine capitalised with a January
// value date after February's run posted is selected by a run of
// January, which is refused as out of order (PO 2026-11-04). January's
// check still fails in 2L, whose February run has not posted, naming
// both machines there; once it has, the check passes with the late
// machine a warning in 0L, the only ledger whose February run posted
// before it: its depreciation missing from January through February and
// the 20.00 a run of February posts for it. After that repeat run of
// February nothing is late.
func TestDepreciationPostedLateAsset(t *testing.T) {
	e := fixture.NewWith(t, nil, withDepreciation, depreciationPermissions...)
	de, tag := open(t, e.Client, e.DE, 1, "")
	base := "/v1/period-closes/" + apitest.String(t, de, "id") + "/tasks/DEPRECIATED:check"
	leading, local := e.Ledger(t, config.LeadingLedger), e.Ledger(t, "2L")
	machine(t, e)
	feb, status := depreciateIn(t, e, leading, 2, false)
	if status != "posted" {
		t.Fatalf("February's run in 0L: got %s, want posted", status)
	}
	machine(t, e)
	p, _ := e.Must(409, "POST", "/v1/depreciation-runs", fmt.Sprintf(`{"legal_entity_id": %q, "fiscal_year": 2026, "period": 1}`, e.DE), "")
	wantFields(t, "January's run after February's", p, map[string]any{"code": "assets.run_out_of_order"})
	task, tag := e.Must(200, "POST", base, "", tag)
	wantLines(t, "2L without a later run", depreciationLines(t, task), []string{
		"false No posted depreciation run of 2026/1 in ledger 2L (2 assets to depreciate); run the depreciation first.",
		"0L Leading ledger 2026-01-31 posted=false run=<nil> to depreciate=1 later=" + feb,
		"2L HGB 2026-01-31 posted=false run=<nil> to depreciate=2",
		"1 warnings",
		"depreciation_catch_up 0L 2 Press from 2026/1 through 2026/2 run=" + feb + " catch-up 20.00 EUR: Asset 2 (Press) in ledger 0L has no " +
			"depreciation booked from 2026/1 through 2026/2, whose run posted before it; a run of 2026/2 or later catches up 20.00 EUR."})
	wantFields(t, "2L without a later run", task, map[string]any{"status": "open"})

	if _, status := depreciateIn(t, e, local, 2, false); status != "posted" {
		t.Fatalf("February's run in 2L: got %s, want posted", status)
	}
	task, tag = e.Must(200, "POST", base, "", tag)
	want := depreciationLines(t, task)
	wantLines(t, "a later run in each ledger", want[:2], []string{
		"true Every covered ledger has a posted depreciation run of 2026/1, nothing to depreciate or a posted run of a later period; ledger 0L " +
			"(1 late asset, caught up by a run of 2026/2 or later), see the warnings.",
		"0L Leading ledger 2026-01-31 posted=false run=<nil> to depreciate=1 later=" + feb})
	wantFields(t, "a later run in each ledger", task, map[string]any{"status": "done"})
	r := task["check_result"].(map[string]any)
	wantFields(t, "the warnings counted", r, map[string]any{"warnings_count": float64(1)})
	var audited string
	pgtest.Scan(t, `SELECT string_agg((detail->>'passed') || ' ' || (detail->>'warnings_count'), ', ' ORDER BY seq) FROM audit.audit_event
		WHERE tenant_id = $1 AND action = 'closing.period_close.check_task'`, []any{e.Tenant.ID}, &audited)
	if audited != "false 1, true 1" {
		t.Errorf("audited checks: got %q, want false 1, true 1", audited)
	}

	if _, status := depreciateIn(t, e, leading, 2, false); status != "posted" {
		t.Fatalf("February's repeat run in 0L: got %s, want posted", status)
	}
	task, _ = e.Must(200, "POST", base, "", tag)
	wantLines(t, "caught up", depreciationLines(t, task)[:2], []string{
		"true Every covered ledger has a posted depreciation run of 2026/1 or nothing to depreciate.",
		"0L Leading ledger 2026-01-31 posted=false run=<nil> to depreciate=0"})
}

// TestDepreciationCheckFailures runs the depreciation check with each of
// its database statements failing in turn, with an asset to depreciate,
// and fails it when the assets context does not answer.
func TestDepreciationCheckFailures(t *testing.T) {
	e := fixture.NewWith(t, nil, withDepreciation, depreciationPermissions...)
	got, tag := open(t, e.Client, e.DE, 1, "")
	id := uuid.MustParse(apitest.String(t, got, "id"))
	machine(t, e)
	h := endpoints(t)
	call := func(ctx context.Context) error {
		_, err := h.CheckCloseTask(ctx, gen.CheckCloseTaskParams{ID: id, Code: "DEPRECIATED", IfMatch: tag})
		return err
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "DEPRECIATED", call)
	h.Depreciation = brokenDepreciation{}
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, call)
	if err == nil || !strings.Contains(err.Error(), "closing: depreciation of ledger 0L: the assets context is down") {
		t.Errorf("the assets context failing: got %v, want its error", err)
	}
}

// TestDepreciationCheckLateFailures runs the depreciation check with two
// late machines in each ledger, each of its database statements failing
// in turn, then counts them in the summary.
func TestDepreciationCheckLateFailures(t *testing.T) {
	e := fixture.NewWith(t, nil, withDepreciation, depreciationPermissions...)
	got, tag := open(t, e.Client, e.DE, 1, "")
	id := uuid.MustParse(apitest.String(t, got, "id"))
	for _, l := range []string{config.LeadingLedger, "2L"} {
		depreciateIn(t, e, e.Ledger(t, l), 2, false)
	}
	machine(t, e)
	machine(t, e)
	h := endpoints(t)
	call := func(ctx context.Context) error {
		_, err := h.CheckCloseTask(ctx, gen.CheckCloseTaskParams{ID: id, Code: "DEPRECIATED", IfMatch: tag})
		return err
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "DEPRECIATED", call)
	task, _ := e.Must(200, "POST", "/v1/period-closes/"+id.String()+"/tasks/DEPRECIATED:check", "", tag)
	lines := depreciationLines(t, task)
	wantLines(t, "two late machines in each ledger", []string{lines[0], lines[3]}, []string{
		"true Every covered ledger has a posted depreciation run of 2026/1, nothing to depreciate or a posted run of a later period; ledger 0L " +
			"(2 late assets, caught up by a run of 2026/2 or later), 2L (2 late assets, caught up by a run of 2026/2 or later), see the warnings.",
		"4 warnings"})
}

// brokenDepreciation is an assets context that does not answer.
type brokenDepreciation struct{}

func (brokenDepreciation) Posted(context.Context, uuid.UUID, uuid.UUID, int16, int16, time.Time, int) (closing.PeriodDepreciation, error) {
	return closing.PeriodDepreciation{}, errors.New("the assets context is down")
}
