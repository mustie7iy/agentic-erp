package procurement_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
)

// TestContractExpiry: a job expires active contracts past their validity,
// once, as a system actor, with the event and audit entry; drafts and
// contracts still valid are left alone; every failure is reported.
func TestContractExpiry(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	past := e.contract(t, e.Supplier.String(), "")
	pastPath := "/v1/contracts/" + id(past)
	e.act(t, pastPath, "release", "")
	current := e.contract(t, e.Vendor.String(), "")
	e.act(t, "/v1/contracts/"+id(current), "release", "")
	draft := e.contract(t, e.Vendor.String(), "")
	// Validity is set by the calendar: the first two end yesterday, the draft too.
	pgtest.Exec(t, `UPDATE procurement.contract SET valid_from = current_date - 400, valid_to = current_date - 1, version = version + 1 WHERE id IN ($1, $2)`, uuid.MustParse(id(past)), uuid.MustParse(id(draft)))
	logs := &syncBuffer{}
	x := &procurement.ContractExpirer{Pool: pgtest.Pool(t), AppRole: "erp_app", WorkerRole: "erp_worker", Log: slog.New(slog.NewTextHandler(logs, nil))}
	n, err := x.RunOnce(ctx)
	if err != nil || n != 1 {
		t.Fatalf("round: %d expired, %v", n, err)
	}
	if got, _ := e.Must(200, "GET", pastPath, "", ""); got["status"] != "expired" {
		t.Fatalf("past contract: %v", got)
	}
	for _, c := range []struct{ id, want string }{{id(current), "active"}, {id(draft), "draft"}} {
		if got, _ := e.Must(200, "GET", "/v1/contracts/"+c.id, "", ""); got["status"] != c.want {
			t.Fatalf("contract %s: %v", c.id, got)
		}
	}
	if ev := events(t, e, "contract_expired"); len(ev) != 1 || ev[0]["contract_id"] != id(past) || ev[0]["previous_status"] != "active" || ev[0]["status"] != "expired" ||
		ev[0]["consumed_value"].(map[string]any)["amount"] != "0.00" || ev[0]["valid_to"] == nil {
		t.Fatalf("event: %v", ev)
	}
	var actor uuid.UUID
	var detail map[string]any
	if err := pgtest.Admin(t).QueryRow(ctx, `SELECT actor_id, detail FROM audit.audit_event WHERE tenant_id = $1 AND action = 'procurement.contract.expire' AND object_id = $2`,
		e.Tenant.ID, uuid.MustParse(id(past))).Scan(&actor, &detail); err != nil || actor != procurement.ExpirerActor || detail["from_status"] != "active" {
		t.Fatalf("audit: %v %v %v", actor, detail, err)
	}
	// Nothing left to do: the next round is quiet.
	if n, err := x.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("second round: %d, %v", n, err)
	}
	// An expired contract takes no new orders and cannot be terminated.
	e.Problem(409, "procurement.invalid_status", "POST", pastPath+":terminate", `{"reason": "Late"}`, "*")
	// Every statement and transaction failing in turn is reported.
	e.act(t, "/v1/contracts/"+id(draft), "release", "")
	pool := pgtest.Pool(t)
	for n := 1; n < 40; n++ {
		count := 0
		x.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, s)
			if err != nil {
				return nil, err
			}
			return &pgtest.FailingTx{Tx: tx, FailAt: n, Count: &count}, nil
		}
		_, err := x.RunOnce(ctx)
		if err == nil {
			if n == 1 {
				t.Fatal("expired without a statement to fail")
			}
			break
		}
		if !errors.Is(err, pgtest.ErrInjected) {
			t.Fatalf("statement %d: %v", n, err)
		}
		if got, _ := e.Must(200, "GET", "/v1/contracts/"+id(draft), "", ""); got["status"] != "active" {
			t.Fatalf("statement %d: half expired: %v", n, got)
		}
	}
	if got, _ := e.Must(200, "GET", "/v1/contracts/"+id(draft), "", ""); got["status"] != "expired" {
		t.Fatalf("after the failures: %v", got)
	}
	x.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		tx, err := pool.Begin(ctx, s)
		if err != nil {
			return nil, err
		}
		return &brokenCommit{Tx: tx}, nil
	}
	pgtest.Exec(t, `UPDATE procurement.contract SET status = 'active', version = version + 1 WHERE id = $1`, uuid.MustParse(id(draft)))
	if _, err := x.Expire(ctx, e.Tenant.ID); err == nil || !strings.Contains(err.Error(), "commit failed") {
		t.Fatalf("commit failing: %v", err)
	}
	x.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errors.New("database down") }
	if _, err := x.Expire(ctx, e.Tenant.ID); err == nil || !strings.Contains(err.Error(), "database down") {
		t.Fatalf("expire with the database down: %v", err)
	}
	// Run logs a failed round and a quiet one, and stops with the context.
	runCtx, cancel := context.WithCancel(ctx)
	failed := make(chan struct{})
	go func() { x.Run(runCtx); close(failed) }()
	pgtest.Eventually(t, 10*time.Second, func() bool { return strings.Contains(logs.String(), "contract expiry round failed") },
		"no failed round logged: %s", logs)
	cancel()
	<-failed // the worker reads x.Begin, which the test changes next
	x.Begin = nil
	x.Interval = time.Hour
	runCtx, cancel = context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { x.Run(runCtx); close(done) }()
	pgtest.Eventually(t, 10*time.Second, func() bool { return strings.Contains(logs.String(), "expired=1") },
		"no expiring round logged: %s", logs)
	cancel()
	<-done
}

// syncBuffer collects the worker's logs from its goroutine.
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

type brokenCommit struct{ pgx.Tx }

func (b *brokenCommit) Commit(ctx context.Context) error {
	_ = b.Rollback(ctx)
	return errors.New("commit failed")
}
