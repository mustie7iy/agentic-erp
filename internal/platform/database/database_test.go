package database_test

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func TestScopeSetsRoleTenantAndTimeouts(t *testing.T) {
	pool := pgtest.Pool(t)
	ten := pgtest.NewTenant(t)
	other := pgtest.NewTenant(t)
	ctx := context.Background()
	err := pool.InTx(ctx, database.Scope{Role: "erp_app", TenantID: ten.ID, ReadOnly: true}, func(ctx context.Context, tx pgx.Tx) error {
		var role, tenant, lock string
		if err := tx.QueryRow(ctx, "SELECT current_user, current_setting('app.tenant_id'), current_setting('lock_timeout')").Scan(&role, &tenant, &lock); err != nil {
			return err
		}
		if role != "erp_app" {
			t.Errorf("current_user: got %q, want %q", role, "erp_app")
		}
		if want := ten.ID.String(); tenant != want {
			t.Errorf("app.tenant_id: got %q, want %q", tenant, want)
		}
		if lock != "5s" {
			t.Errorf("lock_timeout: got %q, want %q", lock, "5s")
		}
		// Row-level security: only this tenant's principals are visible.
		var n int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM identity.principal WHERE tenant_id = $1", other.ID).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Error("other tenant visible")
		}
		if err := database.SetActor(ctx, tx, ten.Human.ID); err != nil {
			return err
		}
		var actor string
		if err := tx.QueryRow(ctx, "SELECT current_setting('app.actor_id')").Scan(&actor); err != nil {
			return err
		}
		if actor != ten.Human.ID.String() {
			t.Error("actor not set")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Read-only transactions refuse writes.
	err = pool.InTx(ctx, database.Scope{ReadOnly: true}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "CREATE TEMP TABLE x (id int)")
		return err
	})
	if err == nil {
		t.Fatal("write in read-only transaction")
	}
	// An extract asks for one database state across its statements.
	for level, want := range map[pgx.TxIsoLevel]string{"": "read committed", pgx.RepeatableRead: "repeatable read"} {
		var got string
		if err := pool.InTx(ctx, database.Scope{Isolation: level}, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SHOW transaction_isolation").Scan(&got)
		}); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("isolation %q: got %q, want %q", level, got, want)
		}
	}
	// The setting does not leak to the next transaction on the same pool.
	if err := pool.InTx(ctx, database.Scope{}, func(ctx context.Context, tx pgx.Tx) error {
		var tenant string
		if err := tx.QueryRow(ctx, "SELECT coalesce(current_setting('app.tenant_id', true), '')").Scan(&tenant); err != nil {
			return err
		}
		if tenant != "" {
			t.Errorf("tenant leaked: %q", tenant)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestInTxCommitsAndRollsBack(t *testing.T) {
	pool := pgtest.Pool(t)
	ctx := context.Background()
	pgtest.Exec(t, "CREATE TABLE IF NOT EXISTS public.tx_probe (id uuid PRIMARY KEY)")
	keep, drop := uuid.New(), uuid.New()
	if err := pool.InTx(ctx, database.Scope{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "INSERT INTO public.tx_probe VALUES ($1)", keep)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("abort")
	if err := pool.InTx(ctx, database.Scope{}, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "INSERT INTO public.tx_probe VALUES ($1)", drop); err != nil {
			return err // not the sentinel: the rollback below would prove nothing
		}
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	var n int
	if err := pgtest.Admin(t).QueryRow(ctx, "SELECT count(*) FROM public.tx_probe WHERE id IN ($1, $2)", keep, drop).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("rows %d", n)
	}
	// A commit that fails (deferred constraint) is reported.
	pgtest.Exec(t, `CREATE TABLE IF NOT EXISTS public.tx_deferred (id int UNIQUE DEFERRABLE INITIALLY DEFERRED)`)
	err := pool.InTx(ctx, database.Scope{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "INSERT INTO public.tx_deferred VALUES (1), (1)")
		return err
	})
	if err == nil {
		t.Fatal("deferred violation not reported")
	}
}

func TestOpenAndBeginErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := database.Open(ctx, "::not a url", database.DefaultSettings); err == nil {
		t.Fatal("bad url accepted")
	}
	if _, err := database.Open(ctx, "postgres://nobody@127.0.0.1:1/x?connect_timeout=1", database.DefaultSettings); err == nil {
		t.Fatal("unreachable database accepted")
	}
	if _, err := database.Open(ctx, pgtest.URL(t), database.Settings{MaxConns: -1}); err == nil {
		t.Fatal("pool of a negative size accepted")
	}
	pool := pgtest.Pool(t)
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Begin(ctx, database.Scope{Role: "no_such_role"}); err == nil {
		t.Fatal("unknown role accepted")
	}
	if err := pool.InTx(ctx, database.Scope{Role: "no_such_role"}, func(context.Context, pgx.Tx) error { return nil }); err == nil {
		t.Fatal("unknown role accepted")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := pool.Begin(cancelled, database.Scope{}); err == nil {
		t.Fatal("cancelled context accepted")
	}
	if err := pool.WithConn(cancelled, func(*pgx.Conn) error { return nil }); err == nil {
		t.Fatal("cancelled context accepted")
	}
	var one int
	if err := pool.WithConn(ctx, func(c *pgx.Conn) error { return c.QueryRow(ctx, "SELECT 1").Scan(&one) }); err != nil || one != 1 {
		t.Fatal(one, err)
	}
	// The pool is as large as configured, and no larger.
	small, err := database.Open(ctx, pgtest.URL(t), database.Settings{MaxConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer small.Close()
	held, err := small.Begin(ctx, database.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	brief, done := context.WithTimeout(ctx, 200*time.Millisecond)
	defer done()
	if _, err := small.Begin(brief, database.Scope{}); err == nil {
		t.Fatal("second connection of a pool of one")
	}
	held.Rollback(ctx)
	// Settings outside PostgreSQL's range fail the scope setup.
	bad, err := database.Open(ctx, pgtest.URL(t), database.Settings{LockTimeout: -time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Close()
	if _, err := bad.Begin(ctx, database.Scope{}); err == nil {
		t.Fatal("negative timeout accepted")
	}
	tx, _ := pool.Begin(ctx, database.Scope{})
	tx.Rollback(ctx)
	if err := database.SetActor(ctx, tx, uuid.New()); err == nil {
		t.Fatal("closed transaction accepted")
	}
}

// TestBeginOr: an injected begin is used when set, the pool otherwise.
func TestBeginOr(t *testing.T) {
	pool := pgtest.Pool(t)
	ctx := context.Background()
	t.Run("injected", func(t *testing.T) {
		injected := errors.New("injected")
		_, err := database.BeginOr(ctx, nil, func(context.Context, database.Scope) (pgx.Tx, error) { return nil, injected }, database.Scope{})
		if !errors.Is(err, injected) {
			t.Errorf("got %v, want the injected begin's error", err)
		}
	})
	t.Run("pool", func(t *testing.T) {
		tx, err := database.BeginOr(ctx, pool, nil, database.Scope{ReadOnly: true})
		if err != nil {
			t.Fatalf("got %v, want a transaction from the pool", err)
		}
		defer tx.Rollback(ctx)
		var readOnly string
		if err := tx.QueryRow(ctx, `SHOW transaction_read_only`).Scan(&readOnly); err != nil {
			t.Fatal(err)
		}
		if readOnly != "on" {
			t.Errorf("transaction_read_only: got %q, want on (the scope reaches the pool)", readOnly)
		}
	})
}

// TestPingClosedPool: a pool that is closed reports the failed ping.
func TestPingClosedPool(t *testing.T) {
	ctx := context.Background()
	pool, err := database.Open(ctx, pgtest.URL(t), database.DefaultSettings)
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	pgtest.CheckErr(t, pool.Ping(ctx), "database: ping")
}

// TestToday: the transaction's current_date, and a failed read reported.
func TestToday(t *testing.T) {
	pool := pgtest.Pool(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx, database.Scope{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	var want time.Time
	if err := tx.QueryRow(ctx, `SELECT current_date`).Scan(&want); err != nil {
		t.Fatal(err)
	}
	got, err := database.Today(ctx, tx)
	if err != nil || !got.Equal(want) {
		t.Errorf("got %v, %v, want %v", got, err, want)
	}
	tx.Rollback(ctx)
	got, err = database.Today(ctx, tx)
	pgtest.CheckErr(t, err, "database: read current date") // the closed transaction's read fails
	if !got.IsZero() {
		t.Errorf("closed transaction: got %v, want the zero date", got)
	}
}

// TestPassword: Settings.Password is asked for each new connection, in
// place of the URL's.
func TestPassword(t *testing.T) {
	ctx := context.Background()
	u, err := url.Parse(pgtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	right, ok := u.User.Password()
	if !ok {
		t.Fatal("the test database's URL names no password: the test proves nothing")
	}
	u.User = url.UserPassword(u.User.Username(), "not-the-password")
	if p, err := database.Open(ctx, u.String(), database.DefaultSettings); err == nil {
		p.Close()
		t.Fatal("the test database took a wrong password: the test proves nothing")
	}
	t.Run("asked for each connection", func(t *testing.T) {
		asked := 0
		pool, err := database.Open(ctx, u.String(), database.Settings{Password: func(context.Context) (string, error) {
			asked++
			return right, nil
		}})
		if err != nil {
			t.Fatalf("open with the asked password: %v", err)
		}
		pool.Close()
		if asked == 0 {
			t.Error("the password was not asked for")
		}
	})
	t.Run("its error refuses the connection", func(t *testing.T) {
		boom := errors.New("unreadable")
		_, err := database.Open(ctx, u.String(), database.Settings{Password: func(context.Context) (string, error) { return "", boom }})
		if !errors.Is(err, boom) {
			t.Errorf("open: got %v, want the password's error", err)
		}
	})
}
