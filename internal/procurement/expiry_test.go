package procurement_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
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
	logs := &pgtest.SyncBuffer{}
	x := &procurement.ContractExpirer{Pool: pgtest.Pool(t), AppRole: "erp_app", WorkerRole: "erp_worker", Log: slog.New(slog.NewTextHandler(logs, nil))}
	n, err := x.RunOnce(ctx)
	if err != nil {
		t.Fatalf("round: %v", err)
	}
	if n != 1 {
		t.Fatalf("round: got %d expired, want 1", n)
	}
	if got, _ := e.Must(200, "GET", pastPath, "", ""); got["status"] != "expired" {
		t.Fatalf("past contract: %v", got)
	}
	for _, c := range []struct{ id, want string }{{id(current), "active"}, {id(draft), "draft"}} {
		if got, _ := e.Must(200, "GET", "/v1/contracts/"+c.id, "", ""); got["status"] != c.want {
			t.Errorf("contract %s status: got %v, want %s", c.id, got["status"], c.want)
		}
	}
	ev := events(t, e, "contract_expired")
	if len(ev) != 1 {
		t.Fatalf("expired events: got %d, want 1: %v", len(ev), ev)
	}
	if ev[0]["contract_id"] != id(past) {
		t.Errorf("expired event contract: got %v, want %s", ev[0]["contract_id"], id(past))
	}
	if ev[0]["previous_status"] != "active" {
		t.Errorf("expired event previous status: got %v, want active", ev[0]["previous_status"])
	}
	if ev[0]["status"] != "expired" {
		t.Errorf("expired event status: got %v, want expired", ev[0]["status"])
	}
	if got := ev[0]["consumed_value"].(map[string]any)["amount"]; got != "0.00" {
		t.Errorf("expired event consumed value: got %v, want 0.00", got)
	}
	if ev[0]["valid_to"] == nil {
		t.Errorf("expired event valid to: got none, want a date")
	}
	var actor uuid.UUID
	var detail map[string]any
	if err := pgtest.Admin(t).QueryRow(ctx, `SELECT actor_id, detail FROM audit.audit_event WHERE tenant_id = $1 AND action = 'procurement.contract.expire' AND object_id = $2`,
		e.Tenant.ID, uuid.MustParse(id(past))).Scan(&actor, &detail); err != nil {
		t.Fatalf("audit: %v", err)
	}
	if actor != procurement.ExpirerActor {
		t.Errorf("audit actor: got %v, want %v", actor, procurement.ExpirerActor)
	}
	if detail["from_status"] != "active" {
		t.Errorf("audit from status: got %v, want active", detail["from_status"])
	}
	// Nothing left to do: the next round is quiet.
	n, err = x.RunOnce(ctx)
	if err != nil {
		t.Fatalf("second round: %v", err)
	}
	if n != 0 {
		t.Fatalf("second round: got %d expired, want 0", n)
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
		return &pgtest.FailingTx{Tx: tx, FailCommit: true}, nil
	}
	pgtest.Exec(t, `UPDATE procurement.contract SET status = 'active', version = version + 1 WHERE id = $1`, uuid.MustParse(id(draft)))
	if _, err := x.Expire(ctx, e.Tenant.ID); !errors.Is(err, pgtest.ErrInjected) {
		t.Fatalf("commit failing: got %v, want the injected failure", err)
	}
	x.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errors.New("database down") }
	if _, err := x.Expire(ctx, e.Tenant.ID); err == nil || !strings.Contains(err.Error(), "database down") {
		t.Fatalf("expire with the database down: %v", err)
	}
	// Run logs a failed round and a quiet one, and stops with the context.
	failCtx, stopFailing := context.WithCancel(ctx)
	failed := make(chan struct{})
	go func() { x.Run(failCtx); close(failed) }()
	t.Cleanup(func() { stopFailing(); <-failed }) // stops the worker should the wait fail
	pgtest.Eventually(t, 10*time.Second, func() bool { return strings.Contains(logs.String(), "contract expiry round failed") },
		"no failed round logged: %s", logs)
	stopFailing()
	<-failed // the worker reads x.Begin, which the test changes next
	x.Begin = nil
	x.Interval = time.Hour
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { x.Run(runCtx); close(done) }()
	t.Cleanup(func() { cancel(); <-done }) // stops the worker should the wait fail
	pgtest.Eventually(t, 10*time.Second, func() bool { return strings.Contains(logs.String(), "expired=1") },
		"no expiring round logged: %s", logs)
	cancel()
	<-done
}
