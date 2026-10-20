package directory_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/directory"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
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

// TestPostgresDirectory counts cells, tenants and placements, so it keeps
// its directory in a database of its own: the package one holds the cells
// and tenants other tests, and earlier runs under -count, registered.
func TestPostgresDirectory(t *testing.T) {
	ctx := context.Background()
	dirURL := pgtest.EmptyDatabase(t)
	dir, err := directory.Open(ctx, dirURL)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	again, err := directory.Open(ctx, dirURL) // the schema is created once
	if err != nil {
		t.Fatal(err)
	}
	again.Close()
	s := &dirScenario{dir: dir, url: dirURL, tenant: uuid.New()}
	// Each step builds on the ones before it, so the first failure stops the rest.
	if !t.Run("entries", s.entries) {
		t.FailNow()
	}
	if !t.Run("status changes and moves", s.statusAndMoves) {
		t.FailNow()
	}
	if !t.Run("cells and placement", s.cellsAndPlacement) {
		t.FailNow()
	}
	if !t.Run("listing tenants", s.listingTenants) {
		t.FailNow()
	}
	if !t.Run("opening fails", s.openingFails) {
		t.FailNow()
	}
	if !t.Run("closed directory", s.closedDirectory) {
		t.FailNow()
	}
}

// dirScenario is the state the steps of TestPostgresDirectory share.
type dirScenario struct {
	dir    *directory.Postgres
	url    string    // the directory's own database
	tenant uuid.UUID // the tenant the entry, status and move steps work on
}

// entries: an entry is stored and read back; an upsert moves the version on only when it changes something, and passes the cell's status changes through.
func (s *dirScenario) entries(t *testing.T) {
	ctx := context.Background()
	dir, tenant := s.dir, s.tenant
	if _, err := dir.Lookup(ctx, tenant); !errors.Is(err, directory.ErrUnknown) {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := dir.SetStatus(ctx, tenant, 0, directory.ReadOnly); !errors.Is(err, directory.ErrUnknown) {
		t.Fatalf("status of nobody: %v", err)
	}
	if err := dir.Upsert(ctx, directory.Entry{TenantID: tenant, Region: "eu-west", Cell: "eu-1", APIURL: "https://eu-1.api.example.com", Status: directory.Active}); err != nil {
		t.Fatal(err)
	}
	e, err := dir.Lookup(ctx, tenant)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	for field, c := range map[string]struct{ got, want any }{
		"cell":           {e.Cell, "eu-1"},
		"status":         {e.Status, directory.Active},
		"api url":        {e.APIURL, "https://eu-1.api.example.com"},
		"updated_at set": {!e.UpdatedAt.IsZero(), true},
		"version":        {e.Version, int32(1)},
	} {
		if c.got != c.want {
			t.Errorf("lookup %s: got %v, want %v", field, c.got, c.want)
		}
	}
	// An upsert that changes nothing leaves the version alone; one that changes something moves it on.
	if err := dir.Upsert(ctx, directory.Entry{TenantID: tenant, Region: "eu-west", Cell: "eu-1", APIURL: "https://eu-1.api.example.com", Status: directory.Active}); err != nil {
		t.Fatalf("unchanged upsert: %v", err)
	}
	if e, _ := dir.Lookup(ctx, tenant); e.Version != 1 {
		t.Fatalf("version moved without a change: %+v", e)
	}
	// The cell's status changes pass through; a read_only set in the directory survives the cell's upserts.
	if err := dir.Upsert(ctx, directory.Entry{TenantID: tenant, Region: "eu-west", Cell: "eu-1", APIURL: "https://eu-1.api.example.com", Status: directory.Suspended}); err != nil {
		t.Fatalf("suspending upsert: %v", err)
	}
	if e, _ := dir.Lookup(ctx, tenant); e.Status != directory.Suspended {
		t.Fatalf("suspended status: got %s, want %s", e.Status, directory.Suspended)
	} else if e.Version != 2 {
		t.Fatalf("suspended version: got %d, want 2", e.Version)
	}
}

// statusAndMoves: they check the version they were read at (0: any); a read_only set in the directory survives the cell's upserts; moves go to registered cells of the tenant's region only.
func (s *dirScenario) statusAndMoves(t *testing.T) {
	ctx := context.Background()
	dir, tenant := s.dir, s.tenant
	if _, err := dir.SetStatus(ctx, tenant, 1, directory.ReadOnly); !errors.Is(err, directory.ErrVersion) {
		t.Fatalf("stale version: %v", err)
	}
	frozen, err := dir.SetStatus(ctx, tenant, 2, directory.ReadOnly)
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if frozen.Status != directory.ReadOnly {
		t.Errorf("frozen status: got %s, want %s", frozen.Status, directory.ReadOnly)
	}
	if frozen.Version != 3 {
		t.Errorf("frozen version: got %d, want 3", frozen.Version)
	}
	// Another cell's write leaves the entry where the directory places it.
	if err := dir.Upsert(ctx, directory.Entry{TenantID: tenant, Region: "eu-west", Cell: "eu-2", APIURL: "https://eu-2.api.example.com", Status: directory.Active}); !errors.Is(err, directory.ErrOtherCell) {
		t.Fatalf("upsert from eu-2: got %v, want ErrOtherCell", err)
	}
	if err := dir.Upsert(ctx, directory.Entry{TenantID: tenant, Region: "eu-west", Cell: "eu-1", APIURL: "https://eu-1.api.example.com", Status: directory.Active}); err != nil {
		t.Fatalf("upsert from eu-1: %v", err)
	}
	if e, _ := dir.Lookup(ctx, tenant); e.Status != directory.ReadOnly {
		t.Fatalf("moving status: got %s, want %s", e.Status, directory.ReadOnly)
	} else if e.Cell != "eu-1" {
		t.Fatalf("moving cell: got %s, want eu-1", e.Cell)
	}
	// Moves go to registered cells of the tenant's region only, at the version read.
	if err := dir.UpsertCell(ctx, directory.Cell{Name: "eu-3", Region: "eu-west", APIURL: "https://eu-3.api.example.com", ControlURL: "http://eu-3:9090", Accepting: false}); err != nil {
		t.Fatalf("register eu-3: %v", err)
	}
	if err := dir.UpsertCell(ctx, directory.Cell{Name: "us-1", Region: "us-east", APIURL: "https://us-1.api.example.com", ControlURL: "http://us-1:9090", Accepting: true}); err != nil {
		t.Fatalf("register us-1: %v", err)
	}
	if _, err := dir.Move(ctx, tenant, 0, "eu-9"); !errors.Is(err, directory.ErrUnknownCell) {
		t.Fatalf("move to nowhere: %v", err)
	}
	if _, err := dir.Move(ctx, tenant, 0, "us-1"); !errors.Is(err, directory.ErrRegion) {
		t.Fatalf("move across regions: %v", err)
	}
	if _, err := dir.Move(ctx, tenant, 1, "eu-3"); !errors.Is(err, directory.ErrVersion) {
		t.Fatalf("move at a stale version: %v", err)
	}
	moved, err := dir.Move(ctx, tenant, 0, "eu-3")
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if moved.Cell != "eu-3" {
		t.Errorf("moved cell: got %s, want eu-3", moved.Cell)
	}
	if moved.APIURL != "https://eu-3.api.example.com" {
		t.Errorf("moved api url: got %s, want https://eu-3.api.example.com", moved.APIURL)
	}
	if moved.Status != directory.ReadOnly {
		t.Errorf("moved status: got %s, want %s", moved.Status, directory.ReadOnly)
	}
	if _, err := dir.Move(ctx, uuid.New(), 0, "eu-3"); !errors.Is(err, directory.ErrUnknown) {
		t.Fatalf("move of nobody: %v", err)
	}
	if _, err := dir.SetStatus(ctx, tenant, 0, directory.Active); err != nil {
		t.Fatal(err)
	}
	if err := dir.Upsert(ctx, directory.Entry{TenantID: tenant, Cell: "eu-3", Status: "bogus"}); err == nil || errors.Is(err, directory.ErrOtherCell) {
		t.Fatalf("bogus status: got %v, want a refusal", err)
	}
	if _, err := dir.SetStatus(ctx, tenant, 0, "bogus"); err == nil {
		t.Fatal("bogus status set: got nil error, want a refusal")
	} else if errors.Is(err, directory.ErrUnknown) || errors.Is(err, directory.ErrVersion) {
		t.Fatalf("bogus status set: got %v, want an invalid status, not an unknown tenant or a version mismatch", err)
	}
}

// cellsAndPlacement: cells register themselves; listing orders by load; placement picks the emptiest accepting cell of the region, once per key.
func (s *dirScenario) cellsAndPlacement(t *testing.T) {
	ctx := context.Background()
	dir := s.dir
	for _, c := range []directory.Cell{
		{Name: "eu-1", Region: "eu-west", APIURL: "https://eu-1.api.example.com", ControlURL: "http://eu-1:9090", Accepting: true},
		{Name: "eu-2", Region: "eu-west", APIURL: "https://eu-2.api.example.com", ControlURL: "http://eu-2:9090", Accepting: true},
	} {
		if err := dir.UpsertCell(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if err := dir.UpsertCell(ctx, directory.Cell{Name: "us-1", Region: "us-east", APIURL: "https://us-1.api.example.com", ControlURL: "http://us-1:9090", Accepting: false}); err != nil { // updated
		t.Fatalf("update us-1: %v", err)
	}
	cells, err := dir.ListCells(ctx, "eu-west")
	if err != nil || len(cells) != 3 {
		t.Fatalf("cells of eu-west: got %+v %v, want 3", cells, err)
	}
	for i, want := range map[int]struct {
		name    string
		tenants int
	}{0: {"eu-1", 0}, 2: {"eu-3", 1}} {
		if cells[i].Name != want.name || cells[i].Tenants != want.tenants {
			t.Errorf("cell %d: got %s with %d tenants, want %s with %d", i, cells[i].Name, cells[i].Tenants, want.name, want.tenants)
		}
	}
	if all, _ := dir.ListCells(ctx, ""); len(all) != 4 || all[3].Accepting {
		t.Fatalf("all cells: %+v", all)
	}
	placed, err := dir.Place(ctx, "key-1", "eu-west", "")
	if err != nil || placed.Name != "eu-1" {
		t.Fatalf("placed: %+v %v", placed, err)
	}
	if err := dir.Upsert(ctx, directory.Entry{TenantID: uuid.New(), Region: "eu-west", Cell: "eu-1", APIURL: "https://eu-1.api.example.com", Status: directory.Active}); err != nil {
		t.Fatalf("fill eu-1: %v", err)
	}
	if err := dir.Upsert(ctx, directory.Entry{TenantID: uuid.New(), Region: "eu-west", Cell: "eu-1", APIURL: "https://eu-1.api.example.com", Status: directory.Active}); err != nil {
		t.Fatalf("fill eu-1: %v", err)
	}
	if again, _ := dir.Place(ctx, "key-1", "eu-west", ""); again.Name != "eu-1" { // the same key, the same cell, however full
		t.Fatalf("retry placed elsewhere: %+v", again)
	}
	if next, _ := dir.Place(ctx, "key-2", "eu-west", ""); next.Name != "eu-2" {
		t.Fatalf("second placement: %+v", next)
	}
	if named, _ := dir.Place(ctx, "key-3", "eu-west", "eu-1"); named.Name != "eu-1" {
		t.Fatalf("named placement: %+v", named)
	}
	for name, c := range map[string][2]string{"full cell named": {"eu-west", "eu-3"}, "other region's cell": {"eu-west", "us-1"}, "region without cells": {"ap-south", ""}, "unknown cell": {"eu-west", "eu-9"}} {
		t.Run(name, func(t *testing.T) {
			if _, err := dir.Place(ctx, "key-"+name, c[0], c[1]); !errors.Is(err, directory.ErrUnknownCell) {
				t.Errorf("got %v, want %v", err, directory.ErrUnknownCell)
			}
		})
	}
}

// listingTenants: filters, order and pages.
func (s *dirScenario) listingTenants(t *testing.T) {
	ctx := context.Background()
	dir := s.dir
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("k", 32)))
	ks, _ := pages.Keyset(directory.Sort, "list", pagination.Request{PageSize: 1, PageSizeSet: true})
	first, next, err := dir.ListTenants(ctx, directory.Filter{Cell: "eu-1"}, ks)
	if err != nil {
		t.Fatalf("list by cell: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("list by cell: got %d tenants, want 1", len(first))
	}
	if next == "" {
		t.Fatalf("list by cell: got no next page token, want one")
	}
	ks, _ = pages.Keyset(directory.Sort, "list", pagination.Request{OrderBy: "cell asc", PageSize: 1, PageSizeSet: true})
	byCell, next, err := dir.ListTenants(ctx, directory.Filter{Region: "eu-west"}, ks)
	if err != nil {
		t.Fatalf("by cell: %v", err)
	}
	if len(byCell) != 1 {
		t.Fatalf("by cell: got %d tenants, want 1", len(byCell))
	}
	if next == "" {
		t.Fatalf("by cell: got no next page token, want one")
	}
	ks, _ = pages.Keyset(directory.Sort, "list", pagination.Request{})
	if none, _, err := dir.ListTenants(ctx, directory.Filter{Status: directory.Offboarded}, ks); err != nil || len(none) != 0 {
		t.Fatalf("none: %+v %v", none, err)
	}
}

// openingFails: on a bad URL, and on a database where the schema cannot be created.
func (s *dirScenario) openingFails(t *testing.T) {
	ctx := context.Background()
	if _, err := directory.Open(ctx, "postgres://nobody@127.0.0.1:1/x?connect_timeout=1"); err == nil {
		t.Fatal("opened nothing")
	}
	if _, err := directory.Open(ctx, "://"); err == nil {
		t.Fatal("opened a URL that is not one")
	}
	pgtest.Exec(t, `DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'dir_limited') THEN CREATE ROLE dir_limited LOGIN PASSWORD 'x'; END IF; END $$`)
	limited, _ := url.Parse(pgtest.URL(t))
	limited.User = url.UserPassword("dir_limited", "x")
	_, err := directory.Open(ctx, limited.String())
	pgtest.WantErr(t, err, "schema")
}

// closedDirectory: lookups failing are reported.
func (s *dirScenario) closedDirectory(t *testing.T) {
	ctx := context.Background()
	dirURL, tenant := s.url, s.tenant
	closed, _ := directory.Open(ctx, dirURL)
	closed.Close()
	if _, err := closed.Lookup(ctx, tenant); err == nil || errors.Is(err, directory.ErrUnknown) {
		t.Fatalf("closed lookup: %v", err)
	}
	if err := closed.Upsert(ctx, directory.Entry{TenantID: tenant, Status: directory.Active}); err == nil {
		t.Fatal("closed upsert")
	}
	if _, err := closed.SetStatus(ctx, tenant, 0, directory.Active); err == nil || errors.Is(err, directory.ErrUnknown) {
		t.Fatalf("closed set status: %v", err)
	}
	if err := closed.UpsertCell(ctx, directory.Cell{Name: "x"}); err == nil {
		t.Fatal("closed upsert cell")
	}
	if _, err := closed.Move(ctx, tenant, 0, "eu-1"); err == nil || errors.Is(err, directory.ErrUnknown) {
		t.Fatalf("closed move: %v", err)
	}
	if _, err := closed.ListCells(ctx, ""); err == nil {
		t.Fatal("closed list cells")
	}
	if _, err := closed.Place(ctx, "k", "eu-west", ""); err == nil || errors.Is(err, directory.ErrUnknownCell) {
		t.Fatalf("closed place: %v", err)
	}
	if _, _, err := closed.ListTenants(ctx, directory.Filter{}, mustKeyset(t)); err == nil {
		t.Fatal("closed list")
	}
}

func TestCache(t *testing.T) {
	ctx := context.Background()
	clock := time.Now()
	store := &memory{}
	cache := &directory.Cache{Store: store, TTL: time.Minute, Now: func() time.Time { return clock }}
	tenant := uuid.New()
	if err := store.Upsert(ctx, directory.Entry{TenantID: tenant, Cell: "eu-1", Status: directory.Active}); err != nil {
		t.Fatalf("seed the store: %v", err)
	}
	for i := 0; i < 3; i++ {
		if e, err := cache.Lookup(ctx, tenant); err != nil || e.Cell != "eu-1" {
			t.Fatalf("lookup %d: got %+v %v, want cell eu-1", i, e, err)
		}
	}
	other := uuid.New()
	for i := 0; i < 3; i++ {
		if _, err := cache.Lookup(ctx, other); !errors.Is(err, directory.ErrUnknown) {
			t.Fatalf("miss %d: got %v, want %v", i, err, directory.ErrUnknown)
		}
	}
	if store.lookups != 2 {
		t.Fatalf("%d lookups reached the store", store.lookups)
	}
	// After the TTL the store is asked again; a write through the cache forgets the tenant at once.
	clock = clock.Add(2 * time.Minute)
	if _, err := cache.Lookup(ctx, tenant); err != nil {
		t.Fatalf("lookup after the TTL: %v", err)
	}
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
	pool := pgtest.Pool(t)
	s := &directory.Sync{Pool: pool, ControlRole: "erp_control", Directory: store, Cell: "eu-1", APIURL: "https://eu-1.api.example.com", Log: slog.New(slog.NewTextHandler(&logs, nil))}
	// With a Postgres directory the cell registers itself first, and a directory refusing that stops the round.
	dir, err := directory.Open(ctx, pgtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	withCells := &directory.Sync{Pool: pool, ControlRole: "erp_control", Directory: dir, Cell: "sync-cell", APIURL: "https://sync.api.example.com", Region: "eu-west", ControlURL: "http://sync:9090", Accepting: true, Log: s.Log}
	if _, err := withCells.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if cells, _ := dir.ListCells(ctx, "eu-west"); len(cells) == 0 || cells[len(cells)-1].Name != "sync-cell" && cells[0].Name != "sync-cell" {
		t.Fatalf("the cell did not register itself: %+v", cells)
	}
	broken, _ := directory.Open(ctx, pgtest.URL(t))
	broken.Close()
	withCells.Directory = broken
	if _, err := withCells.RunOnce(ctx); err == nil {
		t.Fatal("round with a closed directory")
	}
	n, err := s.RunOnce(ctx)
	if err != nil || n < 3 {
		t.Fatalf("%d %v", n, err)
	}
	for name, c := range map[string]struct {
		id   uuid.UUID
		want string
	}{"active": {active.ID, directory.Active}, "suspended": {suspended.ID, directory.Suspended}, "provisioning": {fresh.ID, directory.Active}} {
		t.Run(name, func(t *testing.T) {
			e := store.entries[c.id]
			if e.Status != c.want {
				t.Errorf("status: got %s, want %s", e.Status, c.want)
			}
			if e.Cell != "eu-1" {
				t.Errorf("cell: got %s, want eu-1", e.Cell)
			}
			if e.APIURL != "https://eu-1.api.example.com" {
				t.Errorf("api url: got %s, want https://eu-1.api.example.com", e.APIURL)
			}
			if e.Region == "" {
				t.Errorf("region: got none, want one")
			}
		})
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
		tx, err := pool.Begin(ctx, scope)
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
	// Run rounds until stopped, logging failures. The log buffer is not safe
	// to read while the worker writes, so the test waits for a second
	// transaction instead: it begins only after the first round failed and
	// was logged.
	var begun atomic.Int32
	s.Begin = func(context.Context, database.Scope) (pgx.Tx, error) {
		begun.Add(1)
		return nil, errors.New("pool down")
	}
	s.Interval = time.Millisecond
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { s.Run(runCtx); close(done) }()
	pgtest.Eventually(t, 10*time.Second, func() bool { return begun.Load() >= 2 }, "the running sync did not begin a second round")
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

func mustKeyset(t *testing.T) *pagination.Keyset {
	t.Helper()
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("k", 32)))
	ks, err := pages.Keyset(directory.Sort, "list", pagination.Request{})
	if err != nil {
		t.Fatal(err)
	}
	return ks
}

// TestDirectoryStatementFailures: every statement of every operation
// failing in turn is reported, never hidden.
func TestDirectoryStatementFailures(t *testing.T) {
	ctx := context.Background()
	dir, err := directory.Open(ctx, pgtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	tenant := uuid.New()
	if err := dir.UpsertCell(ctx, directory.Cell{Name: "sf-1", Region: "sf-west", APIURL: "https://sf-1", ControlURL: "http://sf-1:9090", Accepting: true}); err != nil {
		t.Fatalf("register sf-1: %v", err)
	}
	if err := dir.UpsertCell(ctx, directory.Cell{Name: "sf-2", Region: "sf-west", APIURL: "https://sf-2", ControlURL: "http://sf-2:9090", Accepting: true}); err != nil {
		t.Fatalf("register sf-2: %v", err)
	}
	if err := dir.Upsert(ctx, directory.Entry{TenantID: tenant, Region: "sf-west", Cell: "sf-1", APIURL: "https://sf-1", Status: directory.ReadOnly}); err != nil {
		t.Fatalf("place the tenant on sf-1: %v", err)
	}
	pool, upserted := dir.Pool, uuid.New()
	for name, op := range map[string]func() error{
		"lookup": func() error { _, err := dir.Lookup(ctx, tenant); return err },
		"upsert": func() error { // a tenant of its own: "move" may run first and take the other away from sf-1
			return dir.Upsert(ctx, directory.Entry{TenantID: upserted, Region: "sf-west", Cell: "sf-1", APIURL: "https://sf-1", Status: directory.Active})
		},
		"set status": func() error { _, err := dir.SetStatus(ctx, tenant, 0, directory.ReadOnly); return err },
		"move":       func() error { _, err := dir.Move(ctx, tenant, 0, "sf-2"); return err },
		"list tenants": func() error {
			_, _, err := dir.ListTenants(ctx, directory.Filter{Region: "sf-west"}, mustKeyset(t))
			return err
		},
		"upsert cell": func() error {
			return dir.UpsertCell(ctx, directory.Cell{Name: "sf-1", Region: "sf-west", APIURL: "https://sf-1", ControlURL: "http://sf-1:9090", Accepting: true})
		},
		"list cells":   func() error { _, err := dir.ListCells(ctx, "sf-west"); return err },
		"cell tenants": func() error { _, err := dir.CellTenants(ctx, "sf-1"); return err },
		"reinstate":    func() error { return dir.Reinstate(ctx, tenant, "sf-1") },
		"place":        func() error { _, err := dir.Place(ctx, "sf-key-"+uuid.NewString(), "sf-west", ""); return err },
	} {
		t.Run(name, func(t *testing.T) {
			for n := 1; n < 10; n++ {
				dir.InTx = func(ctx context.Context, scope database.Scope, fn func(context.Context, pgx.Tx) error) error {
					return pool.InTx(ctx, scope, func(ctx context.Context, tx pgx.Tx) error { return fn(ctx, &pgtest.FailingTx{Tx: tx, FailAt: n}) })
				}
				err := op()
				if err == nil {
					if n == 1 {
						t.Errorf("got success without a statement to fail, want statement 1 to fail it")
					}
					break
				}
				if !errors.Is(err, pgtest.ErrInjected) {
					t.Errorf("statement %d: got %v, want %v", n, err, pgtest.ErrInjected)
					break
				}
			}
		})
	}
	dir.InTx = nil
}

// TestErasedAndOffboardedOutliveARestore: the directory is the record
// outside the cell (ADR-0040). Once offboarded an entry stays so but for
// erased, once erased it stays so, whatever a cell restored to an earlier
// point upserts; a directory made before erased gains it.
func TestErasedAndOffboardedOutliveARestore(t *testing.T) {
	ctx := context.Background()
	u := pgtest.EmptyDatabase(t)
	// A directory as it was before erased.
	conn, err := pgx.Connect(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	_, err = conn.Exec(ctx, `CREATE SCHEMA directory; CREATE TABLE directory.tenant (tenant_id uuid PRIMARY KEY, region text NOT NULL, cell text NOT NULL,
		api_url text NOT NULL, status text NOT NULL CHECK (status IN ('active', 'suspended', 'offboarded', 'read_only')), updated_at timestamptz NOT NULL DEFAULT now())`)
	_ = conn.Close(ctx) // the old table is made; closing loses nothing
	if err != nil {
		t.Fatal(err)
	}
	dir, err := directory.Open(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	again, err := directory.Open(ctx, u) // a second opener finds the constraint done
	if err != nil {
		t.Fatal(err)
	}
	again.Close()
	tenant, other := uuid.New(), uuid.New()
	upsert := func(id uuid.UUID, status string) {
		t.Helper()
		if err := dir.Upsert(ctx, directory.Entry{TenantID: id, Region: "eu-west", Cell: "eu-1", APIURL: "https://eu-1", Status: status}); err != nil {
			t.Fatalf("upsert %s: %v", status, err)
		}
	}
	for _, step := range []struct{ name, upsert, want string }{
		{"active", directory.Active, directory.Active},
		{"offboarded", directory.Offboarded, directory.Offboarded},
		{"a restored cell says active", directory.Active, directory.Offboarded},
		{"erased", directory.Erased, directory.Erased},
		{"a restored cell says offboarded", directory.Offboarded, directory.Erased},
		{"a restored cell says active again", directory.Active, directory.Erased},
	} {
		upsert(tenant, step.upsert)
		if e, err := dir.Lookup(ctx, tenant); err != nil || e.Status != step.want {
			t.Errorf("%s: got %s (%v), want %s", step.name, e.Status, err, step.want)
		}
	}
	// An import back into the cell reinstates it; read_only (a move) stays.
	if err := dir.Reinstate(ctx, tenant, "eu-1"); err != nil {
		t.Fatal(err)
	}
	if e, _ := dir.Lookup(ctx, tenant); e.Status != directory.Active {
		t.Errorf("reinstated: got %s, want %s", e.Status, directory.Active)
	}
	upsert(tenant, directory.Offboarded)
	if _, err := dir.SetStatus(ctx, tenant, 0, directory.ReadOnly); err != nil {
		t.Fatal(err)
	}
	if err := dir.Reinstate(ctx, tenant, "eu-1"); err != nil {
		t.Fatal(err)
	}
	if e, _ := dir.Lookup(ctx, tenant); e.Status != directory.ReadOnly {
		t.Errorf("reinstated while moving: got %s, want %s", e.Status, directory.ReadOnly)
	}
	// Erased in its old cell and imported into another: that cell's sync
	// leaves the entry; it is moved there first, still erased, and the
	// import reinstates it; the old cell's sync leaves it then.
	moved := uuid.New()
	upsert(moved, directory.Erased)
	there := directory.Entry{TenantID: moved, Region: "eu-west", Cell: "eu-2", APIURL: "https://eu-2", Status: directory.Active}
	if err := dir.Upsert(ctx, there); !errors.Is(err, directory.ErrOtherCell) {
		t.Errorf("upsert from eu-2 before the move: got %v, want ErrOtherCell", err)
	}
	if err := dir.UpsertCell(ctx, directory.Cell{Name: "eu-2", Region: "eu-west", APIURL: "https://eu-2", ControlURL: "http://eu-2:9090"}); err != nil {
		t.Fatal(err)
	}
	if _, err := dir.Move(ctx, moved, 0, "eu-2"); err != nil {
		t.Fatal(err)
	}
	if err := dir.Reinstate(ctx, moved, "eu-2"); err != nil {
		t.Fatal(err)
	}
	if err := dir.Upsert(ctx, directory.Entry{TenantID: moved, Region: "eu-west", Cell: "eu-1", APIURL: "https://eu-1", Status: directory.Active}); !errors.Is(err, directory.ErrOtherCell) {
		t.Errorf("upsert from eu-1 after the move: got %v, want ErrOtherCell", err)
	}
	if e, _ := dir.Lookup(ctx, moved); e.Status != directory.Active || e.Cell != "eu-2" {
		t.Errorf("moved: got %s in %s, want %s in eu-2", e.Status, e.Cell, directory.Active)
	}
	upsert(other, directory.Suspended)
	entries, err := dir.CellTenants(ctx, "eu-1")
	if err != nil || len(entries) != 2 {
		t.Fatalf("cell tenants: got %d (%v), want 2", len(entries), err)
	}
	if there, err := dir.CellTenants(ctx, "eu-2"); err != nil || len(there) != 1 {
		t.Errorf("the other cell's tenants: got %d (%v), want the moved one", len(there), err)
	}
}

func TestStatusOf(t *testing.T) {
	at := time.Now()
	for _, c := range []struct {
		name string
		in   tenancy.Lifecycle
		want string
	}{
		{"provisioning", tenancy.Lifecycle{Status: "provisioning"}, directory.Active},
		{"active", tenancy.Lifecycle{Status: "active"}, directory.Active},
		{"offboarded", tenancy.Lifecycle{Status: "offboarded"}, directory.Offboarded},
		{"erased", tenancy.Lifecycle{Status: "offboarded", ErasedAt: &at}, directory.Erased},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := directory.StatusOf(c.in); got != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
}

// TestSyncLeavesTenantsAnotherCellOwns: after a tenant's entry is moved,
// the cell it left still holds its rows until they are removed (05 §5.4
// step 5); its sync and registrations must not point the entry back.
func TestSyncLeavesTenantsAnotherCellOwns(t *testing.T) {
	ctx := context.Background()
	tenant := pgtest.NewTenant(t)
	dir, err := directory.Open(ctx, pgtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	var logs bytes.Buffer
	old := &directory.Sync{Pool: pgtest.Pool(t), ControlRole: "erp_control", Directory: dir, Cell: "move-old", APIURL: "https://move-old", Log: slog.New(slog.NewTextHandler(&logs, nil))}
	if _, err := old.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	e, err := dir.Lookup(ctx, tenant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := dir.UpsertCell(ctx, directory.Cell{Name: "move-new", Region: e.Region, APIURL: "https://move-new", ControlURL: "http://move-new:9090"}); err != nil {
		t.Fatal(err)
	}
	if _, err := dir.Move(ctx, tenant.ID, e.Version, "move-new"); err != nil {
		t.Fatal(err)
	}
	if _, err := old.RunOnce(ctx); err != nil {
		t.Fatalf("round after the move: %v", err)
	}
	if e, _ := dir.Lookup(ctx, tenant.ID); e.Cell != "move-new" || e.APIURL != "https://move-new" {
		t.Errorf("after the old cell's round: got %s (%s), want move-new", e.Cell, e.APIURL)
	}
	if !strings.Contains(logs.String(), "level=INFO msg=\"directory sync left tenants another cell owns\"") {
		t.Errorf("round logs: got %s, want the skipped tenants at info", logs.String())
	}
	old.Register(ctx, tenant.ID, e.Region, directory.Suspended)
	if e, _ := dir.Lookup(ctx, tenant.ID); e.Cell != "move-new" || e.Status != directory.Active {
		t.Errorf("after the old cell's registration: got %s in %s, want active in move-new", e.Status, e.Cell)
	}
	if !strings.Contains(logs.String(), "level=INFO msg=\"directory registration left a tenant another cell owns\"") {
		t.Errorf("registration logs: got %s, want the skip at info", logs.String())
	}
	// Offboarded in the new cell, the tenant stays so whatever the old cell's
	// copy still says.
	if err := dir.Upsert(ctx, directory.Entry{TenantID: tenant.ID, Region: e.Region, Cell: "move-new", APIURL: "https://move-new", Status: directory.Offboarded}); err != nil {
		t.Fatal(err)
	}
	if _, err := old.RunOnce(ctx); err != nil {
		t.Fatalf("round after the offboarding: %v", err)
	}
	if e, _ := dir.Lookup(ctx, tenant.ID); e.Cell != "move-new" || e.Status != directory.Offboarded {
		t.Errorf("after the offboarding: got %s in %s, want offboarded in move-new", e.Status, e.Cell)
	}
}
