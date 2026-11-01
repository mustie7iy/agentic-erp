package workflows_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	sdkworkflow "go.temporal.io/sdk/workflow"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/workflows"
)

// Recurring entries (ADR-0056): one workflow per template and due date
// parks one entry for the template's owner, however often it runs.

type recurScenario struct {
	e        fixture.Env
	rc       *workflows.Recurrences
	now      time.Time
	template uuid.UUID
}

// recurSetup saves a monthly template on day 1 from January 2026, its
// clock at 15 February 2026: January and February are due.
func recurSetup(t *testing.T) *recurScenario {
	t.Helper()
	e := fixture.New(t, "ledger.entry.read", "ledger.entry.park", "ledger.entry.post")
	rent, bank := e.Account(t, "2000", "520000"), e.Account(t, "2000", "100000")
	tpl, _ := e.Must(201, "POST", "/v1/entry-templates", fmt.Sprintf(`{"legal_entity_id": %q, "name": "Office rent", "currency": "EUR",
		"schedule": {"frequency": "monthly", "day_of_month": 1, "starts_on": "2026-01-01"},
		"lines": [{"account_id": %q, "amount": {"amount": "4500.00", "currency": "EUR"}}, {"account_id": %q, "amount": {"amount": "-4500.00", "currency": "EUR"}}]}`,
		e.DE, rent, bank), "")
	s := &recurScenario{e: e, now: time.Date(2026, 2, 15, 12, 0, 0, 0, time.UTC), template: uuid.MustParse(tpl["id"].(string))}
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	s.rc = &workflows.Recurrences{Pool: pgtest.Pool(t), AppRole: "erp_app", WorkerRole: "erp_worker",
		Endpoints: app.Ledger(app.Config{Pages: pages, Now: func() time.Time { return s.now }})}
	return s
}

func (s *recurScenario) in(date string) workflows.RecurInput {
	return workflows.RecurInput{TenantID: s.e.Tenant.ID, TemplateID: s.template, Date: date}
}

// entries counts the entries parked from the template for the date.
func (s *recurScenario) entries(t *testing.T, date string) int {
	t.Helper()
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM ledger.parked_entry WHERE template_id = $1 AND occurrence_date = $2`, []any{s.template, date}, &n)
	return n
}

func (s *recurScenario) run(t *testing.T, in workflows.RecurInput) error {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := workflowEnv(&suite)
	env.RegisterWorkflow(workflows.ParkRecurringEntry)
	env.RegisterActivity(s.rc)
	env.ExecuteWorkflow(workflows.ParkRecurringEntry, in)
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	return env.GetWorkflowError()
}

func TestParkRecurringEntry(t *testing.T) {
	s := recurSetup(t)
	for range 2 {
		if err := s.run(t, s.in("2026-01-01")); err != nil {
			t.Fatalf("workflow: %v", err)
		}
	}
	if n := s.entries(t, "2026-01-01"); n != 1 {
		t.Errorf("entries after two runs: got %d, want 1", n)
	}
	t.Run("an unreadable date fails for good", func(t *testing.T) {
		err := s.run(t, s.in("1 February"))
		var app *temporal.ApplicationError
		if !errors.As(err, &app) || !app.NonRetryable() || app.Type() != "InvalidDate" {
			t.Errorf("got %v, want a non-retryable InvalidDate", err)
		}
	})
}

// TestParkOccurrenceTwice: the activity run twice, as after a completion
// the engine lost, parks one entry and publishes one event.
func TestParkOccurrenceTwice(t *testing.T) {
	s := recurSetup(t)
	ctx := context.Background()
	for i, want := range []string{ledger.OccurrenceParked, ledger.OccurrenceSkipped} {
		got, err := s.rc.ParkOccurrence(ctx, s.in("2026-02-01"))
		if err != nil || got != want {
			t.Fatalf("run %d: got %s %v, want %s", i+1, got, err, want)
		}
	}
	if n := s.entries(t, "2026-02-01"); n != 1 {
		t.Errorf("entries: got %d, want 1", n)
	}
	if n := len(apitest.Published(t, s.e.Tenant, outbox.Event{Context: "ledger", Name: "parked_journal_entry_changed"})); n != 1 {
		t.Errorf("events: got %d, want 1", n)
	}
	var actor string
	pgtest.Scan(t, `SELECT actor_id::text FROM audit.audit_event WHERE tenant_id = $1 AND action = 'ledger.parked_entry.park'`, []any{s.e.Tenant.ID}, &actor)
	if actor != ledger.RecurrerActor.String() {
		t.Errorf("audited actor: got %s, want the recurrer", actor)
	}
}

// TestDispatchRecurring: the dispatcher starts one workflow per due date,
// named by the template and the date, once across rounds; a refused start
// is reported and tried again.
func TestDispatchRecurring(t *testing.T) {
	s := recurSetup(t)
	engine := &fakeEngine{}
	d := &workflows.Dispatcher{Pool: pgtest.Pool(t), WorkerRole: "erp_worker", Client: engine, Queue: "erp-test", Recurring: s.rc,
		Now: func() time.Time { return s.now }}
	ctx := context.Background()
	jan, feb := workflows.RecurWorkflowID(s.in("2026-01-01")), workflows.RecurWorkflowID(s.in("2026-02-01"))
	if want := fmt.Sprintf("%s-entry_template-%s-2026-01-01", s.e.Tenant.ID, s.template); jan != want {
		t.Errorf("workflow id: got %s, want %s", jan, want)
	}
	ours := func() []string {
		var ids []string
		for _, st := range engine.started {
			if strings.Contains(st.ID, s.template.String()) {
				ids = append(ids, st.ID)
			}
		}
		return ids
	}
	engine.fail = feb
	if _, err := d.RunOnce(ctx); err == nil || !strings.Contains(err.Error(), feb) {
		t.Fatalf("refused start: got %v, want it reported", err)
	}
	engine.fail = ""
	if _, err := d.RunOnce(ctx); err != nil {
		t.Fatalf("round: %v", err)
	}
	if got := ours(); len(got) != 2 || got[0] != jan || got[1] != feb {
		t.Fatalf("started: got %v, want January then February", got)
	}
	if _, err := d.RunOnce(ctx); err != nil || len(ours()) != 2 {
		t.Errorf("another round: got %v %v, want nothing more", ours(), err)
	}
	t.Run("database failures", func(t *testing.T) {
		pool := pgtest.Pool(t)
		defer func() { s.rc.Begin = nil }()
		for n := 1; n < 40; n++ {
			count := 0
			s.rc.Begin = func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
				tx, err := pool.Begin(ctx, scope)
				if err != nil {
					return nil, err
				}
				return &pgtest.FailingTx{Tx: tx, FailAt: n, Count: &count}, nil
			}
			_, err := d.RunOnce(ctx)
			if err == nil {
				break
			}
			if !errors.Is(err, pgtest.ErrInjected) {
				t.Fatalf("statement %d: %v", n, err)
			}
		}
		s.rc.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errors.New("database down") }
		if _, err := s.rc.Due(ctx, s.now); err == nil || !strings.Contains(err.Error(), "database down") {
			t.Errorf("the worker's transaction failing: got %v, want it reported", err)
		}
	})
}

// TestRecurrer: without a workflow engine the in-process worker parks every
// due date, oldest first, and a second round parks nothing.
func TestRecurrer(t *testing.T) {
	s := recurSetup(t)
	logs := &pgtest.SyncBuffer{}
	w := &workflows.Recurrer{Recurrences: s.rc, Now: func() time.Time { return s.now }, Log: slog.New(slog.NewTextHandler(logs, nil))}
	ctx := context.Background()
	// Every tenant's due dates are parked, those of the package's other
	// tests among them.
	if n, err := w.RunOnce(ctx); err != nil || n < 2 {
		t.Fatalf("first round: got %d %v, want at least this tenant's two parked", n, err)
	}
	if n, err := w.RunOnce(ctx); err != nil || n != 0 {
		t.Errorf("second round: got %d %v, want none", n, err)
	}
	for _, date := range []string{"2026-01-01", "2026-02-01"} {
		if n := s.entries(t, date); n != 1 {
			t.Errorf("entries of %s: got %d, want 1", date, n)
		}
	}
	t.Run("a failing round", func(t *testing.T) {
		s.now = time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)
		calls := 0
		s.rc.Begin = func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
			if scope.TenantID == s.e.Tenant.ID {
				calls++
			}
			if calls == 2 { // the tenant's dates, then the activity
				return nil, errors.New("database down")
			}
			return pgtest.Pool(t).Begin(ctx, scope)
		}
		defer func() { s.rc.Begin = nil }()
		if _, err := w.RunOnce(ctx); err == nil || !strings.Contains(err.Error(), "database down") {
			t.Errorf("got %v, want the failure reported", err)
		}
	})
	t.Run("run loop", func(t *testing.T) {
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { w.Run(runCtx); close(done) }()
		t.Cleanup(func() { cancel(); <-done })
		pgtest.Eventually(t, 10*time.Second, func() bool { return strings.Contains(logs.String(), "recurring entries parked") },
			"the worker did not log the entries it parked: %s", logs)
		cancel()
		<-done
	})
}

// TestParkRecurringEntryReplay: a history of the first version of
// ParkRecurringEntry (testdata, recorded on a development server with a
// stand-in activity) replays on the current code without a
// nondeterminism error. A change to the workflow that issues other
// commands adds a version (workflow.GetVersion, ADR-0012) and keeps this
// history replaying.
func TestParkRecurringEntryReplay(t *testing.T) {
	r := worker.NewWorkflowReplayer()
	r.RegisterWorkflowWithOptions(workflows.ParkRecurringEntry, sdkworkflow.RegisterOptions{Name: workflows.RecurWorkflow})
	if err := r.ReplayWorkflowHistoryFromJSONFile(nil, filepath.Join("testdata", "park_recurring_entry_v1.json")); err != nil {
		t.Fatalf("replay of the version 1 history: %v", err)
	}
}
