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
	sc := &verificationScenario{ctx: ctx, e: e, pool: pool, store: store, anchorer: anchorer, logs: logs, clock: &clock, v: v}
	if !t.Run("first round records an intact chain", sc.firstRoundRecordsAnIntactChain) {
		t.FailNow()
	}
	if !t.Run("anchor check with and without a store", sc.anchorCheckWithAndWithoutAStore) {
		t.FailNow()
	}
	if !t.Run("rewritten event raises one alarm", sc.rewrittenEventRaisesOneAlarm) {
		t.FailNow()
	}
	if !t.Run("rounds count the broken chain", sc.roundsCountTheBrokenChain) {
		t.FailNow()
	}
	if !t.Run("every statement failing", sc.everyStatementFailing) {
		t.FailNow()
	}
	if !t.Run("transactions failing", sc.transactionsFailing) {
		t.FailNow()
	}
	t.Run("running rounds", sc.runningRounds)
}

// verificationScenario is the state TestScheduledVerification's steps share.
type verificationScenario struct {
	ctx      context.Context
	e        fixture.Env
	pool     *database.Pool
	store    blobstore.Dir
	anchorer *audit.Anchorer
	logs     *pgtest.SyncBuffer // the verifier's
	clock    *time.Time         // the verifier's
	v        *audit.Verifier
}

// firstRoundRecordsAnIntactChain is the step "first round records an intact chain" of TestScheduledVerification.
func (sc *verificationScenario) firstRoundRecordsAnIntactChain(t *testing.T) {
	var err error
	var ok int
	ok, _, err = sc.v.RunOnce(sc.ctx) // other tests' tenants anchored to stores of their own count as broken here
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
	row := pgtest.Admin(t).QueryRow(sc.ctx, `SELECT ok, head_seq, checked, anchors_checked, broken_at FROM audit.verification WHERE tenant_id = $1 ORDER BY verified_at DESC LIMIT 1`, sc.e.Tenant.ID)
	if err := row.Scan(&rec.OK, &rec.HeadSeq, &rec.Checked, &rec.AnchorsChecked, &rec.BrokenAt); err != nil {
		t.Fatalf("read the verification: %v", err)
	}
	if !rec.OK {
		t.Fatalf("verification ok: got false, want true")
	}
	if rec.HeadSeq == 0 {
		t.Fatalf("verification head seq: got 0, want the chain's head")
	}
	if rec.Checked != rec.HeadSeq {
		t.Fatalf("verification checked: got %d, want the head seq %d", rec.Checked, rec.HeadSeq)
	}
	if rec.AnchorsChecked != 1 {
		t.Fatalf("verification anchors checked: got %d, want 1", rec.AnchorsChecked)
	}
	if rec.BrokenAt != nil {
		t.Fatalf("verification broken at: got %d, want none", *rec.BrokenAt)
	}
}

// anchorCheckWithAndWithoutAStore is the step "anchor check with and without a store" of TestScheduledVerification.
func (sc *verificationScenario) anchorCheckWithAndWithoutAStore(t *testing.T) {
	var err error
	// While the chain is intact, a store that cannot be read fails the anchor check; without a store the anchors are compared to the events only.
	sc.v.Store = failingStore{err: errors.New("store down")}
	_, err = sc.v.Verify(sc.ctx, sc.e.Tenant.ID)
	pgtest.WantErr(t, err, "store down")
	sc.v.Store = nil
	*sc.clock = sc.clock.Add(time.Minute)
	if ver, err := sc.v.Verify(sc.ctx, sc.e.Tenant.ID); err != nil {
		t.Fatalf("without a store: %v", err)
	} else if !ver.OK || ver.AnchorsChecked != 1 {
		t.Fatalf("without a store: got ok %v with %d anchors checked, want ok with 1", ver.OK, ver.AnchorsChecked)
	}
	sc.v.Store = sc.store
}

// rewrittenEventRaisesOneAlarm is the step "rewritten event raises one alarm" of TestScheduledVerification.
func (sc *verificationScenario) rewrittenEventRaisesOneAlarm(t *testing.T) {
	// An event rewritten behind the chain's back: the next round records the break, logs it and publishes the alarm, once.
	pgtest.Exec(t, `DO $$ BEGIN SET LOCAL session_replication_role = replica;
		UPDATE audit.audit_event SET hash = '\x`+strings.Repeat("ab", 32)+`' WHERE tenant_id = '`+sc.e.Tenant.ID.String()+`' AND seq = 1; END $$`)
	for i := 0; i < 2; i++ {
		*sc.clock = sc.clock.Add(time.Hour)
		ver, err := sc.v.Verify(sc.ctx, sc.e.Tenant.ID)
		if err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		if ver.OK {
			t.Fatalf("round %d ok: got true, want false", i)
		}
		if ver.BrokenAt != 1 {
			t.Fatalf("round %d broken at: got %d, want 1", i, ver.BrokenAt)
		}
		if !ver.VerifiedAt.Equal(*sc.clock) {
			t.Fatalf("round %d verified at: got %v, want %v", i, ver.VerifiedAt, *sc.clock)
		}
	}
	alarms := 0
	for _, line := range strings.Split(sc.logs.String(), "\n") {
		if strings.Contains(line, "audit chain does not verify") && strings.Contains(line, sc.e.Tenant.ID.String()) {
			alarms++
		}
	}
	if alarms != 1 {
		t.Fatalf("%d alarms logged for the tenant:\n%s", alarms, sc.logs.String())
	}
	events := apitest.Published(t, sc.e.Tenant, outbox.Event{Context: "platform", Name: "audit_chain_tampered"})
	if len(events) != 1 || events[0]["broken_at"] != float64(1) {
		t.Fatalf("events: %v", events)
	}
}

// roundsCountTheBrokenChain is the step "rounds count the broken chain" of TestScheduledVerification.
func (sc *verificationScenario) roundsCountTheBrokenChain(t *testing.T) {
	// Rounds keep counting the broken chain; from here the real clock keeps rows apart.
	sc.v.Now = nil
	if _, broken, err := sc.v.RunOnce(sc.ctx); err != nil || broken < 1 {
		t.Fatalf("round with a broken chain: %d %v", broken, err)
	}
}

// everyStatementFailing is the step "every statement failing" of TestScheduledVerification.
func (sc *verificationScenario) everyStatementFailing(t *testing.T) {
	// Database failures of every statement, on a chain whose alarm is about to sound (so the event's statement is among them), and of the transactions, are reported.
	second := fixture.New(t, "audit.event.read")
	if ver, err := sc.v.Verify(sc.ctx, second.Tenant.ID); err != nil || !ver.OK {
		t.Fatalf("second tenant: %+v %v", ver, err)
	}
	pgtest.Exec(t, `DO $$ BEGIN SET LOCAL session_replication_role = replica;
		UPDATE audit.audit_event SET hash = '\x`+strings.Repeat("cd", 32)+`' WHERE tenant_id = '`+second.Tenant.ID.String()+`' AND seq = 1; END $$`)
	for n := 1; n < 20; n++ {
		count := 0
		sc.v.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := sc.pool.Begin(ctx, s)
			if err != nil {
				return nil, err
			}
			return &pgtest.FailingTx{Tx: tx, FailAt: n, Count: &count}, nil
		}
		_, err := sc.v.Verify(sc.ctx, second.Tenant.ID)
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
}

// transactionsFailing is the step "transactions failing" of TestScheduledVerification.
func (sc *verificationScenario) transactionsFailing(t *testing.T) {
	var err error
	sc.v.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		tx, err := sc.pool.Begin(ctx, s)
		if err != nil {
			return nil, err
		}
		return &pgtest.FailingTx{Tx: tx, FailCommit: true}, nil
	}
	if _, err := sc.v.Verify(sc.ctx, sc.e.Tenant.ID); !errors.Is(err, pgtest.ErrInjected) {
		t.Fatalf("commit: got %v, want %v", err, pgtest.ErrInjected)
	}
	sc.v.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errors.New("pool down") }
	_, _, err = sc.v.RunOnce(sc.ctx)
	pgtest.WantErr(t, err, "pool down")
	sc.v.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		tx, err := sc.pool.Begin(ctx, s)
		if err != nil {
			return nil, err
		}
		return &pgtest.FailingTx{Tx: tx, FailAt: 1}, nil // the listing of the tenants
	}
	if _, _, err := sc.v.RunOnce(sc.ctx); !errors.Is(err, pgtest.ErrInjected) || !strings.Contains(err.Error(), "list tenants with a chain") {
		t.Fatalf("tenant listing: got %v, want the injected failure", err)
	}
	calls := 0
	sc.v.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		calls++
		if calls == 2 {
			return nil, errors.New("pool down")
		}
		return sc.pool.Begin(ctx, s)
	}
	_, _, err = sc.v.RunOnce(sc.ctx)
	pgtest.WantErr(t, err, "verifying tenant")
	sc.v.Begin = nil
}

// runningRounds is the step "running rounds" of TestScheduledVerification.
func (sc *verificationScenario) runningRounds(t *testing.T) {
	// Run rounds until stopped: a round that works is logged, the default interval, and a failing round.
	sc.v.Interval = time.Millisecond
	liveCtx, stopLive := context.WithCancel(sc.ctx)
	lived := make(chan struct{})
	go func() { sc.v.Run(liveCtx); close(lived) }()
	t.Cleanup(func() { stopLive(); <-lived }) // stops the worker should the wait fail
	pgtest.Eventually(t, 10*time.Second, func() bool { return strings.Contains(sc.logs.String(), `msg="audit verification round" verified=`) },
		"no round logged")
	stopLive()
	<-lived
	sc.v.Interval = 0
	stopped, cancelNow := context.WithCancel(sc.ctx)
	cancelNow()
	sc.v.Run(stopped)
	sc.v.Interval, sc.v.Begin = time.Millisecond, func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errors.New("pool down") }
	runCtx, cancel := context.WithCancel(sc.ctx)
	done := make(chan struct{})
	go func() { sc.v.Run(runCtx); close(done) }()
	t.Cleanup(func() { cancel(); <-done }) // stops the worker should the wait fail
	pgtest.Eventually(t, 10*time.Second, func() bool { return strings.Contains(sc.logs.String(), "audit verification round failed") },
		"logs: %s", sc.logs)
	cancel()
	<-done
}
