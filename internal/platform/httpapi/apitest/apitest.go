// Package apitest builds the real public API on the test database with a
// test identity provider, for end-to-end tests of contexts. It is test
// support and is excluded from the coverage figure.
package apitest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn/authntest"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/fieldcrypt"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/telemetry"
)

// API is a running public API.
type API struct {
	t       testing.TB
	Handler http.Handler
	Issuer  *authntest.Issuer
	Metrics *telemetry.Metrics
	logs    *syncBuffer
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

// New builds the API; it is skipped without DATABASE_URL.
func New(t testing.TB) *API {
	t.Helper()
	return NewWith(t, nil)
}

// NewWith builds the API after configure, when not nil, adjusts its
// configuration (for example to plug in a bank connector).
func NewWith(t testing.TB, configure func(*httpapi.Config)) *API {
	t.Helper()
	pool := pgtest.Pool(t)
	is := authntest.New(t)
	v, err := authn.New(context.Background(), []authn.Issuer{is.Config()})
	if err != nil {
		t.Fatal(err)
	}
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	a := &API{t: t, Issuer: is, Metrics: telemetry.NewMetrics(), logs: &syncBuffer{}}
	cfg := httpapi.Config{
		Pool: pool, AppRole: "erp_app", Verifier: v, Pages: pages, FieldKeys: FieldKeys(),
		Log: telemetry.NewLogger(a.logs, -4), Metrics: a.Metrics,
		Tracer: tracenoop.NewTracerProvider(), Meter: noop.NewMeterProvider(), Now: time.Now,
	}
	if configure != nil {
		configure(&cfg)
	}
	h, err := httpapi.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	a.Handler = h
	return a
}

// FieldKeys are the field encryption keys the test API uses.
func FieldKeys() *fieldcrypt.Keys {
	k, _ := fieldcrypt.New([]byte(strings.Repeat("f", 32)))
	return k
}

// Logs returns everything logged so far.
func (a *API) Logs() string {
	a.logs.mu.Lock()
	defer a.logs.mu.Unlock()
	return a.logs.b.String()
}

// Req is one API call.
type Req struct {
	Method, Path, Body string
	Tenant             uuid.UUID
	As                 pgtest.Actor // zero: no Authorization header
	Key                string       // Idempotency-Key
	IfMatch            string
	Header             http.Header
}

// Do performs the call.
func (a *API) Do(r Req) *httptest.ResponseRecorder {
	a.t.Helper()
	req := httptest.NewRequest(r.Method, r.Path, strings.NewReader(r.Body))
	if r.Body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if r.As.Subject != "" {
		req.Header.Set("Authorization", "Bearer "+a.Issuer.Token(a.t, r.Tenant, r.As.Subject, nil))
	}
	if r.Key != "" {
		req.Header.Set("Idempotency-Key", r.Key)
	}
	if r.IfMatch != "" {
		req.Header.Set("If-Match", r.IfMatch)
	}
	for k, vs := range r.Header {
		req.Header[k] = vs
	}
	rec := httptest.NewRecorder()
	a.Handler.ServeHTTP(rec, req)
	return rec
}

// JSON decodes a response body.
func JSON(t testing.TB, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("not JSON (%d): %s", rec.Code, rec.Body)
	}
	return m
}

// NewKey returns a fresh Idempotency-Key.
func NewKey() string { return uuid.NewString() }

// InRequest runs fn as actor inside a transaction set up the way the unit of
// work sets it up (tenant, actor, principal, audit buffer), for calling
// handlers and context functions directly. The transaction is rolled back.
func InRequest(t testing.TB, ten pgtest.Tenant, a pgtest.Actor, fn func(ctx context.Context) error) error {
	t.Helper()
	return inRequest(t, ten, a, 0, fn)
}

var (
	poolsMu sync.Mutex
	pools   = map[testing.TB]*database.Pool{}
)

// pool is one pool per test, so repeated calls do not exhaust connections.
func pool(t testing.TB) *database.Pool {
	poolsMu.Lock()
	defer poolsMu.Unlock()
	if p, ok := pools[t]; ok {
		return p
	}
	p := pgtest.Pool(t)
	pools[t] = p
	t.Cleanup(func() {
		poolsMu.Lock()
		delete(pools, t)
		poolsMu.Unlock()
	})
	return p
}

func inRequest(t testing.TB, ten pgtest.Tenant, a pgtest.Actor, failAt int, fn func(ctx context.Context) error) error {
	t.Helper()
	pool := pool(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx, database.Scope{Role: "erp_app", TenantID: ten.ID})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := database.SetActor(ctx, tx, a.ID); err != nil {
		t.Fatal(err)
	}
	var used pgx.Tx = tx
	if failAt > 0 {
		used = &pgtest.FailingTx{Tx: tx, FailAt: failAt}
	}
	ctx = reqctx.WithPrincipal(reqctx.WithTx(ctx, used), reqctx.Principal{ID: a.ID, Kind: a.Kind, TenantID: ten.ID})
	ctx, _ = audit.WithBuffer(ctx)
	return fn(ctx)
}

// EveryFailure calls fn repeatedly as actor, failing its first, second, ...
// database statement, and fails the test if fn ever hides the failure. It
// stops when fn succeeds with no statement left to fail.
func EveryFailure(t testing.TB, ten pgtest.Tenant, a pgtest.Actor, name string, fn func(ctx context.Context) error) {
	t.Helper()
	for n := 1; n < 100; n++ {
		err := inRequest(t, ten, a, n, fn)
		if err == nil {
			if n == 1 {
				t.Errorf("%s: succeeded without any database statement to fail", name)
			}
			return
		}
		if !errors.Is(err, pgtest.ErrInjected) {
			if p := problem.From(err); p.Status < 500 || !errors.Is(problem.Cause(p), pgtest.ErrInjected) {
				t.Errorf("%s: statement %d failed, but the error was %v", name, n, err)
				return
			}
		}
	}
	t.Errorf("%s: still failing after 100 statements", name)
}

// Client calls the API as one actor of one tenant. Unsafe calls get a fresh
// Idempotency-Key; PATCH bodies are sent as merge patches.
type Client struct {
	T      testing.TB
	API    *API
	Tenant pgtest.Tenant
	As     pgtest.Actor
}

// Do performs a call and returns the status, the decoded body and the ETag.
func (c Client) Do(method, path, body, ifMatch string) (int, map[string]any, string) {
	c.T.Helper()
	req := Req{Method: method, Path: path, Body: body, Tenant: c.Tenant.ID, As: c.As, IfMatch: ifMatch}
	if method != http.MethodGet {
		req.Key = NewKey()
	}
	if method == http.MethodPatch {
		req.Header = http.Header{"Content-Type": {"application/merge-patch+json"}}
	}
	r := c.API.Do(req)
	return r.Code, JSON(c.T, r), r.Header().Get("ETag")
}

// Must performs a call and fails the test unless it answers with code.
func (c Client) Must(code int, method, path, body, ifMatch string) (map[string]any, string) {
	c.T.Helper()
	got, m, tag := c.Do(method, path, body, ifMatch)
	if got != code {
		c.T.Fatalf("%s %s: %d, want %d: %v", method, path, got, code, m)
	}
	return m, tag
}

// Problem performs a call and fails the test unless it answers with the
// status and problem code.
func (c Client) Problem(status int, code, method, path, body, ifMatch string) map[string]any {
	c.T.Helper()
	got, m, _ := c.Do(method, path, body, ifMatch)
	if got != status || m["code"] != code {
		c.T.Errorf("%s %s: %d %v, want %d %s", method, path, got, m, status, code)
	}
	return m
}

// Committed runs fn like InRequest but commits, flushing the audit entries
// as the unit of work does: for fixtures that go through context functions
// without an API operation (tenant bootstrap).
func Committed(t testing.TB, ten pgtest.Tenant, a pgtest.Actor, fn func(ctx context.Context) error) error {
	t.Helper()
	ctx := context.Background()
	tx, err := pool(t).Begin(ctx, database.Scope{Role: "erp_app", TenantID: ten.ID})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := database.SetActor(ctx, tx, a.ID); err != nil {
		t.Fatal(err)
	}
	p := reqctx.Principal{ID: a.ID, Kind: a.Kind, TenantID: ten.ID}
	ctx = reqctx.WithPrincipal(reqctx.WithTx(ctx, tx), p)
	ctx, buf := audit.WithBuffer(ctx)
	if err := fn(ctx); err != nil {
		return err
	}
	if err := buf.Flush(ctx, tx, p, "", time.Now()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
