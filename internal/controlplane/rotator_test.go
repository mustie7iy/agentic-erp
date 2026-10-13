package controlplane_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/controlplane"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestKeyRotationPolicy: the worker rotates the keys of active tenants
// whose key is older than the age, once per round, as a system actor.
func TestKeyRotationPolicy(t *testing.T) {
	p := newPlane(t)
	ctx := context.Background()
	created, _ := p.must(201, "ops|alice", "POST", tenants, tenantBody(t, "Aging"), "Idempotency-Key", "key-"+uuid.NewString())
	id := uuid.MustParse(created["id"].(string))
	pool := p.s.Pool
	var logs pgtest.SyncBuffer // Run logs on its goroutine while the test polls
	k := &controlplane.KeyRotator{Pool: pool, Keys: apitest.FieldKeys(), AppRole: "erp_app", WorkerRole: "erp_worker", Age: 24 * time.Hour,
		Log: slog.New(slog.NewTextHandler(&logs, nil))}
	admin := pgtest.Admin(t)
	keys := func() int {
		var n int
		if err := admin.QueryRow(ctx, `SELECT count(*) FROM tenancy.tenant_key WHERE tenant_id = $1`, id).Scan(&n); err != nil {
			t.Fatalf("count tenant keys: %v", err)
		}
		return n
	}
	age := func() { // the active key gets older than the policy allows
		pgtest.Exec(t, `UPDATE tenancy.tenant_key SET created_at = now() - interval '25 hours', version = version + 1 WHERE tenant_id = $1 AND status = 'active'`, id)
	}
	// A fresh key is not due (other tests' tenants on the legacy key are, and get keys of their own); an aged one is, once.
	if _, err := k.RunOnce(ctx); err != nil || keys() != 1 {
		t.Fatalf("fresh: %d keys, %v", keys(), err)
	}
	age()
	n, err := k.RunOnce(ctx)
	if err != nil || n < 1 || keys() != 2 {
		t.Fatalf("due: %d rotated, %d keys, %v", n, keys(), err)
	}
	if _, err := k.RunOnce(ctx); err != nil || keys() != 2 {
		t.Fatalf("again: %d keys, %v", keys(), err)
	}
	if detail := auditDetail(t, id, "tenancy.tenant.rotate_key"); detail["policy"] != "24h0m0s" || detail["resealed"] == nil {
		t.Fatalf("audit entry: %v", detail)
	}
	// A tenant that is not active is skipped by the round and refused on request.
	p.must(200, "ops|alice", "POST", tenants+"/"+id.String()+":suspend", "", "If-Match", "*")
	age()
	if _, err := k.RunOnce(ctx); err != nil || keys() != 2 {
		t.Fatalf("suspended: %d keys, %v", keys(), err)
	}
	if _, err := k.Rotate(ctx, id); err == nil || !strings.Contains(err.Error(), "suspended") {
		t.Fatalf("rotate a suspended tenant: %v", err)
	}
	if _, err := k.Rotate(ctx, uuid.New()); err == nil {
		t.Fatal("rotate nobody")
	}
	p.must(200, "ops|alice", "POST", tenants+"/"+id.String()+":resume", "", "If-Match", "*")
	// Every statement and transaction failing in turn is reported.
	for n := 1; n < 60; n++ {
		count := 0
		k.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, s)
			if err != nil {
				return nil, err
			}
			return &pgtest.FailingTx{Tx: tx, FailAt: n, Count: &count}, nil
		}
		_, err := k.Rotate(ctx, id)
		if err == nil {
			if n == 1 {
				t.Fatal("rotated without a statement to fail")
			}
			break
		}
		if !errors.Is(err, pgtest.ErrInjected) {
			t.Fatalf("statement %d: %v", n, err)
		}
	}
	k.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		tx, err := pool.Begin(ctx, s)
		if err != nil {
			return nil, err
		}
		return &brokenCommit{Tx: tx}, nil
	}
	age()
	if _, err := k.Rotate(ctx, id); err == nil || !strings.Contains(err.Error(), "commit failed") {
		t.Fatalf("commit: %v", err)
	}
	k.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errors.New("pool down") }
	if _, err := k.RunOnce(ctx); err == nil || !strings.Contains(err.Error(), "pool down") {
		t.Fatalf("due: %v", err)
	}
	k.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		tx, err := pool.Begin(ctx, s)
		if err != nil {
			return nil, err
		}
		return &pgtest.FailingTx{Tx: tx, FailAt: 1}, nil
	}
	if _, err := k.RunOnce(ctx); !errors.Is(err, pgtest.ErrInjected) {
		t.Fatalf("due query: got %v, want %v", err, pgtest.ErrInjected)
	}
	calls := 0
	k.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		calls++
		if calls == 2 {
			return nil, errors.New("pool down")
		}
		return pool.Begin(ctx, s)
	}
	age()
	if _, err := k.RunOnce(ctx); err == nil || !strings.Contains(err.Error(), "rotating the key") {
		t.Fatalf("rotate begin: %v", err)
	}
	k.Begin = nil
	// Run rounds until stopped: work done is logged, a failing round too, and the default interval.
	age()
	k.Interval = time.Millisecond
	runCtx, cancel := context.WithCancel(ctx)
	defer func() { cancel() }() // a failed wait below still stops the worker running then
	done := make(chan struct{})
	go func() { k.Run(runCtx); close(done) }()
	pgtest.Eventually(t, 30*time.Second, func() bool { return strings.Contains(logs.String(), `msg="key rotation round" rotated=`) },
		"no round logged its work; logs: %s", &logs)
	cancel()
	<-done
	k.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errors.New("pool down") }
	runCtx, cancel = context.WithCancel(ctx)
	done = make(chan struct{})
	go func() { k.Run(runCtx); close(done) }()
	pgtest.Eventually(t, 30*time.Second, func() bool { return strings.Contains(logs.String(), "key rotation round failed") },
		"no failing round logged; logs: %s", &logs)
	cancel()
	<-done
	k.Begin, k.Interval = nil, 0
	stopped, cancelNow := context.WithCancel(ctx)
	cancelNow()
	k.Run(stopped)
}

// TestARotationFailureDoesNotStopTheRound: a tenant whose key cannot be
// rotated (its master key gone, say) is reported, and the tenants after it
// in the round are still rotated rather than starved round after round.
func TestARotationFailureDoesNotStopTheRound(t *testing.T) {
	p := newPlane(t)
	ctx := context.Background()
	var ids []uuid.UUID
	for _, name := range []string{"Stuck", "Behind"} {
		created, _ := p.must(201, "ops|alice", "POST", tenants, tenantBody(t, name), "Idempotency-Key", "key-"+uuid.NewString())
		id := uuid.MustParse(created["id"].(string))
		pgtest.Exec(t, `UPDATE tenancy.tenant_key SET created_at = now() - interval '25 hours', version = version + 1 WHERE tenant_id = $1 AND status = 'active'`, id)
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	stuck, behind := ids[0], ids[1] // the round takes tenants in id order: the failing one first
	pool := p.s.Pool
	k := &controlplane.KeyRotator{Pool: pool, Keys: apitest.FieldKeys(), AppRole: "erp_app", WorkerRole: "erp_worker", Age: 24 * time.Hour,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Begin: func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			if s.TenantID == stuck {
				return nil, pgtest.ErrInjected
			}
			return pool.Begin(ctx, s)
		}}
	_, err := k.RunOnce(ctx)
	if !errors.Is(err, pgtest.ErrInjected) || !strings.Contains(err.Error(), stuck.String()) {
		t.Fatalf("round error: got %v, want the injected failure of tenant %s", err, stuck)
	}
	var keys int
	if err := pgtest.Admin(t).QueryRow(ctx, `SELECT count(*) FROM tenancy.tenant_key WHERE tenant_id = $1`, behind).Scan(&keys); err != nil {
		t.Fatalf("count tenant keys: %v", err)
	}
	if keys != 2 {
		t.Errorf("keys of the tenant behind the failing one: got %d, want 2", keys)
	}
}
