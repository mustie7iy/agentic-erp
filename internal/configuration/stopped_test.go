package configuration_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// stops lists the config.change_request.simulate_failed audit entries of
// an object, each as "object type, code, operation".
func stops(t *testing.T, tenant, object uuid.UUID) []string {
	t.Helper()
	var got []string
	pgtest.Scan(t, `SELECT coalesce(array_agg(object_type || ' ' || coalesce(detail->>'code', '') || ' ' || coalesce(detail->>'operation_id', '') ORDER BY seq), '{}')
		FROM audit.audit_event WHERE tenant_id = $1 AND object_id = $2 AND action = 'config.change_request.simulate_failed'`, []any{tenant, object}, &got)
	return got
}

// opStatus is an operation's status and, when it failed, its problem's code.
func opStatus(t *testing.T, op uuid.UUID) string {
	t.Helper()
	var got string
	pgtest.Scan(t, `SELECT status || ' ' || coalesce(error->>'code', '') FROM platform.operation WHERE id = $1`, []any{op}, &got)
	return got
}

// queuedOp queues a config.simulate operation with the given request, as
// only a damaged or hand-made row would name it.
func (s sim) queuedOp(t *testing.T, request string) uuid.UUID {
	t.Helper()
	op := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO platform.operation (tenant_id, id, kind, status, request, created_by, updated_by) VALUES ($1, $2, $3, 'queued', $4, $5, $5)`,
		s.Tenant.ID, op, configuration.SimulateKind, request, s.Tenant.Human.ID)
	return op
}

// TestSimulationFailuresAudited (review:T57f, ADR-0011): every problem that
// fails a simulation's operation is recorded once, in the job's
// transaction, as config.change_request.simulate_failed on the change
// request (on the operation when it names none), with the problem's code
// and the operation; run again, the job is done and records nothing more.
// A transient error records nothing and leaves the operation queued.
func TestSimulationFailuresAudited(t *testing.T) {
	s := sim{fixture.New(t, append(perms, "platform.operation.read")...)}
	s.suite(t)
	ctx := context.Background()
	type setup struct {
		object, op uuid.UUID
		w          *configuration.SimulationWorker
	}
	passing := func() *fakeSimulator {
		return &fakeSimulator{sim: configuration.Simulation{Passed: true, Summary: "fine", Detail: map[string]any{}}}
	}
	queued := func(t *testing.T, rate string) (uuid.UUID, uuid.UUID) {
		t.Helper()
		cr, op := s.in(t).tier3(t, rate)
		return uuid.MustParse(apitest.String(t, cr, "id")), uuid.MustParse(apitest.String(t, op, "id"))
	}
	for _, c := range []struct {
		name, kind, code string
		setup            func(t *testing.T) setup
	}{
		{"no change request named", "platform.operation", "config.invalid_request", func(t *testing.T) setup {
			t.Helper()
			op := s.queuedOp(t, `{}`)
			return setup{op, op, worker(t, passing())}
		}},
		{"change request gone", "config.change_request", "request.not_found", func(t *testing.T) setup {
			t.Helper()
			cr := uuid.Must(uuid.NewV7())
			return setup{cr, s.queuedOp(t, `{"change_request_id": "`+cr.String()+`"}`), worker(t, passing())}
		}},
		{"no longer awaiting evidence", "config.change_request", "config.invalid_status", func(t *testing.T) setup {
			t.Helper()
			cr, op := queued(t, "21")
			path := "/v1/change-requests/" + cr.String()
			_, tag := s.in(t).Must(200, "GET", path, "", "")
			s.in(t).Must(200, "POST", path+":withdraw", `{"reason": "Changed my mind"}`, tag)
			return setup{cr, op, worker(t, passing())}
		}},
		{"proposal this replica cannot read", "config.change_request", "config.unknown_keys", func(t *testing.T) setup {
			t.Helper()
			cr, op := queued(t, "22")
			newer(t, cr.String())
			return setup{cr, op, worker(t, passing())}
		}},
		{"simulator refuses", "config.change_request", "config.unreplayable", func(t *testing.T) setup {
			t.Helper()
			cr, op := queued(t, "23")
			return setup{cr, op, worker(t, &fakeSimulator{err: problem.Unprocessable("config.unreplayable", "The proposal removes company 2000.")})}
		}},
		{"regression refuses", "config.change_request", "config.unreplayable_suite", func(t *testing.T) setup {
			t.Helper()
			cr, op := queued(t, "24")
			w := worker(t, passing())
			w.Dependencies.Regressor = &fakeRegressor{errs: map[string]error{"24": problem.Unprocessable("config.unreplayable_suite", "A case cannot be replayed.")}}
			return setup{cr, op, w}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := c.setup(t)
			if did, err := got.w.Simulate(ctx, s.Tenant.ID, got.op); !did || err != nil {
				t.Fatalf("simulate: got %t %v, want the job done", did, err)
			}
			if status := opStatus(t, got.op); status != "failed "+c.code {
				t.Errorf("operation: got %q, want failed with %s", status, c.code)
			}
			want := c.kind + " " + c.code + " " + got.op.String()
			if entries := stops(t, s.Tenant.ID, got.object); len(entries) != 1 || entries[0] != want {
				t.Errorf("audit entries: got %q, want one %q", entries, want)
			}
			if did, err := got.w.Simulate(ctx, s.Tenant.ID, got.op); did || err != nil {
				t.Errorf("run again: got %t %v, want nothing to do", did, err)
			}
			if entries := stops(t, s.Tenant.ID, got.object); len(entries) != 1 {
				t.Errorf("audit entries after running again: got %q, want still one", entries)
			}
		})
	}
	for _, c := range []struct {
		name, rate string
		err        error
	}{
		{"transient error", "25", errors.New("database hiccup")},
		{"server-side problem", "27", problem.New(http.StatusServiceUnavailable, "request.unavailable", "Unavailable", "Try again.")},
	} {
		t.Run(c.name, func(t *testing.T) {
			cr, op := queued(t, c.rate)
			if _, err := worker(t, &fakeSimulator{err: c.err}).Simulate(ctx, s.Tenant.ID, op); err == nil {
				t.Fatal("simulate: got no error, want the transient one")
			}
			if status := opStatus(t, op); status != "queued " {
				t.Errorf("operation: got %q, want queued", status)
			}
			if entries := stops(t, s.Tenant.ID, cr); len(entries) != 0 {
				t.Errorf("audit entries: got %q, want none", entries)
			}
		})
	}
}

// TestSimulationFailureAuditFails: any statement of a stopped simulation's
// job, the audit entry's among them, can fail; the job then leaves the
// operation queued with no entry, for another attempt, until one round
// fails it with its one entry.
func TestSimulationFailureAuditFails(t *testing.T) {
	s := sim{fixture.New(t, append(perms, "platform.operation.read")...)}
	s.suite(t)
	ctx := context.Background()
	crm, opm := s.tier3(t, "26")
	cr, op := uuid.MustParse(apitest.String(t, crm, "id")), uuid.MustParse(apitest.String(t, opm, "id"))
	pool := pgtest.Pool(t)
	for failAt := 1; ; failAt++ {
		w := worker(t, &fakeSimulator{err: problem.Unprocessable("config.unreplayable", "The proposal removes company 2000.")})
		w.Begin = func(ctx context.Context, sc database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, sc)
			if err != nil {
				return tx, err
			}
			return &pgtest.FailingTx{Tx: tx, FailAt: failAt}, nil
		}
		_, err := w.Simulate(ctx, s.Tenant.ID, op)
		if err == nil {
			if status := opStatus(t, op); status != "failed config.unreplayable" {
				t.Errorf("after %d failures: got operation %q, want failed", failAt-1, status)
			}
			if entries := stops(t, s.Tenant.ID, cr); len(entries) != 1 {
				t.Errorf("after %d failures: got audit entries %q, want one", failAt-1, entries)
			}
			return
		}
		if !errors.Is(err, pgtest.ErrInjected) {
			t.Fatalf("statement %d: got %v, want %v", failAt, err, pgtest.ErrInjected)
		}
		if status := opStatus(t, op); status != "queued " {
			t.Fatalf("statement %d: got operation %q, want queued", failAt, status)
		}
		if entries := stops(t, s.Tenant.ID, cr); len(entries) != 0 {
			t.Fatalf("statement %d: got audit entries %q, want none", failAt, entries)
		}
		if failAt > 100 {
			t.Fatalf("statement %d: still failing, want a round to succeed within 100 statements", failAt)
		}
	}
}
