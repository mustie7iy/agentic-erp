package workflows_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"
	sdkworkflow "go.temporal.io/sdk/workflow"

	"github.com/mustie7iy/agentic-erp/internal/einvoicing/einvoicetest"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/workflow"
	"github.com/mustie7iy/agentic-erp/internal/workflows"
)

// Scheduled e-invoice fetches (ADR-0061 §4, T65b): once per period the
// dispatcher starts ScheduledEInvoiceFetch for each active connector of an
// active tenant, which records the fetch's Operation and fetches.

// runScheduled runs ScheduledEInvoiceFetch under the workflow id in the
// SDK's test environment with the activities of f.
func runScheduled(t *testing.T, f *workflows.EInvoiceFetches, id string, in workflows.EInvoiceFetchInput) error {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := workflowEnv(&suite)
	env.RegisterWorkflowWithOptions(workflows.FetchEInvoices, sdkworkflow.RegisterOptions{Name: workflows.EInvoiceFetchWorkflow})
	env.RegisterWorkflowWithOptions(workflows.ScheduledEInvoiceFetch, sdkworkflow.RegisterOptions{Name: workflows.ScheduledEInvoiceFetchWorkflow})
	env.RegisterActivity(f)
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: id})
	env.ExecuteWorkflow(workflows.ScheduledEInvoiceFetchWorkflow, in)
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	return env.GetWorkflowError()
}

// TestScheduledEInvoiceFetch: a scheduled fetch records its Operation
// under the id its input names, naming its workflow, and records the
// provider's messages under it; the next period's fetch counts a message
// delivered again with other bytes as changed; recording the Operation
// twice records it once (run-twice); a database that fails outlasts the
// step's tries and fails the workflow.
func TestScheduledEInvoiceFetch(t *testing.T) {
	s := einvoiceSetup(t)
	s.srv.Deliver("MSG-1", fetchParticipant, einvoicetest.Invoice("INV-1"))
	period := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	first := workflows.ScheduledEInvoiceFetchInput(s.e.Tenant.ID, s.connector, period)
	id := workflows.ScheduledEInvoiceFetchWorkflowID(s.e.Tenant.ID, s.connector, period)
	if err := runScheduled(t, s.f, id, first); err != nil {
		t.Fatalf("the scheduled fetch: %v", err)
	}
	var workflowID, recordedBy string
	pgtest.Scan(t, `SELECT workflow_id FROM platform.operation WHERE id = $1`, []any{first.OperationID}, &workflowID)
	pgtest.Scan(t, `SELECT fetch_operation_id::text FROM einvoicing.einvoice WHERE tenant_id = $1 AND message_id = 'MSG-1'`, []any{s.e.Tenant.ID}, &recordedBy)
	for name, c := range map[string]struct{ got, want string }{
		"the Operation":        {fetchOutcome(t, first), `succeeded {"changed": 0, "recorded": 1, "already_recorded": 0}`},
		"its workflow":         {workflowID, id},
		"the document's fetch": {recordedBy, first.OperationID.String()},
	} {
		if c.got != c.want {
			t.Errorf("%s: got %s, want %s", name, c.got, c.want)
		}
	}
	if err := s.f.QueueEInvoiceFetch(context.Background(), first, "another"); err != nil {
		t.Fatalf("queue again: %v", err)
	}
	pgtest.Scan(t, `SELECT workflow_id FROM platform.operation WHERE id = $1`, []any{first.OperationID}, &workflowID)
	if got := fetchOutcome(t, first); workflowID != id || !strings.HasPrefix(got, "succeeded") {
		t.Errorf("queued again: got %s %s, want the Operation as it was", workflowID, got)
	}
	s.srv.Redeliver("MSG-1", einvoicetest.Invoice("INV-1 corrected"))
	next := workflows.ScheduledEInvoiceFetchInput(s.e.Tenant.ID, s.connector, period.Add(time.Hour))
	if err := runScheduled(t, s.f, workflows.ScheduledEInvoiceFetchWorkflowID(s.e.Tenant.ID, s.connector, period.Add(time.Hour)), next); err != nil {
		t.Fatalf("the next period's fetch: %v", err)
	}
	if got := fetchOutcome(t, next); got != `succeeded {"changed": 1, "recorded": 0, "already_recorded": 0}` {
		t.Errorf("the next period's fetch: got %s, want the message changed", got)
	}
	t.Run("the database failing", func(t *testing.T) {
		failing := *s.f
		failing.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errors.New("database down") }
		later := workflows.ScheduledEInvoiceFetchInput(s.e.Tenant.ID, s.connector, period.Add(2*time.Hour))
		if err := runScheduled(t, &failing, "failing", later); err == nil || !strings.Contains(err.Error(), "database down") {
			t.Errorf("got %v, want the workflow failed with the database's error", err)
		}
	})
}

// TestScheduledEInvoiceFetchInput: the Operation of a connector's period
// is a UUIDv7 of the period's start, the same each time, another for
// another period or connector.
func TestScheduledEInvoiceFetchInput(t *testing.T) {
	tenant, connector := uuid.New(), uuid.New()
	period := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	in := workflows.ScheduledEInvoiceFetchInput(tenant, connector, period)
	sec, nsec := in.OperationID.Time().UnixTime()
	for name, c := range map[string]struct{ got, want any }{
		"version":       {in.OperationID.Version(), uuid.Version(7)},
		"variant":       {in.OperationID.Variant(), uuid.RFC4122},
		"time":          {time.Unix(sec, nsec).UTC(), period},
		"the same":      {workflows.ScheduledEInvoiceFetchInput(tenant, connector, period).OperationID, in.OperationID},
		"the connector": {in.ConnectorID, connector},
	} {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", name, c.got, c.want)
		}
	}
	for name, other := range map[string]workflows.EInvoiceFetchInput{
		"another period":    workflows.ScheduledEInvoiceFetchInput(tenant, connector, period.Add(time.Hour)),
		"another connector": workflows.ScheduledEInvoiceFetchInput(tenant, uuid.New(), period),
	} {
		if other.OperationID == in.OperationID {
			t.Errorf("%s: got the same Operation, want another", name)
		}
	}
}

// TestDispatchEInvoiceFetches: once per period the dispatcher starts a
// scheduled fetch for each active einvoice_provider connector of an active
// tenant, not a disabled connector's nor a suspended tenant's; a refused
// start is tried again in the same period; with no period it starts none.
func TestDispatchEInvoiceFetches(t *testing.T) {
	s := einvoiceSetup(t)
	engine := &fakeEngine{}
	now := time.Date(2026, 11, 9, 10, 30, 0, 0, time.UTC)
	d := &workflows.Dispatcher{Pool: pgtest.Pool(t), WorkerRole: "erp_worker", Client: engine, Queue: "erp-test", EInvoices: time.Hour,
		Now: func() time.Time { return now }}
	ctx := context.Background()
	off := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO integration.connector (tenant_id, id, kind, name, settings, secret_ref, status, created_by, updated_by)
		SELECT tenant_id, $2, kind, 'Off', settings, secret_ref, 'disabled', created_by, updated_by FROM integration.connector WHERE id = $1`, s.connector, off)
	period := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	want := workflows.ScheduledEInvoiceFetchWorkflowID(s.e.Tenant.ID, s.connector, period)
	ours := func() []string {
		var ids []string
		for _, st := range engine.started {
			if strings.HasPrefix(st.ID, s.e.Tenant.ID.String()+"-connector-") {
				ids = append(ids, st.ID)
			}
		}
		return ids
	}
	engine.fail = want
	if _, err := d.RunOnce(ctx); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("refused start: got %v, want it reported", err)
	}
	engine.fail = ""
	if _, err := d.RunOnce(ctx); err != nil {
		t.Fatalf("round: %v", err)
	}
	if got := ours(); !slices.Equal(got, []string{want}) {
		t.Fatalf("started: got %v, want %s alone", got, want)
	}
	i := slices.IndexFunc(engine.started, func(st workflow.Start) bool { return st.ID == want })
	if st := engine.started[i]; st.Name != workflows.ScheduledEInvoiceFetchWorkflow || len(st.Args) != 1 ||
		st.Args[0] != workflows.ScheduledEInvoiceFetchInput(s.e.Tenant.ID, s.connector, period) {
		t.Errorf("the start: got %+v, want ScheduledEInvoiceFetch of the connector's period", st)
	}
	if _, err := d.RunOnce(ctx); err != nil || len(ours()) != 1 {
		t.Errorf("same period: got %v %v, want nothing more", ours(), err)
	}
	pgtest.Exec(t, `UPDATE tenancy.tenant SET status = 'suspended', version = version + 1 WHERE id = $1`, s.e.Tenant.ID)
	now = now.Add(time.Hour)
	if _, err := d.RunOnce(ctx); err != nil || len(ours()) != 1 {
		t.Errorf("a suspended tenant: got %v %v, want no fetch", ours(), err)
	}
	pgtest.Exec(t, `UPDATE tenancy.tenant SET status = 'active', version = version + 1 WHERE id = $1`, s.e.Tenant.ID)
	now = now.Add(time.Hour)
	if _, err := d.RunOnce(ctx); err != nil || len(ours()) != 2 {
		t.Errorf("active again, the next period: got %v %v, want a second fetch", ours(), err)
	}
	t.Run("no period", func(t *testing.T) {
		none := *d
		none.EInvoices = 0
		now = now.Add(time.Hour)
		if _, err := none.RunOnce(ctx); err != nil || len(ours()) != 2 {
			t.Errorf("got %v %v, want no fetch", ours(), err)
		}
	})
	t.Run("database failures", func(t *testing.T) {
		pool := pgtest.Pool(t)
		defer func() { d.Begin = nil }()
		for n := 1; n < 20; n++ {
			now = now.Add(time.Hour)
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
				break
			}
			if !errors.Is(err, pgtest.ErrInjected) {
				t.Fatalf("statement %d: %v", n, err)
			}
		}
	})
}

// TestScheduledEInvoiceFetchWaitsWhileSuspended: a scheduled fetch whose
// tenant is suspended as it lists the provider's messages records nothing
// while suspended, hands the step to the wait without spending its tries,
// and records the message once the tenant is active again (po:2026-11-09).
func TestScheduledEInvoiceFetchWaitsWhileSuspended(t *testing.T) {
	s := einvoiceSetup(t)
	s.srv.Deliver("MSG-1", fetchParticipant, einvoicetest.Invoice("INV-1"))
	period := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	in := workflows.ScheduledEInvoiceFetchInput(s.e.Tenant.ID, s.connector, period)
	var mu sync.Mutex
	started := map[string]int{}
	var failed error
	setStatus := func(status string) {
		if _, err := pgtest.Admin(t).Exec(context.Background(), `UPDATE tenancy.tenant SET status = $2, version = version + 1 WHERE id = $1`,
			s.e.Tenant.ID, status); err != nil {
			failed = err
		}
	}
	t.Cleanup(func() { setStatus("active") })
	recordedWhileSuspended := -1
	var suite testsuite.WorkflowTestSuite
	env := workflowEnv(&suite)
	env.RegisterWorkflowWithOptions(workflows.ScheduledEInvoiceFetch, sdkworkflow.RegisterOptions{Name: workflows.ScheduledEInvoiceFetchWorkflow})
	env.RegisterActivity(s.f)
	env.RegisterActivity(&workflows.Tenants{Pool: pgtest.Pool(t), AppRole: "erp_app"})
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: workflows.ScheduledEInvoiceFetchWorkflowID(s.e.Tenant.ID, s.connector, period)})
	env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
		mu.Lock()
		defer mu.Unlock()
		started[info.ActivityType.Name]++
		switch n := started[info.ActivityType.Name]; {
		case info.ActivityType.Name == "PendingEInvoices" && n == 1:
			setStatus("suspended")
		case info.ActivityType.Name == "AwaitActive" && n == 2:
			if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT count(*) FROM einvoicing.einvoice WHERE tenant_id = $1`,
				s.e.Tenant.ID).Scan(&recordedWhileSuspended); err != nil {
				failed = err
			}
			setStatus("active")
		}
	})
	env.ExecuteWorkflow(workflows.ScheduledEInvoiceFetchWorkflow, in)
	if failed != nil {
		t.Fatalf("set the tenant's status: %v", failed)
	}
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("the scheduled fetch: got %v, want nil", err)
	}
	for name, c := range map[string]struct{ got, want int }{
		"attempts of PendingEInvoices (one while suspended, handed to the wait at once, and one after)": {started["PendingEInvoices"], 2},
		"attempts of AwaitActive":            {started["AwaitActive"], 2},
		"documents recorded while suspended": {recordedWhileSuspended, 0},
	} {
		if c.got != c.want {
			t.Errorf("%s: got %d, want %d", name, c.got, c.want)
		}
	}
	if got := fetchOutcome(t, in); got != `succeeded {"changed": 0, "recorded": 1, "already_recorded": 0}` {
		t.Errorf("the fetch once active again: got %s, want MSG-1 recorded", got)
	}
}
