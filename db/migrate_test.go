package db

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// emptyDatabase creates a database for one test and drops it afterwards.
func emptyDatabase(t *testing.T) *pgx.Conn {
	t.Helper()
	need(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("erp_migrate_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(adminURL)
	u.Path = "/" + name
	conn, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = conn.Close(ctx) // the drop below forces any session off; a test may have closed it already
		if _, err := admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
		_ = admin.Close(ctx) // best-effort: the drop's outcome is checked above
	})
	return conn
}

// mustExec runs a statement a test sets its scene with; a failure stops the test
// rather than surfacing as a misleading assertion later.
func mustExec(t *testing.T, conn *pgx.Conn, sql string) {
	t.Helper()
	if _, err := conn.Exec(context.Background(), sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func TestMigrateAppliesEverythingOnce(t *testing.T) {
	conn := emptyDatabase(t)
	ctx := context.Background()
	if v, err := CurrentVersion(ctx, conn); err == nil || v != 0 {
		// The version table does not exist before the first run.
		t.Fatalf("version before migrating: %d, %v", v, err)
	}
	applied, err := Migrate(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != len(Migrations()) {
		t.Fatalf("applied %d of %d", len(applied), len(Migrations()))
	}
	if v, err := CurrentVersion(ctx, conn); err != nil || v != LatestVersion() {
		t.Fatalf("current version %d, %v; want %d", v, err, LatestVersion())
	}
	again, err := Migrate(ctx, conn)
	if err != nil || len(again) != 0 {
		t.Fatalf("second run applied %v, %v", again, err)
	}
}

func TestMigrateIsSafeForConcurrentReplicas(t *testing.T) {
	conn := emptyDatabase(t)
	ctx := context.Background()
	other, err := pgx.Connect(ctx, conn.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close(ctx) }() // best-effort: the database is dropped at cleanup
	var wg sync.WaitGroup
	results := make([][]string, 2)
	errs := make([]error, 2)
	for i, c := range []*pgx.Conn{conn, other} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = Migrate(ctx, c)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("migration %d: got %v, want nil", i, err)
		}
	}
	if total := len(results[0]) + len(results[1]); total != len(Migrations()) {
		t.Fatalf("migrations applied %d times in total, want %d", total, len(Migrations()))
	}
}

// TestMigrateWaitsForTheLock: while another session holds the migration
// lock, Migrate tries again every lockPoll, outside any statement, and
// gives up when its context ends.
func TestMigrateWaitsForTheLock(t *testing.T) {
	conn := emptyDatabase(t)
	ctx := context.Background()
	holder, err := pgx.Connect(ctx, conn.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Close(ctx) }() // closing ends the session and its lock
	mustExec(t, holder, fmt.Sprintf("SELECT pg_advisory_lock(%d)", lockKey))
	lockPoll = time.Hour // one try, then the wait, which the context ends
	t.Cleanup(func() { lockPoll = 100 * time.Millisecond })
	waiting, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if _, err := Migrate(waiting, conn); err == nil || !strings.Contains(err.Error(), "migrate: lock: context deadline exceeded") {
		t.Fatalf("migrate with the lock held: got %v, want the lock wait to end with the context", err)
	}
	var tables int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM pg_tables WHERE tablename = 'erp_schema_migration'").Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Fatalf("version tables after waiting: got %d, want 0", tables)
	}
}

func TestMigrateRunsNoTransactionMigrationsAlone(t *testing.T) {
	conn := emptyDatabase(t)
	ctx := context.Background()
	ms := []Migration{
		{Name: "000001_table", Up: "CREATE TABLE t (id int); CREATE TABLE u (id int);"},
		{Name: "000002_index", Up: NoTransaction + "\nCREATE INDEX CONCURRENTLY t_id ON t (id)"},
	}
	if _, err := migrate(ctx, conn, ms); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM pg_indexes WHERE indexname = 't_id'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("concurrent index not created")
	}
	// A newer version applied by a later release is left alone.
	mustExec(t, conn, "INSERT INTO public.erp_schema_migration VALUES (99, '000099_future', now())")
	if applied, err := migrate(ctx, conn, ms); err != nil || len(applied) != 0 {
		t.Fatalf("applied %v, %v", applied, err)
	}
}

func TestMigrateStopsAtTheFirstFailure(t *testing.T) {
	conn := emptyDatabase(t)
	ctx := context.Background()
	ms := []Migration{
		{Name: "000001_ok", Up: "CREATE TABLE ok (id int)"},
		{Name: "000002_bad", Up: "CREATE TABLE ok (id int)"},
		{Name: "000003_never", Up: "CREATE TABLE never (id int)"},
	}
	applied, err := migrate(ctx, conn, ms)
	if err == nil || !strings.Contains(err.Error(), "000002_bad") {
		t.Fatalf("want failure naming 000002_bad, got %v", err)
	}
	if len(applied) != 1 {
		t.Fatalf("applied %v", applied)
	}
	if v, _ := CurrentVersion(ctx, conn); v != 1 {
		t.Fatalf("failed migration recorded: version %d", v)
	}
	// Failures in the no-transaction path and in recording are reported too.
	_, err = migrate(ctx, conn, []Migration{{Name: "000004_bad", Up: NoTransaction + "\nCREATE INDEX CONCURRENTLY x ON missing (id)"}})
	if err == nil {
		t.Fatal("no-transaction failure not reported")
	}
	if _, err := migrate(ctx, conn, []Migration{{Name: "bad", Up: "SELECT 1"}}); err == nil {
		t.Fatal("bad name accepted")
	}
}

func TestMigrateWithoutPrivileges(t *testing.T) {
	conn := emptyDatabase(t)
	ctx := context.Background()
	// A role that may not create tables cannot create the version table.
	mustExec(t, conn, "SET ROLE erp_readonly")
	if _, err := Migrate(ctx, conn); err == nil || !strings.Contains(err.Error(), "version table") {
		t.Fatalf("want version table error, got %v", err)
	}
	// Nor read it once it exists.
	mustExec(t, conn, "RESET ROLE")
	mustExec(t, conn, "CREATE TABLE public.erp_schema_migration (version integer PRIMARY KEY, name text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())")
	mustExec(t, conn, "GRANT CREATE ON SCHEMA public TO erp_readonly") // this test database only
	mustExec(t, conn, "SET ROLE erp_readonly")
	if _, err := Migrate(ctx, conn); err == nil || !strings.Contains(err.Error(), "read versions") {
		t.Fatalf("want read error, got %v", err)
	}
}

func TestMigrationVersions(t *testing.T) {
	for _, name := range []string{"x", "abc_def", "000000_zero"} {
		t.Run(name, func(t *testing.T) {
			if v, err := (Migration{Name: name}).Version(); err == nil {
				t.Errorf("Version(%q): got %d and no error, want an error", name, v)
			}
		})
	}
	if v, err := (Migration{Name: "000012_platform_runtime"}).Version(); err != nil || v != 12 {
		t.Fatal(v, err)
	}
}

func TestMigrateReportsDatabaseErrors(t *testing.T) {
	conn := emptyDatabase(t)
	ctx := context.Background()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := Migrate(cancelled, conn); err == nil {
		t.Fatal("cancelled context accepted")
	}
	if err := conn.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(ctx, conn); err == nil {
		t.Fatal("closed connection accepted")
	}
	if _, err := CurrentVersion(ctx, conn); err == nil {
		t.Fatal("closed connection accepted")
	}
}

// deadPeer is a connection to a server that stops answering once the
// unlock is sent: the unlock's bytes go nowhere and no reply ever comes,
// as when the peer dies without closing the connection.
type deadPeer struct {
	net.Conn
}

func (c deadPeer) Write(b []byte) (int, error) {
	if bytes.Contains(b, []byte("pg_advisory_unlock")) {
		return len(b), nil
	}
	return c.Conn.Write(b) //nolint:wrapcheck // a pass-through connection: pgx sees the socket's own error
}

// TestMigrateReturnsWhenTheUnlockGetsNoAnswer: a server that stops
// answering after the migrations ran does not hold Migrate up; the unlock
// gives up after unlockTimeout and the connection, whose session holds the
// lock, is closed.
func TestMigrateReturnsWhenTheUnlockGetsNoAnswer(t *testing.T) {
	ctx := context.Background()
	cfg, err := pgx.ParseConfig(emptyDatabase(t).Config().ConnString())
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	dial := cfg.DialFunc
	cfg.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dial(ctx, network, addr)
		return deadPeer{c}, err
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(ctx) }) // already closed when the test passes; the database is dropped either way
	unlockTimeout = 100 * time.Millisecond
	t.Cleanup(func() { unlockTimeout = 10 * time.Second })
	result := make(chan error, 1)
	go func() {
		_, err := migrate(ctx, conn, []Migration{{Name: "000001_t", Up: "CREATE TABLE t (id int)"}})
		result <- err
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("got %v, want the migrations' success", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("got Migrate still waiting for the unlock after 30s, want it to give up")
	}
	if !conn.IsClosed() {
		t.Error("got the connection open after the unlock gave up, want it closed so its session's lock ends")
	}
}
