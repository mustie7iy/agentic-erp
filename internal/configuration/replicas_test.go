package configuration_test

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/platform/buildinfo"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Mixed versions (ADR-0038 rollout, 06 §6.4): an activation whose document
// sets a key some running replica does not know waits until every replica
// knows it; a replica that stopped without saying so stops counting at its
// expiry, measured on the database's clock.

// running records a replica of version that knows every key but lacking,
// last sighted ago (a PostgreSQL interval) before now, until the test ends.
func running(t *testing.T, version, revision, ago string, lacking ...string) uuid.UUID {
	t.Helper()
	keys := slices.DeleteFunc(config.Keys(), func(k string) bool { return slices.Contains(lacking, k) })
	id := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO config.replica (id, version, revision, config_keys, seen_at) VALUES ($1, $2, $3, $4, now() - $5::interval)`,
		id, version, revision, keys, ago)
	// The package's other tests activate configurations: they meet no replica of this one's.
	t.Cleanup(func() { pgtest.Exec(t, `DELETE FROM config.replica WHERE id = $1`, id) })
	return id
}

// wantLacking checks that err refuses the activation for exactly the keys,
// each naming the versions that lack it.
func wantLacking(t *testing.T, err error, want map[string]string) {
	t.Helper()
	var p *problem.Problem
	if !errors.As(err, &p) {
		t.Fatalf("activation: got %v, want 409 config.replica_lacks_key", err)
	}
	if p.Status != 409 || p.Code != "config.replica_lacks_key" {
		t.Errorf("activation: got %d %s, want 409 config.replica_lacks_key", p.Status, p.Code)
	}
	got := map[string]string{}
	for _, e := range p.Errors {
		if e.Code != "replica_lacks_key" {
			t.Errorf("error for %s: got code %s, want replica_lacks_key", e.Field, e.Code)
		}
		got[e.Field] = e.Message
	}
	if len(got) != len(want) {
		t.Errorf("keys refused: got %v, want %v", got, want)
	}
	for key, versions := range want {
		if !strings.Contains(got[key], "version "+versions+" do not know") {
			t.Errorf("error for %s: got %q, want it to name %s", key, got[key], versions)
		}
		if !strings.Contains(p.Detail, key) {
			t.Errorf("detail: got %q, want it to name %s", p.Detail, key)
		}
	}
}

// TestActivationWaitsForEveryReplica: a two-person release limit is not
// activated while a replica that would ignore it runs; a crashed replica
// past its expiry does not count; the same activation succeeds once the
// older replica has gone, and ordinary activations never wait.
func TestActivationWaitsForEveryReplica(t *testing.T) {
	e := setup(t, true)
	limited := document(t)
	limited.Companies[0].ReleaseDualLimit = "50000.00"
	older := running(t, "v1.0.0", "0123abcd", "10 seconds", "companies[].release_dual_limit")
	oldest := running(t, "v0.9.0", "", "10 seconds", "companies[].release_dual_limit")
	running(t, "v0.8.0", "", "6 minutes", "companies[].code") // crashed: past its expiry
	wantLacking(t, e.bootstrap(t, jsonOf(t, limited)), map[string]string{"companies[].release_dual_limit": "v0.9.0, v1.0.0 (0123abcd)"})

	if err := e.bootstrap(t, jsonOf(t, document(t))); err != nil {
		t.Fatalf("an activation every replica reads whole: got %v, want it activated", err)
	}
	pgtest.Exec(t, `DELETE FROM config.replica WHERE id = $1`, oldest)                                               // stopped, its row deleted
	pgtest.Exec(t, `UPDATE config.replica SET seen_at = now() - interval '5 minutes 1 second' WHERE id = $1`, older) // crashed, unannounced
	other := setup(t, true)
	if err := other.bootstrap(t, jsonOf(t, limited)); err != nil {
		t.Fatalf("the older replicas gone: got %v, want the limit activated", err)
	}
}

// TestChangeRequestWaitsForEveryReplica: :activate answers 409
// config.replica_lacks_key while a replica lacking a key the proposal sets
// runs, and activates it once that replica is upgraded.
func TestChangeRequestWaitsForEveryReplica(t *testing.T) {
	e := setup(t, false)
	doc := document(t)
	doc.NumberRanges = append(doc.NumberRanges, config.NumberRange{Company: "1000", DocumentType: "payment", FiscalYear: 2026, Prefix: "PY-", First: 100, Last: 999})
	cr := e.propose(t, 1, doc)
	if code, m := e.transition(t, cr, "submit"); code != 200 {
		t.Fatalf("submit: got %d %v, want 200", code, m)
	}
	older := running(t, "v1.0.0", "", "0 seconds", "number_ranges[].prefix")
	m := e.Problem(409, "config.replica_lacks_key", "POST", "/v1/change-requests/"+cr["id"].(string)+":activate", "", "*")
	if errs, _ := m["errors"].([]any); len(errs) != 1 || errs[0].(map[string]any)["field"] != "number_ranges[].prefix" {
		t.Errorf("errors: got %v, want number_ranges[].prefix alone", m["errors"])
	}
	pgtest.Exec(t, `UPDATE config.replica SET config_keys = $2 WHERE id = $1`, older, config.Keys()) // upgraded
	if code, m := e.transition(t, cr, "activate"); code != 200 || m["status"] != "active" {
		t.Errorf("activate once upgraded: got %d %v, want 200 active", code, m)
	}
}

// replica is a registry recording a version of its own, for the test to
// find its row by.
func replica(t *testing.T, begin database.BeginFunc) (*configuration.Replica, string, *pgtest.SyncBuffer) {
	t.Helper()
	version := "v0.0.0-" + uuid.NewString()[:8]
	logs := &pgtest.SyncBuffer{}
	r := &configuration.Replica{Pool: pgtest.Pool(t), WorkerRole: "erp_worker", Build: buildinfo.Info{Version: version, Revision: "abc"},
		Interval: time.Millisecond, Log: slog.New(slog.NewTextHandler(logs, nil)), Begin: begin}
	t.Cleanup(func() { pgtest.Exec(t, `DELETE FROM config.replica WHERE version = $1`, version) })
	return r, version, logs
}

func sighted(t *testing.T, version string) (n int, seen time.Time) {
	t.Helper()
	pgtest.Scan(t, `SELECT count(*), coalesce(max(seen_at), 'epoch') FROM config.replica WHERE version = $1`, []any{version}, &n, &seen)
	return n, seen
}

// TestReplicaRecords: a replica records its version and every key its
// binary knows, again at each interval, removes the rows of replicas past
// their expiry, and deletes its own row when it stops.
func TestReplicaRecords(t *testing.T) {
	running(t, "v0.1.0", "", "5 minutes 1 second")
	r, version, _ := replica(t, nil)
	if err := r.Register(context.Background()); err != nil {
		t.Fatalf("register: %v", err)
	}
	var keys []string
	pgtest.Scan(t, `SELECT config_keys FROM config.replica WHERE version = $1`, []any{version}, &keys)
	if !slices.Equal(keys, config.Keys()) {
		t.Errorf("keys recorded: got %d keys, want config.Keys()'s %d", len(keys), len(config.Keys()))
	}
	if n, _ := sighted(t, "v0.1.0"); n != 0 {
		t.Errorf("replica past its expiry: got %d rows, want it removed", n)
	}
	_, first := sighted(t, version)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var runErr error
	wg.Go(func() { runErr = r.Run(ctx) })
	pgtest.Eventually(t, 10*time.Second, func() bool { _, seen := sighted(t, version); return seen.After(first) }, "a later record of %s", version)
	cancel()
	wg.Wait()
	if runErr != nil {
		t.Errorf("run: got %v, want nil once stopped", runErr)
	}
	if n, _ := sighted(t, version); n != 0 {
		t.Errorf("rows of a stopped replica: got %d, want 0", n)
	}
}

// TestReplicaStopsWhenUnrecorded: records failing for half the expiry
// fail Run, so the server stops rather than serve uncounted; failures
// before that are logged and tried again. The clock is injected: each
// reading is a minute after the last.
func TestReplicaStopsWhenUnrecorded(t *testing.T) {
	failing := func(context.Context, database.Scope) (pgx.Tx, error) { return nil, pgtest.ErrInjected }
	r, _, logs := replica(t, failing)
	clock := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	r.Now = func() time.Time { clock = clock.Add(time.Minute); return clock }
	err := r.Run(context.Background())
	pgtest.WantErr(t, err, "has not been recorded since 2026-11-01T12:01:00Z")
	if n := strings.Count(logs.String(), "recording the replica failed"); n != 2 {
		t.Errorf("failures logged before stopping: got %d, want 2 (at one and two minutes)", n)
	}
}

// TestReplicaFailures: every database failure of a record fails it; a
// record failing because the replica is stopping is not counted against
// it; a failure to delete the row on stopping is logged, the row expiring
// instead.
func TestReplicaFailures(t *testing.T) {
	for name, wrap := range map[string]func(pgx.Tx) pgx.Tx{
		"record":             func(tx pgx.Tx) pgx.Tx { return &pgtest.FailingTx{Tx: tx, FailAt: 1} },
		"remove the expired": func(tx pgx.Tx) pgx.Tx { return &pgtest.FailingTx{Tx: tx, FailAt: 2} },
		"commit":             func(tx pgx.Tx) pgx.Tx { return &pgtest.FailingTx{Tx: tx, FailCommit: true} },
	} {
		t.Run(name, func(t *testing.T) {
			pool := pgtest.Pool(t)
			r, _, _ := replica(t, func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
				tx, err := pool.Begin(ctx, s)
				return wrap(tx), err
			})
			pgtest.WantErr(t, r.Register(context.Background()), pgtest.ErrInjected.Error())
		})
	}
	t.Run("begin", func(t *testing.T) {
		r, _, _ := replica(t, func(context.Context, database.Scope) (pgx.Tx, error) { return nil, pgtest.ErrInjected })
		pgtest.WantErr(t, r.Register(context.Background()), "record the replica")
	})
	t.Run("stopping", func(t *testing.T) {
		pool := pgtest.Pool(t)
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		r, version, logs := replica(t, func(c context.Context, s database.Scope) (pgx.Tx, error) {
			if calls++; calls == 1 {
				return pool.Begin(c, s)
			}
			cancel() // stopped while recording; and the row is not deleted
			return nil, pgtest.ErrInjected
		})
		if err := r.Register(ctx); err != nil {
			t.Fatalf("register: %v", err)
		}
		if err := r.Run(ctx); err != nil {
			t.Errorf("run: got %v, want nil when stopping", err)
		}
		if !strings.Contains(logs.String(), "removing the replica's record failed") {
			t.Errorf("logs: got %q, want the failed removal", logs.String())
		}
		if n, _ := sighted(t, version); n != 1 {
			t.Errorf("rows: got %d, want the row left to expire", n)
		}
	})
	t.Run("default interval", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		r, _, _ := replica(t, nil)
		r.Interval = 0
		if err := r.Run(ctx); err != nil {
			t.Errorf("run: got %v, want nil when stopped", err)
		}
	})
}
