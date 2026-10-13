package configuration_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// fakeSimulator answers every replay the same way.
type fakeSimulator struct {
	sim   configuration.Simulation
	err   error
	calls int
}

func (f *fakeSimulator) Simulate(context.Context, *config.Config, *config.Config) (configuration.Simulation, error) {
	f.calls++
	return f.sim, f.err
}

// worker runs simulations with the given simulator and a regressor whose
// suites always pass.
func worker(t *testing.T, s configuration.Simulator) *configuration.SimulationWorker {
	t.Helper()
	deps := fixture.Deps()
	deps.Simulator = s
	deps.Regressor = &fakeRegressor{cases: []configuration.RegressionCase{caseOf(1, "2000", "auto_post")}, failures: map[string][]configuration.RegressionFailure{}}
	return &configuration.SimulationWorker{Dependencies: deps, Pool: pgtest.Pool(t), AppRole: "erp_app", WorkerRole: "erp_worker"}
}

type sim struct {
	fixture.Env
}

// suite captures a regression suite on version 1 (the fake regressor
// supplies the cases), which tier 3 evidence needs.
func (s sim) suite(t *testing.T) {
	t.Helper()
	deps := fixture.Deps()
	deps.Regressor = &fakeRegressor{cases: []configuration.RegressionCase{caseOf(1, "2000", "auto_post")}}
	if err := apitest.Committed(t, s.Tenant, s.Tenant.Human, func(ctx context.Context) error {
		_, err := configuration.CaptureRegressionSuite(ctx, deps, configuration.NewRegressionSuite{Name: "Baseline"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// evidenceOf is the latest evidence of a kind on a change request.
func evidenceOf(got map[string]any, kind string) map[string]any {
	var out map[string]any
	for _, e := range got["evidence"].([]any) {
		if m := e.(map[string]any); m["kind"] == kind {
			out = m
		}
	}
	return out
}

// tier3 proposes and submits a posting-relevant change (a tax rate) and
// asks for its simulation; it returns the change request and the operation.
func (s sim) tier3(t *testing.T, rate string) (map[string]any, map[string]any) {
	t.Helper()
	doc := fixture.Document(t)
	doc.Companies[1].TaxCodes[0].RatePercent = rate
	cr, _ := s.Must(201, "POST", "/v1/change-requests", fmt.Sprintf(`{"title": "Tax %s", "rationale": "Law", "base_version": 1, "proposed": %s}`, rate, jsonOf(t, doc)), "")
	got, tag := s.Must(200, "POST", "/v1/change-requests/"+cr["id"].(string)+":submit", "", `"1"`)
	if got["status"] != "awaiting_evidence" {
		t.Fatalf("submitted: got status %v, want awaiting_evidence", got["status"])
	}
	if got["tier"] != float64(3) {
		t.Fatalf("submitted: got tier %v, want 3", got["tier"])
	}
	op, _ := s.Must(202, "POST", "/v1/change-requests/"+cr["id"].(string)+":simulate", "", tag)
	return got, op
}

func (s sim) status(t *testing.T, path string) map[string]any {
	t.Helper()
	m, _ := s.Must(200, "GET", path, "", "")
	return m
}

// TestSimulationEvidenceMovesTier3On is the tier 3 lifecycle: the worker
// records the simulation as evidence, the request moves to approval, two
// people approve, one activates.
func TestSimulationEvidenceMovesTier3On(t *testing.T) {
	s := sim{fixture.New(t, append(perms, "platform.operation.read", "platform.operation.cancel")...)}
	s.suite(t)
	cr, op := s.tier3(t, "9")
	path, opPath := "/v1/change-requests/"+cr["id"].(string), "/v1/operations/"+op["id"].(string)
	fake := &fakeSimulator{sim: configuration.Simulation{Passed: true, Summary: "3 invoices replayed", Detail: map[string]any{"invoices": 3}}}
	w := worker(t, fake)
	// The package database holds other tests' tenants too, so a round may run more than this job.
	if n, err := w.RunOnce(context.Background()); err != nil || n < 1 {
		t.Fatalf("run: got %d %v, want at least one job", n, err)
	}
	if fake.calls < 1 {
		t.Fatalf("simulator calls: got %d, want at least 1", fake.calls)
	}
	got := s.status(t, path)
	if got["status"] != "pending_approval" {
		t.Errorf("after the simulation: got status %v, want pending_approval", got["status"])
	}
	if got["workflow_request_id"] == nil {
		t.Error("after the simulation: got no workflow_request_id, want one")
	}
	if n := len(got["evidence"].([]any)); n != 2 {
		t.Fatalf("after the simulation: got %d pieces of evidence %v, want 2", n, got["evidence"])
	}
	e := evidenceOf(got, "simulation")
	for field, want := range map[string]any{"passed": true, "summary": "3 invoices replayed", "recorded_by": s.Tenant.Human.ID.String()} {
		if e[field] != want {
			t.Errorf("simulation evidence %s: got %v, want %v", field, e[field], want)
		}
	}
	if invoices := e["detail"].(map[string]any)["invoices"]; invoices != float64(3) {
		t.Errorf("simulation evidence invoices: got %v, want 3", invoices)
	}
	if passed := evidenceOf(got, "regression")["passed"]; passed != true {
		t.Errorf("regression evidence passed: got %v, want true", passed)
	}
	done := s.status(t, opPath)
	for field, want := range map[string]any{"status": "succeeded", "resource": path} {
		if done[field] != want {
			t.Errorf("operation %s: got %v, want %v", field, done[field], want)
		}
	}
	if result := done["result"].(map[string]any)["status"]; result != "pending_approval" {
		t.Errorf("operation result status: got %v, want pending_approval", result)
	}
	validated := events(t, s.Tenant, "change_request_validated")
	last := validated[len(validated)-1]
	if last["outcome"] != "pending_approval" {
		t.Errorf("last validated event outcome: got %v, want pending_approval", last["outcome"])
	}
	if last["workflow_request_id"] == nil {
		t.Error("last validated event workflow_request_id: got nil, want a request")
	}
	if r := s.Pending(t, cr["id"].(string)); len(r["steps"].([]any)) != 2 {
		t.Fatalf("approval request: %v", r)
	}
	s.Approve(t, cr["id"].(string))
	approved, tag := s.Must(200, "GET", path, "", "")
	if approved["status"] != "approved" {
		t.Fatalf("approved: %v", approved)
	}
	active, _ := s.Must(200, "POST", path+":activate", "", tag)
	if active["status"] != "active" {
		t.Errorf("activated status: got %v, want active", active["status"])
	}
	if active["activated_version"] != float64(2) {
		t.Errorf("activated version: got %v, want 2", active["activated_version"])
	}
	if n, err := w.RunOnce(context.Background()); err != nil || n != 0 {
		t.Fatalf("nothing left: %d %v", n, err)
	}
}

func TestSimulationOutcomes(t *testing.T) {
	s := sim{fixture.New(t, append(perms, "platform.operation.read", "platform.operation.cancel")...)}
	s.suite(t)
	ctx := context.Background()

	// A simulation that does not pass is recorded; the request waits for a better proposal or another run.
	cr, op := s.tier3(t, "8")
	path, opPath := "/v1/change-requests/"+cr["id"].(string), "/v1/operations/"+op["id"].(string)
	fake := &fakeSimulator{sim: configuration.Simulation{Passed: false, Summary: "2 cannot be evaluated", Detail: map[string]any{}}}
	w := worker(t, fake)
	if n, err := w.RunOnce(ctx); err != nil || n < 1 {
		t.Fatalf("failed simulation: %d %v", n, err)
	}
	got := s.status(t, path)
	if got["status"] != "awaiting_evidence" {
		t.Errorf("after a failed simulation: got status %v, want awaiting_evidence", got["status"])
	}
	if passed := evidenceOf(got, "simulation")["passed"]; passed != false {
		t.Errorf("after a failed simulation: got passed %v, want false", passed)
	}
	done := s.status(t, opPath)
	if done["status"] != "succeeded" {
		t.Errorf("operation status: got %v, want succeeded", done["status"])
	}
	if passed := done["result"].(map[string]any)["passed"]; passed != false {
		t.Errorf("operation result passed: got %v, want false", passed)
	}
	// Simulated again with a passing result, the latest evidence applies.
	_, tag := s.Must(200, "GET", path, "", "")
	s.Must(202, "POST", path+":simulate", "", tag)
	fake.sim = configuration.Simulation{Passed: true, Summary: "fine", Detail: map[string]any{}}
	if n, err := w.RunOnce(ctx); err != nil || n < 1 {
		t.Fatalf("second simulation: %d %v", n, err)
	}
	got = s.status(t, path)
	if got["status"] != "pending_approval" {
		t.Errorf("after the second simulation: got status %v, want pending_approval", got["status"])
	}
	if n := len(got["evidence"].([]any)); n != 4 {
		t.Errorf("after the second simulation: got %d pieces of evidence, want 4", n)
	}

	// A proposal the simulator refuses fails the operation; a transient error leaves it queued.
	cr, op = s.tier3(t, "7")
	path, opPath = "/v1/change-requests/"+cr["id"].(string), "/v1/operations/"+op["id"].(string)
	fake.err = errors.New("database hiccup")
	if n, err := w.RunOnce(ctx); err == nil || n != 0 {
		t.Fatalf("transient error: %d %v", n, err)
	}
	if queued := s.status(t, opPath); queued["status"] != "queued" {
		t.Fatalf("after a transient error: %v", queued)
	}
	fake.err = problem.Unprocessable("config.unreplayable", "The proposal removes company 2000.")
	if n, err := w.RunOnce(ctx); err != nil || n < 1 {
		t.Fatalf("refused: %d %v", n, err)
	}
	failed := s.status(t, opPath)
	if failed["status"] != "failed" {
		t.Errorf("refused operation: got status %v, want failed", failed["status"])
	}
	if code := failed["error"].(map[string]any)["code"]; code != "config.unreplayable" {
		t.Errorf("refused operation: got code %v, want config.unreplayable", code)
	}
	got = s.status(t, path)
	if got["status"] != "awaiting_evidence" {
		t.Errorf("after a refusal: got status %v, want awaiting_evidence", got["status"])
	}
	if n := len(got["evidence"].([]any)); n != 0 {
		t.Errorf("after a refusal: got %d pieces of evidence, want 0", n)
	}
	fake.err = nil

	// A request withdrawn before the worker gets to it fails the operation.
	cr, op = s.tier3(t, "6")
	path, opPath = "/v1/change-requests/"+cr["id"].(string), "/v1/operations/"+op["id"].(string)
	_, tag = s.Must(200, "GET", path, "", "")
	s.Must(200, "POST", path+":withdraw", `{"reason": "Changed my mind"}`, tag)
	if n, err := w.RunOnce(ctx); err != nil || n < 1 {
		t.Fatalf("withdrawn: %d %v", n, err)
	}
	failed = s.status(t, opPath)
	if failed["status"] != "failed" {
		t.Errorf("withdrawn operation: got status %v, want failed", failed["status"])
	}
	if code := failed["error"].(map[string]any)["code"]; code != "config.invalid_status" {
		t.Errorf("withdrawn operation: got code %v, want config.invalid_status", code)
	}
	// A cancelled operation is left alone; an operation naming no request fails.
	_, op = s.tier3(t, "5")
	_, tag = s.Must(200, "GET", "/v1/operations/"+op["id"].(string), "", "")
	s.Must(200, "POST", "/v1/operations/"+op["id"].(string)+":cancel", "", tag)
	bogus := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO platform.operation (tenant_id, id, kind, status, request, created_by, updated_by) VALUES ($1, $2, $3, 'queued', '{}', $4, $4)`,
		s.Tenant.ID, bogus, configuration.SimulateKind, s.Tenant.Human.ID)
	if n, err := w.RunOnce(ctx); err != nil || n < 1 {
		t.Fatalf("bogus and cancelled: %d %v", n, err)
	}
	failed = s.status(t, "/v1/operations/"+bogus.String())
	if failed["status"] != "failed" {
		t.Errorf("bogus operation: got status %v, want failed", failed["status"])
	}
	if code := failed["error"].(map[string]any)["code"]; code != "config.invalid_request" {
		t.Errorf("bogus operation: got code %v, want config.invalid_request", code)
	}
	if n, err := w.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("cancelled left alone: %d %v", n, err)
	}
}

func TestSimulationWorkerFailures(t *testing.T) {
	s := sim{fixture.New(t, append(perms, "platform.operation.read")...)}
	s.suite(t)
	ctx := context.Background()
	fake := &fakeSimulator{sim: configuration.Simulation{Passed: true, Summary: "fine", Detail: map[string]any{}}}
	cr, op := s.tier3(t, "4")
	opPath := "/v1/operations/" + op["id"].(string)
	status := func() string { return s.status(t, opPath)["status"].(string) }

	bad := worker(t, fake)
	bad.WorkerRole = "no_such_role"
	if _, err := bad.RunOnce(ctx); err == nil {
		t.Fatal("found jobs without the worker role")
	}
	bad = worker(t, fake)
	bad.AppRole = "no_such_role"
	if n, err := bad.RunOnce(ctx); err == nil || n != 0 {
		t.Fatalf("ran without the app role: got %d %v, want an error and no job", n, err)
	}
	if got := status(); got != "queued" {
		t.Fatalf("ran without the app role: got operation %s, want queued", got)
	}
	// Every statement of a round can fail; the job stays queued until one succeeds.
	pool := pgtest.Pool(t)
	for failAt := 1; ; failAt++ {
		w := worker(t, fake)
		w.Timeout, w.Now = time.Minute, time.Now
		w.Begin = func(ctx context.Context, sc database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, sc)
			if err != nil || sc.TenantID == uuid.Nil {
				return tx, err
			}
			return &pgtest.FailingTx{Tx: tx, FailAt: failAt}, nil
		}
		n, err := w.RunOnce(ctx)
		if err == nil {
			if n < 1 {
				t.Errorf("after %d failures: got %d jobs, want at least 1", failAt-1, n)
			}
			if got := status(); got != "succeeded" {
				t.Errorf("after %d failures: got operation %s, want succeeded", failAt-1, got)
			}
			if got := s.status(t, "/v1/change-requests/"+cr["id"].(string))["status"]; got != "pending_approval" {
				t.Errorf("after %d failures: got change request %v, want pending_approval", failAt-1, got)
			}
			break
		}
		if !errors.Is(err, pgtest.ErrInjected) {
			t.Fatalf("statement %d: got %v, want %v", failAt, err, pgtest.ErrInjected)
		}
		if got := status(); got != "queued" {
			t.Fatalf("statement %d: got operation %s, want queued", failAt, got)
		}
		if failAt > 100 {
			t.Fatalf("statement %d: still failing, want a round to succeed within 100 statements", failAt)
		}
	}
	for name, begin := range map[string]func(context.Context, database.Scope) (pgx.Tx, error){
		"begin": func(context.Context, database.Scope) (pgx.Tx, error) { return nil, pgtest.ErrInjected },
		"find": func(ctx context.Context, sc database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, sc)
			return &pgtest.FailingTx{Tx: tx, FailAt: 1}, err
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := worker(t, fake)
			w.Begin = begin
			if _, err := w.RunOnce(ctx); !errors.Is(err, pgtest.ErrInjected) {
				t.Errorf("got %v, want %v", err, pgtest.ErrInjected)
			}
		})
	}
	_, op3 := s.tier3(t, "3")
	begun := 0
	w := worker(t, fake)
	w.Begin = func(ctx context.Context, sc database.Scope) (pgx.Tx, error) {
		if begun++; sc.TenantID != uuid.Nil {
			return nil, pgtest.ErrInjected
		}
		return pool.Begin(ctx, sc)
	}
	if _, err := w.RunOnce(ctx); !errors.Is(err, pgtest.ErrInjected) {
		t.Fatalf("tenant transaction: got %v, want %v", err, pgtest.ErrInjected)
	}
	if begun < 2 {
		t.Fatalf("tenant transaction: got %d transactions begun, want at least 2", begun)
	}
	// A failed commit leaves the job queued for another attempt.
	failed := worker(t, fake)
	failed.Begin = func(ctx context.Context, sc database.Scope) (pgx.Tx, error) {
		tx, err := pool.Begin(ctx, sc)
		if err != nil || sc.TenantID == uuid.Nil {
			return tx, err
		}
		return &pgtest.FailingTx{Tx: tx, FailCommit: true}, nil
	}
	if _, err := failed.RunOnce(ctx); !errors.Is(err, pgtest.ErrInjected) {
		t.Fatalf("failed commit: got %v, want %v", err, pgtest.ErrInjected)
	}
	if got := s.status(t, "/v1/operations/"+op3["id"].(string))["status"]; got != "queued" {
		t.Fatalf("after a failed commit: got %v, want queued", got)
	}

	// Run logs failed rounds and stops with its context.
	var logs pgtest.SyncBuffer
	failing := worker(t, fake)
	failing.WorkerRole, failing.Interval, failing.Log = "no_such_role", time.Millisecond, slog.New(slog.NewTextHandler(&logs, nil))
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { failing.Run(runCtx); close(done) }()
	t.Cleanup(func() { cancel(); <-done }) // stops the worker should the wait fail
	pgtest.Eventually(t, 10*time.Second, func() bool { return logs.String() != "" }, "the failing worker logged nothing")
	cancel()
	<-done
	if !strings.Contains(logs.String(), "simulation failed") {
		t.Fatal(logs.String())
	}
	// With the simulation done by a working run, the real simulator is wired too.
	if n, err := worker(t, fixture.Deps().Simulator).RunOnce(ctx); err != nil || n < 1 {
		t.Fatalf("real simulator: %d %v", n, err)
	}
}

var _ = apitest.NewKey
