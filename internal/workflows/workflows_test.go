package workflows_test

import (
	"bytes"
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
	"go.temporal.io/sdk/testsuite"

	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/workflow"
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
	pgtest.Admin(t).QueryRow(context.Background(), `SELECT count(*) FROM partner.bank_account WHERE partner_id = $1`, e.Supplier).Scan(&n)
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
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	return &workflows.Payments{Pool: pgtest.Pool(t), AppRole: "erp_app", Endpoints: httpapi.Payments(httpapi.Config{Pages: pages, FieldKeys: apitest.FieldKeys()})}
}

func input(e env, op map[string]any, run string) workflows.ProposeInput {
	return workflows.ProposeInput{TenantID: e.Tenant.ID, RunID: uuid.MustParse(run), OperationID: uuid.MustParse(id(op))}
}

// execute runs the proposal workflow in the SDK's test environment with the
// real activities against the package database.
func execute(t *testing.T, a *workflows.Payments, in workflows.ProposeInput, cancel bool) error {
	t.Helper()
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
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
	e := setup(t)
	e.invoice(t, "INV-1", "100.00")
	a := activities(t)
	op, run := e.propose(t, "2026-04-15")
	if err := execute(t, a, input(e, op, run), false); err != nil {
		t.Fatal(err)
	}
	built, _ := e.Must(200, "GET", "/v1/payment-runs/"+run, "", "")
	done, _ := e.Must(200, "GET", "/v1/operations/"+id(op), "", "")
	if built["status"] != "proposed" || built["total_amount"].(map[string]any)["amount"] != "100.00" || done["status"] != "succeeded" {
		t.Fatalf("built: %v %v", built, done)
	}
	// Again: nothing left to do, nothing done twice.
	if err := execute(t, a, input(e, op, run), false); err != nil {
		t.Fatal(err)
	}
	if again, _ := e.Must(200, "GET", "/v1/payment-runs/"+run, "", ""); again["version"] != built["version"] {
		t.Fatalf("built twice: %v", again)
	}
	// The operation cancelled before the work ran: the run fails, its items are free.
	e.invoice(t, "INV-2", "50.00")
	op2, run2 := e.propose(t, "2026-04-16")
	e.Must(200, "POST", "/v1/operations/"+id(op2)+":cancel", "", `"1"`)
	if err := execute(t, a, input(e, op2, run2), false); err != nil {
		t.Fatal(err)
	}
	if failed, _ := e.Must(200, "GET", "/v1/payment-runs/"+run2, "", ""); failed["status"] != "failed" {
		t.Fatalf("cancelled: %v", failed)
	}
	// The workflow itself cancelled: the detached activity still settles the run.
	op3, run3 := e.propose(t, "2026-04-17")
	if err := execute(t, a, input(e, op3, run3), true); err != nil {
		t.Fatal(err)
	}
	if settled, _ := e.Must(200, "GET", "/v1/payment-runs/"+run3, "", ""); settled["status"] != "proposed" {
		t.Fatalf("after the cancelled workflow: %v", settled)
	}
	// Database failures are returned for the retry policy, and nothing is half done.
	e.invoice(t, "INV-3", "25.00")
	op4, run4 := e.propose(t, "2026-04-18")
	pool := pgtest.Pool(t)
	for n := 1; n < 60; n++ {
		count := 0
		a.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, s)
			if err != nil {
				return nil, err
			}
			return &pgtest.FailingTx{Tx: tx, FailAt: n, Count: &count}, nil
		}
		err := a.Propose(context.Background(), input(e, op4, run4))
		if err == nil {
			if n == 1 {
				t.Fatal("built without a statement to fail")
			}
			break
		}
		if !errors.Is(err, pgtest.ErrInjected) {
			t.Fatalf("statement %d: %v", n, err)
		}
		if r, _ := e.Must(200, "GET", "/v1/payment-runs/"+run4, "", ""); r["status"] != "proposing" {
			t.Fatalf("statement %d: half built: %v", n, r)
		}
	}
	a.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errors.New("database down") }
	if err := a.Propose(context.Background(), input(e, op4, run4)); err == nil || !strings.Contains(err.Error(), "database down") {
		t.Fatalf("database down: %v", err)
	}
	a.Begin = nil
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
	pgtest.Admin(t).QueryRow(context.Background(), `SELECT max(occurred_at) FROM audit.audit_event WHERE tenant_id = $1 AND object_id = $2`, e.Tenant.ID, uuid.MustParse(run5)).Scan(&at)
	if !at.Equal(a.Now()) {
		t.Fatalf("audit time %v, want the activity's clock", at)
	}
}

type brokenCommit struct{ pgx.Tx }

func (b *brokenCommit) Commit(ctx context.Context) error {
	_ = b.Tx.Rollback(ctx)
	return errors.New("commit failed")
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
	e := setup(t)
	ctx := context.Background()
	op1, run1 := e.propose(t, "2026-04-15")
	op2, _ := e.propose(t, "2026-04-16")
	engine := &fakeEngine{}
	logs := &syncBuffer{}
	d := &workflows.Dispatcher{Pool: pgtest.Pool(t), WorkerRole: "erp_worker", Client: engine, Queue: "erp-test", Log: slog.New(slog.NewTextHandler(logs, nil))}
	n, err := d.RunOnce(ctx)
	if err != nil || n < 2 {
		t.Fatalf("round: %d %v", n, err)
	}
	var number string
	pgtest.Admin(t).QueryRow(ctx, `SELECT document_number FROM payments.payment_run WHERE id = $1`, uuid.MustParse(run1)).Scan(&number)
	want := workflows.WorkflowID(e.Tenant.ID, e.DE, number)
	var workflowID *string
	pgtest.Admin(t).QueryRow(ctx, `SELECT workflow_id FROM platform.operation WHERE id = $1`, uuid.MustParse(id(op1))).Scan(&workflowID)
	if workflowID == nil || *workflowID != want {
		t.Fatalf("workflow id on the operation: %v, want %s", workflowID, want)
	}
	found := false
	for _, s := range engine.started {
		in := s.Args[0].(workflows.ProposeInput)
		found = found || s.ID == want && s.Queue == "erp-test" && s.Name == workflows.ProposeWorkflow && in.RunID == uuid.MustParse(run1) && in.OperationID == uuid.MustParse(id(op1)) && in.TenantID == e.Tenant.ID
	}
	if !found {
		t.Fatalf("starts: %+v", engine.started)
	}
	// Nothing is dispatched twice.
	if n, err := d.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("second round: %d %v", n, err)
	}
	// A cancelled operation still gets its workflow (which fails the run); a refused start is retried next round.
	op3, run3 := e.propose(t, "2026-04-17")
	e.Must(200, "POST", "/v1/operations/"+id(op3)+":cancel", "", `"1"`)
	op4, _ := e.propose(t, "2026-04-18")
	var number4 string
	pgtest.Admin(t).QueryRow(ctx, `SELECT document_number FROM payments.payment_run WHERE operation_id = $1`, uuid.MustParse(id(op4))).Scan(&number4)
	engine.fail = number4
	n, err = d.RunOnce(ctx)
	if n != 1 || err == nil || !strings.Contains(err.Error(), "engine refused") {
		t.Fatalf("round with a refusal: %d %v", n, err)
	}
	var id3 *string
	pgtest.Admin(t).QueryRow(ctx, `SELECT workflow_id FROM platform.operation WHERE id = $1`, uuid.MustParse(id(op3))).Scan(&id3)
	if id3 == nil || !strings.Contains(*id3, run3[:0]+e.DE.String()) {
		t.Fatalf("cancelled operation not dispatched: %v", id3)
	}
	engine.fail = ""
	if n, err := d.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("retry round: %d %v", n, err)
	}
	_ = op2
	// Database failures are reported.
	pool := pgtest.Pool(t)
	e.propose(t, "2026-04-19")
	for n := 1; n < 10; n++ {
		count := 0
		d.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, s)
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
	if _, err := d.RunOnce(ctx); err == nil || !strings.Contains(err.Error(), "database down") {
		t.Fatalf("database down: %v", err)
	}
	d.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		tx, err := pool.Begin(ctx, s)
		if err != nil {
			return nil, err
		}
		return &brokenCommit{Tx: tx}, nil
	}
	if _, err := d.RunOnce(ctx); err == nil || !strings.Contains(err.Error(), "commit failed") {
		t.Fatalf("commit failing: %v", err)
	}
	// Run logs a failed round, then a quiet one, and stops with the context.
	runCtx, cancel := context.WithCancel(ctx)
	go func() { d.Run(runCtx) }()
	for !strings.Contains(logs.String(), "workflow dispatch failed") {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	d.Begin = nil
	d.Interval = time.Hour
	e.propose(t, "2026-04-20")
	runCtx, cancel = context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { d.Run(runCtx); close(done) }()
	for !strings.Contains(logs.String(), "workflows started") {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
