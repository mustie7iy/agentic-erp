package workflows_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.temporal.io/sdk/testsuite"
	sdkworkflow "go.temporal.io/sdk/workflow"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/einvoicing"
	"github.com/mustie7iy/agentic-erp/internal/einvoicing/einvoicetest"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/egress"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/secrets"
	"github.com/mustie7iy/agentic-erp/internal/workflows"
)

// E-invoice fetches (ADR-0061 §4, T65a): the workflow lists the provider's
// messages, records each and ends the fetch's Operation with the counts,
// through the einvoicing context's port.

type einvoiceScenario struct {
	e         fixture.Env
	cfg       app.Config
	srv       *einvoicetest.Server
	f         *workflows.EInvoiceFetches
	connector uuid.UUID
}

const fetchParticipant = "0235:100000000000003"

// einvoiceSetup is a tenant with an einvoice_provider connector to a stand-in
// whose participant is company 1000's. The test's own fetches are carried
// out by the test: at the end their Operations still without a workflow
// are marked as taken, as conSetup's.
func einvoiceSetup(t *testing.T) *einvoiceScenario {
	t.Helper()
	s := &einvoiceScenario{e: fixture.New(t, "einvoicing.einvoice.read", "einvoicing.einvoice.manage", "platform.operation.read"),
		srv: einvoicetest.NewServer(t, "tok")}
	t.Cleanup(func() {
		pgtest.Exec(t, `UPDATE platform.operation SET workflow_id = 'carried out by the test', version = version + 1
			WHERE tenant_id = $1 AND kind = $2 AND workflow_id IS NULL`, s.e.Tenant.ID, workflows.EInvoiceFetchKind)
	})
	client, err := einvoicing.NewClient(egress.LoopbackForTests(s.srv.Roots))
	if err != nil {
		t.Fatal(err)
	}
	s.cfg = app.Config{Secrets: secrets.Local{Getenv: func(string) string { return "tok" }}, Objects: blobstore.Dir{Root: t.TempDir()}, Providers: client}
	s.f = &workflows.EInvoiceFetches{Pool: pgtest.Pool(t), AppRole: "erp_app", WorkerRole: "erp_worker", Fetches: app.EInvoiceFetches(s.cfg)}
	s.connector = uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO integration.connector (tenant_id, id, kind, name, settings, secret_ref, status, created_by, updated_by)
		VALUES ($1, $2, 'einvoice_provider', 'Provider', jsonb_build_object('adapter', 'standin', 'base_url', $3::text, 'format', 'pint_ae',
		'participants', jsonb_build_object('1000', $4::text)), 'env:PROVIDER_TOKEN', 'active', $5, $5)`,
		s.e.Tenant.ID, s.connector, s.srv.URL, fetchParticipant, s.e.Tenant.Human.ID)
	return s
}

// queue queues a fetch as the requester; taken marks it as carried out by
// the test, so no dispatcher or in-process fetcher of the package takes it.
func (s *einvoiceScenario) queue(t *testing.T, taken bool) workflows.EInvoiceFetchInput {
	t.Helper()
	var op uuid.UUID
	if err := apitest.Committed(t, s.e.Tenant, s.e.Tenant.Human, func(ctx context.Context) error {
		res, err := app.EInvoicing(s.cfg).FetchEInvoices(ctx, &gen.EInvoiceFetch{ConnectorID: s.connector}, gen.FetchEInvoicesParams{})
		if err == nil {
			op = res.(*gen.OperationHeaders).Response.ID
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if taken {
		pgtest.Exec(t, `UPDATE platform.operation SET workflow_id = 'carried out by the test', version = version + 1 WHERE id = $1`, op)
	}
	return workflows.EInvoiceFetchInput{TenantID: s.e.Tenant.ID, ConnectorID: s.connector, OperationID: op}
}

// outcome is the fetch's Operation: its status, its result or its
// problem's code.
func fetchOutcome(t *testing.T, in workflows.EInvoiceFetchInput) string {
	t.Helper()
	var got string
	pgtest.Scan(t, `SELECT status || ' ' || coalesce(result::text, error->>'code', '-') FROM platform.operation WHERE id = $1`, []any{in.OperationID}, &got)
	return got
}

// runEInvoiceFetch runs the workflow in the SDK's test environment with the real
// activities against the package database.
func (s *einvoiceScenario) runEInvoiceFetch(t *testing.T, in workflows.EInvoiceFetchInput) error {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := workflowEnv(&suite)
	env.RegisterWorkflowWithOptions(workflows.FetchEInvoices, sdkworkflow.RegisterOptions{Name: workflows.EInvoiceFetchWorkflow})
	env.RegisterActivity(s.f)
	env.ExecuteWorkflow(workflows.EInvoiceFetchWorkflow, in)
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	return env.GetWorkflowError()
}

// TestFetchEInvoicesWorkflow: the workflow records the provider's messages
// and succeeds the Operation with its counts; a second fetch finds them
// recorded; a message the provider cannot give outlasts its retries, so
// the fetch fails naming it with the others recorded; a connector disabled
// since the fetch was queued fails it with its problem, and a provider
// that cannot be listed with platform.unavailable.
func TestFetchEInvoicesWorkflow(t *testing.T) {
	s := einvoiceSetup(t)
	s.srv.Deliver("MSG-1", fetchParticipant, einvoicetest.Invoice("INV-1"))
	s.srv.Deliver("MSG-2", "", einvoicetest.Invoice("INV-2"))
	first := s.queue(t, true)
	if err := s.runEInvoiceFetch(t, first); err != nil {
		t.Fatalf("the fetch: %v", err)
	}
	if got := fetchOutcome(t, first); got != `succeeded {"changed": 0, "recorded": 2, "already_recorded": 0}` {
		t.Errorf("the fetch: got %s, want both recorded", got)
	}
	s.srv.Fail("POST /messages/MSG-1/acknowledge", 1) // lost once: the activity's retry acknowledges it
	s.srv.Deliver("MSG-3", fetchParticipant, einvoicetest.Invoice("INV-3"))
	s.srv.Deliver("BAD", fetchParticipant, einvoicetest.Invoice("INV-4"))
	s.srv.Fail("GET /messages/BAD", 10)
	s.srv.Deliver("BIG", fetchParticipant, bytes.Repeat([]byte("x"), einvoicing.MaxDocument+1))
	second := s.queue(t, true)
	err := s.runEInvoiceFetch(t, second)
	for _, want := range []string{"BAD (platform.unavailable)", "BIG (einvoicing.document_too_large)"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("messages failing: got %v, want the workflow failed naming %s", err, want)
		}
	}
	if got := fetchOutcome(t, second); got != "failed einvoicing.fetch_incomplete" {
		t.Errorf("a message failing: got %s, want the fetch incomplete", got)
	}
	var recorded int
	pgtest.Scan(t, `SELECT count(*) FROM einvoicing.einvoice WHERE tenant_id = $1 AND fetch_operation_id = $2`, []any{s.e.Tenant.ID, second.OperationID}, &recorded)
	if recorded != 1 {
		t.Errorf("recorded by the second fetch: got %d, want MSG-3 alone", recorded)
	}
	s.srv.Fail("GET /messages", 10)
	unlisted := s.queue(t, true)
	if err := s.runEInvoiceFetch(t, unlisted); err == nil {
		t.Error("the provider unreachable: got nil, want the workflow failed")
	}
	if got := fetchOutcome(t, unlisted); got != "failed platform.unavailable" {
		t.Errorf("the provider unreachable: got %s, want platform.unavailable", got)
	}
	disabled := s.queue(t, true)
	pgtest.Exec(t, `UPDATE integration.connector SET status = 'disabled', version = version + 1 WHERE id = $1`, s.connector)
	if err := s.runEInvoiceFetch(t, disabled); err != nil {
		t.Errorf("a connector disabled since: got %v, want the fetch failed without an error", err)
	}
	if got := fetchOutcome(t, disabled); got != "failed integration.connector_disabled" {
		t.Errorf("a connector disabled since: got %s, want integration.connector_disabled", got)
	}
}

// TestEInvoiceFetcher: without a workflow engine the in-process fetcher
// carries out every fetch still queued with the workflow's steps; one
// failing on the machinery is reported and stays queued; Run logs its
// rounds and stops with its context.
func TestEInvoiceFetcher(t *testing.T) {
	s := einvoiceSetup(t)
	s.srv.Deliver("MSG-1", fetchParticipant, einvoicetest.Invoice("INV-1"))
	in := s.queue(t, false)
	logs := &pgtest.SyncBuffer{}
	w := &workflows.EInvoiceFetcher{Fetches: s.f, Log: slog.New(slog.NewTextHandler(logs, nil))}
	ctx := context.Background()
	if n, err := w.RunOnce(ctx); err != nil || n < 1 {
		t.Fatalf("round: got %d %v, want the fetch ended", n, err)
	}
	if got := fetchOutcome(t, in); got != `succeeded {"changed": 0, "recorded": 1, "already_recorded": 0}` {
		t.Errorf("the fetch: got %s, want MSG-1 recorded", got)
	}
	t.Run("failures", func(t *testing.T) {
		stuck := s.queue(t, false)
		defer func() { s.f.Begin = nil }()
		s.f.Begin = func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
			if scope.TenantID == s.e.Tenant.ID {
				return nil, errors.New("database down")
			}
			return pgtest.Pool(t).Begin(ctx, scope)
		}
		if _, err := w.RunOnce(ctx); err == nil || !strings.Contains(err.Error(), stuck.OperationID.String()) {
			t.Errorf("a step failing: got %v, want the fetch named", err)
		}
		if got := fetchOutcome(t, stuck); got != "queued -" {
			t.Errorf("the fetch: got %s, want it still queued", got)
		}
		s.f.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errors.New("database down") }
		if _, err := w.RunOnce(ctx); err == nil || !strings.Contains(err.Error(), "database down") {
			t.Errorf("the queue unreadable: got %v, want the failure", err)
		}
		s.f.Begin = func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
			tx, err := pgtest.Pool(t).Begin(ctx, scope)
			if err != nil || scope.TenantID != uuid.Nil {
				return tx, err
			}
			return &pgtest.FailingTx{Tx: tx, FailAt: 1}, nil
		}
		if _, err := w.RunOnce(ctx); !errors.Is(err, pgtest.ErrInjected) {
			t.Errorf("the queue's statement failing: got %v, want the injected failure", err)
		}
		s.f.Begin = nil
		if n, err := w.RunOnce(ctx); err != nil || n < 1 {
			t.Errorf("the next round: got %d %v, want the fetch carried out", n, err)
		}
		if got := fetchOutcome(t, stuck); got != `succeeded {"changed": 0, "recorded": 0, "already_recorded": 0}` {
			t.Errorf("the fetch after the next round: got %s, want it succeeded", got)
		}
	})
	t.Run("a lost acknowledgement found already recorded by the next fetch", func(t *testing.T) {
		s.srv.Deliver("MSG-2", fetchParticipant, einvoicetest.Invoice("INV-2"))
		s.srv.Fail("POST /messages/MSG-2/acknowledge", 1)
		lost := s.queue(t, false)
		if _, err := w.RunOnce(ctx); err == nil || !strings.Contains(err.Error(), "MSG-2 (platform.unavailable)") {
			t.Errorf("round: got %v, want MSG-2 named", err)
		}
		if got := fetchOutcome(t, lost); got != "failed einvoicing.fetch_incomplete" {
			t.Errorf("the fetch: got %s, want it incomplete", got)
		}
		again := s.queue(t, false)
		if _, err := w.RunOnce(ctx); err != nil {
			t.Fatalf("round: %v", err)
		}
		if got := fetchOutcome(t, again); got != `succeeded {"changed": 0, "recorded": 0, "already_recorded": 1}` {
			t.Errorf("the next fetch: got %s, want MSG-2 already recorded", got)
		}
	})
	t.Run("the Operation not ended leaves the fetch queued", func(t *testing.T) {
		left := s.queue(t, false)
		defer func() { s.f.Begin = nil }()
		begins := 0
		s.f.Begin = func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
			if scope.TenantID == s.e.Tenant.ID {
				if begins++; begins > 1 { // the listing's transaction, then the finish's
					return nil, errors.New("database down")
				}
			}
			return pgtest.Pool(t).Begin(ctx, scope)
		}
		if _, err := w.RunOnce(ctx); err == nil || !strings.Contains(err.Error(), "database down") {
			t.Errorf("round: got %v, want the finish's failure", err)
		}
		if got := fetchOutcome(t, left); got != "queued -" {
			t.Errorf("the fetch: got %s, want it still queued", got)
		}
		s.f.Begin = nil
		if _, err := w.RunOnce(ctx); err != nil {
			t.Errorf("the next round: got %v, want no error", err)
		}
		if got := fetchOutcome(t, left); got != `succeeded {"changed": 0, "recorded": 0, "already_recorded": 0}` {
			t.Errorf("the fetch after the next round: got %s, want it succeeded", got)
		}
	})
	t.Run("the provider failing once fails the fetch, tried once", func(t *testing.T) {
		s.srv.Fail("GET /messages", 1)
		once := s.queue(t, false)
		if _, err := w.RunOnce(ctx); err == nil {
			t.Error("round: got nil, want the fetch's failure reported")
		}
		if got := fetchOutcome(t, once); got != "failed platform.unavailable" {
			t.Errorf("the fetch: got %s, want it failed with platform.unavailable", got)
		}
	})
	t.Run("Run logs its rounds and stops with the context", func(t *testing.T) {
		s.queue(t, false)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { w.Run(ctx); close(done) }()
		pgtest.Eventually(t, 10*time.Second, func() bool { return strings.Contains(logs.String(), "e-invoice fetches ended") }, "the round logged")
		cancel()
		<-done
	})
}

// TestDispatcherStartsFetches: a queued fetch is started as the workflow
// FetchEInvoices under its Operation's id, once.
func TestDispatcherStartsFetches(t *testing.T) {
	s := einvoiceSetup(t)
	in := s.queue(t, false)
	engine := &fakeEngine{}
	d := &workflows.Dispatcher{Pool: pgtest.Pool(t), WorkerRole: "erp_worker", Client: engine, Queue: "erp-test", Log: slog.New(slog.DiscardHandler)}
	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatalf("round: %v", err)
	}
	want := workflows.EInvoiceFetchWorkflowID(in.TenantID, in.OperationID)
	found := 0
	for _, st := range engine.started {
		if st.ID == want && st.Name == workflows.EInvoiceFetchWorkflow && len(st.Args) == 1 && st.Args[0] == in {
			found++
		}
	}
	if found != 1 {
		t.Errorf("started: got %d of %s in %v, want it once", found, want, engine.started)
	}
	var workflowID string
	pgtest.Scan(t, `SELECT workflow_id FROM platform.operation WHERE id = $1`, []any{in.OperationID}, &workflowID)
	if workflowID != want {
		t.Errorf("the Operation's workflow: got %s, want %s", workflowID, want)
	}
}
