package procurement_test

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// TestContractExpiry: a job expires active contracts past their validity,
// once, as a system actor, with the event and audit entry; drafts and
// contracts still valid are left alone; every failure is reported.
func TestContractExpiry(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	logs := &pgtest.SyncBuffer{}
	at := time.Date(2026, 3, 1, 9, 30, 0, 0, time.UTC)
	x := &procurement.ContractExpirer{Pool: pgtest.Pool(t), AppRole: "erp_app", WorkerRole: "erp_worker", Log: slog.New(slog.NewTextHandler(logs, nil)),
		Settings: configuration.Settings{}, LegalEntities: tenancy.Directory{}, Partners: partner.Payees{}, Now: func() time.Time { return at }}
	// The round is the cell's, and other tests leave due contracts in
	// tenants whose round they made fail: a first round expires them, so
	// the counts below are this tenant's.
	if _, err := x.RunOnce(ctx); err != nil {
		t.Fatalf("clearing round: %v", err)
	}
	past := e.contract(t, e.Supplier.String(), "")
	pastPath := "/v1/contracts/" + id(past)
	e.act(t, pastPath, "release", "")
	current := e.contract(t, e.Vendor.String(), "")
	e.act(t, "/v1/contracts/"+id(current), "release", "")
	draft := e.contract(t, e.Vendor.String(), "")
	// Validity is set by the expirer's clock: the first two end the day
	// before it, the draft too.
	pgtest.Exec(t, `UPDATE procurement.contract SET valid_from = '2025-01-01', valid_to = '2026-02-28', version = version + 1 WHERE id IN ($1, $2)`,
		uuid.MustParse(id(past)), uuid.MustParse(id(draft)))
	// The supplier's name differs from its number, so the event is seen to
	// give each.
	pgtest.Exec(t, `UPDATE partner.business_partner SET name = 'Supplier GmbH', version = version + 1 WHERE id = $1`, e.Supplier)
	round, err := x.RunOnce(ctx)
	if err != nil {
		t.Fatalf("round: %v", err)
	}
	if round.Expired != 1 {
		t.Fatalf("round: got %d expired, want 1", round.Expired)
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
	want(t, "expired event", ev[0], map[string]any{"valid_to": "2026-02-28", "expired_on": "2026-03-01", "time_zone": "UTC",
		"legal_entity_code": "2000", "supplier_number": "BP90000001", "supplier_name": "Supplier GmbH"})
	webhookEvents(t, e, "contract_expired", ev)
	var actor uuid.UUID
	var detail map[string]any
	var occurred time.Time
	if err := pgtest.Admin(t).QueryRow(ctx, `SELECT actor_id, detail, occurred_at FROM audit.audit_event WHERE tenant_id = $1 AND action = 'procurement.contract.expire' AND object_id = $2`,
		e.Tenant.ID, uuid.MustParse(id(past))).Scan(&actor, &detail, &occurred); err != nil {
		t.Fatalf("audit: %v", err)
	}
	if !occurred.Equal(at) {
		t.Errorf("audit time: got %v, want the expirer's clock %v", occurred, at)
	}
	if actor != procurement.ExpirerActor {
		t.Errorf("audit actor: got %v, want %v", actor, procurement.ExpirerActor)
	}
	if detail["from_status"] != "active" {
		t.Errorf("audit from status: got %v, want active", detail["from_status"])
	}
	// Nothing left to do: the next round is quiet.
	round, err = x.RunOnce(ctx)
	if err != nil {
		t.Fatalf("second round: %v", err)
	}
	if round.Expired != 0 {
		t.Fatalf("second round: got %d expired, want 0", round.Expired)
	}
	webhookEvents(t, e, "contract_expired", events(t, e, "contract_expired"))
	if n := len(events(t, e, "contract_expired")); n != 1 {
		t.Errorf("ContractExpired events after the second round: got %d, want 1", n)
	}
	// An expired contract takes no new orders and cannot be terminated.
	e.Problem(409, "procurement.invalid_status", "POST", pastPath+":terminate", `{"reason": "Late"}`, "*")
	// Every statement and transaction failing in turn is reported: the
	// cell's list of tenants, the first tenant's round (the rounds after
	// it go on, so this tenant's contract may expire: it is made active
	// again), and this tenant's round statement by statement (others'
	// contracts share the database).
	e.act(t, "/v1/contracts/"+id(draft), "release", "")
	pool := pgtest.Pool(t)
	for n := 1; n <= 2; n++ {
		count := 0
		x.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, s)
			if err != nil {
				return nil, err
			}
			return &pgtest.FailingTx{Tx: tx, FailAt: n, Count: &count}, nil
		}
		if _, err := x.RunOnce(ctx); !errors.Is(err, pgtest.ErrInjected) {
			t.Fatalf("round, statement %d failing: got %v, want the injected failure", n, err)
		}
	}
	pgtest.Exec(t, `UPDATE procurement.contract SET status = 'active', version = version + 1 WHERE id = $1`, uuid.MustParse(id(draft)))
	x.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errors.New("database down") }
	if _, err := x.RunOnce(ctx); err == nil {
		t.Fatal("round without a database: got nil error, want an error")
	}
	for n := 1; n < 60; n++ {
		count := 0
		x.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, s)
			if err != nil {
				return nil, err
			}
			return &pgtest.FailingTx{Tx: tx, FailAt: n, Count: &count}, nil
		}
		_, err := x.Expire(ctx, e.Tenant.ID)
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
	_, err = x.Expire(ctx, e.Tenant.ID)
	pgtest.WantErr(t, err, "database down")
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

// webhookEvents checks the .v2 events of name, the ones offered to
// webhooks, against the .v1 events v1 of the same round: one each, the same
// payload without the supplier's name (product owner 2026-11-05, ADR-0041),
// the supplier still given by its number. Published refuses a field the
// .v2 schema does not declare, and Conforms the whole payload against it.
func webhookEvents(t *testing.T, e env, name string, v1 []map[string]any) {
	t.Helper()
	typ := outbox.Event{Context: "procurement", Name: name, Version: 2}
	v2 := apitest.Published(t, e.Tenant, typ)
	if len(v2) != len(v1) {
		t.Fatalf("%s.v2: got %d events, want %d, one per .v1 event", name, len(v2), len(v1))
	}
	for i, p := range v2 {
		apitest.Conforms(t, typ, p)
		if _, named := p["supplier_name"]; named {
			t.Errorf("%s.v2 %v: got supplier_name %v, want none", name, p["contract_id"], p["supplier_name"])
		}
		if p["supplier_number"] == nil {
			t.Errorf("%s.v2 %v: got no supplier_number, want the supplier's number", name, p["contract_id"])
		}
		want := maps.Clone(v1[i])
		delete(want, "supplier_name")
		if !reflect.DeepEqual(p, want) {
			t.Errorf("%s.v2: got %v, want the .v1 event without the supplier's name, %v", name, p, want)
		}
	}
}
