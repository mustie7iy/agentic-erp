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
	sc := &evidenceScenario{s: s, ctx: ctx, deps: deps, reg: reg, passing: passing, w: w}
	if !t.Run("without a suite", sc.withoutASuite) {
		t.FailNow()
	}
	if !t.Run("suite captured", sc.suiteCaptured) {
		t.FailNow()
	}
	t.Run("suite handlers failing", sc.suiteHandlersFailing)
	if !t.Run("suites page one at a time", sc.suitesPageOneAtATime) {
		t.FailNow()
	}
	if !t.Run("stale suite", sc.staleSuite) {
		t.FailNow()
	}
	if !t.Run("regression outside the touched companies", sc.regressionOutsideTheTouchedCompanies) {
		t.FailNow()
	}
	if !t.Run("changes inside the touched companies", sc.changesInsideTheTouchedCompanies) {
		t.FailNow()
	}
	if !t.Run("replay errors", sc.replayErrors) {
		t.FailNow()
	}
	t.Run("capture failures", sc.captureFailures)
}

// evidenceScenario is the state TestRegressionEvidence's steps share.
type evidenceScenario struct {
	s       sim // each step takes it with s.in(t)
	ctx     context.Context
	deps    configuration.Dependencies
	reg     *fakeRegressor
	passing *fakeSimulator
	w       *configuration.SimulationWorker
	path    string                        // the change request
	opPath  string                        // its simulation's operation
	op      map[string]any                // the simulation's operation
	suite   configuration.RegressionSuite // captured on version 1
}

// withoutASuite is the step "without a suite" of TestRegressionEvidence.
func (sc *evidenceScenario) withoutASuite(t *testing.T) {
	s := sc.s.in(t)
	// Without a suite the regression evidence fails and the request waits.
	var cr map[string]any
	cr, sc.op = s.tier3(t, "9")
	sc.path, sc.opPath = "/v1/change-requests/"+apitest.String(t, cr, "id"), "/v1/operations/"+apitest.String(t, sc.op, "id")
	if _, err := sc.w.RunOnce(sc.ctx); err != nil {
		t.Fatal(err)
	}
	got := s.status(t, sc.path)
	if got["status"] != "awaiting_evidence" {
		t.Errorf("without a suite: got status %v, want awaiting_evidence", got["status"])
	}
	if n := len(apitest.List(t, got, "evidence")); n != 2 {
		t.Fatalf("without a suite: got %d pieces of evidence, want 2: %v", n, got)
	}
	regression := evidenceOf(t, got, "regression")
	if regression["passed"] != false {
		t.Errorf("regression evidence passed: got %v, want false", regression["passed"])
	}
	if summary := apitest.String(t, regression, "summary"); !strings.Contains(summary, "No regression suite captured on version 1") {
		t.Errorf("regression evidence summary: got %q, want no suite on version 1", summary)
	}
	done := s.status(t, sc.opPath)
	if done["status"] != "succeeded" {
		t.Errorf("operation status: got %v, want succeeded", done["status"])
	}
	if passed := apitest.Object(t, done, "result")["regression_passed"]; passed != false {
		t.Errorf("operation regression_passed: got %v, want false", passed)
	}
}

// suiteCaptured is the step "suite captured" of TestRegressionEvidence.
func (sc *evidenceScenario) suiteCaptured(t *testing.T) {
	s := sc.s.in(t)
	// A captured suite (the fake regressor supplies the cases) is recorded with its cases and strata.
	if err := apitest.Committed(t, s.Tenant, s.Tenant.Human, func(ctx context.Context) (err error) {
		sc.suite, err = configuration.CaptureRegressionSuite(ctx, sc.deps, configuration.NewRegressionSuite{Name: "Baseline"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Must(200, "GET", "/v1/regression-suites/"+sc.suite.ID.String(), "", "")
	for field, want := range map[string]any{"case_count": float64(2), "config_version": float64(1), "number": "RS-0001"} {
		if got[field] != want {
			t.Errorf("suite %s: got %v, want %v", field, got[field], want)
		}
	}
	if n := len(apitest.List(t, got, "strata")); n != 2 {
		t.Errorf("suite strata: got %d, want 2", n)
	}
	if page, _ := s.Must(200, "GET", "/v1/regression-suites?config_version=1", "", ""); len(page["items"].([]any)) != 1 {
		t.Fatalf("list: %v", page)
	}
}

// suiteHandlersFailing is the step "suite handlers failing" of TestRegressionEvidence.
func (sc *evidenceScenario) suiteHandlersFailing(t *testing.T) {
	s := sc.s.in(t)
	// The handlers outside a unit of work, and every statement of a list failing in turn.
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	h := &configuration.Endpoints{Pages: pages, Dependencies: sc.deps}
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
			_, err := h.GetRegressionSuite(ctx, gen.GetRegressionSuiteParams{ID: sc.suite.ID})
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
}

// suitesPageOneAtATime is the step "suites page one at a time" of TestRegressionEvidence.
func (sc *evidenceScenario) suitesPageOneAtATime(t *testing.T) {
	s := sc.s.in(t)
	// Two suites page one at a time.
	s.suite(t)
	page, _ := s.Must(200, "GET", "/v1/regression-suites?page_size=1", "", "")
	if n := len(apitest.List(t, page, "items")); n != 1 {
		t.Errorf("page 1: got %d items, want 1", n)
	}
	if page["next_page_token"] == nil {
		t.Fatalf("page 1: got no next_page_token, want one: %v", page)
	}
	if rest, _ := s.Must(200, "GET", "/v1/regression-suites?page_size=1&page_token="+page["next_page_token"].(string), "", ""); len(rest["items"].([]any)) != 1 {
		t.Fatalf("page 2: %v", rest)
	}
}

// staleSuite is the step "stale suite" of TestRegressionEvidence.
func (sc *evidenceScenario) staleSuite(t *testing.T) {
	s := sc.s.in(t)
	// Stale: the suite no longer matches the base version.
	sc.reg.failures["19"] = []configuration.RegressionFailure{{Case: sc.reg.cases[0], Decision: "review"}}
	_, tag := s.Must(200, "GET", sc.path, "", "")
	s.Must(202, "POST", sc.path+":simulate", "", tag)
	if _, err := sc.w.RunOnce(sc.ctx); err != nil {
		t.Fatal(err)
	}
	got := s.status(t, sc.path)
	last := evidenceOf(t, got, "regression")
	if got["status"] != "awaiting_evidence" {
		t.Errorf("stale: got status %v, want awaiting_evidence", got["status"])
	}
	if summary := apitest.String(t, last, "summary"); !strings.Contains(summary, "no longer matches version 1") {
		t.Errorf("stale: got summary %q, want the suite no longer matching version 1", summary)
	}
	delete(sc.reg.failures, "19")
}

// regressionOutsideTheTouchedCompanies is the step "regression outside the touched companies" of TestRegressionEvidence.
func (sc *evidenceScenario) regressionOutsideTheTouchedCompanies(t *testing.T) {
	s := sc.s.in(t)
	// A regression outside the companies the change touches (company 1000 for a company 2000 change) fails; inside, it is the change at work.
	sc.reg.failures["9"] = []configuration.RegressionFailure{{Case: sc.reg.cases[1], Decision: "auto_post"}}
	_, tag := s.Must(200, "GET", sc.path, "", "")
	s.Must(202, "POST", sc.path+":simulate", "", tag)
	if _, err := sc.w.RunOnce(sc.ctx); err != nil {
		t.Fatal(err)
	}
	got := s.status(t, sc.path)
	last := evidenceOf(t, got, "regression")
	if got["status"] != "awaiting_evidence" {
		t.Errorf("regression outside: got status %v, want awaiting_evidence", got["status"])
	}
	if last["passed"] != false {
		t.Errorf("regression outside: got passed %v, want false", last["passed"])
	}
	if summary := apitest.String(t, last, "summary"); !strings.Contains(summary, "1 of them outside the companies") {
		t.Errorf("regression outside: got summary %q, want one outside the companies", summary)
	}
	if n := len(apitest.List(t, apitest.Object(t, last, "detail"), "regressions")); n != 1 {
		t.Errorf("regression outside: got %d regressions, want 1", n)
	}
}

// changesInsideTheTouchedCompanies is the step "changes inside the touched companies" of TestRegressionEvidence.
func (sc *evidenceScenario) changesInsideTheTouchedCompanies(t *testing.T) {
	s := sc.s.in(t)
	sc.reg.failures["9"] = []configuration.RegressionFailure{{Case: sc.reg.cases[0], Decision: "review"}}
	_, tag := s.Must(200, "GET", sc.path, "", "")
	s.Must(202, "POST", sc.path+":simulate", "", tag)
	if _, err := sc.w.RunOnce(sc.ctx); err != nil {
		t.Fatal(err)
	}
	got := s.status(t, sc.path)
	last := evidenceOf(t, got, "regression")
	detail := apitest.Object(t, last, "detail")
	if got["status"] != "pending_approval" {
		t.Errorf("changes inside: got status %v, want pending_approval", got["status"])
	}
	if last["passed"] != true {
		t.Errorf("changes inside: got passed %v, want true", last["passed"])
	}
	if n := len(apitest.List(t, detail, "changed")); n != 1 {
		t.Errorf("changes inside: got %d changed, want 1", n)
	}
	if n := len(apitest.List(t, detail, "regressions")); n != 0 {
		t.Errorf("changes inside: got %d regressions, want 0", n)
	}
	if touched := apitest.List(t, detail, "touched_companies")[0]; touched != "2000" {
		t.Errorf("changes inside: got touched company %v, want 2000", touched)
	}
}

// replayErrors is the step "replay errors" of TestRegressionEvidence.
func (sc *evidenceScenario) replayErrors(t *testing.T) {
	s := sc.s.in(t)
	// Replay errors, on the base or on the proposal: a problem fails the operation, anything else leaves it queued.
	_, sc.op = s.tier3(t, "8")
	sc.opPath = "/v1/operations/" + apitest.String(t, sc.op, "id")
	sc.reg.errs = map[string]error{"19": errors.New("hiccup")}
	if _, err := sc.w.RunOnce(sc.ctx); err == nil {
		t.Error("transient replay error on the base: got no error, want one")
	}
	if got := s.status(t, sc.opPath)["status"]; got != "queued" {
		t.Fatalf("transient replay error on the base: got operation %v, want queued", got)
	}
	sc.reg.errs = map[string]error{"8": errors.New("hiccup")}
	if _, err := sc.w.RunOnce(sc.ctx); err == nil {
		t.Error("transient replay error on the proposal: got no error, want one")
	}
	if got := s.status(t, sc.opPath)["status"]; got != "queued" {
		t.Fatalf("transient replay error on the proposal: got operation %v, want queued", got)
	}
	sc.reg.errs = map[string]error{"8": problem.Unprocessable("config.unreplayable", "cannot replay")}
	if _, err := sc.w.RunOnce(sc.ctx); err != nil {
		t.Fatalf("refused replay: %v", err)
	}
	if got := s.status(t, sc.opPath)["status"]; got != "failed" {
		t.Fatalf("refused replay: got operation %v, want failed", got)
	}
	sc.reg.errs = nil
	s.Problem(404, "request.not_found", "GET", "/v1/regression-suites/"+uuid.NewString(), "", "")
}

// captureFailures is the step "capture failures" of TestRegressionEvidence.
func (sc *evidenceScenario) captureFailures(t *testing.T) {
	s := sc.s.in(t)
	// Capturing fails like the regressor does, and with nothing to capture (the API's own regressor sees no invoices here).
	sc.reg.err = errors.New("down")
	if err := apitest.InRequest(t, s.Tenant, s.Tenant.Human, func(ctx context.Context) error {
		_, err := configuration.CaptureRegressionSuite(ctx, sc.deps, configuration.NewRegressionSuite{Name: "x"})
		return err
	}); err == nil || err.Error() != "down" {
		t.Fatalf("capture error: %v", err)
	}
	sc.reg.err = nil
	s.Problem(422, "config.no_cases", "POST", "/v1/regression-suites", `{"name": "x", "window_days": 30}`, "")
	// Every statement of a capture can fail.
	sc.reg.cases = []configuration.RegressionCase{caseOf(3, "2000", "review")}
	capture := func(ctx context.Context) error {
		_, err := configuration.CaptureRegressionSuite(ctx, sc.deps, configuration.NewRegressionSuite{Name: "x"})
		return err
	}
	if capture(context.Background()) == nil {
		t.Fatal("captured outside a unit of work")
	}
	apitest.EveryFailure(t, s.Tenant, s.Tenant.Human, "capture", capture)
	get := func(ctx context.Context) error {
		_, err := configuration.GetRegressionSuite(ctx, sc.suite.ID)
		return err
	}
	if get(context.Background()) == nil {
		t.Fatal("read outside a unit of work")
	}
	apitest.EveryFailure(t, s.Tenant, s.Tenant.Human, "get", get)
}
