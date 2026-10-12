package directory_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/directory"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// memory is an in-memory store that can be made to fail.
type memory struct {
	mu      sync.Mutex
	entries map[uuid.UUID]directory.Entry
	lookups int
	err     error
}

func (m *memory) Lookup(_ context.Context, tenant uuid.UUID) (directory.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lookups++
	if m.err != nil {
		return directory.Entry{}, m.err
	}
	e, ok := m.entries[tenant]
	if !ok {
		return directory.Entry{}, directory.ErrUnknown
	}
	return e, nil
}

func (m *memory) Upsert(_ context.Context, e directory.Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	if m.entries == nil {
		m.entries = map[uuid.UUID]directory.Entry{}
	}
	m.entries[e.TenantID] = e
	return nil
}

func TestPostgresDirectory(t *testing.T) {
	ctx := context.Background()
	dir, err := directory.Open(ctx, pgtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	again, err := directory.Open(ctx, pgtest.URL(t)) // the schema is created once
	if err != nil {
		t.Fatal(err)
	}
	again.Close()
	tenant := uuid.New()
	if _, err := dir.Lookup(ctx, tenant); !errors.Is(err, directory.ErrUnknown) {
		t.Fatalf("unknown: %v", err)
	}
	if err := dir.SetStatus(ctx, tenant, directory.ReadOnly); !errors.Is(err, directory.ErrUnknown) {
		t.Fatalf("status of nobody: %v", err)
	}
	if err := dir.Upsert(ctx, directory.Entry{TenantID: tenant, Region: "eu-west", Cell: "eu-1", APIURL: "https://eu-1.api.example.com", Status: directory.Active}); err != nil {
		t.Fatal(err)
	}
	e, err := dir.Lookup(ctx, tenant)
	if err != nil || e.Cell != "eu-1" || e.Status != directory.Active || e.APIURL != "https://eu-1.api.example.com" || e.UpdatedAt.IsZero() {
		t.Fatalf("%+v %v", e, err)
	}
	// The cell's status changes pass through; a read_only set in the directory survives the cell's upserts.
	dir.Upsert(ctx, directory.Entry{TenantID: tenant, Region: "eu-west", Cell: "eu-1", APIURL: "https://eu-1.api.example.com", Status: directory.Suspended})
	if e, _ := dir.Lookup(ctx, tenant); e.Status != directory.Suspended {
		t.Fatalf("suspended: %+v", e)
	}
	if err := dir.SetStatus(ctx, tenant, directory.ReadOnly); err != nil {
		t.Fatal(err)
	}
	dir.Upsert(ctx, directory.Entry{TenantID: tenant, Region: "eu-west", Cell: "eu-2", APIURL: "https://eu-2.api.example.com", Status: directory.Active})
	if e, _ := dir.Lookup(ctx, tenant); e.Status != directory.ReadOnly || e.Cell != "eu-2" {
		t.Fatalf("moving: %+v", e)
	}
	if err := dir.SetStatus(ctx, tenant, directory.Active); err != nil {
		t.Fatal(err)
	}
	if err := dir.Upsert(ctx, directory.Entry{TenantID: tenant, Status: "bogus"}); err == nil {
		t.Fatal("bogus status stored")
	}
	if err := dir.SetStatus(ctx, tenant, "bogus"); err == nil {
		t.Fatal("bogus status set")
	}
	// Opening fails on a bad URL, and on a database where the schema cannot be created.
	if _, err := directory.Open(ctx, "postgres://nobody@127.0.0.1:1/x?connect_timeout=1"); err == nil {
		t.Fatal("opened nothing")
	}
	if _, err := directory.Open(ctx, "://"); err == nil {
		t.Fatal("opened a URL that is not one")
	}
	pgtest.Exec(t, `DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'dir_limited') THEN CREATE ROLE dir_limited LOGIN PASSWORD 'x'; END IF; END $$`)
	limited := strings.Replace(pgtest.URL(t), "postgres:dev@", "dir_limited:x@", 1)
	if _, err := directory.Open(ctx, limited); err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("limited user: %v", err)
	}
	// Lookups failing are reported.
	closed, _ := directory.Open(ctx, pgtest.URL(t))
	closed.Close()
	if _, err := closed.Lookup(ctx, tenant); err == nil || errors.Is(err, directory.ErrUnknown) {
		t.Fatalf("closed lookup: %v", err)
	}
	if err := closed.Upsert(ctx, directory.Entry{TenantID: tenant, Status: directory.Active}); err == nil {
		t.Fatal("closed upsert")
	}
	if err := closed.SetStatus(ctx, tenant, directory.Active); err == nil || errors.Is(err, directory.ErrUnknown) {
		t.Fatalf("closed set status: %v", err)
	}
}

func TestCache(t *testing.T) {
	ctx := context.Background()
	clock := time.Now()
	store := &memory{}
	cache := &directory.Cache{Store: store, TTL: time.Minute, Now: func() time.Time { return clock }}
	tenant := uuid.New()
	store.Upsert(ctx, directory.Entry{TenantID: tenant, Cell: "eu-1", Status: directory.Active})
	for i := 0; i < 3; i++ {
		if e, err := cache.Lookup(ctx, tenant); err != nil || e.Cell != "eu-1" {
			t.Fatalf("%+v %v", e, err)
		}
	}
	other := uuid.New()
	for i := 0; i < 3; i++ {
		if _, err := cache.Lookup(ctx, other); !errors.Is(err, directory.ErrUnknown) {
			t.Fatalf("miss: %v", err)
		}
	}
	if store.lookups != 2 {
		t.Fatalf("%d lookups reached the store", store.lookups)
	}
	// After the TTL the store is asked again; a write through the cache forgets the tenant at once.
	clock = clock.Add(2 * time.Minute)
	cache.Lookup(ctx, tenant)
	if store.lookups != 3 {
		t.Fatalf("%d lookups after the TTL", store.lookups)
	}
	store.entries[tenant] = directory.Entry{TenantID: tenant, Cell: "eu-2", Status: directory.Active}
	if e, _ := cache.Lookup(ctx, tenant); e.Cell != "eu-1" { // a change behind the cache's back waits for the TTL
		t.Fatalf("direct change seen at once: %+v", e)
	}
	if err := cache.Upsert(ctx, directory.Entry{TenantID: tenant, Cell: "eu-3", Status: directory.Active}); err != nil {
		t.Fatal(err)
	}
	if e, _ := cache.Lookup(ctx, tenant); e.Cell != "eu-3" {
		t.Fatalf("after upsert: %+v", e)
	}
	// Failures of the store are not remembered, and reach the caller.
	store.err = errors.New("directory down")
	if _, err := cache.Lookup(ctx, uuid.New()); err == nil || errors.Is(err, directory.ErrUnknown) {
		t.Fatalf("failure: %v", err)
	}
	if err := cache.Upsert(ctx, directory.Entry{TenantID: tenant}); err == nil {
		t.Fatal("upsert through a failing store")
	}
	store.err = nil
	// The default TTL applies without one.
	plain := &directory.Cache{Store: store}
	if e, err := plain.Lookup(ctx, tenant); err != nil || e.Cell != "eu-3" {
		t.Fatalf("%+v %v", e, err)
	}
}

func TestSync(t *testing.T) {
	ctx := context.Background()
	active, suspended := pgtest.NewTenant(t), pgtest.NewTenant(t)
	pgtest.Exec(t, `UPDATE tenancy.tenant SET status = 'suspended', version = version + 1 WHERE id = $1`, suspended.ID)
	fresh := pgtest.NewTenant(t)
	pgtest.Exec(t, `UPDATE tenancy.tenant SET status = 'provisioning', version = version + 1 WHERE id = $1`, fresh.ID)
	store := &memory{}
	var logs bytes.Buffer
	s := &directory.Sync{Pool: pgtest.Pool(t), ControlRole: "erp_control", Directory: store, Cell: "eu-1", APIURL: "https://eu-1.api.example.com", Log: slog.New(slog.NewTextHandler(&logs, nil))}
	n, err := s.RunOnce(ctx)
	if err != nil || n < 3 {
		t.Fatalf("%d %v", n, err)
	}
	for id, want := range map[uuid.UUID]string{active.ID: directory.Active, suspended.ID: directory.Suspended, fresh.ID: directory.Active} {
		if e := store.entries[id]; e.Status != want || e.Cell != "eu-1" || e.APIURL != "https://eu-1.api.example.com" || e.Region == "" {
			t.Errorf("%s: %+v", id, e)
		}
	}
	// A change registered at once; a directory that refuses is logged, not fatal.
	s.Register(ctx, active.ID, "eu-west", directory.Offboarded)
	if store.entries[active.ID].Status != directory.Offboarded {
		t.Fatal("not registered")
	}
	store.err = errors.New("directory down")
	s.Register(ctx, active.ID, "eu-west", directory.Active)
	if !strings.Contains(logs.String(), "directory registration failed") {
		t.Fatalf("logs: %s", logs.String())
	}
	if _, err := s.RunOnce(ctx); err == nil {
		t.Fatal("round with a failing directory")
	}
	store.err = nil
	// Database failures: the statement and the transaction.
	s.Begin = func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
		tx, err := pgtest.Pool(t).Begin(ctx, scope)
		if err != nil {
			return nil, err
		}
		return &pgtest.FailingTx{Tx: tx, FailAt: 1}, nil
	}
	if _, err := s.RunOnce(ctx); !errors.Is(err, pgtest.ErrInjected) {
		t.Fatalf("statement: %v", err)
	}
	s.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errors.New("pool down") }
	if _, err := s.RunOnce(ctx); err == nil {
		t.Fatal("begin")
	}
	// Run rounds until stopped, logging failures.
	s.Interval = time.Millisecond
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { s.Run(runCtx); close(done) }()
	time.Sleep(5 * time.Millisecond)
	cancel()
	<-done
	if !strings.Contains(logs.String(), "directory sync failed") {
		t.Fatalf("logs: %s", logs.String())
	}
	s.Begin, s.Interval = nil, 0
	stopped, cancelNow := context.WithCancel(ctx)
	cancelNow()
	s.Run(stopped) // the default interval, one round, then the stop
}
