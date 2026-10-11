package configuration_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
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

func worker(t *testing.T, s configuration.Simulator) *configuration.SimulationWorker {
	t.Helper()
	deps := fixture.Deps()
	deps.Simulator = s
	return &configuration.SimulationWorker{Dependencies: deps, Pool: pgtest.Pool(t), AppRole: "erp_app", WorkerRole: "erp_worker"}
}

type sim struct {
	fixture.Env
}

// tier3 proposes and submits a posting-relevant change (a tax rate) and
// asks for its simulation; it returns the change request and the operation.
func (s sim) tier3(t *testing.T, rate string) (map[string]any, map[string]any) {
	t.Helper()
	doc := fixture.Document(t)
	doc.Companies[1].TaxCodes[0].RatePercent = rate
	cr, _ := s.Must(201, "POST", "/v1/change-requests", fmt.Sprintf(`{"title": "Tax %s", "rationale": "Law", "base_version": 1, "proposed": %s}`, rate, jsonOf(t, doc)), "")
	got, tag := s.Must(200, "POST", "/v1/change-requests/"+cr["id"].(string)+":submit", "", `"1"`)
	if got["status"] != "awaiting_evidence" || got["tier"] != float64(3) {
		t.Fatalf("submitted: %v", got)
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
	cr, op := s.tier3(t, "9")
	path, opPath := "/v1/change-requests/"+cr["id"].(string), "/v1/operations/"+op["id"].(string)
	fake := &fakeSimulator{sim: configuration.Simulation{Passed: true, Summary: "3 invoices replayed", Detail: map[string]any{"invoices": 3}}}
	w := worker(t, fake)
	// The package database holds other tests' tenants too, so a round may run more than this job.
	if n, err := w.RunOnce(context.Background()); err != nil || n < 1 || fake.calls < 1 {
		t.Fatalf("run: %d %v", n, err)
	}
	got := s.status(t, path)
	evidence := got["evidence"].([]any)
	if got["status"] != "pending_approval" || got["workflow_request_id"] == nil || len(evidence) != 1 {
		t.Fatalf("after the simulation: %v", got)
	}
	e := evidence[0].(map[string]any)
	if e["kind"] != "simulation" || e["passed"] != true || e["summary"] != "3 invoices replayed" || e["detail"].(map[string]any)["invoices"] != float64(3) ||
		e["recorded_by"] != s.Tenant.Human.ID.String() {
		t.Fatalf("evidence: %v", e)
	}
	done := s.status(t, opPath)
	if done["status"] != "succeeded" || done["resource"] != path || done["result"].(map[string]any)["status"] != "pending_approval" {
		t.Fatalf("operation: %v", done)
	}
	validated := events(t, s.Tenant, "change_request_validated")
	if last := validated[len(validated)-1]; last["outcome"] != "pending_approval" || last["workflow_request_id"] == nil {
		t.Fatalf("events: %v", validated)
	}
	if r := s.Pending(t, cr["id"].(string)); len(r["steps"].([]any)) != 2 {
		t.Fatalf("approval request: %v", r)
	}
	s.Approve(t, cr["id"].(string))
	approved, tag := s.Must(200, "GET", path, "", "")
	if approved["status"] != "approved" {
		t.Fatalf("approved: %v", approved)
	}
	if active, _ := s.Must(200, "POST", path+":activate", "", tag); active["status"] != "active" || active["activated_version"] != float64(2) {
		t.Fatalf("activated: %v", active)
	}
	if n, err := w.RunOnce(context.Background()); err != nil || n != 0 {
		t.Fatalf("nothing left: %d %v", n, err)
	}
}

func TestSimulationOutcomes(t *testing.T) {
	s := sim{fixture.New(t, append(perms, "platform.operation.read", "platform.operation.cancel")...)}
	ctx := context.Background()

	// A simulation that does not pass is recorded; the request waits for a better proposal or another run.
	cr, op := s.tier3(t, "8")
	path, opPath := "/v1/change-requests/"+cr["id"].(string), "/v1/operations/"+op["id"].(string)
	fake := &fakeSimulator{sim: configuration.Simulation{Passed: false, Summary: "2 cannot be evaluated", Detail: map[string]any{}}}
	w := worker(t, fake)
	if n, err := w.RunOnce(ctx); err != nil || n < 1 {
		t.Fatalf("failed simulation: %d %v", n, err)
	}
	if got := s.status(t, path); got["status"] != "awaiting_evidence" || got["evidence"].([]any)[0].(map[string]any)["passed"] != false {
		t.Fatalf("after a failed simulation: %v", got)
	}
	if done := s.status(t, opPath); done["status"] != "succeeded" || done["result"].(map[string]any)["passed"] != false {
		t.Fatalf("operation: %v", done)
	}
	// Simulated again with a passing result, the latest evidence applies.
	_, tag := s.Must(200, "GET", path, "", "")
	s.Must(202, "POST", path+":simulate", "", tag)
	fake.sim = configuration.Simulation{Passed: true, Summary: "fine", Detail: map[string]any{}}
	if n, err := w.RunOnce(ctx); err != nil || n < 1 {
		t.Fatalf("second simulation: %d %v", n, err)
	}
	if got := s.status(t, path); got["status"] != "pending_approval" || len(got["evidence"].([]any)) != 2 {
		t.Fatalf("after the second simulation: %v", got)
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
	if failed := s.status(t, opPath); failed["status"] != "failed" || failed["error"].(map[string]any)["code"] != "config.unreplayable" {
		t.Fatalf("refused operation: %v", failed)
	}
	if got := s.status(t, path); got["status"] != "awaiting_evidence" || len(got["evidence"].([]any)) != 0 {
		t.Fatalf("after a refusal: %v", got)
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
	if failed := s.status(t, opPath); failed["status"] != "failed" || failed["error"].(map[string]any)["code"] != "config.invalid_status" {
		t.Fatalf("withdrawn operation: %v", failed)
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
	if failed := s.status(t, "/v1/operations/"+bogus.String()); failed["status"] != "failed" || failed["error"].(map[string]any)["code"] != "config.invalid_request" {
		t.Fatalf("bogus operation: %v", failed)
	}
	if n, err := w.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("cancelled left alone: %d %v", n, err)
	}
}

func TestSimulationWorkerFailures(t *testing.T) {
	s := sim{fixture.New(t, append(perms, "platform.operation.read")...)}
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
	if n, err := bad.RunOnce(ctx); err == nil || n != 0 || status() != "queued" {
		t.Fatalf("ran without the app role: %d %v", n, err)
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
			if n < 1 || status() != "succeeded" || s.status(t, "/v1/change-requests/"+cr["id"].(string))["status"] != "pending_approval" {
				t.Fatalf("after %d failures: %d %s", failAt-1, n, status())
			}
			break
		}
		if !errors.Is(err, pgtest.ErrInjected) || status() != "queued" || failAt > 100 {
			t.Fatalf("statement %d: %v (%s)", failAt, err, status())
		}
	}
	for name, begin := range map[string]func(context.Context, database.Scope) (pgx.Tx, error){
		"begin": func(context.Context, database.Scope) (pgx.Tx, error) { return nil, pgtest.ErrInjected },
		"find": func(ctx context.Context, sc database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, sc)
			return &pgtest.FailingTx{Tx: tx, FailAt: 1}, err
		},
	} {
		w := worker(t, fake)
		w.Begin = begin
		if _, err := w.RunOnce(ctx); !errors.Is(err, pgtest.ErrInjected) {
			t.Errorf("%s: %v", name, err)
		}
	}
	s.tier3(t, "3")
	begun := 0
	w := worker(t, fake)
	w.Begin = func(ctx context.Context, sc database.Scope) (pgx.Tx, error) {
		if begun++; sc.TenantID != uuid.Nil {
			return nil, pgtest.ErrInjected
		}
		return pool.Begin(ctx, sc)
	}
	if _, err := w.RunOnce(ctx); !errors.Is(err, pgtest.ErrInjected) || begun < 2 {
		t.Fatalf("tenant transaction: %v after %d", err, begun)
	}

	// Run logs failed rounds and stops with its context.
	var logs syncBuffer
	failing := worker(t, fake)
	failing.WorkerRole, failing.Interval, failing.Log = "no_such_role", time.Millisecond, slog.New(slog.NewTextHandler(&logs, nil))
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { failing.Run(runCtx); close(done) }()
	for logs.Len() == 0 {
		time.Sleep(time.Millisecond)
	}
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

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Len()
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

var _ = apitest.NewKey
