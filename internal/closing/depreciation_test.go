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
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
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
	op, _ := e.Must(202, "POST", "/v1/depreciation-runs", fmt.Sprintf(`{"legal_entity_id": %q, "ledger_id": %q, "fiscal_year": 2026, "period": 1}`,
		e.DE, ledger), "")
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
	}
	return out
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
		"0L Leading ledger 2026-01-31 posted=false run=<nil> to depreciate=0"})
	wantFields(t, "nothing to depreciate", task, map[string]any{"status": "done"})

	de, tag := open(t, e.Client, e.DE, 1, "")
	base := "/v1/period-closes/" + apitest.String(t, de, "id") + "/tasks/DEPRECIATED:check"
	machine(t, e)
	task, tag = e.Must(200, "POST", base, "", tag)
	wantLines(t, "before any run", depreciationLines(t, task), []string{
		"false No posted depreciation run of 2026/1 in ledger 0L (1 asset to depreciate), 2L (1 asset to depreciate); run the depreciation first.",
		"0L Leading ledger 2026-01-31 posted=false run=<nil> to depreciate=1", "2L HGB 2026-01-31 posted=false run=<nil> to depreciate=1"})
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
		"false No posted depreciation run of 2026/1 in ledger 2L (1 asset to depreciate); run the depreciation first.",
		"0L Leading ledger 2026-01-31 posted=true run=" + run0 + " to depreciate=<nil>", "2L HGB 2026-01-31 posted=false run=<nil> to depreciate=1"})

	run2, _ := depreciate(t, e, local, false)
	task, _ = e.Must(200, "POST", base, "", tag)
	wantLines(t, "every ledger's run", depreciationLines(t, task), []string{
		"true Every covered ledger has a posted depreciation run of 2026/1 or nothing to depreciate.",
		"0L Leading ledger 2026-01-31 posted=true run=" + run0 + " to depreciate=<nil>", "2L HGB 2026-01-31 posted=true run=" + run2 + " to depreciate=<nil>"})
	wantFields(t, "every ledger's run", task, map[string]any{"status": "done"})
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

// brokenDepreciation is an assets context that does not answer.
type brokenDepreciation struct{}

func (brokenDepreciation) Posted(context.Context, uuid.UUID, uuid.UUID, int16, int16, time.Time) (*uuid.UUID, int, error) {
	return nil, 0, errors.New("the assets context is down")
}
