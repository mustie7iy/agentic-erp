package nfr

import (
	"context"
	"os"
	osexec "os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// failover is the rehearsal's environment (scripts/failover.sh): the
// primary and the replica directly, PgBouncer's admin console and the
// configuration file it reloads.
type failover struct {
	primary, replica, bouncer, ini, replicaHost, primaryContainer string
}

func failoverEnv(t *testing.T) failover {
	t.Helper()
	f := failover{
		primary: os.Getenv("ERP_FAILOVER_PRIMARY_URL"), replica: os.Getenv("ERP_FAILOVER_REPLICA_URL"),
		bouncer: os.Getenv("ERP_FAILOVER_PGBOUNCER_URL"), ini: os.Getenv("ERP_FAILOVER_PGBOUNCER_INI"),
		replicaHost: os.Getenv("ERP_FAILOVER_REPLICA_HOST"), primaryContainer: os.Getenv("ERP_FAILOVER_PRIMARY_CONTAINER"),
	}
	if f.primaryContainer == "" || f.primary == "" || f.replica == "" || f.bouncer == "" || f.ini == "" || f.replicaHost == "" {
		t.Skip("the failover rehearsal runs under make failover (scripts/failover.sh)")
	}
	return f
}

func connect(t *testing.T, url string) *pgx.Conn {
	t.Helper()
	c, err := pgx.Connect(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) }) // teardown: the rehearsal is over
	return c
}

func run(t *testing.T, c *pgx.Conn, sql string) {
	t.Helper()
	if _, err := c.Exec(context.Background(), sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// TestResilienceDatabaseFailover promotes a streaming replica behind
// PgBouncer under load, as a Multi-AZ failover does (ADR-0040, T41c): the
// primary restarts as a standby, so a statement that still reaches it fails
// with 25006, which must be a retryable 503; once the replica has replayed
// everything it is promoted and PgBouncer repointed (PAUSE, RELOAD,
// RESUME). Requests fail only with 503, recover within
// 60 s, and every post retried with its key is posted once.
func TestResilienceDatabaseFailover(t *testing.T) {
	f := failoverEnv(t)
	replica, bouncer := connect(t, f.replica), connect(t, f.bouncer)
	s := Start(t, Options{Users: 8, Events: true})
	st, stop := background(s, 3*loadCap)
	running(t, st)

	began := time.Now()
	caughtUp(t, replica, fence(t, f))
	// Until PgBouncer moves, writes reach the former primary: wait for one
	// to be refused with 25006, the case the rehearsal is for.
	pgtest.Eventually(t, 30*time.Second, func() bool {
		return strings.Contains(s.Env.API.Logs(), "SQLSTATE 25006")
	}, "no write reached the former primary")
	var promoted bool
	if err := replica.QueryRow(context.Background(), "SELECT pg_promote(true, 60)").Scan(&promoted); err != nil || !promoted {
		t.Fatalf("the replica was not promoted within 60 s: %v", err)
	}
	run(t, bouncer, "PAUSE")
	repoint(t, f)
	run(t, bouncer, "RELOAD")
	run(t, bouncer, "RESUME")

	recovered := awaitRecovery(t, s, 60*time.Second)
	afterOutage(t, s, st, stop, "a failover to the replica", began, recovered, 60*time.Second)
	t.Logf("%d writes reached the former primary and were answered 503", strings.Count(s.Env.API.Logs(), "SQLSTATE 25006"))
}

// fence restarts the primary as a standby, as a Multi-AZ failover reboots
// the former writer as a reader: its sessions end (a 503 the client
// retries) and a statement that reaches it after is refused with 25006.
// A read-only setting would not do: PgBouncer restores the client's
// default_transaction_read_only on every server connection. It returns
// the WAL position the replica must replay before it is promoted.
func fence(t *testing.T, f failover) string {
	t.Helper()
	docker(t, "exec", f.primaryContainer, "bash", "-c", `touch "$PGDATA/standby.signal"`)
	// SIGINT is PostgreSQL's fast shutdown; docker's SIGTERM would wait for
	// PgBouncer's connections to close until docker kills it.
	docker(t, "kill", "--signal=SIGINT", f.primaryContainer)
	docker(t, "wait", f.primaryContainer)
	docker(t, "start", f.primaryContainer)
	ctx := context.Background()
	var lsn string
	var err error
	pgtest.Eventually(t, 60*time.Second, func() bool {
		c, cerr := pgx.Connect(ctx, f.primary)
		if err = cerr; err != nil {
			return false
		}
		defer func() { _ = c.Close(ctx) }() // a probe: closing loses nothing
		err = c.QueryRow(ctx, "SELECT checkpoint_lsn::text FROM pg_control_checkpoint()").Scan(&lsn)
		return err == nil
	}, "the former primary did not come back as a standby: %v", &err)
	return lsn
}

func docker(t *testing.T, args ...string) {
	t.Helper()
	if out, err := osexec.CommandContext(context.Background(), "docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("docker %v: %v: %s", args, err, out)
	}
}

// caughtUp waits until the replica has replayed all the former primary
// wrote, its shutdown checkpoint included, so the promotion loses nothing.
func caughtUp(t *testing.T, replica *pgx.Conn, lsn string) {
	t.Helper()
	ctx := context.Background()
	var behind bool
	var err error
	pgtest.Eventually(t, 30*time.Second, func() bool {
		err = replica.QueryRow(ctx, "SELECT pg_last_wal_replay_lsn() < $1::pg_lsn", lsn).Scan(&behind)
		return err == nil && !behind
	}, "the replica did not replay the primary's WAL: %v", &err)
}

// repoint rewrites PgBouncer's database host to the promoted replica.
func repoint(t *testing.T, f failover) {
	t.Helper()
	b, err := os.ReadFile(f.ini)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(b), "\n")
	found := false
	for i, l := range lines {
		if strings.HasPrefix(l, "* = ") {
			lines[i], found = "* = host="+f.replicaHost+" port=5432", true
		}
	}
	if !found {
		t.Fatalf("%s has no \"* = \" database line to repoint", f.ini)
	}
	if err := os.WriteFile(f.ini, []byte(strings.Join(lines, "\n")), 0o644); err != nil { //nolint:gosec // PgBouncer, another user in its container, reads it
		t.Fatal(err)
	}
}
