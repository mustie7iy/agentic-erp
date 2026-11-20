package workflows_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.temporal.io/sdk/testsuite"
	sdkworkflow "go.temporal.io/sdk/workflow"

	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/workflows"
)

// E-invoice sends (ADR-0061 As built E3): the workflow and the in-process
// sender advance a send step by step through the einvoicing context's
// port, sleeping as each step says, until a step is done. The port is the
// test's: the steps themselves are the einvoicing context's tests'.

// scriptedSends is the port, answering its steps in order (the last one
// again), each run in a transaction of the send's tenant, and listing the
// sends to advance it is given (unfinished), with the time it was asked
// at.
type scriptedSends struct {
	mu         sync.Mutex
	tenant     uuid.UUID // the scenario's: other tenants' lists are empty
	steps      []workflows.EInvoiceSendStep
	err        error
	calls      int
	unfinished map[uuid.UUID]uuid.UUID
	listErr    error
	asked      time.Time
}

func (s *scriptedSends) UnfinishedEInvoiceSends(ctx context.Context, tx pgx.Tx, now time.Time) (map[uuid.UUID]uuid.UUID, error) {
	var tenant string
	if err := tx.QueryRow(ctx, `SELECT current_setting('app.tenant_id')`).Scan(&tenant); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if tenant != s.tenant.String() {
		return nil, nil
	}
	s.asked = now
	return s.unfinished, s.listErr
}

func (s *scriptedSends) Sender(tenant uuid.UUID) reqctx.Principal {
	return reqctx.Principal{ID: uuid.NewSHA1(uuid.NameSpaceURL, []byte("test sender")), Kind: reqctx.KindSystem, TenantID: tenant}
}

func (s *scriptedSends) AdvanceEInvoiceSubmission(ctx context.Context, _, _ uuid.UUID, in workflows.Within) (workflows.EInvoiceSendStep, error) {
	if err := in(ctx, func(ctx context.Context) error {
		_, _, err := reqctx.Request(ctx)
		return err
	}); err != nil {
		return workflows.EInvoiceSendStep{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return workflows.EInvoiceSendStep{}, s.err
	}
	return s.steps[min(s.calls, len(s.steps))-1], nil
}

func (s *scriptedSends) called() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// sendScenario is a tenant and the send activities over the scripted port.
func sendScenario(t *testing.T, steps ...workflows.EInvoiceSendStep) (fixture.Env, *scriptedSends, *workflows.EInvoiceSenders) {
	t.Helper()
	f := fixture.New(t)
	port := &scriptedSends{steps: steps, tenant: f.Tenant.ID}
	return f, port, &workflows.EInvoiceSenders{Pool: pgtest.Pool(t), AppRole: "erp_app", WorkerRole: "erp_worker", Sends: port}
}

// queueSend records a queued send's Operation for a new e-invoice id;
// taken ones at the end, so no other runner takes them.
func queueSend(t *testing.T, f fixture.Env) workflows.EInvoiceSendInput {
	t.Helper()
	in := workflows.EInvoiceSendInput{TenantID: f.Tenant.ID, OutgoingID: uuid.New(), OperationID: uuid.Must(uuid.NewV7())}
	pgtest.Exec(t, `INSERT INTO platform.operation (tenant_id, id, kind, status, request, resource, created_by, updated_by)
		VALUES ($1, $2, $3, 'queued', '{}', $4, $5, $5)`, f.Tenant.ID, in.OperationID, workflows.EInvoiceSendKind,
		"/v1/outgoing-e-invoices/"+in.OutgoingID.String(), f.Tenant.Human.ID)
	t.Cleanup(func() {
		pgtest.Exec(t, `UPDATE platform.operation SET workflow_id = 'carried out by the test', version = version + 1 WHERE id = $1 AND workflow_id IS NULL`,
			in.OperationID)
	})
	return in
}

func runSend(t *testing.T, a *workflows.EInvoiceSenders, in workflows.EInvoiceSendInput) error {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := workflowEnv(&suite)
	env.RegisterWorkflowWithOptions(workflows.SendEInvoice, sdkworkflow.RegisterOptions{Name: workflows.EInvoiceSendWorkflow})
	env.RegisterActivity(a)
	env.ExecuteWorkflow(workflows.EInvoiceSendWorkflow, in)
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	return env.GetWorkflowError()
}

// TestSendEInvoiceWorkflow: the workflow takes the steps, sleeping between
// them, until one is done; a step's problem fails it; after its steps per
// run it continues as new; a cancellation cuts its wait short and the
// steps go on, waiting as they say, until one is done (it ended the
// cancelled send).
func TestSendEInvoiceWorkflow(t *testing.T) {
	f, port, a := sendScenario(t, workflows.EInvoiceSendStep{Wait: 30 * time.Second}, workflows.EInvoiceSendStep{}, workflows.EInvoiceSendStep{Done: true})
	in := queueSend(t, f)
	if err := runSend(t, a, in); err != nil || port.called() != 3 {
		t.Errorf("the send: got %v after %d steps, want done after 3", err, port.called())
	}
	port.calls, port.err = 0, problem.Conflict("einvoicing.test", "refused")
	if err := runSend(t, a, in); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("a step's problem: got %v, want the workflow failed with it", err)
	}
	port.calls, port.err, port.steps = 0, nil, []workflows.EInvoiceSendStep{{Wait: time.Minute}}
	var cont *sdkworkflow.ContinueAsNewError
	if err := runSend(t, a, in); !errors.As(err, &cont) {
		t.Errorf("a send without an end: got %v, want it continued as new", err)
	}
	if got, want := workflows.EInvoiceSendWorkflowID(in.TenantID, in.OperationID), in.TenantID.String()+"-einvoice_send-"+in.OperationID.String(); got != want {
		t.Errorf("the workflow id: got %s, want %s", got, want)
	}
	t.Run("cancelled", func(t *testing.T) {
		port.calls, port.steps = 0, []workflows.EInvoiceSendStep{{Wait: time.Hour}, {Wait: time.Minute}, {Done: true}}
		var suite testsuite.WorkflowTestSuite
		env := workflowEnv(&suite)
		env.RegisterWorkflowWithOptions(workflows.SendEInvoice, sdkworkflow.RegisterOptions{Name: workflows.EInvoiceSendWorkflow})
		env.RegisterActivity(a)
		env.RegisterDelayedCallback(env.CancelWorkflow, time.Minute)
		start := env.Now()
		env.ExecuteWorkflow(workflows.EInvoiceSendWorkflow, in)
		if !env.IsWorkflowCompleted() {
			t.Fatal("cancelled: the workflow did not complete")
		}
		if err := env.GetWorkflowError(); err != nil {
			t.Errorf("cancelled: got %v, want it completed without an error", err)
		}
		if got := port.called(); got != 3 {
			t.Errorf("cancelled: got %d steps, want 3", got)
		}
		if took := env.Now().Sub(start); took >= time.Hour {
			t.Errorf("cancelled: took %v, want the hour's wait cut short", took)
		}
	})
}

// TestEInvoiceSender: without a workflow engine the in-process sender
// asks the einvoicing context, in the scope of each tenant with send
// Operations and at its clock, for the sends to advance (due, or
// cancelled) and advances each once; none listed, none advanced; a step
// failing, or a tenant's list, is reported and tried again; Run logs its
// rounds and stops with its context.
func TestEInvoiceSender(t *testing.T) {
	f, port, a := sendScenario(t, workflows.EInvoiceSendStep{Wait: time.Hour}, workflows.EInvoiceSendStep{Done: true})
	now := time.Now()
	a.Now = func() time.Time { return now }
	in := queueSend(t, f)
	logs := &pgtest.SyncBuffer{}
	w := &workflows.EInvoiceSender{Senders: a, Log: slog.New(slog.NewTextHandler(logs, nil))}
	ctx := context.Background()
	mine := func() int { return port.called() }
	if n, err := w.RunOnce(ctx); err != nil || n != 0 || mine() != 0 {
		t.Fatalf("none listed: got %v after %d steps, want none", err, mine())
	}
	if !port.asked.Equal(now) {
		t.Errorf("asked at: got %v, want the sender's clock %v", port.asked, now)
	}
	port.unfinished = map[uuid.UUID]uuid.UUID{in.OutgoingID: in.OperationID}
	if n, err := w.RunOnce(ctx); err != nil || n != 1 || mine() != 1 {
		t.Errorf("one listed: got %v after %d steps, want one step", err, mine())
	}
	t.Run("failures", func(t *testing.T) {
		port.listErr = errors.New("list down")
		if _, err := w.RunOnce(ctx); err == nil || !strings.Contains(err.Error(), "list down") {
			t.Errorf("a tenant's list failing: got %v, want it reported", err)
		}
		port.listErr = nil
		port.err = errors.New("step down")
		if _, err := w.RunOnce(ctx); err == nil || !strings.Contains(err.Error(), "step down") {
			t.Errorf("a step failing: got %v, want it reported", err)
		}
		port.err = nil
		a.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errors.New("database down") }
		if _, err := w.RunOnce(ctx); err == nil || !strings.Contains(err.Error(), "database down") {
			t.Errorf("the queue unreadable: got %v, want the failure", err)
		}
		a.Begin = func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
			tx, err := pgtest.Pool(t).Begin(ctx, scope)
			if err != nil || scope.TenantID != uuid.Nil {
				return tx, err
			}
			return &pgtest.FailingTx{Tx: tx, FailAt: 1}, nil
		}
		if _, err := w.RunOnce(ctx); !errors.Is(err, pgtest.ErrInjected) {
			t.Errorf("the queue's statement failing: got %v, want the injected failure", err)
		}
		a.Begin = nil
	})
	t.Run("Run logs its rounds and stops with the context", func(t *testing.T) {
		queueSend(t, f)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { w.Run(ctx); close(done) }()
		pgtest.Eventually(t, 10*time.Second, func() bool { return strings.Contains(logs.String(), "e-invoice send steps taken") }, "the round logged")
		cancel()
		<-done
	})
}

// TestDispatcherStartsSends: a queued send is started as the workflow
// SendEInvoice under its Operation's id.
func TestDispatcherStartsSends(t *testing.T) {
	f, _, _ := sendScenario(t)
	in := queueSend(t, f)
	engine := &fakeEngine{}
	d := &workflows.Dispatcher{Pool: pgtest.Pool(t), WorkerRole: "erp_worker", Client: engine, Queue: "erp-test", Log: slog.New(slog.DiscardHandler)}
	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatalf("round: %v", err)
	}
	want := workflows.EInvoiceSendWorkflowID(in.TenantID, in.OperationID)
	found := 0
	for _, st := range engine.started {
		if st.ID == want && st.Name == workflows.EInvoiceSendWorkflow && len(st.Args) == 1 && st.Args[0] == in {
			found++
		}
	}
	if found != 1 {
		t.Errorf("started: got %d of %s, want it once", found, want)
	}
}
