package workflows_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	sdkworkflow "go.temporal.io/sdk/workflow"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/workflow"
	"github.com/mustie7iy/agentic-erp/internal/sap"
	"github.com/mustie7iy/agentic-erp/internal/workflows"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var perms = []string{"payments.run.read", "payments.run.propose", "payments.run.release", "payables.invoice.read", "payables.invoice.capture",
	"payables.invoice.post", "ledger.entry.read", "ledger.entry.post", "platform.operation.read", "platform.operation.cancel"}

type env struct {
	fixture.Env
	bank    uuid.UUID
	expense uuid.UUID
}

func setup(t *testing.T) env {
	t.Helper()
	f := fixture.New(t, perms...)
	e := env{Env: f, expense: f.Account(t, "2000", "520000")}
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT id FROM payments.house_bank WHERE legal_entity_id = $1`, f.DE).Scan(&e.bank); err != nil {
		t.Fatal(err)
	}
	return e
}

func id(m map[string]any) string { return m["id"].(string) }

// payable gives the fixture's supplier a bank account, once.
func (e env) payable(t *testing.T) {
	t.Helper()
	var n int
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT count(*) FROM partner.bank_account WHERE partner_id = $1`, e.Supplier).Scan(&n); err != nil {
		t.Fatalf("count bank accounts: %v", err)
	}
	if n > 0 {
		return
	}
	change, id := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	iban := "DE89370400440532013000"
	pgtest.Exec(t, `INSERT INTO partner.partner_change (tenant_id, id, number, kind, partner_id, payload, reason, status, requested_by, applied_at,
		created_by, updated_by) VALUES ($1, $2, $3, 'create', $4, '{}', 'test', 'applied', $5, now(), $5, $5)`, e.Tenant.ID, change, change.String(), e.Supplier, e.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO partner.bank_account (tenant_id, id, partner_id, account_cipher, account_fingerprint, account_last4, account_scheme, bic,
		bank_country_code, holder_name, valid_from, valid_to, partner_change_id, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, 'iban', 'COBADEFFXXX', 'DE', 'Supplier', '2026-01-01', NULL, $7, $8)`, e.Tenant.ID, id, e.Supplier, apitest.Seal(t, e.Tenant.ID, iban),
		apitest.Fingerprint(t, e.Tenant.ID, iban), iban[len(iban)-4:], change, e.Tenant.Human.ID)
}

// invoice posts an invoice so a run has something to pay.
func (e env) invoice(t *testing.T, ref, gross string) {
	t.Helper()
	e.payable(t)
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "supplier_reference": %q,
		"invoice_date": "2026-03-01", "gross_amount": {"amount": %q, "currency": "EUR"}, "tax_amount": {"amount": "0.00", "currency": "EUR"},
		"items": [{"account_id": %q, "description": "Fees", "quantity": {"value": "1", "unit": "EA"}, "amount": {"amount": %q, "currency": "EUR"}}]}`,
		e.DE, e.Supplier, ref, gross, e.expense, gross), "")
	e.Must(200, "POST", "/v1/supplier-invoices/"+id(inv)+":post", `{"posting_date": "2026-03-01"}`, `"1"`)
}

// propose asks for a run and returns its operation and the run's id.
func (e env) propose(t *testing.T, runDate string) (op map[string]any, run string) {
	t.Helper()
	op, _ = e.Must(202, "POST", "/v1/payment-runs:propose", fmt.Sprintf(`{"legal_entity_id": %q, "house_bank_id": %q, "run_date": %q, "due_by": %q}`,
		e.DE, e.bank, runDate, runDate), "")
	return op, strings.TrimPrefix(op["resource"].(string), "/v1/payment-runs/")
}

func activities(t *testing.T) *workflows.Payments {
	t.Helper()
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	return &workflows.Payments{Pool: pgtest.Pool(t), AppRole: "erp_app", Endpoints: app.Payments(app.Config{Pages: pages, FieldKeys: apitest.FieldKeys()})}
}

func input(e env, op map[string]any, run string) workflows.ProposeInput {
	return workflows.ProposeInput{TenantID: e.Tenant.ID, RunID: uuid.MustParse(run), OperationID: uuid.MustParse(id(op))}
}

// workflowTestTimeout bounds one workflow run in the SDK's test environment.
// The environment's default is 3 s of wall-clock time, and the activities
// here do real work against PostgreSQL (a migration loads 43 entries): on a
// machine busy with the rest of the suite under -race a run outlives 3 s and
// the environment fails it as timed out although nothing is wrong. A minute
// is far above a run's normal time and still ends a run that truly hangs.
const workflowTestTimeout = time.Minute

// workflowEnv is a test workflow environment with workflowTestTimeout.
func workflowEnv(s *testsuite.WorkflowTestSuite) *testsuite.TestWorkflowEnvironment {
	env := s.NewTestWorkflowEnvironment()
	env.SetTestTimeout(workflowTestTimeout)
	return env
}

// execute runs the proposal workflow in the SDK's test environment with the
// real activities against the package database.
func execute(t *testing.T, a *workflows.Payments, in workflows.ProposeInput, cancel bool) error {
	t.Helper()
	var s testsuite.WorkflowTestSuite
	env := workflowEnv(&s)
	env.RegisterWorkflow(workflows.ProposePaymentRun)
	env.RegisterActivity(a)
	if cancel {
		env.RegisterDelayedCallback(env.CancelWorkflow, 0)
	}
	env.ExecuteWorkflow(workflows.ProposePaymentRun, in)
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	return env.GetWorkflowError()
}

// TestProposalWorkflow: the workflow builds the run through one activity;
// a cancelled operation fails the run; a run already built is left alone;
// a cancelled workflow still settles the run; failures are reported.
func TestProposalWorkflow(t *testing.T) {
	s := &proposalScenario{e: setup(t), a: activities(t)}
	// Each step proposes from the invoices the ones before it left open, and
	// the second runs again what the first built, so the first failure stops
	// the rest.
	if !t.Run("the workflow builds the run", s.buildTheRun) {
		t.FailNow()
	}
	if !t.Run("a run already built is left alone", s.runAgain) {
		t.FailNow()
	}
	if !t.Run("a cancelled operation fails the run", s.cancelledOperation) {
		t.FailNow()
	}
	if !t.Run("a cancelled workflow still settles the run", s.cancelledWorkflow) {
		t.FailNow()
	}
	if !t.Run("database failures are reported", s.databaseFailures) {
		t.FailNow()
	}
	t.Run("the activity's clock dates the audit", s.clock)
}

// proposalScenario is what the steps of TestProposalWorkflow share: the
// tenant, the activities, and the run the first step builds for the second.
type proposalScenario struct {
	e     env
	a     *workflows.Payments
	op    map[string]any // the first proposal's operation
	run   string         // the first proposal's run
	built map[string]any // that run as the workflow built it
}

// get reads a resource for its body. The ETag that Must also returns is
// what a following write would send; the checks that use get write nothing
// after it, so dropping it loses nothing.
func (e env) get(path string) map[string]any {
	e.T.Helper()
	got, _ := e.Must(200, "GET", path, "", "")
	return got
}

// buildTheRun: the workflow builds the run through its activity and the
// operation succeeds.
func (s *proposalScenario) buildTheRun(t *testing.T) {
	e := s.e.in(t)
	e.invoice(t, "INV-1", "100.00")
	s.op, s.run = e.propose(t, "2026-04-15")
	if err := execute(t, s.a, input(e, s.op, s.run), false); err != nil {
		t.Fatal(err)
	}
	s.built = e.get("/v1/payment-runs/" + s.run)
	done := e.get("/v1/operations/" + id(s.op))
	if s.built["status"] != "proposed" {
		t.Fatalf("built run status: got %v, want proposed", s.built["status"])
	}
	if amount := s.built["total_amount"].(map[string]any)["amount"]; amount != "100.00" {
		t.Fatalf("built run total: got %v, want 100.00", amount)
	}
	if done["status"] != "succeeded" {
		t.Fatalf("operation status: got %v, want succeeded", done["status"])
	}
}

// runAgain: the workflow run again finds nothing left to do and does
// nothing twice.
func (s *proposalScenario) runAgain(t *testing.T) {
	e := s.e.in(t)
	if err := execute(t, s.a, input(e, s.op, s.run), false); err != nil {
		t.Fatal(err)
	}
	if again := e.get("/v1/payment-runs/" + s.run); again["version"] != s.built["version"] {
		t.Fatalf("built twice: %v", again)
	}
}

// cancelledOperation: the operation cancelled before the work ran fails the
// run, and its items are free again.
func (s *proposalScenario) cancelledOperation(t *testing.T) {
	e := s.e.in(t)
	e.invoice(t, "INV-2", "50.00")
	op2, run2 := e.propose(t, "2026-04-16")
	e.Must(200, "POST", "/v1/operations/"+id(op2)+":cancel", "", `"1"`)
	if err := execute(t, s.a, input(e, op2, run2), false); err != nil {
		t.Fatal(err)
	}
	if failed := e.get("/v1/payment-runs/" + run2); failed["status"] != "failed" {
		t.Fatalf("cancelled: %v", failed)
	}
}

// cancelledWorkflow: the workflow itself cancelled, its detached activity
// still settles the run.
func (s *proposalScenario) cancelledWorkflow(t *testing.T) {
	e := s.e.in(t)
	op3, run3 := e.propose(t, "2026-04-17")
	if err := execute(t, s.a, input(e, op3, run3), true); err != nil {
		t.Fatal(err)
	}
	if settled := e.get("/v1/payment-runs/" + run3); settled["status"] != "proposed" {
		t.Fatalf("after the cancelled workflow: %v", settled)
	}
}

// databaseFailures: a failing commit, statement or begin is returned for the
// retry policy, and nothing is half done. The activities' Begin is left
// unset for the next step.
func (s *proposalScenario) databaseFailures(t *testing.T) {
	e, a, pool := s.e.in(t), s.a, pgtest.Pool(t)
	defer func() { a.Begin = nil }()
	e.invoice(t, "INV-3", "25.00")
	op4, run4 := e.propose(t, "2026-04-18")
	a.Begin = func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
		tx, err := pool.Begin(ctx, scope)
		if err != nil || scope.TenantID == uuid.Nil {
			return tx, err
		}
		return &pgtest.FailingTx{Tx: tx, FailCommit: true}, nil
	}
	if err := a.Propose(context.Background(), input(e, op4, run4)); !errors.Is(err, pgtest.ErrInjected) || !strings.Contains(err.Error(), "commit") {
		t.Fatalf("failed commit: got %v, want the injected failure", err)
	}
	if r := e.get("/v1/payment-runs/" + run4); r["status"] != "proposing" {
		t.Fatalf("after a failed commit: got %v, want proposing", r["status"])
	}
	s.everyStatementFails(t, pool, input(e, op4, run4))
	a.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errors.New("database down") }
	pgtest.WantErr(t, a.Propose(context.Background(), input(e, op4, run4)), "database down")
}

// everyStatementFails proposes in with its first, second, ... statement
// failing until the run is built, and checks each failure left the run
// proposing.
func (s *proposalScenario) everyStatementFails(t *testing.T, pool *database.Pool, in workflows.ProposeInput) {
	t.Helper()
	e, a := s.e.in(t), s.a
	for n := 1; n < 60; n++ {
		count := 0
		a.Begin = func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, scope)
			if err != nil {
				return nil, err
			}
			return &pgtest.FailingTx{Tx: tx, FailAt: n, Count: &count}, nil
		}
		err := a.Propose(context.Background(), in)
		if err == nil {
			if n == 1 {
				t.Fatal("built without a statement to fail")
			}
			break
		}
		if !errors.Is(err, pgtest.ErrInjected) {
			t.Fatalf("statement %d: %v", n, err)
		}
		if r := e.get("/v1/payment-runs/" + in.RunID.String()); r["status"] != "proposing" {
			t.Fatalf("statement %d: half built: %v", n, r)
		}
	}
}

// clock: an unknown run is nothing to do, and the activities' clock, not
// the database's, dates the audit entries.
func (s *proposalScenario) clock(t *testing.T) {
	e, a := s.e.in(t), s.a
	a.Timeout = time.Minute
	a.Now = func() time.Time { return time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC) } // later than any request's own entry
	if err := a.Propose(context.Background(), workflows.ProposeInput{TenantID: e.Tenant.ID, RunID: uuid.New()}); err != nil {
		t.Fatalf("unknown run: %v", err)
	}
	e.invoice(t, "INV-5", "5.00")
	op5, run5 := e.propose(t, "2026-04-19")
	if err := a.Propose(context.Background(), input(e, op5, run5)); err != nil {
		t.Fatalf("with a clock: %v", err)
	}
	var at time.Time
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT max(occurred_at) FROM audit.audit_event WHERE tenant_id = $1 AND object_id = $2`, e.Tenant.ID, uuid.MustParse(run5)).Scan(&at); err != nil {
		t.Fatalf("read the audit time: %v", err)
	}
	if !at.Equal(a.Now()) {
		t.Fatalf("audit time %v, want the activity's clock", at)
	}
}

// fakeEngine records starts and refuses what it is told to.
type fakeEngine struct {
	mu      sync.Mutex
	started []workflow.Start
	known   map[string]bool
	fail    string // start ids containing this fail
}

func (f *fakeEngine) Start(_ context.Context, s workflow.Start) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != "" && strings.Contains(s.ID, f.fail) {
		return false, errors.New("engine refused " + s.ID)
	}
	if f.known == nil {
		f.known = map[string]bool{}
	}
	if f.known[s.ID] {
		return false, nil
	}
	f.known[s.ID] = true
	f.started = append(f.started, s)
	return true, nil
}

func (f *fakeEngine) Cancel(context.Context, string) error { return nil }

// TestDispatcher: queued proposals become workflows named by the run's
// business key, once; the id is recorded on the operation; a refusal
// leaves the operation for the next round and the others go on.
func TestDispatcher(t *testing.T) {
	engine := &fakeEngine{}
	logs := &pgtest.SyncBuffer{}
	s := &dispatchScenario{e: setup(t), engine: engine, logs: logs, pool: pgtest.Pool(t),
		d: &workflows.Dispatcher{Pool: pgtest.Pool(t), WorkerRole: "erp_worker", Client: engine, Queue: "erp-test", Log: slog.New(slog.NewTextHandler(logs, nil))}}
	// Each step goes on from the operations the ones before it dispatched,
	// so the first failure stops the rest.
	if !t.Run("queued proposals start their workflows once", s.proposalsStartOnce) {
		t.FailNow()
	}
	if !t.Run("a cancelled operation is dispatched and a refused start retried", s.cancelledAndRefused) {
		t.FailNow()
	}
	if !t.Run("a release is dispatched by its attempt", s.releaseAttempts) {
		t.FailNow()
	}
	if !t.Run("a resumed release is the next attempt", s.resumedRelease) {
		t.FailNow()
	}
	if !t.Run("database failures are reported", s.databaseFailures) {
		t.FailNow()
	}
	t.Run("Run logs its rounds and stops with the context", s.runLoop)
}

// dispatchScenario is what the steps of TestDispatcher share: the tenant,
// the dispatcher with its engine and log, the pool the failure steps
// wrap, and the release one step dispatches for the next to resume.
type dispatchScenario struct {
	e         env
	d         *workflows.Dispatcher
	engine    *fakeEngine
	logs      *pgtest.SyncBuffer
	pool      *database.Pool
	builtRun  string                 // the run releaseAttempts releases
	rel       workflows.ReleaseInput // its first release
	relNumber string                 // its document number
}

// proposalsStartOnce: queued proposals start their workflows under the
// run's business key, recorded on the operation, and nothing twice.
func (s *dispatchScenario) proposalsStartOnce(t *testing.T) {
	e, d, ctx := s.e.in(t), s.d, context.Background()
	op1, run1 := e.propose(t, "2026-04-15")
	e.propose(t, "2026-04-16")
	n, err := d.RunOnce(ctx)
	if err != nil || n < 2 {
		t.Fatalf("round: %d %v", n, err)
	}
	var number string
	if err := pgtest.Admin(t).QueryRow(ctx, `SELECT document_number FROM payments.payment_run WHERE id = $1`, uuid.MustParse(run1)).Scan(&number); err != nil {
		t.Fatalf("read run 1 number: %v", err)
	}
	want := workflows.WorkflowID(e.Tenant.ID, e.DE, number)
	var workflowID *string
	if err := pgtest.Admin(t).QueryRow(ctx, `SELECT workflow_id FROM platform.operation WHERE id = $1`, uuid.MustParse(id(op1))).Scan(&workflowID); err != nil {
		t.Fatalf("read the workflow id: %v", err)
	}
	if workflowID == nil || *workflowID != want {
		t.Fatalf("workflow id on the operation: %v, want %s", workflowID, want)
	}
	found := false
	for _, st := range s.engine.started {
		// This is the scenario's first round and its engine is new, and no
		// test of the package leaves work queued (R66): only proposals are
		// queued at this point, so any other start is work this test should
		// not see.
		in, ok := st.Args[0].(workflows.ProposeInput)
		if !ok {
			t.Fatalf("start %s: got input %T, want workflows.ProposeInput: only proposals are queued", st.ID, st.Args[0])
		}
		found = found || st.ID == want && st.Queue == "erp-test" && st.Name == workflows.ProposeWorkflow && in.RunID == uuid.MustParse(run1) && in.OperationID == uuid.MustParse(id(op1)) && in.TenantID == e.Tenant.ID
	}
	if !found {
		t.Fatalf("starts: %+v", s.engine.started)
	}
	if n, err := d.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("second round: %d %v", n, err)
	}
}

// cancelledAndRefused: a cancelled operation still gets its workflow (which
// fails the run); a refused start is retried next round.
func (s *dispatchScenario) cancelledAndRefused(t *testing.T) {
	e, d, ctx := s.e.in(t), s.d, context.Background()
	op3, run3 := e.propose(t, "2026-04-17")
	e.Must(200, "POST", "/v1/operations/"+id(op3)+":cancel", "", `"1"`)
	op4, _ := e.propose(t, "2026-04-18")
	var number4 string
	if err := pgtest.Admin(t).QueryRow(ctx, `SELECT document_number FROM payments.payment_run WHERE operation_id = $1`, uuid.MustParse(id(op4))).Scan(&number4); err != nil {
		t.Fatalf("read run 4 number: %v", err)
	}
	s.engine.fail = number4
	n, err := d.RunOnce(ctx)
	pgtest.WantErr(t, err, "engine refused")
	if n != 1 {
		t.Fatalf("round with a refusal: got %d dispatched, want 1", n)
	}
	var id3 *string
	if err := pgtest.Admin(t).QueryRow(ctx, `SELECT workflow_id FROM platform.operation WHERE id = $1`, uuid.MustParse(id(op3))).Scan(&id3); err != nil {
		t.Fatalf("read the workflow id: %v", err)
	}
	if id3 == nil || !strings.Contains(*id3, run3[:0]+e.DE.String()) {
		t.Fatalf("cancelled operation not dispatched: %v", id3)
	}
	s.engine.fail = ""
	if n, err := d.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("retry round: %d %v", n, err)
	}
}

// releaseAttempts: a release operation is dispatched under the run's key
// and its first attempt.
func (s *dispatchScenario) releaseAttempts(t *testing.T) {
	e, d, ctx := s.e.in(t), s.d, context.Background()
	e.invoice(t, "INV-D", "3.00")
	built, builtRun := e.propose(t, "2026-04-21")
	if _, err := d.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := execute(t, activities(t), input(e, built, builtRun), false); err != nil {
		t.Fatal(err)
	}
	rel := e.releaseAsked(t, builtRun)
	if n, err := d.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("release dispatch: %d %v", n, err)
	}
	var relNumber string
	if err := pgtest.Admin(t).QueryRow(ctx, `SELECT document_number FROM payments.payment_run WHERE id = $1`, rel.RunID).Scan(&relNumber); err != nil {
		t.Fatalf("read the released run number: %v", err)
	}
	wantRel := workflows.ReleaseWorkflowID(e.Tenant.ID, e.DE, relNumber, 1)
	last, got := s.lastRelease(t)
	if last.ID != wantRel {
		t.Fatalf("release start id: got %s, want %s", last.ID, wantRel)
	}
	if last.Name != workflows.ReleaseWorkflow {
		t.Fatalf("release start workflow: got %s, want %s", last.Name, workflows.ReleaseWorkflow)
	}
	if got != rel {
		t.Fatalf("release start input: got %+v, want %+v", got, rel)
	}
	s.builtRun, s.rel, s.relNumber = builtRun, rel, relNumber
}

// resumedRelease: the release fails with nothing posted, so the run is
// proposed again, and the next release is dispatched as attempt 2.
func (s *dispatchScenario) resumedRelease(t *testing.T) {
	e, d, ctx := s.e.in(t), s.d, context.Background()
	if err := apitest.Committed(t, e.Tenant, e.Approver, func(ctx context.Context) error {
		r, err := payments.GetRun(ctx, s.rel.RunID)
		if err != nil {
			return err
		}
		_, err = activities(t).Endpoints.FailRelease(ctx, r, problem.Unprocessable("payments.bank_file_invalid", "test"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rel2 := e.releaseAsked(t, s.builtRun)
	if n, err := d.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("second release dispatch: %d %v", n, err)
	}
	last, got := s.lastRelease(t)
	if want := workflows.ReleaseWorkflowID(e.Tenant.ID, e.DE, s.relNumber, 2); last.ID != want {
		t.Errorf("second release start id: got %s, want %s", last.ID, want)
	}
	if got != rel2 {
		t.Errorf("second release start input: got %+v, want %+v", got, rel2)
	}
}

// lastRelease returns the engine's latest start and its release input. The
// round before it dispatched exactly one operation, the release asked for,
// so a latest start that is not a release fails the step.
func (s *dispatchScenario) lastRelease(t *testing.T) (workflow.Start, workflows.ReleaseInput) {
	t.Helper()
	last := s.engine.started[len(s.engine.started)-1]
	in, ok := last.Args[0].(workflows.ReleaseInput)
	if !ok {
		t.Fatalf("start %s: got input %T, want workflows.ReleaseInput", last.ID, last.Args[0])
	}
	return last, in
}

// databaseFailures: a failing statement, begin or commit of a round is
// reported. The dispatcher's Begin is left unset for the next step.
func (s *dispatchScenario) databaseFailures(t *testing.T) {
	e, d, pool, ctx := s.e.in(t), s.d, s.pool, context.Background()
	defer func() { d.Begin = nil }()
	e.propose(t, "2026-04-19")
	for n := 1; n < 10; n++ {
		count := 0
		d.Begin = func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, scope)
			if err != nil {
				return nil, err
			}
			return &pgtest.FailingTx{Tx: tx, FailAt: n, Count: &count}, nil
		}
		_, err := d.RunOnce(ctx)
		if err == nil {
			if n == 1 {
				t.Fatal("dispatched without a statement to fail")
			}
			break
		}
		if !errors.Is(err, pgtest.ErrInjected) {
			t.Fatalf("statement %d: %v", n, err)
		}
	}
	d.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errors.New("database down") }
	_, err := d.RunOnce(ctx)
	pgtest.WantErr(t, err, "database down")
	d.Begin = func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
		tx, err := pool.Begin(ctx, scope)
		if err != nil {
			return nil, err
		}
		return &pgtest.FailingTx{Tx: tx, FailCommit: true}, nil
	}
	if _, err := d.RunOnce(ctx); !errors.Is(err, pgtest.ErrInjected) {
		t.Fatalf("commit failing: got %v, want %v", err, pgtest.ErrInjected)
	}
}

// runLoop: Run logs a failed round, then a quiet one, and stops with the
// context. Each loop is waited for before the dispatcher is changed: a loop
// still running would read Begin and Interval while they are written.
func (s *dispatchScenario) runLoop(t *testing.T) {
	e, d, logs, ctx := s.e.in(t), s.d, s.logs, context.Background()
	d.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errors.New("database down") }
	failCtx, stopFailing := context.WithCancel(ctx)
	stopped := make(chan struct{})
	go func() { d.Run(failCtx); close(stopped) }()
	t.Cleanup(func() { stopFailing(); <-stopped }) // stops the dispatcher should the wait fail
	pgtest.Eventually(t, 10*time.Second, func() bool { return strings.Contains(logs.String(), "workflow dispatch failed") },
		"the dispatcher did not log its failed round: %s", logs)
	stopFailing()
	<-stopped
	d.Begin = nil
	d.Interval = time.Hour
	e.propose(t, "2026-04-20")
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { d.Run(runCtx); close(done) }()
	t.Cleanup(func() { cancel(); <-done }) // stops the dispatcher should the wait fail
	pgtest.Eventually(t, 10*time.Second, func() bool { return strings.Contains(logs.String(), "workflows started") },
		"the dispatcher did not log the workflows it started: %s", logs)
	cancel()
	<-done
}

// releaseAsked asks for the run's release as the approver and returns the
// release input the workflow takes.
func (e env) releaseAsked(t *testing.T, run string) workflows.ReleaseInput {
	t.Helper()
	_, tag := e.Must(200, "GET", "/v1/payment-runs/"+run, "", "")
	op, _ := e.As(e.Approver).Must(202, "POST", "/v1/payment-runs/"+run+":release", "", tag)
	return workflows.ReleaseInput{TenantID: e.Tenant.ID, RunID: uuid.MustParse(run), OperationID: uuid.MustParse(id(op))}
}

// executeRelease runs the release workflow in the SDK's test environment
// with the real activities against the package database, set first.
func executeRelease(t *testing.T, a *workflows.Payments, in workflows.ReleaseInput, set ...func(env *testsuite.TestWorkflowEnvironment)) error {
	t.Helper()
	var s testsuite.WorkflowTestSuite
	env := workflowEnv(&s)
	env.RegisterWorkflow(workflows.ReleasePaymentRun)
	env.RegisterActivity(a)
	for _, f := range set {
		f(env)
	}
	env.ExecuteWorkflow(workflows.ReleasePaymentRun, in)
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	return env.GetWorkflowError()
}

// in returns e reporting to t, so a step's failure stops that step rather
// than its parent.
func (e env) in(t *testing.T) env {
	t.Helper()
	e.T = t
	return e
}

// queuedReleases counts the tenant's release operations the releaser would
// pick up: those of other tests are the releaser's too, so a test counts
// its own tenant's.
func queuedReleases(t *testing.T, tenant uuid.UUID) int {
	t.Helper()
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM platform.operation o
		JOIN payments.payment_run r ON r.tenant_id = o.tenant_id AND r.release_operation_id = o.id
		WHERE o.tenant_id = $1 AND o.kind = $2 AND o.status IN ('queued', 'running') AND r.status = 'releasing'`,
		[]any{tenant, payments.ReleaseKind}, &n)
	return n
}

// setHolder sets the holder of the fixture supplier's bank account, past
// its validation, so a bank file is refused (blank) or accepted again.
func (e env) setHolder(t *testing.T, holder string) {
	t.Helper()
	ctx := context.Background()
	c := pgtest.Admin(t) // a connection of its own, so the replica role below ends with it
	if _, err := c.Exec(ctx, `SET session_replication_role = replica`); err != nil {
		t.Fatalf("skip the bank account's triggers: %v", err)
	}
	if _, err := c.Exec(ctx, `UPDATE partner.bank_account SET holder_name = $1 WHERE partner_id = $2`, holder, e.Supplier); err != nil {
		t.Fatalf("set the holder: %v", err)
	}
}

// TestReleaseWorkflow: the release posts one payment per activity and
// writes the bank file; a business problem fails the operation through the
// workflow; a failure of the machinery is retried and then left for a
// resumed release; the in-process releaser does the same steps.
func TestReleaseWorkflow(t *testing.T) {
	a := activities(t)
	logs := &pgtest.SyncBuffer{}
	s := &releaseScenario{e: setup(t), a: a, logs: logs,
		w: &workflows.Releaser{Payments: a, Pool: pgtest.Pool(t), WorkerRole: "erp_worker", Log: slog.New(slog.NewTextHandler(logs, nil))}}
	// Each step goes on from the runs the ones before it left, so the first failure stops the rest.
	if !t.Run("the release posts and writes the bank file", s.releaseAndWriteTheFile) {
		t.FailNow()
	}
	if !t.Run("a refused bank file fails the operation", s.refuseTheBankFile) {
		t.FailNow()
	}
	if !t.Run("the machinery failing leaves the run releasing", s.machineryFails) {
		t.FailNow()
	}
	if !t.Run("the in-process releaser finishes it", s.resumeWithTheReleaser) {
		t.FailNow()
	}
	if !t.Run("a proposed run has no releaser", s.proposedRunHasNoReleaser) {
		t.FailNow()
	}
	if !t.Run("the releaser records a problem and reports failures", s.releaserFailures) {
		t.FailNow()
	}
	t.Run("every failing statement of a two-payment release is reported", s.twoPaymentFailures)
}

// releaseScenario is what the steps of TestReleaseWorkflow share: the
// tenant, the activities and the releaser, and the runs one step leaves
// for the next.
type releaseScenario struct {
	e    env
	a    *workflows.Payments
	w    *workflows.Releaser
	logs *pgtest.SyncBuffer
	run2 string // left releasing by the refused bank file
	run4 string // proposed, then released by the releaser
}

// machineryFails: the activities are retried, then the workflow fails and
// the run stays releasing for a resumed release.
func (s *releaseScenario) machineryFails(t *testing.T) {
	e, a, run2 := s.e.in(t), s.a, s.run2
	e.setHolder(t, "Supplier")
	in3 := e.releaseAsked(t, run2)
	a.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errors.New("database down") }
	pgtest.WantErr(t, executeRelease(t, a, in3), "database down")
	a.Begin = nil
	if still := e.get("/v1/payment-runs/" + run2); still["status"] != "releasing" {
		t.Fatalf("after the machinery failed: %v", still)
	}
}

// proposedRunHasNoReleaser: activities outside a release refuse a run
// that is only proposed.
func (s *releaseScenario) proposedRunHasNoReleaser(t *testing.T) {
	e, a := s.e.in(t), s.a
	e.invoice(t, "INV-3", "25.00")
	op4, run4 := e.propose(t, "2026-04-17")
	if err := execute(t, a, input(e, op4, run4), false); err != nil {
		t.Fatal(err)
	}
	_, err := a.PaymentsToPost(context.Background(), workflows.ReleaseInput{TenantID: e.Tenant.ID, RunID: uuid.MustParse(run4)})
	pgtest.WantErr(t, err, "not releasing")
	s.run4 = run4
}

// releaseAndWriteTheFile: a release posts its payment and writes the bank
// file.
func (s *releaseScenario) releaseAndWriteTheFile(t *testing.T) {
	e, a := s.e.in(t), s.a
	e.invoice(t, "INV-1", "100.00")
	op, run := e.propose(t, "2026-04-15")
	if err := execute(t, a, input(e, op, run), false); err != nil {
		t.Fatal(err)
	}
	in := e.releaseAsked(t, run)
	if err := executeRelease(t, a, in); err != nil {
		t.Fatal(err)
	}
	released := e.get("/v1/payment-runs/" + run)
	done := e.get("/v1/operations/" + in.OperationID.String())
	if released["status"] != "released" {
		t.Fatalf("run status: got %v, want released", released["status"])
	}
	if done["status"] != "succeeded" {
		t.Fatalf("operation status: got %v, want succeeded", done["status"])
	}
	if progress := done["progress"].(map[string]any)["done"]; progress != float64(1) {
		t.Fatalf("operation progress done: got %v, want 1", progress)
	}
	if done["result"].(map[string]any)["bank_file_id"] == nil {
		t.Fatalf("operation result bank_file_id: got nil, want the bank file: %v", done["result"])
	}
}

// refuseTheBankFile: the vendor's bank account has no holder, so the file
// is refused after the posting, and the run is left releasing. The
// release's failure, failing on the machinery seven times first, is tried
// until it is recorded (review:T68d).
func (s *releaseScenario) refuseTheBankFile(t *testing.T) {
	e, a := s.e.in(t), s.a
	e.setHolder(t, " ")
	e.invoice(t, "INV-2", "50.00")
	op2, run2 := e.propose(t, "2026-04-16")
	if err := execute(t, a, input(e, op2, run2), false); err != nil {
		t.Fatal(err)
	}
	in2 := e.releaseAsked(t, run2)
	var failTries atomic.Int32
	flakyFailure := func(env *testsuite.TestWorkflowEnvironment) {
		env.OnActivity(a.FailRelease, mock.Anything, mock.Anything, mock.Anything).Return(
			func(ctx context.Context, in workflows.ReleaseInput, why problem.Problem) error {
				if failTries.Add(1) <= 7 {
					return errors.New("audit unreachable")
				}
				return a.FailRelease(ctx, in, why)
			})
	}
	if err := executeRelease(t, a, in2, flakyFailure); err != nil {
		t.Fatal(err)
	}
	if n := failTries.Load(); n != 8 {
		t.Errorf("the failure's tries: got %d, want 8", n)
	}
	failed := e.get("/v1/operations/" + in2.OperationID.String())
	after := e.get("/v1/payment-runs/" + run2)
	if failed["status"] != "failed" {
		t.Fatalf("refused file operation status: got %v, want failed", failed["status"])
	}
	if code := failed["error"].(map[string]any)["code"]; code != "payments.bank_file_invalid" {
		t.Fatalf("refused file operation error: got %v, want payments.bank_file_invalid", code)
	}
	if after["status"] != "releasing" {
		t.Fatalf("refused file run status: got %v, want releasing", after["status"])
	}
	s.run2 = run2
}

// resumeWithTheReleaser: the in-process releaser finishes the run the
// machinery left releasing; the posted payment is left alone, the file is
// written. The releaser serves every tenant, so its count includes other
// tests' runs; this tenant's queue is what is checked exactly.
func (s *releaseScenario) resumeWithTheReleaser(t *testing.T) {
	e, w, run2 := s.e.in(t), s.w, s.run2
	if n := queuedReleases(t, e.Tenant.ID); n != 1 {
		t.Fatalf("this tenant's queued releases before the round: got %d, want 1", n)
	}
	if n, err := w.RunOnce(context.Background()); err != nil || n < 1 {
		t.Fatalf("releaser: %d %v", n, err)
	}
	if n := queuedReleases(t, e.Tenant.ID); n != 0 {
		t.Fatalf("this tenant's queued releases after the round: got %d, want 0", n)
	}
	if got := e.get("/v1/payment-runs/" + run2); got["status"] != "released" {
		t.Fatalf("resumed by the releaser: %v", got)
	}
}

// releaserFailures: the releaser's problem path and its loop: a refused
// file is recorded, a dead database is logged, the loop stops with the
// context; a failing listing is reported. It ends with leftForTheNextRound,
// which checks that a release whose steps fail is reported and left queued
// for the next round, and runs that round.
func (s *releaseScenario) releaserFailures(t *testing.T) {
	e, w, logs, run4 := s.e.in(t), s.w, s.logs, s.run4
	e.setHolder(t, " ")
	in4 := e.releaseAsked(t, run4)
	if n, err := w.RunOnce(context.Background()); err != nil || n < 1 {
		t.Fatalf("releaser with a refused file: %d %v", n, err)
	}
	if n := queuedReleases(t, e.Tenant.ID); n != 0 {
		t.Fatalf("this tenant's queued releases after the refused file: got %d, want 0", n)
	}
	if failed := e.get("/v1/operations/" + in4.OperationID.String()); failed["status"] != "failed" {
		t.Fatalf("releaser did not record the problem: %v", failed)
	}
	w.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errors.New("database down") }
	runCtx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { w.Run(runCtx); close(stopped) }()
	t.Cleanup(func() { cancel(); <-stopped }) // stops the releaser should the wait fail
	pgtest.Eventually(t, 10*time.Second, func() bool { return strings.Contains(logs.String(), "payment release failed") },
		"the releaser did not log its failed round: %s", logs)
	cancel()
	<-stopped // before Begin is replaced: the loop reads it
	pool := pgtest.Pool(t)
	w.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		tx, err := pool.Begin(ctx, s)
		if err != nil {
			return nil, err
		}
		return &pgtest.FailingTx{Tx: tx, FailAt: 1}, nil // the listing of the pending releases
	}
	if _, err := w.RunOnce(context.Background()); !errors.Is(err, pgtest.ErrInjected) || !strings.Contains(err.Error(), "read queued release operations") {
		t.Fatalf("pending listing: got %v, want the injected failure", err)
	}
	w.Begin = nil
	s.leftForTheNextRound(t)
}

// leftForTheNextRound: a pending release whose steps fail is reported and
// left for the next round, which then takes it. That round is run here:
// the releaser and the dispatcher serve every tenant, so a release left
// queued would be taken by another test's round.
func (s *releaseScenario) leftForTheNextRound(t *testing.T) {
	t.Helper()
	e, a, w := s.e.in(t), s.a, s.w
	in := e.releaseAsked(t, s.run4)
	a.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errors.New("database down") }
	_, err := w.RunOnce(context.Background())
	pgtest.WantErr(t, err, "database down")
	a.Begin = nil
	if n := queuedReleases(t, e.Tenant.ID); n != 1 {
		t.Fatalf("this tenant's queued releases after the failed round: got %d, want 1", n)
	}
	if n, err := w.RunOnce(context.Background()); err != nil || n < 1 {
		t.Fatalf("next round: %d %v", n, err)
	}
	if n := queuedReleases(t, e.Tenant.ID); n != 0 {
		t.Fatalf("this tenant's queued releases after the next round: got %d, want 0", n)
	}
	// The holder is still blank, so the next round records the refused file.
	failed := e.get("/v1/operations/" + in.OperationID.String())
	if failed["status"] != "failed" {
		t.Fatalf("release after the next round, status: got %v, want failed", failed["status"])
	}
	if code := failed["error"].(map[string]any)["code"]; code != "payments.bank_file_invalid" {
		t.Fatalf("release after the next round, error: got %v, want payments.bank_file_invalid", code)
	}
}

// twoPaymentFailures: every statement of the release activities failing in
// turn is reported and leaves nothing half done; a run of two payments
// walks the loop.
func (s *releaseScenario) twoPaymentFailures(t *testing.T) {
	e, a, w := s.e.in(t), s.a, s.w
	e.setHolder(t, "Supplier")
	e.payableVendor(t)
	e.invoice(t, "INV-6", "11.00")
	e.vendorInvoice(t, "INV-7", "13.00")
	op5, run5 := e.propose(t, "2026-04-18")
	if err := execute(t, a, input(e, op5, run5), false); err != nil {
		t.Fatal(err)
	}
	if pays := e.get("/v1/payment-runs/" + run5 + "/payments"); len(pays["items"].([]any)) != 2 {
		t.Fatalf("two payments expected: %v", pays)
	}
	in5 := e.releaseAsked(t, run5)
	pool := pgtest.Pool(t)
	// The first posting failing stops the loop before the second, in the workflow and in the releaser alike.
	secondCall := func(onwards bool) func(context.Context, database.Scope) (pgx.Tx, error) { // the second transaction fails, once or (for the retrying workflow) from then on
		calls := 0
		return func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			if calls++; calls == 2 || onwards && calls > 2 {
				return nil, errors.New("database down")
			}
			return pool.Begin(ctx, s)
		}
	}
	a.Begin = secondCall(false)
	pgtest.WantErr(t, w.Release(context.Background(), in5), "database down")
	a.Begin = secondCall(true)
	pgtest.WantErr(t, executeRelease(t, a, in5), "database down")
	a.Now = func() time.Time { return time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC) } // the activities' clock, on the audit entries
	a.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		tx, err := pool.Begin(ctx, s)
		if err != nil || s.TenantID == uuid.Nil {
			return tx, err
		}
		return &pgtest.FailingTx{Tx: tx, FailCommit: true}, nil
	}
	if err := w.Release(context.Background(), in5); !errors.Is(err, pgtest.ErrInjected) || !strings.Contains(err.Error(), "commit") {
		t.Fatalf("failed commit: got %v, want the injected failure", err)
	}
	everyStatementFails(t, a, w, pool, in5)
	a.Begin = nil
	if got := e.get("/v1/payment-runs/" + run5); got["status"] != "released" {
		t.Fatalf("two-payment run after the failures: %v", got)
	}
}

// everyStatementFails releases in5 with its first, second, ... statement
// failing until the release goes through.
func everyStatementFails(t *testing.T, a *workflows.Payments, w *workflows.Releaser, pool *database.Pool, in5 workflows.ReleaseInput) {
	t.Helper()
	for n := 1; n < 120; n++ {
		count := 0
		a.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, s)
			if err != nil {
				return nil, err
			}
			return &pgtest.FailingTx{Tx: tx, FailAt: n, Count: &count}, nil
		}
		err := w.Release(context.Background(), in5)
		if err == nil {
			if n == 1 {
				t.Fatal("released without a statement to fail")
			}
			break
		}
		if !errors.Is(err, pgtest.ErrInjected) {
			t.Fatalf("statement %d: %v", n, err)
		}
	}
}

// payableVendor gives the fixture's second supplier a bank account.
func (e env) payableVendor(t *testing.T) {
	t.Helper()
	change, id := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	iban := "DE02120300000000202051"
	pgtest.Exec(t, `INSERT INTO partner.partner_change (tenant_id, id, number, kind, partner_id, payload, reason, status, requested_by, applied_at,
		created_by, updated_by) VALUES ($1, $2, $3, 'create', $4, '{}', 'test', 'applied', $5, now(), $5, $5)`, e.Tenant.ID, change, change.String(), e.Vendor, e.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO partner.bank_account (tenant_id, id, partner_id, account_cipher, account_fingerprint, account_last4, account_scheme, bic,
		bank_country_code, holder_name, valid_from, valid_to, partner_change_id, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, 'iban', 'COBADEFFXXX', 'DE', 'Vendor', '2026-01-01', NULL, $7, $8)`, e.Tenant.ID, id, e.Vendor, apitest.Seal(t, e.Tenant.ID, iban),
		apitest.Fingerprint(t, e.Tenant.ID, iban), iban[len(iban)-4:], change, e.Tenant.Human.ID)
}

// vendorInvoice posts an invoice of the second supplier.
func (e env) vendorInvoice(t *testing.T, ref, gross string) {
	t.Helper()
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "supplier_reference": %q,
		"invoice_date": "2026-03-01", "gross_amount": {"amount": %q, "currency": "EUR"}, "tax_amount": {"amount": "0.00", "currency": "EUR"},
		"items": [{"account_id": %q, "description": "Fees", "quantity": {"value": "1", "unit": "EA"}, "amount": {"amount": %q, "currency": "EUR"}}]}`,
		e.DE, e.Vendor, ref, gross, e.expense, gross), "")
	e.Must(200, "POST", "/v1/supplier-invoices/"+id(inv)+":post", `{"posting_date": "2026-03-01"}`, `"1"`)
}

// simulationAsked brings a tier 3 change request to awaiting evidence and
// asks for its simulation; it returns the operation and the request's path.
func simulationAsked(t *testing.T) (env, workflows.SimulateInput, string) {
	t.Helper()
	f := fixture.New(t, "config.change.read", "config.change.propose", "config.change.activate", "config.version.read", "platform.operation.read", "platform.operation.cancel")
	e := env{Env: f}
	doc := fixture.Document(t)
	doc.Companies[0].TaxCodes[0].RatePercent = "9" // posting-relevant: tier 3, which needs simulation evidence
	body, _ := json.Marshal(doc)
	cr, _ := e.Must(201, "POST", "/v1/change-requests", fmt.Sprintf(`{"title": "Tax rate", "base_version": 1, "proposed": %s}`, body), "")
	path := "/v1/change-requests/" + id(cr)
	_, tag := e.Must(200, "GET", path, "", "")
	if submitted, _ := e.Must(200, "POST", path+":submit", "", tag); submitted["status"] != "awaiting_evidence" {
		t.Fatalf("submitted: %v", submitted)
	}
	_, tag = e.Must(200, "GET", path, "", "")
	op, _ := e.Must(202, "POST", path+":simulate", "", tag)
	return e, workflows.SimulateInput{TenantID: e.Tenant.ID, OperationID: uuid.MustParse(id(op))}, path
}

func simulations(t *testing.T) *workflows.Simulations {
	t.Helper()
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	return &workflows.Simulations{Worker: &configuration.SimulationWorker{Dependencies: app.Configuration(app.Config{Pages: pages, FieldKeys: apitest.FieldKeys()}),
		Pool: pgtest.Pool(t), AppRole: "erp_app", WorkerRole: "erp_worker"}}
}

// TestSimulationWorkflow: a change request's simulation runs as a workflow
// whose activity is the simulation worker's unit of work; an operation no
// longer queued is left alone; failures are reported.
func TestSimulationWorkflow(t *testing.T) {
	e, in, path := simulationAsked(t)
	s := simulations(t)
	var suite testsuite.WorkflowTestSuite
	run := func() error {
		env := workflowEnv(&suite)
		env.RegisterWorkflow(workflows.SimulateChange)
		env.RegisterActivity(s)
		env.ExecuteWorkflow(workflows.SimulateChange, in)
		return env.GetWorkflowError()
	}
	if err := run(); err != nil {
		t.Fatal(err)
	}
	done := e.get("/v1/operations/" + in.OperationID.String())
	cr := e.get(path)
	if done["status"] != "succeeded" {
		t.Fatalf("simulated operation status: got %v, want succeeded: %v", done["status"], cr)
	}
	result := done["result"].(map[string]any)
	if result["kind"] != "simulation" {
		t.Fatalf("simulated result kind: got %v, want simulation", result["kind"])
	}
	if result["passed"] != true {
		t.Fatalf("simulated result passed: got %v, want true", result["passed"])
	}
	if err := run(); err != nil { // nothing left to do
		t.Fatal(err)
	}
	if again := e.get(path); again["version"] != cr["version"] {
		t.Fatalf("simulated twice: %v", again)
	}
	s.Worker.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errors.New("database down") }
	pgtest.WantErr(t, s.Simulate(context.Background(), in), "database down")
	// Dispatched under the request's number and attempt; a cancelled attempt is followed by the next.
	e2, in2, path2 := simulationAsked(t)
	engine := &fakeEngine{}
	d := &workflows.Dispatcher{Pool: pgtest.Pool(t), WorkerRole: "erp_worker", Client: engine, Queue: "erp-test", Log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))}
	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var number string
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT number FROM config.change_request WHERE id = $1`, uuid.MustParse(strings.TrimPrefix(path2, "/v1/change-requests/"))).Scan(&number); err != nil {
		t.Fatalf("read the change request number: %v", err)
	}
	found := false
	for _, st := range engine.started {
		found = found || st.ID == workflows.SimulateWorkflowID(e2.Tenant.ID, number, 1) && st.Name == workflows.SimulateWorkflow && st.Args[0].(workflows.SimulateInput) == in2
	}
	if !found {
		t.Fatalf("simulation start: %+v", engine.started)
	}
	e2.Must(200, "POST", "/v1/operations/"+in2.OperationID.String()+":cancel", "", "*") // the dispatch recorded the workflow id, so the version moved
	_, tag := e2.Must(200, "GET", path2, "", "")
	op3, _ := e2.Must(202, "POST", path2+":simulate", "", tag)
	if n, err := d.RunOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("second attempt dispatch: %d %v", n, err)
	}
	if last := engine.started[len(engine.started)-1]; last.ID != workflows.SimulateWorkflowID(e2.Tenant.ID, number, 2) || last.Args[0].(workflows.SimulateInput).OperationID != uuid.MustParse(id(op3)) {
		t.Fatalf("second attempt: %+v", last)
	}
}

// migrationAsked sets up a tenant with a file connector, asks for a run's
// start through an API that hands runs to a workflow engine, and returns
// the environment, the run's path and the workflow input.
func migrationAsked(t *testing.T, file string) (env, string, workflows.MigrateInput, *workflows.Migrations) {
	t.Helper()
	f := fixture.New(t, "integration.connector.read", "integration.connector.manage", "integration.migration.read", "integration.migration.run",
		"platform.operation.read", "ledger.entry.read", "ledger.entry.post", "payables.invoice.read", "procurement.order.read")
	dir := t.TempDir()
	if err := sap.WriteExtract(filepath.Join(dir, "extract.json"), sap.GenerateMock(42, "2026")); err != nil {
		t.Fatal(err)
	}
	f.API = apitest.NewWith(t, func(c *app.Config) { c.ExtractDir = dir; c.Workflows = &fakeEngine{} })
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2025-01-01', 0.9, 'ECB', $2)`, f.Tenant.ID, f.Tenant.Human.ID)
	e := env{Env: f}
	c, _ := e.Must(201, "POST", "/v1/connectors", fmt.Sprintf(`{"kind": "sap_file", "name": "SAP", "settings": {"path": %q}}`, file), "")
	r, _ := e.Must(201, "POST", "/v1/migration-runs", fmt.Sprintf(`{"connector_id": %q, "scope": {"legal_entity_ids": [%q], "fiscal_year": 2026}, "mode": "documents", "is_rehearsal": false}`, id(c), e.US), "")
	path := "/v1/migration-runs/" + id(r)
	_, tag := e.Must(200, "GET", path, "", "")
	op, _ := e.Must(202, "POST", path+":start", "", tag)
	if op["status"] != "queued" {
		t.Fatalf("start with an engine: %v", op)
	}
	if running := e.get(path); running["status"] != "running" || running["operation_id"] != id(op) {
		t.Fatalf("run handed to the engine: %v", running)
	}
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	m := &workflows.Migrations{Pool: pgtest.Pool(t), AppRole: "erp_app", Actors: identity.Actors{},
		Endpoints: app.Integration(app.Config{Pages: pages, FieldKeys: apitest.FieldKeys(), ExtractDir: dir, Workflows: &fakeEngine{}})}
	return e, path, workflows.MigrateInput{TenantID: e.Tenant.ID, RunID: uuid.MustParse(id(r)), OperationID: uuid.MustParse(id(op))}, m
}

func executeMigration(t *testing.T, m *workflows.Migrations, in workflows.MigrateInput) error {
	t.Helper()
	var s testsuite.WorkflowTestSuite
	env := workflowEnv(&s)
	env.RegisterWorkflow(workflows.MigrateRun)
	env.RegisterActivity(m)
	env.ExecuteWorkflow(workflows.MigrateRun, in)
	return env.GetWorkflowError()
}

// TestMigrationWorkflow: with an engine a run's start and reconciliation
// are workflows whose activity does the integration context's work; a run
// whose extract cannot be read goes back to queued with the problem on its
// operation; an operation already done is left alone; failures are reported.
func TestMigrationWorkflow(t *testing.T) {
	s := &migrationScenario{engine: &fakeEngine{}}
	s.e, s.path, s.in, s.m = migrationAsked(t, "extract.json")
	// The second run and the dispatcher are the test's, not one step's:
	// what they open is closed when the test that opened them ends.
	s.e3, s.path3, s.in3, s.m3 = migrationAsked(t, "nowhere.json")
	s.d = &workflows.Dispatcher{Pool: pgtest.Pool(t), WorkerRole: "erp_worker", Client: s.engine, Queue: "erp-test", Log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))}
	// Each step goes on from the runs and operations the ones before it
	// left, so the first failure stops the rest.
	if !t.Run("the start loads the run", s.load) {
		t.FailNow()
	}
	if !t.Run("reconciliation runs the same way", s.reconcile) {
		t.FailNow()
	}
	if !t.Run("a start is dispatched by its run, kind and attempt", s.dispatch) {
		t.FailNow()
	}
	if !t.Run("an unreadable extract fails the operation and queues the run", s.unreadableExtract) {
		t.FailNow()
	}
	if !t.Run("a second start is the next attempt", s.startAgain) {
		t.FailNow()
	}
	if !t.Run("the machinery failing is reported", s.machineryFails) {
		t.FailNow()
	}
	t.Run("every failing statement is reported", s.everyStatementFails)
}

// migrationScenario is what the steps of TestMigrationWorkflow share: the
// run that loads (e, path, in, m) and its reconciliation (in2), the run
// whose extract is missing (e3, path3, in3, m3) and its second start
// (again), and the dispatcher with its engine.
type migrationScenario struct {
	e, e3       env
	path, path3 string
	in, in2     workflows.MigrateInput
	in3         workflows.MigrateInput
	m, m3       *workflows.Migrations
	engine      *fakeEngine
	d           *workflows.Dispatcher
	again       map[string]any
}

// load: the workflow loads the run and the operation succeeds; run again,
// the operation is done and nothing happens.
func (s *migrationScenario) load(t *testing.T) {
	e := s.e.in(t)
	if err := executeMigration(t, s.m, s.in); err != nil {
		t.Fatal(err)
	}
	loaded := e.get(s.path)
	done := e.get("/v1/operations/" + s.in.OperationID.String())
	if loaded["status"] != "loaded" {
		t.Fatalf("run status: got %v, want loaded", loaded["status"])
	}
	if done["status"] != "succeeded" {
		t.Fatalf("operation status: got %v, want succeeded", done["status"])
	}
	if n := loaded["report"].(map[string]any)["loaded"].(map[string]any)["journal_entries"]; n != float64(43) {
		t.Fatalf("loaded journal entries: got %v, want 43", n)
	}
	if err := executeMigration(t, s.m, s.in); err != nil { // the operation is done: nothing happens
		t.Fatal(err)
	}
}

// reconcile: the loaded run's reconciliation is a workflow too.
func (s *migrationScenario) reconcile(t *testing.T) {
	e := s.e.in(t)
	_, tag := e.Must(200, "GET", s.path, "", "")
	rec, _ := e.Must(202, "POST", s.path+":reconcile", "", tag) // the operation's ETag: nothing writes to it
	s.in2 = workflows.MigrateInput{TenantID: e.Tenant.ID, RunID: s.in.RunID, OperationID: uuid.MustParse(id(rec))}
	if err := executeMigration(t, s.m, s.in2); err != nil {
		t.Fatal(err)
	}
	if got := e.get(s.path); got["status"] != "reconciled" {
		t.Fatalf("reconciled by the workflow: %v", got)
	}
}

// dispatch: a queued start is dispatched under the run, the operation's
// kind and its attempt.
func (s *migrationScenario) dispatch(t *testing.T) {
	if n, err := s.d.RunOnce(context.Background()); err != nil || n < 1 {
		t.Fatalf("dispatch: %d %v", n, err)
	}
	found := false
	for _, st := range s.engine.started {
		found = found || st.ID == workflows.MigrateWorkflowID(s.e3.Tenant.ID, s.in3.RunID, "integration.migrate", 1) && st.Name == workflows.MigrateWorkflow && st.Args[0].(workflows.MigrateInput) == s.in3
	}
	if !found {
		t.Fatalf("migration start: %+v", s.engine.started)
	}
}

// unreadableExtract: the extract cannot be read, so the operation fails
// with the problem and the run is queued again.
func (s *migrationScenario) unreadableExtract(t *testing.T) {
	e3 := s.e3.in(t)
	if err := executeMigration(t, s.m3, s.in3); err != nil {
		t.Fatal(err)
	}
	failed := e3.get("/v1/operations/" + s.in3.OperationID.String())
	back := e3.get(s.path3)
	if failed["status"] != "failed" {
		t.Fatalf("unreadable extract operation status: got %v, want failed", failed["status"])
	}
	if code := failed["error"].(map[string]any)["code"]; code != "platform.unavailable" {
		t.Fatalf("unreadable extract operation error: got %v, want platform.unavailable", code)
	}
	if back["status"] != "queued" {
		t.Fatalf("unreadable extract run status: got %v, want queued", back["status"])
	}
}

// startAgain: the queued run started again is dispatched as the next
// attempt.
func (s *migrationScenario) startAgain(t *testing.T) {
	e3 := s.e3.in(t)
	_, tag := e3.Must(200, "GET", s.path3, "", "")
	s.again, _ = e3.Must(202, "POST", s.path3+":start", "", tag) // the operation's ETag: nothing writes to it
	if n, err := s.d.RunOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("second dispatch: %d %v", n, err)
	}
	if last := s.engine.started[len(s.engine.started)-1]; last.ID != workflows.MigrateWorkflowID(e3.Tenant.ID, s.in3.RunID, "integration.migrate", 2) || last.Args[0].(workflows.MigrateInput).OperationID != uuid.MustParse(id(s.again)) {
		t.Fatalf("second attempt: %+v", last)
	}
}

// machineryFails: a failing begin is reported; a finished operation is
// nothing to do; an unknown operation is refused.
func (s *migrationScenario) machineryFails(t *testing.T) {
	s.m3.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errors.New("database down") }
	pgtest.WantErr(t, s.m3.Execute(context.Background(), s.in3), "database down")
	s.m3.Begin = nil
	if err := s.m.Execute(context.Background(), workflows.MigrateInput{TenantID: s.e.Tenant.ID, RunID: s.in.RunID, OperationID: s.in2.OperationID}); err != nil {
		t.Fatalf("executing a finished run does nothing: %v", err)
	}
	if err := s.m.Execute(context.Background(), workflows.MigrateInput{TenantID: s.e.Tenant.ID, RunID: s.in.RunID, OperationID: uuid.New()}); err == nil {
		t.Fatal("unknown operation")
	}
}

// everyStatementFails: the second start's commit and then each of its
// statements failing in turn is reported, until the start goes through.
func (s *migrationScenario) everyStatementFails(t *testing.T) {
	m3, pool := s.m3, pgtest.Pool(t)
	defer func() { m3.Begin = nil }()
	in4 := workflows.MigrateInput{TenantID: s.e3.Tenant.ID, RunID: s.in3.RunID, OperationID: uuid.MustParse(id(s.again))}
	m3.Now = func() time.Time { return time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC) }
	m3.Begin = func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
		tx, err := pool.Begin(ctx, scope)
		if err != nil || scope.TenantID == uuid.Nil {
			return tx, err
		}
		return &pgtest.FailingTx{Tx: tx, FailCommit: true}, nil
	}
	if err := m3.Execute(context.Background(), in4); !errors.Is(err, pgtest.ErrInjected) || !strings.Contains(err.Error(), "commit") {
		t.Fatalf("failed commit: got %v, want the injected failure", err)
	}
	for n := 1; n < 60; n++ {
		count := 0
		m3.Begin = func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, scope)
			if err != nil {
				return nil, err
			}
			return &pgtest.FailingTx{Tx: tx, FailAt: n, Count: &count}, nil
		}
		err := m3.Execute(context.Background(), in4)
		if err == nil {
			if n == 1 {
				t.Fatal("executed without a statement to fail")
			}
			break
		}
		if !errors.Is(err, pgtest.ErrInjected) {
			t.Fatalf("statement %d: %v", n, err)
		}
	}
}

// approvalPending submits a requisition that needs approval and returns the
// environment and its pending approval request.
func approvalPending(t *testing.T) (env, map[string]any, string) {
	t.Helper()
	f := fixture.New(t, "procurement.requisition.read", "procurement.requisition.create")
	e := env{Env: f}
	office := f.Account(t, "2000", "520000")
	req, _ := e.Must(201, "POST", "/v1/requisitions", fmt.Sprintf(`{"legal_entity_id": %q, "currency": "EUR", "supplier_id": %q, "items": [{"description": "Paper",
		"quantity": {"value": "1", "unit": "EA"}, "unit_price": {"amount": "1.00", "currency": "EUR"}, "account_id": %q}]}`, e.DE, e.Supplier, office), "")
	_, tag := e.Must(200, "GET", "/v1/requisitions/"+id(req), "", "")
	e.Must(200, "POST", "/v1/requisitions/"+id(req)+":submit", "", tag)
	return e, e.Pending(t, id(req)), id(req)
}

// TestApprovalDeadline: a pending request is escalated each period up to
// the limit, with the event and audit entry, as a system actor; a decided
// request ends the workflow at the next check; the dispatcher watches every
// pending request once; failures are reported.
func TestApprovalDeadline(t *testing.T) {
	s := &deadlineScenario{engine: &fakeEngine{}}
	var pending map[string]any
	s.e, pending, s.reqID = approvalPending(t)
	s.a = &workflows.Approvals{Pool: pgtest.Pool(t), AppRole: "erp_app"}
	s.in = workflows.DeadlineInput{TenantID: s.e.Tenant.ID, RequestID: uuid.MustParse(id(pending)), After: 48 * time.Hour, Max: 2}
	// The second request and the dispatcher are the test's, not one step's:
	// what they open is closed when the test that opened them ends.
	s.e2, s.pending2, _ = approvalPending(t) // its requisition is not needed: the dispatcher watches the request
	s.d = &workflows.Dispatcher{Deadline: time.Hour, Escalations: 3, Pool: pgtest.Pool(t), WorkerRole: "erp_worker", Client: s.engine, Queue: "erp-test", Log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))}
	// Each step goes on from the requests the ones before it escalated,
	// decided or watched, so the first failure stops the rest.
	if !t.Run("a pending request is escalated up to the limit", s.escalate) {
		t.FailNow()
	}
	if !t.Run("a decided request ends the workflow", s.decided) {
		t.FailNow()
	}
	if !t.Run("the dispatcher watches each pending request once", s.dispatch) {
		t.FailNow()
	}
	if !t.Run("the activity's database failures are reported", s.activityFailures) {
		t.FailNow()
	}
	if !t.Run("the dispatcher's database failures are reported", s.dispatcherFailures) {
		t.FailNow()
	}
	t.Run("a failing activity ends the workflow", s.failingWorkflow)
}

// deadlineScenario is what the steps of TestApprovalDeadline share: the
// request the workflow escalates (e, reqID, in) and its activities, the
// request the dispatcher watches (e2, pending2), and the dispatcher with
// its engine.
type deadlineScenario struct {
	e, e2    env
	reqID    string
	pending2 map[string]any
	in       workflows.DeadlineInput
	a        *workflows.Approvals
	suite    testsuite.WorkflowTestSuite
	engine   *fakeEngine
	d        *workflows.Dispatcher
}

// run executes the deadline workflow for in with the real activities.
func (s *deadlineScenario) run(in workflows.DeadlineInput) error {
	env := workflowEnv(&s.suite)
	env.RegisterWorkflow(workflows.ApprovalDeadline)
	env.RegisterActivity(s.a)
	env.ExecuteWorkflow(workflows.ApprovalDeadline, in)
	return env.GetWorkflowError()
}

// escalate: the workflow escalates the pending request each period up to
// the limit, with an event each time and the audit entry of a system actor.
func (s *deadlineScenario) escalate(t *testing.T) {
	e := s.e.in(t)
	if err := s.run(s.in); err != nil {
		t.Fatal(err)
	}
	got := e.get("/v1/approval-requests/" + s.in.RequestID.String())
	ev := apitest.Published(t, e.Tenant, outbox.Event{Context: "workflow", Name: "approval_escalated"})
	if got["escalations"] != float64(2) {
		t.Errorf("escalations: got %v, want 2", got["escalations"])
	}
	if got["escalated_at"] == nil {
		t.Errorf("escalated_at: got nil, want a time")
	}
	if got["status"] != "pending" {
		t.Errorf("status: got %v, want pending", got["status"])
	}
	if len(ev) != 2 {
		t.Fatalf("approval_escalated events: got %d, want 2: %v", len(ev), ev)
	}
	if ev[1]["escalations"] != float64(2) {
		t.Errorf("second event escalations: got %v, want 2", ev[1]["escalations"])
	}
	if ev[0]["current_step"] != float64(1) {
		t.Errorf("first event current_step: got %v, want 1", ev[0]["current_step"])
	}
	var actor uuid.UUID
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT actor_id FROM audit.audit_event WHERE tenant_id = $1 AND action = 'workflow.approval_request.escalate' LIMIT 1`, e.Tenant.ID).Scan(&actor); err != nil {
		t.Fatalf("read the escalating actor: %v", err)
	}
	if actor != approvals.EscalatorActor {
		t.Fatalf("escalated by %v", actor)
	}
}

// decided: once the request is decided, the next check ends the workflow
// without an escalation.
func (s *deadlineScenario) decided(t *testing.T) {
	e := s.e.in(t)
	e.Approve(t, s.reqID)
	if err := s.run(s.in); err != nil {
		t.Fatal(err)
	}
	if got := e.get("/v1/approval-requests/" + s.in.RequestID.String()); got["escalations"] != float64(2) || got["status"] != "approved" {
		t.Fatalf("after the decision: %v", got)
	}
}

// dispatch: the dispatcher starts one deadline workflow per pending request
// and records it; a refused start is retried; a watched request is not
// watched twice.
func (s *deadlineScenario) dispatch(t *testing.T) {
	d, engine := s.d, s.engine
	engine.fail = id(s.pending2)
	_, err := d.RunOnce(context.Background())
	pgtest.WantErr(t, err, "engine refused")
	engine.fail = ""
	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := workflows.DeadlineWorkflowID(s.e2.Tenant.ID, uuid.MustParse(id(s.pending2)))
	found := false
	for _, st := range engine.started {
		in := st.Args[0]
		found = found || st.ID == want && st.Name == workflows.DeadlineWorkflow && in.(workflows.DeadlineInput).After == time.Hour && in.(workflows.DeadlineInput).Max == 3
	}
	var recorded *string
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT workflow_id FROM workflow.approval_request WHERE id = $1`, uuid.MustParse(id(s.pending2))).Scan(&recorded); err != nil {
		t.Fatalf("read the recorded workflow id: %v", err)
	}
	if !found {
		t.Fatalf("deadline dispatch: got starts %+v, want %s", engine.started, want)
	}
	if recorded == nil || *recorded != want {
		t.Fatalf("deadline dispatch recorded: got %v, want %s", recorded, want)
	}
	before := len(engine.started)
	if _, err := d.RunOnce(context.Background()); err != nil || len(engine.started) != before {
		t.Fatalf("watched twice: %v %d", err, len(engine.started))
	}
}

// activityFailures: a failing begin and each failing statement of the
// escalation activity are reported. The activities' Begin is left unset.
func (s *deadlineScenario) activityFailures(t *testing.T) {
	a, pool := s.a, pgtest.Pool(t)
	defer func() { a.Begin = nil }()
	a.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errors.New("database down") }
	_, err := a.Escalate(context.Background(), workflows.EscalateInput{DeadlineInput: s.in})
	pgtest.WantErr(t, err, "database down")
	in2 := workflows.DeadlineInput{TenantID: s.e2.Tenant.ID, RequestID: uuid.MustParse(id(s.pending2)), After: time.Hour, Max: 1}
	a.Now = func() time.Time { return time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC) }
	for n := 1; n < 30; n++ {
		count := 0
		a.Begin = func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, scope)
			if err != nil {
				return nil, err
			}
			return &pgtest.FailingTx{Tx: tx, FailAt: n, Count: &count}, nil
		}
		_, err := a.Escalate(context.Background(), workflows.EscalateInput{DeadlineInput: in2, Prior: new(int16)})
		if err == nil {
			if n == 1 {
				t.Fatal("escalated without a statement to fail")
			}
			break
		}
		if !errors.Is(err, pgtest.ErrInjected) {
			t.Fatalf("statement %d: %v", n, err)
		}
	}
}

// dispatcherFailures: with a new pending request to watch, each failing
// statement of the dispatcher's round is reported. Its Begin is left unset.
func (s *deadlineScenario) dispatcherFailures(t *testing.T) {
	d, pool := s.d, pgtest.Pool(t)
	defer func() { d.Begin = nil }()
	approvalPending(t) // only its request is needed: the round below watches it
	for n := 1; n < 10; n++ {
		count := 0
		d.Begin = func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, scope)
			if err != nil {
				return nil, err
			}
			return &pgtest.FailingTx{Tx: tx, FailAt: n, Count: &count}, nil
		}
		_, err := d.RunOnce(context.Background())
		if err == nil {
			if n == 1 {
				t.Fatal("dispatched without a statement to fail")
			}
			break
		}
		if !errors.Is(err, pgtest.ErrInjected) {
			t.Fatalf("dispatcher statement %d: %v", n, err)
		}
	}
}

// failingWorkflow: the activity's error ends the workflow.
func (s *deadlineScenario) failingWorkflow(t *testing.T) {
	s.a.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errors.New("database down") }
	t.Cleanup(func() { s.a.Begin = nil })
	if err := s.run(workflows.DeadlineInput{TenantID: s.e.Tenant.ID, RequestID: s.in.RequestID, After: time.Minute, Max: 1}); err == nil {
		t.Fatal("workflow with a failing activity succeeded")
	}
}

// TestEscalateTwice: the escalation activity run twice with the same input,
// as Temporal retries it after a commit whose completion was lost, escalates
// the request once, with one event and one audit entry (ADR-0008, ADR-0012).
func TestEscalateTwice(t *testing.T) {
	e, pending, _ := approvalPending(t)
	a := &workflows.Approvals{Pool: pgtest.Pool(t), AppRole: "erp_app"}
	in := workflows.EscalateInput{DeadlineInput: workflows.DeadlineInput{TenantID: e.Tenant.ID, RequestID: uuid.MustParse(id(pending)), After: time.Hour, Max: 3},
		Prior: new(int16)}
	for run := 1; run <= 2; run++ {
		escalated, err := a.Escalate(context.Background(), in)
		if err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		if !escalated {
			t.Fatalf("run %d: got pending false, want true", run)
		}
	}
	got := e.get("/v1/approval-requests/" + id(pending))
	if got["escalations"] != float64(1) {
		t.Errorf("escalations: got %v, want 1", got["escalations"])
	}
	if ev := apitest.Published(t, e.Tenant, outbox.Event{Context: "workflow", Name: "approval_escalated"}); len(ev) != 1 {
		t.Errorf("approval_escalated events: got %d, want 1", len(ev))
	}
	if n := escalationAudits(t, e.Tenant.ID); n != 1 {
		t.Errorf("audit entries: got %d, want 1", n)
	}
}

// TestApprovalDeadlineReplay: a history recorded under the first version
// of the escalation loop (testdata, recorded from the T29 code at 18c4f10 on
// a development server: escalated once, then found decided) replays on the
// current code without a nondeterminism error.
func TestApprovalDeadlineReplay(t *testing.T) {
	r := worker.NewWorkflowReplayer()
	r.RegisterWorkflowWithOptions(workflows.ApprovalDeadline, sdkworkflow.RegisterOptions{Name: workflows.DeadlineWorkflow})
	if err := r.ReplayWorkflowHistoryFromJSONFile(nil, filepath.Join("testdata", "approval_deadline_v1.json")); err != nil {
		t.Fatalf("replay of the version 1 history: %v", err)
	}
}

// TestReleasePaymentRunReplay: histories of ReleasePaymentRun recorded from
// the code before R69 moved its body into releaseSteps.run (testdata,
// recorded from 9ab06f8 on a development server with stand-in activities)
// replay on the current code without a nondeterminism error, so the steps
// issue the same commands in the same order: a release of two payments,
// and one whose second posting fails with a business problem and goes to
// FailRelease.
func TestReleasePaymentRunReplay(t *testing.T) {
	for _, file := range []string{"release_payment_run_v1.json", "release_payment_run_problem_v1.json"} {
		t.Run(file, func(t *testing.T) {
			r := worker.NewWorkflowReplayer()
			r.RegisterWorkflowWithOptions(workflows.ReleasePaymentRun, sdkworkflow.RegisterOptions{Name: workflows.ReleaseWorkflow})
			if err := r.ReplayWorkflowHistoryFromJSONFile(nil, filepath.Join("testdata", file)); err != nil {
				t.Fatalf("replay of %s: %v", file, err)
			}
		})
	}
}

// TestApprovalDeadlineVersions: histories without the version marker and
// those recorded under version 1 escalate without the prior count, as they
// did; version 2, the one new workflows record, passes it.
func TestApprovalDeadlineVersions(t *testing.T) {
	cases := []struct {
		name    string
		version sdkworkflow.Version
		guarded bool
	}{
		{"no marker", sdkworkflow.DefaultVersion, false},
		{"version 1", 1, false},
		{"version 2", 2, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var s testsuite.WorkflowTestSuite
			env := workflowEnv(&s)
			env.RegisterWorkflow(workflows.ApprovalDeadline)
			var got []*int16
			env.RegisterActivityWithOptions(func(_ context.Context, in workflows.EscalateInput) (bool, error) {
				got = append(got, in.Prior)
				return len(got) < 2, nil // escalated once, then decided
			}, activity.RegisterOptions{Name: "Escalate"})
			env.OnGetVersion("escalation-loop", sdkworkflow.DefaultVersion, 2).Return(c.version)
			env.ExecuteWorkflow(workflows.ApprovalDeadline, workflows.DeadlineInput{TenantID: uuid.New(), RequestID: uuid.New(), After: time.Hour, Max: 3})
			if err := env.GetWorkflowError(); err != nil {
				t.Fatal(err)
			}
			if len(got) != 2 {
				t.Fatalf("activity runs: got %d, want 2", len(got))
			}
			for i, prior := range got {
				switch {
				case !c.guarded && prior != nil:
					t.Errorf("run %d: got prior %d, want none", i, *prior)
				case c.guarded && prior == nil:
					t.Errorf("run %d: got no prior, want %d", i, i)
				case c.guarded && int(*prior) != i:
					t.Errorf("run %d: got prior %d, want %d", i, *prior, i)
				}
			}
		})
	}
}

// escalationAudits counts the tenant's escalation audit entries.
func escalationAudits(t *testing.T, tenant uuid.UUID) int {
	t.Helper()
	var n int
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'workflow.approval_request.escalate'`, tenant).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestEscalateGuard: an activity scheduled before the guard (no prior)
// escalates unguarded as it did; a pending request with fewer escalations
// than the workflow counted is an error; a request the scope cannot see
// ends the workflow; a failure reading the request after the guard refused
// is reported.
func TestEscalateGuard(t *testing.T) {
	e, pending, _ := approvalPending(t)
	a := &workflows.Approvals{Pool: pgtest.Pool(t), AppRole: "erp_app"}
	in := workflows.DeadlineInput{TenantID: e.Tenant.ID, RequestID: uuid.MustParse(id(pending)), After: time.Hour, Max: 3}
	// The subtests share the request and run in order: "workflow ahead of
	// the request" and "read fails" rely on the two escalations made here,
	// so its failure stops the rest.
	if !t.Run("no prior", func(t *testing.T) {
		for run := 1; run <= 2; run++ {
			if escalated, err := a.Escalate(context.Background(), workflows.EscalateInput{DeadlineInput: in}); err != nil || !escalated {
				t.Fatalf("run %d: got %v, %v, want true, nil", run, escalated, err)
			}
		}
		if n := escalationAudits(t, e.Tenant.ID); n != 2 {
			t.Errorf("audit entries: got %d, want 2", n)
		}
	}) {
		t.FailNow()
	}
	t.Run("workflow ahead of the request", func(t *testing.T) {
		ahead := int16(5)
		_, err := a.Escalate(context.Background(), workflows.EscalateInput{DeadlineInput: in, Prior: &ahead})
		if !errors.Is(err, approvals.ErrEscalationCount) {
			t.Fatalf("got %v, want ErrEscalationCount", err)
		}
		var app *temporal.ApplicationError
		if !errors.As(err, &app) || !app.NonRetryable() {
			t.Errorf("got %v, want a non-retryable application error", err)
		}
	})
	t.Run("failed commit", func(t *testing.T) {
		e2, pending2, _ := approvalPending(t)
		pool := pgtest.Pool(t)
		failing := &workflows.Approvals{AppRole: "erp_app", Begin: func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, s)
			if err != nil {
				return nil, err
			}
			return &pgtest.FailingTx{Tx: tx, FailCommit: true}, nil
		}}
		in2 := workflows.DeadlineInput{TenantID: e2.Tenant.ID, RequestID: uuid.MustParse(id(pending2)), After: time.Hour, Max: 3}
		_, err := failing.Escalate(context.Background(), workflows.EscalateInput{DeadlineInput: in2, Prior: new(int16)})
		if !errors.Is(err, pgtest.ErrInjected) || !strings.Contains(err.Error(), "commit") {
			t.Errorf("got %v, want the injected failure", err)
		}
	})
	t.Run("request not visible", func(t *testing.T) {
		other := in
		other.TenantID = pgtest.NewTenant(t).ID // RLS hides the request from another tenant's scope
		if escalated, err := a.Escalate(context.Background(), workflows.EscalateInput{DeadlineInput: other, Prior: new(int16)}); err != nil || escalated {
			t.Errorf("got %v, %v, want false, nil", escalated, err)
		}
	})
	t.Run("read fails", func(t *testing.T) {
		pool := pgtest.Pool(t)
		count := 0
		a := &workflows.Approvals{AppRole: "erp_app", Begin: func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, s)
			if err != nil {
				return nil, err
			}
			return &pgtest.FailingTx{Tx: tx, FailAt: 4, Count: &count}, nil // tenant status, set actor, guarded update, read
		}}
		stale := int16(0)
		if _, err := a.Escalate(context.Background(), workflows.EscalateInput{DeadlineInput: in, Prior: &stale}); !errors.Is(err, pgtest.ErrInjected) {
			t.Errorf("got %v, want the injected failure", err)
		}
	})
}

// TestApprovalDeadlineCancelled: a deadline workflow cancelled while it
// waits ends as cancelled, its error wrapped with the request.
func TestApprovalDeadlineCancelled(t *testing.T) {
	var s testsuite.WorkflowTestSuite
	env := workflowEnv(&s)
	env.RegisterWorkflow(workflows.ApprovalDeadline)
	env.RegisterDelayedCallback(env.CancelWorkflow, time.Minute)
	request := uuid.New()
	env.ExecuteWorkflow(workflows.ApprovalDeadline, workflows.DeadlineInput{TenantID: uuid.New(), RequestID: request, After: time.Hour, Max: 3})
	err := env.GetWorkflowError()
	if !temporal.IsCanceledError(err) {
		t.Fatalf("got %v, want the workflow cancelled", err)
	}
	if !strings.Contains(err.Error(), "approval deadline of request "+request.String()) {
		t.Errorf("got %q, want the request named", err.Error())
	}
}

// TestMigrationRequesterBlocked: a run whose requester is blocked after
// the start (PO 2026-11-03) loads nothing: the activity fails the
// operation with identity.principal_blocked, audited, and puts the run
// back to queued, to be started again by someone active; the workflow
// ends without an error, so the refusal is not retried, and the activity
// run again does nothing.
func TestMigrationRequesterBlocked(t *testing.T) {
	e, _, in, m := migrationAsked(t, "extract.json")
	pgtest.Exec(t, `UPDATE identity.principal SET status = 'blocked', version = version + 1 WHERE id = $1`, e.Tenant.Human.ID)
	if err := executeMigration(t, m, in); err != nil {
		t.Fatalf("the workflow: got %v, want it ended without an error", err)
	}
	state := func() string {
		var s string
		pgtest.Scan(t, `SELECT r.status || ' ' || o.status || ' ' || coalesce(o.error->>'code', '') || ' ' ||
				(SELECT count(*) FROM ledger.journal_entry j WHERE j.tenant_id = r.tenant_id) || ' ' ||
				(SELECT count(*) FROM audit.audit_event a WHERE a.tenant_id = r.tenant_id AND a.action = 'integration.migration_run.fail' AND a.object_id = r.id)
			FROM integration.migration_run r JOIN platform.operation o ON o.id = $2 WHERE r.id = $1`, []any{in.RunID, in.OperationID}, &s)
		return s
	}
	if got := state(); got != "queued failed identity.principal_blocked 0 1" {
		t.Fatalf("the run, its operation, entries and audit records: got %q, want queued, failed with identity.principal_blocked, nothing loaded, one record", got)
	}
	if err := m.Execute(context.Background(), in); err != nil {
		t.Fatalf("run again: got %v, want nil", err)
	}
	if got := state(); got != "queued failed identity.principal_blocked 0 1" {
		t.Errorf("run again: got %q, want nothing changed", got)
	}
}
