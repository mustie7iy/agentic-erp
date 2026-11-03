package workflows_test

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
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/cash"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/workflows"
)

// Snapshots of the liquidity forecast (ADR-0051 §6, T53f): one workflow
// per company and date takes one daily snapshot, however often it runs,
// and deletes the company's snapshots past their retention.

type snapshotScenario struct {
	e    fixture.Env
	sn   *workflows.Snapshots
	now  time.Time
	le   uuid.UUID
	item uuid.UUID // a supplier open item of Maple Inc, due 30 April
}

// snapshotSetup is Maple Inc managing cash (a liquidity item for supplier
// payments) with one supplier invoice open, its clock at 15:00 UTC on
// Friday 17 April 2026, past its snapshot time (07:00 in UTC, the
// fixture's zone).
func snapshotSetup(t *testing.T) *snapshotScenario {
	t.Helper()
	e := fixture.NewWith(t, []fixture.Company{fixture.MapleCompany}, func(doc *config.Config) {
		fixture.Maple(doc)
		doc.Cash = config.Cash{LiquidityItems: []config.LiquidityItem{{Code: "OP_SUPPLIERS", Name: "Supplier payments", Direction: config.DirectionOutflow}},
			Derivation: config.Derivation{Defaults: config.DerivationDefaults{Outflow: "OP_SUPPLIERS"}}}
	})
	s := &snapshotScenario{e: e, now: time.Date(2026, 4, 17, 15, 0, 0, 0, time.UTC)}
	pgtest.Scan(t, `SELECT id FROM tenancy.legal_entity WHERE tenant_id = $1 AND code = '3000'`, []any{e.Tenant.ID}, &s.le)
	s.item = s.openItem(t)
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	s.sn = &workflows.Snapshots{Pool: pgtest.Pool(t), AppRole: "erp_app", WorkerRole: "erp_worker",
		Cash: app.Cash(app.Config{Pool: pgtest.Pool(t), AppRole: "erp_app", Pages: pages, Now: func() time.Time { return s.now }})}
	return s
}

// openItem writes a CAD 1,000.00 supplier invoice of 31 March, due 30
// April, straight into Maple Inc's leading ledger.
func (s *snapshotScenario) openItem(t *testing.T) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	var ledger uuid.UUID
	pgtest.Scan(t, `SELECT id FROM ledger.ledger WHERE tenant_id = $1 AND code = '0L'`, []any{s.e.Tenant.ID}, &ledger)
	payables, expense := s.e.Account(t, "3000", "211000"), s.e.Account(t, "3000", "520000")
	admin := pgtest.Admin(t)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(context.Background(), sql, args...); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
	}
	exec(`SET session_replication_role = replica`)
	defer exec(`RESET session_replication_role`)
	exec(`INSERT INTO ledger.journal_entry (tenant_id, fiscal_year, id, legal_entity_id, ledger_id, fiscal_period, document_type, document_number,
			document_date, posting_date, currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc, source_context, source_document_id, posted_by)
		VALUES ($1, 2026, $2, $3, $4, 3, 'KR', 'KR-1', '2026-03-31', '2026-03-31', 'CAD', 'CAD', 'EUR', 1, 1, 'payables', gen_random_uuid(), $5)`,
		s.e.Tenant.ID, id, s.le, ledger, s.e.Tenant.Human.ID)
	exec(`INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period, posting_date,
			account_id, partner_id, amount_tc, amount_lc, amount_gc)
		VALUES ($1, 2026, $2, 1, $3, $4, 3, '2026-03-31', $5, $6, -100000, -100000, -100000),
		       ($1, 2026, $2, 2, $3, $4, 3, '2026-03-31', $7, NULL, 100000, 100000, 100000)`, s.e.Tenant.ID, id, s.le, ledger, payables, s.e.Supplier, expense)
	return id
}

func (s *snapshotScenario) in(date string) workflows.SnapshotInput {
	return workflows.SnapshotInput{TenantID: s.e.Tenant.ID, LegalEntityID: s.le, Date: date}
}

// snapshots counts the company's daily snapshots of the date.
func (s *snapshotScenario) snapshots(t *testing.T, date string) int {
	t.Helper()
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM cash.snapshot WHERE legal_entity_id = $1 AND snapshot_date = $2 AND kind = 'daily'`, []any{s.le, date}, &n)
	return n
}

func (s *snapshotScenario) run(t *testing.T, in workflows.SnapshotInput) error {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := workflowEnv(&suite)
	env.RegisterWorkflow(workflows.TakeCashSnapshot)
	env.RegisterActivity(s.sn)
	env.ExecuteWorkflow(workflows.TakeCashSnapshot, in)
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	return env.GetWorkflowError()
}

// TestTakeCashSnapshot: the workflow run twice for a company and date
// keeps one snapshot, with the supplier invoice at document level; a date
// that does not parse fails for good.
func TestTakeCashSnapshot(t *testing.T) {
	s := snapshotSetup(t)
	for range 2 {
		if err := s.run(t, s.in("2026-04-17")); err != nil {
			t.Fatalf("workflow: %v", err)
		}
	}
	if n := s.snapshots(t, "2026-04-17"); n != 1 {
		t.Errorf("snapshots after two runs: got %d, want 1", n)
	}
	var doc uuid.UUID
	var amount int64
	pgtest.Scan(t, `SELECT f.document_id, f.amount FROM cash.snapshot_flow f JOIN cash.snapshot s ON s.tenant_id = f.tenant_id AND s.id = f.snapshot_id
		WHERE s.legal_entity_id = $1`, []any{s.le}, &doc, &amount)
	if doc != s.item || amount != -100000 {
		t.Errorf("the invoice's line: got %s %d, want %s -100000", doc, amount, s.item)
	}
	for _, date := range []string{"17 April", "2026-02-30"} {
		t.Run("an unreadable date "+date, func(t *testing.T) {
			err := s.run(t, s.in(date))
			var app *temporal.ApplicationError
			if !errors.As(err, &app) || !app.NonRetryable() || app.Type() != "InvalidDate" {
				t.Errorf("got %v, want a non-retryable InvalidDate", err)
			}
		})
	}
	t.Run("an unreadable date to prune", func(t *testing.T) {
		if _, err := s.sn.PruneSnapshots(context.Background(), s.in("17 April")); err == nil {
			t.Error("got no error, want the date refused")
		}
	})
}

// TestTakeSnapshotTwice: the activities run twice, as after a completion
// the engine lost: one snapshot, one event, audited as the snapshotter;
// the old snapshot deleted once.
func TestTakeSnapshotTwice(t *testing.T) {
	s := snapshotSetup(t)
	ctx := context.Background()
	for i, want := range []string{cash.SnapshotTaken, cash.SnapshotExists} {
		got, err := s.sn.TakeSnapshot(ctx, s.in("2026-04-17"))
		if err != nil || got != want {
			t.Fatalf("run %d: got %s %v, want %s", i+1, got, err, want)
		}
	}
	if n := len(apitest.Published(t, s.e.Tenant, outbox.Event{Context: "cash", Name: "cash_snapshot_taken"})); n != 1 {
		t.Errorf("events: got %d, want 1", n)
	}
	var actor string
	pgtest.Scan(t, `SELECT actor_id::text FROM audit.audit_event WHERE tenant_id = $1 AND action = 'cash.snapshot.take'`, []any{s.e.Tenant.ID}, &actor)
	if actor != cash.SnapshotterActor.String() {
		t.Errorf("audited actor: got %s, want the snapshotter", actor)
	}
	old := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO cash.snapshot (tenant_id, id, legal_entity_id, kind, snapshot_date, time_zone, horizon_end, config_version, taken_by)
		VALUES ($1, $2, $3, 'daily', '2024-03-16', 'UTC', '2025-03-16', 1, $4)`, s.e.Tenant.ID, old, s.le, cash.SnapshotterActor)
	for i, want := range []int{1, 0} {
		got, err := s.sn.PruneSnapshots(ctx, s.in("2026-04-17"))
		if err != nil || got != want {
			t.Fatalf("prune %d: got %d %v, want %d", i+1, got, err, want)
		}
	}
	var deleted int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'cash.snapshot.delete' AND object_id = $2`,
		[]any{s.e.Tenant.ID, old}, &deleted)
	if deleted != 1 {
		t.Errorf("deletions audited: got %d, want 1", deleted)
	}
}

// TestDispatchSnapshots: the dispatcher starts one workflow per company
// and date due, named by them, once a minute at most; a refused start is
// reported and tried again the next round.
func TestDispatchSnapshots(t *testing.T) {
	s := snapshotSetup(t)
	engine := &fakeEngine{}
	d := &workflows.Dispatcher{Pool: pgtest.Pool(t), WorkerRole: "erp_worker", Client: engine, Queue: "erp-test", Snapshots: s.sn,
		Now: func() time.Time { return s.now }}
	ctx := context.Background()
	id := workflows.SnapshotWorkflowID(s.in("2026-04-17"))
	if want := fmt.Sprintf("%s-legal_entity-%s-cash_snapshot-2026-04-17", s.e.Tenant.ID, s.le); id != want {
		t.Errorf("workflow id: got %s, want %s", id, want)
	}
	ours := func() int {
		n := 0
		for _, st := range engine.started {
			if st.ID == id {
				n++
			}
		}
		return n
	}
	engine.fail = id
	if _, err := d.RunOnce(ctx); err == nil || !strings.Contains(err.Error(), id) {
		t.Fatalf("refused start: got %v, want it reported", err)
	}
	engine.fail = ""
	for range 2 {
		if _, err := d.RunOnce(ctx); err != nil {
			t.Fatalf("round: %v", err)
		}
	}
	if n := ours(); n != 1 {
		t.Errorf("started: got %d, want once", n)
	}
	s.now = s.now.Add(time.Minute)
	if _, err := s.sn.TakeSnapshot(ctx, s.in("2026-04-17")); err != nil {
		t.Fatal(err)
	}
	if _, err := d.RunOnce(ctx); err != nil {
		t.Fatalf("round after the snapshot: %v", err)
	}
	if n := ours(); n != 1 {
		t.Errorf("started after the snapshot: got %d, want still once", n)
	}
	t.Run("database failures", func(t *testing.T) {
		pool := pgtest.Pool(t)
		defer func() { s.sn.Begin = nil }()
		for n := 1; n < 40; n++ {
			s.now = s.now.Add(time.Minute)
			count := 0
			s.sn.Begin = func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
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
		s.sn.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errors.New("database down") }
		if _, err := s.sn.Due(ctx, s.now); err == nil || !strings.Contains(err.Error(), "database down") {
			t.Errorf("the worker's transaction failing: got %v, want it reported", err)
		}
	})
}

// TestSnapshotter: without a workflow engine the in-process worker takes
// every due snapshot, and a second round takes none.
func TestSnapshotter(t *testing.T) {
	s := snapshotSetup(t)
	logs := &pgtest.SyncBuffer{}
	w := &workflows.Snapshotter{Snapshots: s.sn, Now: func() time.Time { return s.now }, Interval: 10 * time.Millisecond,
		Log: slog.New(slog.NewTextHandler(logs, nil))}
	ctx := context.Background()
	if n, err := w.RunOnce(ctx); err != nil || n < 1 {
		t.Fatalf("first round: got %d %v, want at least this tenant's snapshots", n, err)
	}
	if n := s.snapshots(t, "2026-04-17"); n != 1 {
		t.Errorf("snapshots: got %d, want 1", n)
	}
	t.Run("a failing round", func(t *testing.T) {
		s.now = s.now.AddDate(0, 0, 1)
		calls := 0
		s.sn.Begin = func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
			if scope.TenantID == s.e.Tenant.ID {
				calls++
			}
			if calls == 2 { // the tenant's due snapshots, then the activity
				return nil, errors.New("database down")
			}
			return pgtest.Pool(t).Begin(ctx, scope)
		}
		defer func() { s.sn.Begin = nil }()
		if _, err := w.RunOnce(ctx); err == nil || !strings.Contains(err.Error(), "database down") {
			t.Errorf("got %v, want the failure reported", err)
		}
	})
	t.Run("run loop", func(t *testing.T) {
		s.now = s.now.AddDate(0, 0, 1)
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { w.Run(runCtx); close(done) }()
		t.Cleanup(func() { cancel(); <-done })
		pgtest.Eventually(t, 10*time.Second, func() bool { return strings.Contains(logs.String(), "cash snapshots taken") },
			"the worker did not log the snapshots it took: %s", logs)
		cancel()
		<-done
	})
}
