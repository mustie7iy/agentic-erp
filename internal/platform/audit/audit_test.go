package audit_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func actor(ten pgtest.Tenant) reqctx.Principal {
	return reqctx.Principal{ID: ten.Human.ID, Kind: ten.Human.Kind, TenantID: ten.ID}
}

func asApp(t *testing.T, pool *database.Pool, tenant uuid.UUID, fn func(context.Context, pgx.Tx) error) error {
	t.Helper()
	return pool.InTx(context.Background(), database.Scope{Role: "erp_app", TenantID: tenant}, fn)
}

func verify(t *testing.T, pool *database.Pool, tenant uuid.UUID) audit.Result {
	t.Helper()
	var res audit.Result
	if err := asApp(t, pool, tenant, func(ctx context.Context, tx pgx.Tx) (err error) {
		res, err = audit.Verify(ctx, tx, tenant)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return res
}

func TestBufferedEntriesExtendTheChain(t *testing.T) {
	pool := pgtest.Pool(t)
	ten := pgtest.NewTenant(t)
	res := verify(t, pool, ten.ID)
	if res.Checked != 0 {
		t.Errorf("empty chain: Checked = %d, want 0", res.Checked)
	}
	if res.BrokenAt != 0 {
		t.Errorf("empty chain: BrokenAt = %d, want 0", res.BrokenAt)
	}
	obj := uuid.New()
	for i := 0; i < 3; i++ {
		err := asApp(t, pool, ten.ID, func(ctx context.Context, tx pgx.Tx) error {
			ctx, buf := audit.WithBuffer(ctx)
			if err := audit.Add(ctx, audit.Entry{Action: "platform.operation.cancel", ObjectType: "platform.operation", ObjectID: obj, Outcome: "succeeded",
				Detail: map[string]any{"from": "running", "n": 1.5, "big": 1e21, "nested": map[string]any{"b": 2, "a": []any{true, nil}}}}); err != nil {
				t.Fatal(err)
			}
			if err := audit.Add(ctx, audit.Entry{Action: "platform.request.summary", ObjectType: "platform.request", Outcome: "succeeded"}); err != nil {
				t.Fatal(err)
			}
			if buf.Len() != 2 {
				t.Fatal("buffer")
			}
			if err := buf.Flush(ctx, tx, actor(ten), "trace-1", time.Now()); err != nil {
				return err
			}
			if buf.Len() != 0 {
				t.Fatal("buffer not emptied")
			}
			return buf.Flush(ctx, tx, actor(ten), "", time.Now()) // nothing left: no-op
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	res = verify(t, pool, ten.ID)
	if res.Checked != 6 {
		t.Errorf("chain: Checked = %d, want 6", res.Checked)
	}
	if res.BrokenAt != 0 {
		t.Errorf("chain: BrokenAt = %d, want 0", res.BrokenAt)
	}
}

func TestTamperingIsDetected(t *testing.T) {
	pool := pgtest.Pool(t)
	write := func(ten pgtest.Tenant, n int) {
		for i := 0; i < n; i++ {
			if err := asApp(t, pool, ten.ID, func(ctx context.Context, tx pgx.Tx) error {
				return audit.Write(ctx, tx, actor(ten), "", time.Now(), audit.Entry{Action: "x.y", ObjectType: "x", Outcome: "succeeded", Detail: map[string]any{"i": i}})
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Triggers forbid changes; a superuser disabling them is what the chain guards against.
	tamper := func(sql string, args ...any) {
		c := pgtest.Admin(t)
		ctx := context.Background()
		if _, err := c.Exec(ctx, "SET session_replication_role = replica"); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}

	edited := pgtest.NewTenant(t)
	write(edited, 4)
	tamper(`UPDATE audit.audit_event SET detail = '{"i": 99}' WHERE tenant_id = $1 AND seq = 2`, edited.ID)
	if res := verify(t, pool, edited.ID); res.BrokenAt != 2 {
		t.Fatalf("edited event: %+v", res)
	}

	ranged := func(tenant uuid.UUID, from, to int64) audit.Result {
		t.Helper()
		var res audit.Result
		if err := asApp(t, pool, tenant, func(ctx context.Context, tx pgx.Tx) (err error) {
			res, err = audit.VerifyRange(ctx, tx, tenant, from, to)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return res
	}
	// A range starts from its first event's stored predecessor.
	res := ranged(edited.ID, 3, 0)
	if res.BrokenAt != 0 {
		t.Errorf("range after the edit: BrokenAt = %d, want 0", res.BrokenAt)
	}
	if res.Checked != 2 {
		t.Errorf("range after the edit: Checked = %d, want 2", res.Checked)
	}
	if res.LastSeq != 4 {
		t.Errorf("range after the edit: LastSeq = %d, want 4", res.LastSeq)
	}
	if res := ranged(edited.ID, 1, 2); res.BrokenAt != 2 {
		t.Fatalf("range with the edit: %+v", res)
	}

	truncated := pgtest.NewTenant(t)
	write(truncated, 3)
	tamper(`DELETE FROM audit.audit_event WHERE tenant_id = $1 AND seq = 3`, truncated.ID)
	res = verify(t, pool, truncated.ID)
	if res.BrokenAt != 3 {
		t.Errorf("deleted last event: BrokenAt = %d, want 3", res.BrokenAt)
	}
	if res.Checked != 2 {
		t.Errorf("deleted last event: Checked = %d, want 2", res.Checked)
	}
	res = ranged(truncated.ID, 1, 2)
	if res.BrokenAt != 0 {
		t.Errorf("range before the deletion: BrokenAt = %d, want 0", res.BrokenAt)
	}
	if res.LastSeq != 2 {
		t.Errorf("range before the deletion: LastSeq = %d, want 2", res.LastSeq)
	}
	if res := ranged(truncated.ID, 2, 3); res.BrokenAt != 3 {
		t.Fatalf("range over the deletion: %+v", res)
	}

	gap := pgtest.NewTenant(t)
	write(gap, 3)
	tamper(`DELETE FROM audit.audit_event WHERE tenant_id = $1 AND seq = 2`, gap.ID)
	if res := verify(t, pool, gap.ID); res.BrokenAt != 3 {
		t.Fatalf("deleted middle event: %+v", res)
	}

	wiped := pgtest.NewTenant(t)
	write(wiped, 2)
	tamper(`DELETE FROM audit.audit_event WHERE tenant_id = $1`, wiped.ID)
	if res := verify(t, pool, wiped.ID); res.BrokenAt != 1 {
		t.Fatalf("deleted all events: %+v", res)
	}

	rehashed := pgtest.NewTenant(t)
	write(rehashed, 2)
	tamper(`UPDATE audit.chain_head SET hash = decode(repeat('ab', 32), 'hex') WHERE tenant_id = $1`, rehashed.ID)
	if res := verify(t, pool, rehashed.ID); res.BrokenAt != 3 {
		t.Fatalf("forged head: %+v", res)
	}
}

func TestConcurrentWritersKeepOneChain(t *testing.T) {
	pool := pgtest.Pool(t)
	ten := pgtest.NewTenant(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := asApp(t, pool, ten.ID, func(ctx context.Context, tx pgx.Tx) error {
				return audit.Write(ctx, tx, actor(ten), "", time.Now(), audit.Entry{Action: "x.y", ObjectType: "x", Outcome: "succeeded"})
			}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	res := verify(t, pool, ten.ID)
	if res.Checked != 8 {
		t.Errorf("Checked = %d, want 8", res.Checked)
	}
	if res.BrokenAt != 0 {
		t.Errorf("BrokenAt = %d, want 0", res.BrokenAt)
	}
}

func TestErrors(t *testing.T) {
	if err := audit.Add(context.Background(), audit.Entry{}); !errors.Is(err, audit.ErrNoBuffer) {
		t.Fatal(err)
	}
	pool := pgtest.Pool(t)
	ten, other := pgtest.NewTenant(t), pgtest.NewTenant(t)
	entry := audit.Entry{Action: "x.y", ObjectType: "x", Outcome: "succeeded"}
	// Writing into another tenant's chain is refused by row-level security.
	if err := asApp(t, pool, ten.ID, func(ctx context.Context, tx pgx.Tx) error {
		return audit.Write(ctx, tx, actor(other), "", time.Now(), entry)
	}); err == nil {
		t.Fatal("cross-tenant write accepted")
	}
	// Personal data in a detail is refused at write time (ADR-0026).
	for name, detail := range map[string]map[string]any{
		"key":           {"iban": "x"},
		"nested key":    {"bank": map[string]any{"Holder_Name": "A"}},
		"iban value":    {"account": "DE89 3704 0044 0532 0130 00"},
		"email value":   {"contact": "a.person@example.com"},
		"in a list":     {"items": []any{map[string]any{"ok": 1}, map[string]any{"email": "x"}}},
		"value in list": {"refs": []any{"GB29NWBK60161331926819"}},
	} {
		t.Run(name, func(t *testing.T) {
			if field, found := audit.Personal(detail); !found || field == "" {
				t.Errorf("Personal(%v) = %q, %v; want a field, true", detail, field, found)
			}
			if err := asApp(t, pool, ten.ID, func(ctx context.Context, tx pgx.Tx) error {
				return audit.Write(ctx, tx, actor(ten), "", time.Now(), audit.Entry{Action: "x.y", ObjectType: "x", Outcome: "ok", Detail: detail})
			}); err == nil || !strings.Contains(err.Error(), "personal data") {
				t.Errorf("Write: got %v, want a personal data error", err)
			}
		})
	}
	if field, found := audit.Personal(map[string]any{"supplier_id": "0192a000-0000-7000-8000-000000000301", "amount": "12.00", "status": "parked", "number": "BP90000003", "note": "paid @ once"}); found {
		t.Errorf("identifiers flagged: %s", field)
	}
	// Detail that is not JSON, and actions the table rejects.
	if err := asApp(t, pool, ten.ID, func(ctx context.Context, tx pgx.Tx) error {
		return audit.Write(ctx, tx, actor(ten), "", time.Now(), audit.Entry{Action: "x.y", ObjectType: "x", Outcome: "ok", Detail: map[string]any{"c": make(chan int)}})
	}); err == nil {
		t.Fatal("unencodable detail accepted")
	}
	if err := asApp(t, pool, ten.ID, func(ctx context.Context, tx pgx.Tx) error {
		return audit.Write(ctx, tx, actor(ten), "", time.Now(), audit.Entry{Action: "Not An Action", ObjectType: "x", Outcome: "ok"})
	}); err == nil {
		t.Fatal("invalid action accepted")
	}
	// A failure advancing the head is reported (fault injected with a trigger).
	pgtest.Exec(t, `CREATE OR REPLACE FUNCTION public.fail_head() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.seq > 0 AND NEW.tenant_id = current_setting('test.fail_tenant', true)::uuid THEN RAISE EXCEPTION 'injected'; END IF; RETURN NEW; END $$`)
	pgtest.Exec(t, `CREATE TRIGGER fail_head BEFORE UPDATE ON audit.chain_head FOR EACH ROW EXECUTE FUNCTION public.fail_head()`)
	t.Cleanup(func() { pgtest.Exec(t, `DROP TRIGGER fail_head ON audit.chain_head`) })
	if err := asApp(t, pool, ten.ID, func(ctx context.Context, tx pgx.Tx) error {
		// Checked so the injected trigger, not a failed set_config, is what fails the write.
		if _, err := tx.Exec(ctx, "SELECT set_config('test.fail_tenant', $1, true)", ten.ID.String()); err != nil {
			t.Fatal(err)
		}
		return audit.Write(ctx, tx, actor(ten), "", time.Now(), entry)
	}); err == nil {
		t.Fatal("head failure not reported")
	}
	// Verification on a broken transaction reports the error.
	tx, _ := pool.Begin(context.Background(), database.Scope{Role: "erp_app", TenantID: ten.ID})
	tx.Rollback(context.Background())
	if _, err := audit.Verify(context.Background(), tx, ten.ID); err == nil {
		t.Fatal("closed transaction accepted")
	}
}

// TestRecord: the events go to the outbox and the entry to the buffer, in
// the request's transaction; an event outside a request is refused before
// anything is audited.
func TestRecord(t *testing.T) {
	pool := pgtest.Pool(t)
	ten := pgtest.NewTenant(t)
	id := uuid.New()
	t.Run("in a request", func(t *testing.T) {
		err := asApp(t, pool, ten.ID, func(ctx context.Context, tx pgx.Tx) error {
			ctx, buf := audit.WithBuffer(reqctx.WithPrincipal(reqctx.WithTx(ctx, tx), actor(ten)))
			err := audit.Record(ctx, "platform.operation.cancel", "platform.operation", id, map[string]any{"n": 1},
				outbox.Event{Context: "platform", Name: "operation_completed", AggregateType: "platform.operation", AggregateID: id, Payload: map[string]any{}})
			if err != nil {
				return err
			}
			if buf.Len() != 1 {
				t.Errorf("buffered entries: got %d, want 1", buf.Len())
			}
			var events int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM platform.outbox WHERE aggregate_id = $1`, id).Scan(&events); err != nil {
				return err
			}
			if events != 1 {
				t.Errorf("outbox rows: got %d, want 1", events)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
	t.Run("outside a request", func(t *testing.T) {
		ctx, buf := audit.WithBuffer(context.Background())
		err := audit.Record(ctx, "platform.operation.cancel", "platform.operation", id, nil, outbox.Event{Context: "platform", Name: "operation_completed"})
		if !errors.Is(err, outbox.ErrOutsideRequest) {
			t.Errorf("got %v, want outbox.ErrOutsideRequest", err)
		}
		if buf.Len() != 0 {
			t.Errorf("buffered entries: got %d, want none", buf.Len())
		}
	})
}

// TestObjectTypeOf: the action without its verb; an action without one is
// its own type.
func TestObjectTypeOf(t *testing.T) {
	for action, want := range map[string]string{"payments.payment_run.release": "payments.payment_run", "summary": "summary"} {
		t.Run(action, func(t *testing.T) {
			if got := audit.ObjectTypeOf(action); got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}
