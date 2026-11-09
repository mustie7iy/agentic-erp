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

// TestActiveTenants: the worker role sees through ActiveTenants only the
// tenants that are active, and a suspended, offboarded or provisioning
// tenant again once it is active (ADR-0015).
func TestActiveTenants(t *testing.T) {
	active := pgtest.NewTenant(t)
	pool := pgtest.Pool(t)
	for _, status := range []string{"suspended", "offboarded", "provisioning"} {
		t.Run(status, func(t *testing.T) {
			other := pgtest.NewTenant(t)
			listed := func() []uuid.UUID {
				t.Helper()
				var ids []uuid.UUID
				err := pool.InTx(context.Background(), database.Scope{Role: "erp_worker", ReadOnly: true}, func(ctx context.Context, tx pgx.Tx) error {
					rows, _ := tx.Query(ctx, `SELECT id FROM tenancy.tenant WHERE id = ANY($1) AND id IN `+database.ActiveTenants+` ORDER BY id`,
						[]uuid.UUID{active.ID, other.ID})
					var err error
					ids, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
					return err
				})
				if err != nil {
					t.Fatalf("list the active tenants: %v", err)
				}
				return ids
			}
			other.SetStatus(t, status)
			if got := listed(); len(got) != 1 || got[0] != active.ID {
				t.Errorf("with the other tenant %s: got %v, want only %s", status, got, active.ID)
			}
			pgtest.Exec(t, `UPDATE tenancy.tenant SET status = 'active', version = version + 1 WHERE id = $1`, other.ID)
			if got := listed(); len(got) != 2 {
				t.Errorf("with the other tenant active again: got %v, want both", got)
			}
		})
	}
}

// TestBeginActive: a transaction in an active tenant's scope begins, and
// one without a tenant is not checked; a suspended, offboarded or
// provisioning tenant's is refused with its status and rolled back, and
// begins again once the tenant is active (ADR-0015, po:2026-11-09). A
// failed begin or status read is returned with its cause.
func TestBeginActive(t *testing.T) {
	pool := pgtest.Pool(t)
	ctx := context.Background()
	ten := pgtest.NewTenant(t)
	begins := func(t *testing.T, begin database.BeginFunc, s database.Scope) error {
		t.Helper()
		tx, err := database.BeginActive(ctx, pool, begin, s)
		if err != nil {
			if tx != nil {
				t.Errorf("transaction with the error %v: got one, want none", err)
			}
			return err
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("roll back: %v", err)
		}
		return nil
	}
	app := database.Scope{Role: "erp_app", TenantID: ten.ID}
	t.Run("active and without a tenant", func(t *testing.T) {
		if err := begins(t, nil, app); err != nil {
			t.Errorf("active tenant: got %v, want nil", err)
		}
		ten.SetStatus(t, "suspended")
		if err := begins(t, nil, database.Scope{Role: "erp_worker", ReadOnly: true}); err != nil {
			t.Errorf("without a tenant, one suspended: got %v, want nil", err)
		}
	})
	for _, status := range []string{"suspended", "offboarded", "provisioning"} {
		t.Run(status, func(t *testing.T) {
			ten.SetStatus(t, status)
			err := begins(t, nil, app)
			var inactive *database.TenantInactiveError
			if !errors.As(err, &inactive) {
				t.Fatalf("got %v, want a TenantInactiveError", err)
			}
			if inactive.Status != status {
				t.Errorf("status: got %q, want %q", inactive.Status, status)
			}
			pgtest.Exec(t, `UPDATE tenancy.tenant SET status = 'active', version = version + 1 WHERE id = $1`, ten.ID)
			if err := begins(t, nil, app); err != nil {
				t.Errorf("once active again: got %v, want nil", err)
			}
		})
	}
	t.Run("database failures", func(t *testing.T) {
		down := errors.New("database down")
		if err := begins(t, func(context.Context, database.Scope) (pgx.Tx, error) { return nil, down }, app); !errors.Is(err, down) {
			t.Errorf("failed begin: got %v, want %v", err, down)
		}
		failing := func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, s)
			if err != nil {
				return nil, err
			}
			return &pgtest.FailingTx{Tx: tx, FailAt: 1}, nil
		}
		if err := begins(t, failing, app); !errors.Is(err, pgtest.ErrInjected) {
			t.Errorf("failed status read: got %v, want the injected failure", err)
		}
	})
}
