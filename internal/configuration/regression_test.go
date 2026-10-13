package configuration_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// fakeRegressor captures the cases it is given and replays them as told:
// failures keyed by the document's company 2000 tax rate, so a proposal
// changing it "fails" the cases listed for that rate.
type fakeRegressor struct {
	cases    []configuration.RegressionCase
	failures map[string][]configuration.RegressionFailure // by the rate of V1 in company 2000
	errs     map[string]error                             // replay errors, by the same rate
	err      error                                        // capture error
}

func (f *fakeRegressor) Capture(context.Context, *config.Config, time.Duration, int) ([]configuration.RegressionCase, error) {
	return f.cases, f.err
}

func (f *fakeRegressor) Replay(_ context.Context, doc *config.Config, _ []configuration.RegressionCase) ([]configuration.RegressionFailure, error) {
	rate := doc.Companies[1].TaxCodes[0].RatePercent
	if err := f.errs[rate]; err != nil {
		return nil, err
	}
	return f.failures[rate], nil
}

func caseOf(no int32, company, decision string) configuration.RegressionCase {
	return configuration.RegressionCase{CaseNo: no, InvoiceID: uuid.Must(uuid.NewV7()), DocumentNumber: "SI" + strings.Repeat("0", 5) + string(rune('0'+no)),
		LegalEntityID: uuid.New(), Company: company, Why: "decision " + decision, ExpectedDecision: decision, ExpectedIssues: []string{}}
}

// TestRegressionEvidence takes a tier 3 change through the regression
// evidence: no suite, a stale suite, a suite that passes with changes
// inside the companies the change touches, and one with a regression
// outside them.
func TestRegressionEvidence(t *testing.T) {
	s := sim{fixture.New(t, append(perms, "platform.operation.read", "config.regression.manage")...)}
	ctx := context.Background()
	deps := fixture.Deps()
	reg := &fakeRegressor{cases: []configuration.RegressionCase{caseOf(1, "2000", "auto_post"), caseOf(2, "1000", "exception")}, failures: map[string][]configuration.RegressionFailure{}}
	passing := &fakeSimulator{sim: configuration.Simulation{Passed: true, Summary: "fine", Detail: map[string]any{}}}
	deps.Simulator, deps.Regressor = passing, reg
	w := &configuration.SimulationWorker{Dependencies: deps, Pool: pgtest.Pool(t), AppRole: "erp_app", WorkerRole: "erp_worker"}
	status := func(path string) map[string]any { return s.status(t, path) }

	// Without a suite the regression evidence fails and the request waits.
	cr, op := s.tier3(t, "9")
	path, opPath := "/v1/change-requests/"+cr["id"].(string), "/v1/operations/"+op["id"].(string)
	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got := status(path)
	if got["status"] != "awaiting_evidence" {
		t.Errorf("without a suite: got status %v, want awaiting_evidence", got["status"])
	}
	if n := len(got["evidence"].([]any)); n != 2 {
		t.Fatalf("without a suite: got %d pieces of evidence, want 2: %v", n, got)
	}
	regression := evidenceOf(got, "regression")
	if regression["passed"] != false {
		t.Errorf("regression evidence passed: got %v, want false", regression["passed"])
	}
	if summary := regression["summary"].(string); !strings.Contains(summary, "No regression suite captured on version 1") {
		t.Errorf("regression evidence summary: got %q, want no suite on version 1", summary)
	}
	done := status(opPath)
	if done["status"] != "succeeded" {
		t.Errorf("operation status: got %v, want succeeded", done["status"])
	}
	if passed := done["result"].(map[string]any)["regression_passed"]; passed != false {
		t.Errorf("operation regression_passed: got %v, want false", passed)
	}

	// A captured suite (the fake regressor supplies the cases) is recorded with its cases and strata.
	var suite configuration.RegressionSuite
	if err := apitest.Committed(t, s.Tenant, s.Tenant.Human, func(ctx context.Context) (err error) {
		suite, err = configuration.CaptureRegressionSuite(ctx, deps, configuration.NewRegressionSuite{Name: "Baseline"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Must(200, "GET", "/v1/regression-suites/"+suite.ID.String(), "", "")
	for field, want := range map[string]any{"case_count": float64(2), "config_version": float64(1), "number": "RS-0001"} {
		if got[field] != want {
			t.Errorf("suite %s: got %v, want %v", field, got[field], want)
		}
	}
	if n := len(got["strata"].([]any)); n != 2 {
		t.Errorf("suite strata: got %d, want 2", n)
	}
	if page, _ := s.Must(200, "GET", "/v1/regression-suites?config_version=1", "", ""); len(page["items"].([]any)) != 1 {
		t.Fatalf("list: %v", page)
	}
	// The handlers outside a unit of work, and every statement of a list failing in turn.
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	h := &configuration.Endpoints{Pages: pages, Dependencies: deps}
	for name, call := range map[string]func(ctx context.Context) error{
		"list": func(ctx context.Context) error {
			_, err := h.ListRegressionSuites(ctx, gen.ListRegressionSuitesParams{PageSize: gen.NewOptInt32(1)})
			return err
		},
		"create": func(ctx context.Context) error {
			_, err := h.CreateRegressionSuite(ctx, &gen.RegressionSuiteCreate{Name: "Again"}, gen.CreateRegressionSuiteParams{})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetRegressionSuite(ctx, gen.GetRegressionSuiteParams{ID: suite.ID})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if call(context.Background()) == nil {
				t.Error("outside a unit of work: got no error, want one")
			}
			apitest.EveryFailure(t, s.Tenant, s.Tenant.Human, name, call)
		})
	}
	if err := apitest.InRequest(t, s.Tenant, s.Tenant.Human, func(ctx context.Context) error {
		_, err := h.ListRegressionSuites(ctx, gen.ListRegressionSuitesParams{OrderBy: gen.NewOptString("name")})
		return err
	}); err == nil {
		t.Error("sorted by an unknown field")
	}
	// Two suites page one at a time.
	s.suite(t)
	page, _ := s.Must(200, "GET", "/v1/regression-suites?page_size=1", "", "")
	if n := len(page["items"].([]any)); n != 1 {
		t.Errorf("page 1: got %d items, want 1", n)
	}
	if page["next_page_token"] == nil {
		t.Fatalf("page 1: got no next_page_token, want one: %v", page)
	}
	if rest, _ := s.Must(200, "GET", "/v1/regression-suites?page_size=1&page_token="+page["next_page_token"].(string), "", ""); len(rest["items"].([]any)) != 1 {
		t.Fatalf("page 2: %v", rest)
	}
	// Stale: the suite no longer matches the base version.
	reg.failures["19"] = []configuration.RegressionFailure{{Case: reg.cases[0], Decision: "review"}}
	_, tag := s.Must(200, "GET", path, "", "")
	s.Must(202, "POST", path+":simulate", "", tag)
	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got = status(path)
	last := evidenceOf(got, "regression")
	if got["status"] != "awaiting_evidence" {
		t.Errorf("stale: got status %v, want awaiting_evidence", got["status"])
	}
	if summary := last["summary"].(string); !strings.Contains(summary, "no longer matches version 1") {
		t.Errorf("stale: got summary %q, want the suite no longer matching version 1", summary)
	}
	delete(reg.failures, "19")

	// A regression outside the companies the change touches (company 1000 for a company 2000 change) fails; inside, it is the change at work.
	reg.failures["9"] = []configuration.RegressionFailure{{Case: reg.cases[1], Decision: "auto_post"}}
	_, tag = s.Must(200, "GET", path, "", "")
	s.Must(202, "POST", path+":simulate", "", tag)
	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got = status(path)
	last = evidenceOf(got, "regression")
	if got["status"] != "awaiting_evidence" {
		t.Errorf("regression outside: got status %v, want awaiting_evidence", got["status"])
	}
	if last["passed"] != false {
		t.Errorf("regression outside: got passed %v, want false", last["passed"])
	}
	if summary := last["summary"].(string); !strings.Contains(summary, "1 of them outside the companies") {
		t.Errorf("regression outside: got summary %q, want one outside the companies", summary)
	}
	if n := len(last["detail"].(map[string]any)["regressions"].([]any)); n != 1 {
		t.Errorf("regression outside: got %d regressions, want 1", n)
	}
	reg.failures["9"] = []configuration.RegressionFailure{{Case: reg.cases[0], Decision: "review"}}
	_, tag = s.Must(200, "GET", path, "", "")
	s.Must(202, "POST", path+":simulate", "", tag)
	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got = status(path)
	last = evidenceOf(got, "regression")
	detail := last["detail"].(map[string]any)
	if got["status"] != "pending_approval" {
		t.Errorf("changes inside: got status %v, want pending_approval", got["status"])
	}
	if last["passed"] != true {
		t.Errorf("changes inside: got passed %v, want true", last["passed"])
	}
	if n := len(detail["changed"].([]any)); n != 1 {
		t.Errorf("changes inside: got %d changed, want 1", n)
	}
	if n := len(detail["regressions"].([]any)); n != 0 {
		t.Errorf("changes inside: got %d regressions, want 0", n)
	}
	if touched := detail["touched_companies"].([]any)[0]; touched != "2000" {
		t.Errorf("changes inside: got touched company %v, want 2000", touched)
	}

	// Replay errors, on the base or on the proposal: a problem fails the operation, anything else leaves it queued.
	_, op = s.tier3(t, "8")
	opPath = "/v1/operations/" + op["id"].(string)
	reg.errs = map[string]error{"19": errors.New("hiccup")}
	if _, err := w.RunOnce(ctx); err == nil {
		t.Error("transient replay error on the base: got no error, want one")
	}
	if got := status(opPath)["status"]; got != "queued" {
		t.Fatalf("transient replay error on the base: got operation %v, want queued", got)
	}
	reg.errs = map[string]error{"8": errors.New("hiccup")}
	if _, err := w.RunOnce(ctx); err == nil {
		t.Error("transient replay error on the proposal: got no error, want one")
	}
	if got := status(opPath)["status"]; got != "queued" {
		t.Fatalf("transient replay error on the proposal: got operation %v, want queued", got)
	}
	reg.errs = map[string]error{"8": problem.Unprocessable("config.unreplayable", "cannot replay")}
	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatalf("refused replay: %v", err)
	}
	if got := status(opPath)["status"]; got != "failed" {
		t.Fatalf("refused replay: got operation %v, want failed", got)
	}
	reg.errs = nil
	s.Problem(404, "request.not_found", "GET", "/v1/regression-suites/"+uuid.NewString(), "", "")
	// Capturing fails like the regressor does, and with nothing to capture (the API's own regressor sees no invoices here).
	reg.err = errors.New("down")
	if err := apitest.InRequest(t, s.Tenant, s.Tenant.Human, func(ctx context.Context) error {
		_, err := configuration.CaptureRegressionSuite(ctx, deps, configuration.NewRegressionSuite{Name: "x"})
		return err
	}); err == nil || err.Error() != "down" {
		t.Fatalf("capture error: %v", err)
	}
	reg.err = nil
	s.Problem(422, "config.no_cases", "POST", "/v1/regression-suites", `{"name": "x", "window_days": 30}`, "")
	// Every statement of a capture can fail.
	reg.cases = []configuration.RegressionCase{caseOf(3, "2000", "review")}
	capture := func(ctx context.Context) error {
		_, err := configuration.CaptureRegressionSuite(ctx, deps, configuration.NewRegressionSuite{Name: "x"})
		return err
	}
	if capture(context.Background()) == nil {
		t.Fatal("captured outside a unit of work")
	}
	apitest.EveryFailure(t, s.Tenant, s.Tenant.Human, "capture", capture)
	get := func(ctx context.Context) error {
		_, err := configuration.GetRegressionSuite(ctx, suite.ID)
		return err
	}
	if get(context.Background()) == nil {
		t.Fatal("read outside a unit of work")
	}
	apitest.EveryFailure(t, s.Tenant, s.Tenant.Human, "get", get)
}
