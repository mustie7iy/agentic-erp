package controlplane_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
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
	var logs bytes.Buffer
	k := &controlplane.KeyRotator{Pool: pool, Keys: apitest.FieldKeys(), AppRole: "erp_app", WorkerRole: "erp_worker", Age: 24 * time.Hour,
		Log: slog.New(slog.NewTextHandler(&logs, nil))}
	keys := func() int {
		var n int
		if err := pgtest.Admin(t).QueryRow(ctx, `SELECT count(*) FROM tenancy.tenant_key WHERE tenant_id = $1`, id).Scan(&n); err != nil {
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
	done := make(chan struct{})
	go func() { k.Run(runCtx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	if !strings.Contains(logs.String(), "key rotation round") {
		t.Fatalf("logs: %s", logs.String())
	}
	k.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errors.New("pool down") }
	runCtx, cancel = context.WithCancel(ctx)
	done = make(chan struct{})
	go func() { k.Run(runCtx); close(done) }()
	time.Sleep(5 * time.Millisecond)
	cancel()
	<-done
	if !strings.Contains(logs.String(), "key rotation round failed") {
		t.Fatalf("logs: %s", logs.String())
	}
	k.Begin, k.Interval = nil, 0
	stopped, cancelNow := context.WithCancel(ctx)
	cancelNow()
	k.Run(stopped)
}
