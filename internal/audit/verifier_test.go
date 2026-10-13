package audit_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestScheduledVerification: the worker verifies every tenant's chain and
// anchors in rounds, records each round, and raises the alarm once when a
// chain stops verifying.
func TestScheduledVerification(t *testing.T) {
	ctx := context.Background()
	e := fixture.New(t, "audit.event.read")
	pool := pgtest.Pool(t)
	store := blobstore.Dir{Root: t.TempDir()}
	anchorer := &audit.Anchorer{Pool: pool, Store: store, AppRole: "erp_app", WorkerRole: "erp_worker", Log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))}
	if _, err := anchorer.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	logs := &pgtest.SyncBuffer{}
	clock := time.Now().UTC().Truncate(time.Microsecond)
	v := &audit.Verifier{Pool: pool, Store: store, AppRole: "erp_app", WorkerRole: "erp_worker", Log: slog.New(slog.NewTextHandler(logs, nil)), Now: func() time.Time { return clock }}
	ok, _, err := v.RunOnce(ctx) // other tests' tenants anchored to stores of their own count as broken here
	if err != nil || ok < 1 {
		t.Fatalf("%d %v", ok, err)
	}
	var rec struct {
		OK             bool
		HeadSeq        int64
		Checked        int64
		AnchorsChecked int
		BrokenAt       *int64
	}
	row := pgtest.Admin(t).QueryRow(ctx, `SELECT ok, head_seq, checked, anchors_checked, broken_at FROM audit.verification WHERE tenant_id = $1 ORDER BY verified_at DESC LIMIT 1`, e.Tenant.ID)
	if err := row.Scan(&rec.OK, &rec.HeadSeq, &rec.Checked, &rec.AnchorsChecked, &rec.BrokenAt); err != nil || !rec.OK || rec.HeadSeq == 0 || rec.Checked != rec.HeadSeq || rec.AnchorsChecked != 1 || rec.BrokenAt != nil {
		t.Fatalf("%+v %v", rec, err)
	}
	// While the chain is intact, a store that cannot be read fails the anchor check; without a store the anchors are compared to the events only.
	v.Store = failingStore{err: errors.New("store down")}
	if _, err := v.Verify(ctx, e.Tenant.ID); err == nil || !strings.Contains(err.Error(), "store down") {
		t.Fatalf("anchors on a failing store: %v", err)
	}
	v.Store = nil
	clock = clock.Add(time.Minute)
	if ver, err := v.Verify(ctx, e.Tenant.ID); err != nil || !ver.OK || ver.AnchorsChecked != 1 {
		t.Fatalf("without a store: %+v %v", ver, err)
	}
	v.Store = store
	// An event rewritten behind the chain's back: the next round records the break, logs it and publishes the alarm, once.
	pgtest.Exec(t, `DO $$ BEGIN SET LOCAL session_replication_role = replica;
		UPDATE audit.audit_event SET hash = '\x`+strings.Repeat("ab", 32)+`' WHERE tenant_id = '`+e.Tenant.ID.String()+`' AND seq = 1; END $$`)
	for i := 0; i < 2; i++ {
		clock = clock.Add(time.Hour)
		ver, err := v.Verify(ctx, e.Tenant.ID)
		if err != nil || ver.OK || ver.BrokenAt != 1 || !ver.VerifiedAt.Equal(clock) {
			t.Fatalf("round %d: %+v %v", i, ver, err)
		}
	}
	alarms := 0
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "audit chain does not verify") && strings.Contains(line, e.Tenant.ID.String()) {
			alarms++
		}
	}
	if alarms != 1 {
		t.Fatalf("%d alarms logged for the tenant:\n%s", alarms, logs.String())
	}
	events := apitest.Published(t, e.Tenant, outbox.Event{Context: "platform", Name: "audit_chain_tampered"})
	if len(events) != 1 || events[0]["broken_at"] != float64(1) {
		t.Fatalf("events: %v", events)
	}
	// Rounds keep counting the broken chain; from here the real clock keeps rows apart.
	v.Now = nil
	if _, broken, err := v.RunOnce(ctx); err != nil || broken < 1 {
		t.Fatalf("round with a broken chain: %d %v", broken, err)
	}
	// Database failures of every statement, on a chain whose alarm is about to sound (so the event's statement is among them), and of the transactions, are reported.
	second := fixture.New(t, "audit.event.read")
	if ver, err := v.Verify(ctx, second.Tenant.ID); err != nil || !ver.OK {
		t.Fatalf("second tenant: %+v %v", ver, err)
	}
	pgtest.Exec(t, `DO $$ BEGIN SET LOCAL session_replication_role = replica;
		UPDATE audit.audit_event SET hash = '\x`+strings.Repeat("cd", 32)+`' WHERE tenant_id = '`+second.Tenant.ID.String()+`' AND seq = 1; END $$`)
	for n := 1; n < 20; n++ {
		count := 0
		v.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, s)
			if err != nil {
				return nil, err
			}
			return &pgtest.FailingTx{Tx: tx, FailAt: n, Count: &count}, nil
		}
		_, err := v.Verify(ctx, second.Tenant.ID)
		if err == nil {
			if n == 1 {
				t.Fatal("verified without a statement to fail")
			}
			break
		}
		if !errors.Is(err, pgtest.ErrInjected) {
			t.Fatalf("statement %d: %v", n, err)
		}
	}
	if events := apitest.Published(t, second.Tenant, outbox.Event{Context: "platform", Name: "audit_chain_tampered"}); len(events) != 1 {
		t.Fatalf("alarm after the failures: %v", events)
	}
	v.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		tx, err := pool.Begin(ctx, s)
		if err != nil {
			return nil, err
		}
		return &pgtest.FailingTx{Tx: tx, FailCommit: true}, nil
	}
	if _, err := v.Verify(ctx, e.Tenant.ID); !errors.Is(err, pgtest.ErrInjected) {
		t.Fatalf("commit: got %v, want %v", err, pgtest.ErrInjected)
	}
	v.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errors.New("pool down") }
	if _, _, err := v.RunOnce(ctx); err == nil || !strings.Contains(err.Error(), "pool down") {
		t.Fatalf("tenants: %v", err)
	}
	v.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		tx, err := pool.Begin(ctx, s)
		if err != nil {
			return nil, err
		}
		return &pgtest.FailingTx{Tx: tx, FailAt: 1}, nil // the listing of the tenants
	}
	if _, _, err := v.RunOnce(ctx); !errors.Is(err, pgtest.ErrInjected) || !strings.Contains(err.Error(), "list tenants with a chain") {
		t.Fatalf("tenant listing: got %v, want the injected failure", err)
	}
	calls := 0
	v.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		calls++
		if calls == 2 {
			return nil, errors.New("pool down")
		}
		return pool.Begin(ctx, s)
	}
	if _, _, err := v.RunOnce(ctx); err == nil || !strings.Contains(err.Error(), "verifying tenant") {
		t.Fatalf("verify begin: %v", err)
	}
	v.Begin = nil
	// Run rounds until stopped: a round that works is logged, the default interval, and a failing round.
	v.Interval = time.Millisecond
	liveCtx, stopLive := context.WithCancel(ctx)
	lived := make(chan struct{})
	go func() { v.Run(liveCtx); close(lived) }()
	t.Cleanup(func() { stopLive(); <-lived }) // stops the worker should the wait fail
	pgtest.Eventually(t, 10*time.Second, func() bool { return strings.Contains(logs.String(), `msg="audit verification round" verified=`) },
		"no round logged")
	stopLive()
	<-lived
	v.Interval = 0
	stopped, cancelNow := context.WithCancel(ctx)
	cancelNow()
	v.Run(stopped)
	v.Interval, v.Begin = time.Millisecond, func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errors.New("pool down") }
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { v.Run(runCtx); close(done) }()
	t.Cleanup(func() { cancel(); <-done }) // stops the worker should the wait fail
	pgtest.Eventually(t, 10*time.Second, func() bool { return strings.Contains(logs.String(), "audit verification round failed") },
		"logs: %s", logs)
	cancel()
	<-done
}
