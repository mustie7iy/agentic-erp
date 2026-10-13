package outbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/natsbus"
	"github.com/mustie7iy/agentic-erp/internal/platform/natsbus/natstest"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

type recorder struct {
	mu   sync.Mutex
	msgs []outbox.Message
	fail error
}

func (r *recorder) Publish(_ context.Context, m outbox.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return r.fail
	}
	r.msgs = append(r.msgs, m)
	return nil
}

func (r *recorder) forTenant(tenant uuid.UUID) []outbox.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []outbox.Message
	for _, m := range r.msgs {
		if m.Header["ce-tenantid"] == tenant.String() {
			out = append(out, m)
		}
	}
	return out
}

func inRequest(t *testing.T, pool *database.Pool, ten pgtest.Tenant, fn func(ctx context.Context) error) {
	t.Helper()
	err := pool.InTx(context.Background(), database.Scope{Role: "erp_app", TenantID: ten.ID}, func(ctx context.Context, tx pgx.Tx) error {
		ctx = reqctx.WithTx(ctx, tx)
		ctx = reqctx.WithPrincipal(ctx, reqctx.Principal{ID: ten.Human.ID, Kind: "human", TenantID: ten.ID})
		ctx = reqctx.WithTraceID(ctx, "4bf92f3577b34da6a3ce929d0e0e4736")
		return fn(ctx)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestEventTypes(t *testing.T) {
	e := outbox.Event{Context: "config", Name: "configuration_activated"}
	if e.Type() != "com.example.erp.configuration.configuration_activated.v1" {
		t.Fatal(e.Type())
	}
	e = outbox.Event{Context: "payables", Name: "supplier_invoice_posted", Version: 2}
	if e.Type() != "com.example.erp.payables.supplier_invoice_posted.v2" {
		t.Fatal(e.Type())
	}
}

func TestRelayPublishesInOrderOnce(t *testing.T) {
	pool := pgtest.Pool(t)
	ten := pgtest.NewTenant(t)
	agg := uuid.New()
	inRequest(t, pool, ten, func(ctx context.Context) error {
		for i := 0; i < 3; i++ {
			if _, err := outbox.Add(ctx, outbox.Event{Context: "platform", Name: "operation_completed", AggregateType: "platform.operation",
				AggregateID: agg, Payload: map[string]any{"i": i}}); err != nil {
				return err
			}
		}
		_, err := outbox.Add(ctx, outbox.Event{Context: "config", Name: "configuration_activated", AggregateType: "config.config_version", AggregateID: agg, Payload: map[string]any{}})
		return err
	})
	rec := &recorder{}
	relay := &outbox.Relay{Pool: pool, Role: "erp_relay", Publisher: rec, Cell: "eu1", Batch: 2, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := context.Background()
	for {
		n, err := relay.RunOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	msgs := rec.forTenant(ten.ID)
	if len(msgs) != 4 {
		t.Fatalf("published %d", len(msgs))
	}
	var order []float64
	for _, m := range msgs {
		if m.Header["ce-type"] == "com.example.erp.platform.operation_completed.v1" {
			var p map[string]float64
			if err := json.Unmarshal(m.Data, &p); err != nil {
				t.Fatalf("payload %s: %v", m.Data, err)
			}
			order = append(order, p["i"])
			if want := "erp.eu1.platform." + ten.ID.String() + ".operation_completed"; m.Subject != want {
				t.Errorf("message %s subject: got %q, want %q", m.ID, m.Subject, want)
			}
			for _, h := range []struct{ key, want string }{
				{"ce-source", "/erp/eu1/platform"},
				{"ce-subject", agg.String()},
				{"ce-actorid", ten.Human.ID.String()},
				{"ce-traceid", "4bf92f3577b34da6a3ce929d0e0e4736"},
				{"ce-id", m.ID.String()},
			} {
				if got := m.Header[h.key]; got != h.want {
					t.Errorf("message %s header %s: got %q, want %q", m.ID, h.key, got, h.want)
				}
			}
		} else if want := "erp.eu1.configuration." + ten.ID.String() + ".configuration_activated"; m.Subject != want {
			t.Errorf("message %s subject: got %q, want %q", m.ID, m.Subject, want)
		}
	}
	if want := []float64{0, 1, 2}; !slices.Equal(order, want) {
		t.Errorf("publication order: got %v, want %v", order, want)
	}
	var unpublished int
	if err := pgtest.Admin(t).QueryRow(ctx, "SELECT count(*) FROM platform.outbox WHERE tenant_id = $1 AND published_at IS NULL", ten.ID).Scan(&unpublished); err != nil {
		t.Fatal(err)
	}
	if unpublished != 0 {
		t.Fatal("rows left unpublished")
	}
}

func TestRelayRetriesAfterPublishFailure(t *testing.T) {
	pool := pgtest.Pool(t)
	ten := pgtest.NewTenant(t)
	inRequest(t, pool, ten, func(ctx context.Context) error {
		_, err := outbox.Add(ctx, outbox.Event{Context: "platform", Name: "operation_completed", AggregateID: uuid.New(), Payload: 1})
		return err
	})
	rec := &recorder{fail: errors.New("bus down")}
	relay := &outbox.Relay{Pool: pool, Role: "erp_relay", Publisher: rec, Cell: "eu1"}
	if _, err := relay.RunOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "bus down") {
		t.Fatal(err)
	}
	rec.fail = nil
	for i := 0; i < 3 && len(rec.forTenant(ten.ID)) == 0; i++ {
		if _, err := relay.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(rec.forTenant(ten.ID)) != 1 {
		t.Fatal("event lost after a failed publish")
	}
}

func TestRelayToJetStream(t *testing.T) {
	pool := pgtest.Pool(t)
	ten := pgtest.NewTenant(t)
	bus, err := natsbus.Connect(natstest.Server(t))
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := bus.EnsureStreams(ctx, "eu1"); err != nil {
		t.Fatal(err)
	}
	inRequest(t, pool, ten, func(ctx context.Context) error {
		_, err := outbox.Add(ctx, outbox.Event{Context: "platform", Name: "operation_completed", AggregateID: uuid.New(), Payload: 1})
		return err
	})
	var buf pgtest.SyncBuffer
	relay := &outbox.Relay{Pool: pool, Role: "erp_relay", Publisher: bus, Cell: "eu1", Interval: 10 * time.Millisecond,
		Log: slog.New(slog.NewTextHandler(&buf, nil))}
	done := make(chan struct{})
	go func() { relay.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }() // stops the relay should the wait fail; deferred so it runs before the bus closes
	pgtest.Eventually(t, 10*time.Second, func() bool {
		var n int
		if err := pgtest.Admin(t).QueryRow(context.Background(), "SELECT count(*) FROM platform.outbox WHERE tenant_id = $1 AND published_at IS NULL", ten.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 0
	}, "relay did not publish: %s", &buf)
	cancel()
	<-done
}

func TestRelayLogsErrorsAndKeepsRunning(t *testing.T) {
	pool := pgtest.Pool(t)
	var buf pgtest.SyncBuffer
	relay := &outbox.Relay{Pool: pool, Role: "no_such_role", Publisher: &recorder{}, Cell: "eu1", Interval: 5 * time.Millisecond,
		Log: slog.New(slog.NewTextHandler(&buf, nil))}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	relay.Run(ctx)
	if !strings.Contains(buf.String(), "outbox relay failed") {
		t.Fatal("error not logged")
	}
	// Default interval and batch apply when unset; a full batch is followed at once by the next round.
	ten := pgtest.NewTenant(t)
	inRequest(t, pool, ten, func(ctx context.Context) error {
		for i := 0; i < 2; i++ {
			if _, err := outbox.Add(ctx, outbox.Event{Context: "platform", Name: "operation_completed", AggregateID: uuid.New(), Payload: i}); err != nil {
				return err
			}
		}
		return nil
	})
	rec := &recorder{}
	quick := &outbox.Relay{Pool: pool, Role: "erp_relay", Publisher: rec, Cell: "eu1", Batch: 1}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel2()
	quick.Run(ctx2)
	if len(rec.forTenant(ten.ID)) != 2 {
		t.Fatalf("published %d", len(rec.forTenant(ten.ID)))
	}
}

func TestTheRelayRoleSeesOnlyOutboxes(t *testing.T) {
	pool := pgtest.Pool(t)
	ten := pgtest.NewTenant(t)
	err := pool.InTx(context.Background(), database.Scope{Role: "erp_relay"}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "SELECT 1 FROM identity.principal WHERE tenant_id = $1", ten.ID)
		return err
	})
	if err == nil {
		t.Fatal("relay role can read business tables")
	}
	err = pool.InTx(context.Background(), database.Scope{Role: "erp_relay"}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE platform.outbox SET payload = '{}' WHERE false")
		return err
	})
	if err == nil {
		t.Fatal("relay role can change event payloads")
	}
}

func TestAddAndWriteErrors(t *testing.T) {
	if _, err := outbox.Add(context.Background(), outbox.Event{Context: "platform"}); !errors.Is(err, outbox.ErrOutsideRequest) {
		t.Fatal(err)
	}
	pool := pgtest.Pool(t)
	ten := pgtest.NewTenant(t)
	err := pool.InTx(context.Background(), database.Scope{Role: "erp_app", TenantID: ten.ID}, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := outbox.Write(ctx, tx, ten.ID, ten.Human.ID, "", outbox.Event{Context: "nope", Name: "x"}); err == nil {
			t.Error("unknown context accepted")
		}
		if _, err := outbox.Write(ctx, tx, ten.ID, ten.Human.ID, "", outbox.Event{Context: "platform", Name: "x", Payload: make(chan int)}); err == nil {
			t.Error("unencodable payload accepted")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The outbox refuses event types that do not belong to the context.
	err = pool.InTx(context.Background(), database.Scope{Role: "erp_app", TenantID: ten.ID}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := outbox.Write(ctx, tx, ten.ID, ten.Human.ID, "", outbox.Event{Context: "platform", Name: "Bad-Name", Payload: 1})
		return err
	})
	if err == nil {
		t.Fatal("bad event type accepted")
	}
}

func TestRelayWaitsForTheContextLock(t *testing.T) {
	pool := pgtest.Pool(t)
	ten := pgtest.NewTenant(t)
	inRequest(t, pool, ten, func(ctx context.Context) error {
		_, err := outbox.Add(ctx, outbox.Event{Context: "platform", Name: "operation_completed", AggregateID: uuid.New(), Payload: 1})
		return err
	})
	ctx := context.Background()
	// Another relay instance holds the platform context.
	other, err := pool.Begin(ctx, database.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('erp.outbox.platform'))"); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	relay := &outbox.Relay{Pool: pool, Role: "erp_relay", Publisher: rec, Cell: "eu1"}
	if _, err := relay.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(rec.forTenant(ten.ID)) != 0 {
		t.Fatal("published while another relay held the context")
	}
	other.Rollback(ctx)
	if _, err := relay.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(rec.forTenant(ten.ID)) != 1 {
		t.Fatal("not published after the lock was released")
	}
}

func TestRelayRepublishesWhenMarkingFails(t *testing.T) {
	pool := pgtest.Pool(t)
	ten := pgtest.NewTenant(t)
	inRequest(t, pool, ten, func(ctx context.Context) error {
		_, err := outbox.Add(ctx, outbox.Event{Context: "platform", Name: "operation_completed", AggregateID: uuid.New(), Payload: 1})
		return err
	})
	// A role that can read but not mark rows: the batch is published, the
	// transaction fails, and the next round publishes it again. The bus
	// de-duplicates on the event id (natsbus).
	pgtest.Exec(t, `DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'test_relay_ro') THEN CREATE ROLE test_relay_ro NOLOGIN BYPASSRLS; END IF; END $$`)
	pgtest.Exec(t, `GRANT USAGE ON SCHEMA platform TO test_relay_ro; GRANT SELECT ON platform.outbox TO test_relay_ro`)
	rec := &recorder{}
	ro := &outbox.Relay{Pool: pool, Role: "test_relay_ro", Publisher: rec, Cell: "eu1"}
	if _, err := ro.RunOnce(context.Background()); err == nil {
		t.Fatal("marking failure not reported")
	}
	if _, err := (&outbox.Relay{Pool: pool, Role: "erp_relay", Publisher: rec, Cell: "eu1"}).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	msgs := rec.forTenant(ten.ID)
	if len(msgs) != 2 {
		t.Fatalf("published: got %d, want the same event twice", len(msgs))
	}
	if msgs[0].ID != msgs[1].ID {
		t.Errorf("republished event id: got %s, want %s", msgs[1].ID, msgs[0].ID)
	}
	// A role that cannot read the outbox fails the round.
	if _, err := (&outbox.Relay{Pool: pool, Role: "pg_monitor", Publisher: rec, Cell: "eu1"}).RunOnce(context.Background()); err == nil {
		t.Fatal("unreadable outbox not reported")
	}
}

// TestRelayPublishesAnUnversionedType: an event type without the version
// suffix, which the outbox's CHECK refuses and only a bypassed constraint
// could store, is published under the name it has instead of stopping the
// relay (a panic) or the context's queue behind it (ADR-0007).
func TestRelayPublishesAnUnversionedType(t *testing.T) {
	pool := pgtest.Pool(t)
	ten := pgtest.NewTenant(t)
	ctx := context.Background()
	admin := pgtest.Admin(t)
	var check, def string
	if err := admin.QueryRow(ctx, `SELECT conname, pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conrelid = 'platform.outbox'::regclass AND contype = 'c' AND pg_get_constraintdef(oid) LIKE '%event_type%'`).Scan(&check, &def); err != nil {
		t.Fatal(err)
	}
	pgtest.Exec(t, `ALTER TABLE platform.outbox DROP CONSTRAINT `+pgx.Identifier{check}.Sanitize())
	id := uuid.New()
	t.Cleanup(func() {
		// The lock timeout ends a failed run instead of hanging on a
		// transaction the failure left open.
		pgtest.Exec(t, `DELETE FROM platform.outbox WHERE id = $1`, id)
		pgtest.Exec(t, `SET lock_timeout = '10s'; ALTER TABLE platform.outbox ADD CONSTRAINT `+pgx.Identifier{check}.Sanitize()+` `+def)
	})
	if _, err := admin.Exec(ctx, `INSERT INTO platform.outbox (tenant_id, id, aggregate_type, aggregate_id, event_type, payload, actor_id)
		VALUES ($1, $2, 'platform.operation', $3, 'com.example.erp.platform.operation_completed', '{}', $4)`, ten.ID, id, uuid.New(), ten.Human.ID); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("the relay panicked: %v", r)
			}
		}()
		if _, err := (&outbox.Relay{Pool: pool, Role: "erp_relay", Publisher: rec, Cell: "eu1"}).RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}()
	msgs := rec.forTenant(ten.ID)
	if len(msgs) != 1 {
		t.Fatalf("published: got %d messages, want 1", len(msgs))
	}
	if want := "erp.eu1.platform." + ten.ID.String() + ".operation_completed"; msgs[0].Subject != want {
		t.Errorf("subject: got %q, want %q", msgs[0].Subject, want)
	}
	if got := msgs[0].Header["ce-type"]; got != "com.example.erp.platform.operation_completed" {
		t.Errorf("ce-type: got %q, want the stored type", got)
	}
}
