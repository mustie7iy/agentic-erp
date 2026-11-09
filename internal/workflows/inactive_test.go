package workflows_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/workflows"
)

// Workers leave the queued work of a tenant that is not active where it
// is and pick it up once the tenant is active again (ADR-0015): requests
// are refused at the door, and a worker must not do what the tenant could
// not ask for.

// resume makes the tenant active again, as the control plane does.
func resume(t *testing.T, tenant uuid.UUID) {
	t.Helper()
	pgtest.Exec(t, `UPDATE tenancy.tenant SET status = 'active', version = version + 1 WHERE id = $1`, tenant)
}

// startedFor counts the engine's starts of the tenant's workflows: every
// workflow id begins with its tenant.
func startedFor(engine *fakeEngine, tenant uuid.UUID) int {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	n := 0
	for _, st := range engine.started {
		if strings.HasPrefix(st.ID, tenant.String()+"-") {
			n++
		}
	}
	return n
}

// dispatched is a tenant with work the dispatcher starts, and the
// dispatcher set up for it; later moves its clock to the next period, for
// the work it starts once a period.
type dispatched struct {
	tenant pgtest.Tenant
	d      *workflows.Dispatcher
	later  func()
}

// dispatcher is a dispatcher with the engine; the test gives it the pool.
func dispatcher(engine *fakeEngine) *workflows.Dispatcher {
	return &workflows.Dispatcher{WorkerRole: "erp_worker", Client: engine, Queue: "erp-test"}
}

// TestDispatcherLeavesInactiveTenants: for each kind of work the
// dispatcher starts, a suspended tenant's is not started nor marked, and
// is started once the tenant is active again; an active tenant's work in
// the same round is started.
func TestDispatcherLeavesInactiveTenants(t *testing.T) {
	cases := map[string]func(t *testing.T, engine *fakeEngine) dispatched{
		"payment proposal": func(t *testing.T, engine *fakeEngine) dispatched {
			t.Helper()
			e := setup(t)
			e.propose(t, "2026-04-15")
			return dispatched{tenant: e.Tenant, d: dispatcher(engine), later: func() {}}
		},
		"depreciation run": func(t *testing.T, engine *fakeEngine) dispatched {
			t.Helper()
			s := deprSetup(t)
			s.start(t, 6)
			return dispatched{tenant: s.e.Tenant, d: dispatcher(engine), later: func() {}}
		},
		"consolidation run": func(t *testing.T, engine *fakeEngine) dispatched {
			t.Helper()
			s := conSetup(t)
			s.start(t)
			return dispatched{tenant: s.e.Tenant, d: dispatcher(engine), later: func() {}}
		},
		"configuration simulation": func(t *testing.T, engine *fakeEngine) dispatched {
			t.Helper()
			e, _, _ := simulationAsked(t)
			return dispatched{tenant: e.Tenant, d: dispatcher(engine), later: func() {}}
		},
		"migration run": func(t *testing.T, engine *fakeEngine) dispatched {
			t.Helper()
			e, _, _, _ := migrationAsked(t, "extract.json")
			return dispatched{tenant: e.Tenant, d: dispatcher(engine), later: func() {}}
		},
		"approval deadline": func(t *testing.T, engine *fakeEngine) dispatched {
			t.Helper()
			e, _, _ := approvalPending(t)
			d := dispatcher(engine)
			d.Deadline, d.Escalations = time.Hour, 1
			return dispatched{tenant: e.Tenant, d: d, later: func() {}}
		},
		"recurring entry": func(t *testing.T, engine *fakeEngine) dispatched {
			t.Helper()
			s := recurSetup(t)
			d := dispatcher(engine)
			d.Recurring, d.Now = s.rc, func() time.Time { return s.now }
			return dispatched{tenant: s.e.Tenant, d: d, later: func() {}}
		},
		"cash snapshot": func(t *testing.T, engine *fakeEngine) dispatched {
			t.Helper()
			s := snapshotSetup(t)
			d := dispatcher(engine)
			d.Snapshots, d.Now = s.sn, func() time.Time { return s.now }
			return dispatched{tenant: s.e.Tenant, d: d, later: func() { s.now = s.now.Add(time.Minute) }}
		},
		"statement fetch": func(t *testing.T, engine *fakeEngine) dispatched {
			t.Helper()
			s := fetchSetup(t)
			now := time.Date(2026, 10, 20, 9, 30, 0, 0, time.UTC)
			d := dispatcher(engine)
			d.Statements, d.Now = time.Hour, func() time.Time { return now }
			return dispatched{tenant: s.e.Tenant, d: d, later: func() { now = now.Add(time.Hour) }}
		},
	}
	for name, setUp := range cases {
		t.Run(name, func(t *testing.T) {
			engine := &fakeEngine{}
			c := setUp(t, engine)
			c.d.Pool = pgtest.Pool(t)
			active := setup(t)
			active.propose(t, "2026-04-15")
			c.tenant.SetStatus(t, "suspended")
			marked := func() int {
				var n int
				pgtest.Scan(t, `SELECT (SELECT count(*) FROM platform.operation WHERE tenant_id = $1 AND workflow_id IS NOT NULL)
					+ (SELECT count(*) FROM workflow.approval_request WHERE tenant_id = $1 AND workflow_id IS NOT NULL)`, []any{c.tenant.ID}, &n)
				return n
			}
			before := marked()
			ctx := context.Background()
			if _, err := c.d.RunOnce(ctx); err != nil {
				t.Fatalf("round while suspended: %v", err)
			}
			if n := startedFor(engine, c.tenant.ID); n != 0 {
				t.Errorf("started for the suspended tenant: got %d, want 0", n)
			}
			if got := marked(); got != before {
				t.Errorf("the suspended tenant's work marked as started: got %d, want %d", got, before)
			}
			if n := startedFor(engine, active.Tenant.ID); n != 1 {
				t.Errorf("started for the active tenant in the same round: got %d, want 1", n)
			}
			resume(t, c.tenant.ID)
			c.later()
			if _, err := c.d.RunOnce(ctx); err != nil {
				t.Fatalf("round once active again: %v", err)
			}
			if n := startedFor(engine, c.tenant.ID); n == 0 {
				t.Error("started for the resumed tenant: got none, want its work started")
			}
		})
	}
}

// TestRunnersLeaveInactiveTenants: without a workflow engine, each
// in-process runner leaves a suspended tenant's work as it was and does
// it once the tenant is active again.
func TestRunnersLeaveInactiveTenants(t *testing.T) {
	ctx := context.Background()
	t.Run("depreciation runs", func(t *testing.T) {
		s := deprSetup(t)
		in := s.start(t, 6)
		w := &workflows.Depreciator{Depreciations: s.d}
		left := deprStateOf(t, in)
		s.e.Tenant.SetStatus(t, "suspended")
		if _, err := w.RunOnce(ctx); err != nil {
			t.Fatalf("round while suspended: %v", err)
		}
		if got := deprStateOf(t, in); got != left {
			t.Errorf("the suspended tenant's run: got %+v, want it as it was, %+v", got, left)
		}
		resume(t, s.e.Tenant.ID)
		if _, err := w.RunOnce(ctx); err != nil {
			t.Fatalf("round once active again: %v", err)
		}
		if got := deprStateOf(t, in); !strings.HasPrefix(got.Run, "posted/") {
			t.Errorf("the resumed tenant's run: got %s, want it posted", got.Run)
		}
	})
	t.Run("consolidation runs", func(t *testing.T) {
		s := conSetup(t)
		in := s.start(t)
		w := &workflows.Consolidator{Consolidations: s.c}
		left := conStateOf(t, in)
		s.e.Tenant.SetStatus(t, "suspended")
		if _, err := w.RunOnce(ctx); err != nil {
			t.Fatalf("round while suspended: %v", err)
		}
		if got := conStateOf(t, in); got != left {
			t.Errorf("the suspended tenant's run: got %+v, want it as it was, %+v", got, left)
		}
		resume(t, s.e.Tenant.ID)
		if _, err := w.RunOnce(ctx); err != nil {
			t.Fatalf("round once active again: %v", err)
		}
		if got := conStateOf(t, in); !strings.HasPrefix(got.Run, "computed/") {
			t.Errorf("the resumed tenant's run: got %s, want it computed", got.Run)
		}
	})
	t.Run("recurring entries", func(t *testing.T) {
		s := recurSetup(t)
		w := &workflows.Recurrer{Recurrences: s.rc, Now: func() time.Time { return s.now }}
		s.e.Tenant.SetStatus(t, "suspended")
		if _, err := w.RunOnce(ctx); err != nil {
			t.Fatalf("round while suspended: %v", err)
		}
		if n := s.entries(t, "2026-01-01"); n != 0 {
			t.Errorf("the suspended tenant's entries: got %d, want none", n)
		}
		resume(t, s.e.Tenant.ID)
		if _, err := w.RunOnce(ctx); err != nil {
			t.Fatalf("round once active again: %v", err)
		}
		if n := s.entries(t, "2026-01-01"); n != 1 {
			t.Errorf("the resumed tenant's entries: got %d, want 1", n)
		}
	})
	t.Run("cash snapshots", func(t *testing.T) {
		s := snapshotSetup(t)
		w := &workflows.Snapshotter{Snapshots: s.sn, Now: func() time.Time { return s.now }}
		s.e.Tenant.SetStatus(t, "suspended")
		if _, err := w.RunOnce(ctx); err != nil {
			t.Fatalf("round while suspended: %v", err)
		}
		if n := s.snapshots(t, "2026-04-17"); n != 0 {
			t.Errorf("the suspended tenant's snapshots: got %d, want none", n)
		}
		resume(t, s.e.Tenant.ID)
		if _, err := w.RunOnce(ctx); err != nil {
			t.Fatalf("round once active again: %v", err)
		}
		if n := s.snapshots(t, "2026-04-17"); n != 1 {
			t.Errorf("the resumed tenant's snapshots: got %d, want 1", n)
		}
	})
	t.Run("configuration simulations", func(t *testing.T) {
		e, in, _ := simulationAsked(t)
		w := simulations(t).Worker
		status := func() string {
			var s string
			pgtest.Scan(t, `SELECT status FROM platform.operation WHERE id = $1`, []any{in.OperationID}, &s)
			return s
		}
		e.Tenant.SetStatus(t, "suspended")
		if _, err := w.RunOnce(ctx); err != nil {
			t.Fatalf("round while suspended: %v", err)
		}
		if got := status(); got != "queued" {
			t.Errorf("the suspended tenant's simulation: got %s, want queued", got)
		}
		resume(t, e.Tenant.ID)
		if _, err := w.RunOnce(ctx); err != nil {
			t.Fatalf("round once active again: %v", err)
		}
		if got := status(); got != "succeeded" {
			t.Errorf("the resumed tenant's simulation: got %s, want succeeded", got)
		}
	})
}
