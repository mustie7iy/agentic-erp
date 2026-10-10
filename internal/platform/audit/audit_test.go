package audit_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
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
	if res := verify(t, pool, ten.ID); res.Checked != 0 || res.BrokenAt != 0 {
		t.Fatalf("empty chain: %+v", res)
	}
	obj := uuid.New()
	for i := 0; i < 3; i++ {
		err := asApp(t, pool, ten.ID, func(ctx context.Context, tx pgx.Tx) error {
			ctx, buf := audit.WithBuffer(ctx)
			audit.Add(ctx, audit.Entry{Action: "platform.operation.cancel", ObjectType: "platform.operation", ObjectID: obj, Outcome: "succeeded",
				Detail: map[string]any{"from": "running", "n": 1.5, "big": 1e21, "nested": map[string]any{"b": 2, "a": []any{true, nil}}}})
			audit.Add(ctx, audit.Entry{Action: "platform.request.summary", ObjectType: "platform.request", Outcome: "succeeded"})
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
	if res := verify(t, pool, ten.ID); res.Checked != 6 || res.BrokenAt != 0 {
		t.Fatalf("chain: %+v", res)
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
		c.Exec(ctx, "SET session_replication_role = replica")
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

	truncated := pgtest.NewTenant(t)
	write(truncated, 3)
	tamper(`DELETE FROM audit.audit_event WHERE tenant_id = $1 AND seq = 3`, truncated.ID)
	if res := verify(t, pool, truncated.ID); res.BrokenAt != 3 || res.Checked != 2 {
		t.Fatalf("deleted last event: %+v", res)
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
	if res := verify(t, pool, ten.ID); res.Checked != 8 || res.BrokenAt != 0 {
		t.Fatalf("%+v", res)
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
		tx.Exec(ctx, "SELECT set_config('test.fail_tenant', $1, true)", ten.ID.String())
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
