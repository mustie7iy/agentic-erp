package db

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
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
	name := "erp_migrate_" + strings.ReplaceAll(uuid.NewString(), "-", "") // another binary may create one in the same clock tick
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
	return c.Conn.Write(b)
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

// partitioned creates a table partitioned in two, t_a and t_b, with one row
// in each.
func partitioned(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	mustExec(t, conn, `CREATE TABLE t (k int, v int) PARTITION BY RANGE (k);
		CREATE TABLE t_a PARTITION OF t FOR VALUES FROM (0) TO (10);
		CREATE TABLE t_b PARTITION OF t FOR VALUES FROM (10) TO (20);
		INSERT INTO t VALUES (1, 1), (11, 1)`)
}

// indexValid reports whether the index exists and is valid.
func indexValid(t *testing.T, conn *pgx.Conn, name string) bool {
	t.Helper()
	var valid bool
	err := conn.QueryRow(context.Background(), `SELECT indisvalid FROM pg_index WHERE indexrelid = to_regclass($1)`, name).Scan(&valid)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("read index %s: %v", name, err)
	}
	return valid
}

// TestMigratePartitionedIndex: a PartitionedIndex migration builds the
// index per partition and attaches it, so the parent's is valid and
// enforced; a build that failed is picked up by the next run.
func TestMigratePartitionedIndex(t *testing.T) {
	conn := emptyDatabase(t)
	ctx := context.Background()
	partitioned(t, conn)
	mustExec(t, conn, "INSERT INTO t VALUES (12, 2), (12, 2)") // t_b cannot take the unique index yet
	m := Migration{Name: "000001_t_v", Up: PartitionedIndex + "\n-- Why the index.\nCREATE INDEX t_v ON public.t (v, k) WHERE v > 0;\n"}
	unique := Migration{Name: "000002_t_only_v", Up: PartitionedIndex + "\nCREATE UNIQUE INDEX t_only_v ON public.t (v, k)"}
	if _, err := migrate(ctx, conn, []Migration{m, unique}); err == nil || !strings.Contains(err.Error(), "000002_t_only_v") {
		t.Fatalf("duplicate in t_b: got %v, want 000002_t_only_v to fail", err)
	}
	if v, _ := CurrentVersion(ctx, conn); v != 1 {
		t.Fatalf("version after the failed build: got %d, want 1", v)
	}
	for name, want := range map[string]bool{"t_v": true, "t_only_v_a": true, "t_only_v_b": false, "t_only_v": false} {
		if got := indexValid(t, conn, name); got != want {
			t.Errorf("%s valid: got %v, want %v", name, got, want)
		}
	}
	mustExec(t, conn, "DELETE FROM t WHERE k = 12; INSERT INTO t VALUES (12, 2)")
	if _, err := migrate(ctx, conn, []Migration{m, unique}); err != nil {
		t.Fatalf("run again: %v", err)
	}
	if !indexValid(t, conn, "t_only_v") {
		t.Error("t_only_v after the second run: got invalid, want valid")
	}
	if _, err := conn.Exec(ctx, "INSERT INTO t VALUES (12, 2)"); err == nil || !strings.Contains(err.Error(), "t_only_v_b") {
		t.Errorf("duplicate after the build: got %v, want it refused by t_only_v_b", err)
	}
	mustExec(t, conn, "CREATE TABLE t_c PARTITION OF t FOR VALUES FROM (20) TO (30); INSERT INTO t VALUES (21, 2)")
	if _, err := conn.Exec(ctx, "INSERT INTO t VALUES (21, 2)"); err == nil {
		t.Error("duplicate in a later partition: got it accepted, want it refused by the inherited index")
	}
}

func TestMigratePartitionedIndexRefusals(t *testing.T) {
	conn := emptyDatabase(t)
	ctx := context.Background()
	partitioned(t, conn)
	mustExec(t, conn, "CREATE TABLE z_a (id int)")
	indexLockTimeout = "100ms"
	t.Cleanup(func() { indexLockTimeout = "10s" })
	for name, c := range map[string]struct{ up, want string }{
		"two statements": {"CREATE INDEX a ON public.t (k); CREATE INDEX b ON public.t (v)", "want one CREATE"},
		"unqualified":    {"CREATE INDEX a ON t (k)", "want one CREATE"},
		"no table":       {"CREATE INDEX a ON public.missing (k)", "missing"},
		"not an index":   {"CREATE INDEX t_b ON public.t (k)", "t_b"},
		"name taken":     {"CREATE INDEX z ON public.t (k)", "z_a"}, // a table where the partition's index goes
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := migrate(ctx, conn, []Migration{{Name: "000001_x", Up: PartitionedIndex + "\n" + c.up}}); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error with %q", err, c.want)
			}
		})
	}
	t.Run("lock timeout", func(t *testing.T) {
		other, err := pgx.Connect(ctx, conn.Config().ConnString())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = other.Close(ctx) }() // ends the transaction holding the lock
		mustExec(t, other, "BEGIN; INSERT INTO t VALUES (2, 2)")
		_, err = migrate(ctx, conn, []Migration{{Name: "000001_x", Up: PartitionedIndex + "\nCREATE INDEX x ON public.t (k)"}})
		if err == nil || !strings.Contains(err.Error(), "lock timeout") {
			t.Errorf("got %v, want the lock timeout", err)
		}
	})
	t.Run("partitions not read", func(t *testing.T) {
		cfg, err := pgx.ParseConfig(conn.Config().ConnString())
		if err != nil {
			t.Fatalf("parse config: %v", err)
		}
		dial := cfg.DialFunc
		cfg.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := dial(ctx, network, addr)
			return brokenOn{c, "pg_inherits"}, err
		}
		broken, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		defer func() { _ = broken.Close(ctx) }() // closed by the failed write already
		if _, err := migrate(ctx, broken, []Migration{{Name: "000001_x", Up: PartitionedIndex + "\nCREATE INDEX y ON public.t (k)"}}); err == nil ||
			!strings.Contains(err.Error(), "partitions of public.t") {
			t.Errorf("got %v, want the partitions' read to fail", err)
		}
	})
}

// brokenOn is a connection whose writes fail once they carry marker.
type brokenOn struct {
	net.Conn
	marker string
}

func (c brokenOn) Write(b []byte) (int, error) {
	if bytes.Contains(b, []byte(c.marker)) {
		return 0, net.ErrClosed
	}
	return c.Conn.Write(b)
}
