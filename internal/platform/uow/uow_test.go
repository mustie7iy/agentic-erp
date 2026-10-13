package uow_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn/authntest"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// app is a test API behind authn and the unit of work. Its handlers create
// platform.operation rows so effects are visible in the database.
type app struct {
	t       *testing.T
	issuer  *authntest.Issuer
	handler http.Handler
	mw      *uow.Middleware
	release chan struct{} // closed to let /slow finish
}

func newApp(t *testing.T, pool *database.Pool) *app {
	t.Helper()
	is := authntest.New(t)
	v, err := authn.New(context.Background(), []authn.Issuer{is.Config()})
	if err != nil {
		t.Fatal(err)
	}
	a := &app{t: t, issuer: is, release: make(chan struct{})}
	mux := http.NewServeMux()
	create := func(w http.ResponseWriter, r *http.Request) (uuid.UUID, error) {
		tx, _ := reqctx.TxFrom(r.Context())
		p, _ := reqctx.PrincipalFrom(r.Context())
		id := uuid.Must(uuid.NewV7())
		_, err := tx.Exec(r.Context(), `INSERT INTO platform.operation (tenant_id, id, kind, status, created_by, updated_by)
			VALUES ($1, $2, 'test.create', 'queued', $3, $3)`, p.TenantID, id, p.ID)
		return id, err
	}
	mux.HandleFunc("POST /create", func(w http.ResponseWriter, r *http.Request) {
		id, err := create(w, r)
		if err != nil {
			problem.Write(w, r, problem.From(err))
			return
		}
		a.check(audit.Add(r.Context(), audit.Entry{Action: "test.create", ObjectType: "platform.operation", ObjectID: id, Outcome: "succeeded"}))
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"1"`)
		w.Header().Set("Location", "/v1/operations/"+id.String())
		w.WriteHeader(201)
		a.check(json.NewEncoder(w).Encode(map[string]string{"id": id.String()}))
	})
	mux.HandleFunc("POST /conflict", func(w http.ResponseWriter, r *http.Request) {
		_, err := create(w, r) // undone by the 4xx
		a.check(err)
		problem.Write(w, r, problem.Conflict("test.conflict", "no"))
	})
	mux.HandleFunc("POST /stale", func(w http.ResponseWriter, r *http.Request) {
		_, err := create(w, r)
		a.check(err)
		problem.Write(w, r, problem.PreconditionFailed("re-read"))
	})
	mux.HandleFunc("POST /crash", func(w http.ResponseWriter, r *http.Request) {
		_, err := create(w, r)
		a.check(err)
		problem.Write(w, r, problem.Internal(errors.New("boom")))
	})
	mux.HandleFunc("POST /slow", func(w http.ResponseWriter, r *http.Request) {
		<-a.release
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /not-json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("plain text"))
	})
	mux.HandleFunc("POST /bad-audit", func(w http.ResponseWriter, r *http.Request) {
		// Buffered without validation; the trail rejects it when the unit of work flushes.
		a.check(audit.Add(r.Context(), audit.Entry{Action: "Not Valid", ObjectType: "x", Outcome: "x"}))
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /sabotage", func(w http.ResponseWriter, r *http.Request) {
		tx, _ := reqctx.TxFrom(r.Context())
		_, err := tx.Exec(r.Context(), "RELEASE SAVEPOINT uow")
		a.check(err)
		problem.Write(w, r, problem.Conflict("test.conflict", "no"))
	})
	mux.HandleFunc("POST /deferred", func(w http.ResponseWriter, r *http.Request) {
		// Grants an approval permission to a role an agent holds; the deferred
		// constraint trigger fails the commit.
		tx, _ := reqctx.TxFrom(r.Context())
		p, _ := reqctx.PrincipalFrom(r.Context())
		_, err := tx.Exec(r.Context(), `INSERT INTO identity.role_permission (tenant_id, id, role_id, permission_code, valid_from, created_by)
			SELECT tenant_id, gen_random_uuid(), id, 'workflow.request.decide', current_date, $1 FROM identity.role WHERE code = 'machine'`, p.ID)
		if err != nil {
			problem.Write(w, r, problem.From(err))
			return
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /read", func(w http.ResponseWriter, r *http.Request) {
		p, _ := reqctx.PrincipalFrom(r.Context())
		info := reqctx.InfoFrom(r.Context())
		a.check(json.NewEncoder(w).Encode(map[string]string{"principal": p.ID.String(), "kind": p.Kind, "info": info.PrincipalID.String()}))
	})
	mux.HandleFunc("GET /missing", func(w http.ResponseWriter, r *http.Request) {
		problem.Write(w, r, problem.NotFound("nothing here"))
	})
	a.mw = &uow.Middleware{Pool: pool, Role: "erp_app", Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: time.Now}
	a.handler = v.Middleware(withInfo(a.mw.Wrap(mux)))
	return a
}

func withInfo(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, _ := reqctx.WithInfo(r.Context())
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type call struct {
	method, path, key, body string
	tenant                  uuid.UUID
	subject                 string
}

// check reports an error from a step a handler relies on. It runs inside
// the handler, below the unit of work, so it marks the test failed rather
// than stopping it.
func (a *app) check(err error) {
	a.t.Helper()
	if err != nil {
		a.t.Errorf("handler step: %v", err)
	}
}

func (a *app) do(c call) *httptest.ResponseRecorder {
	a.t.Helper()
	r := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
	r.Header.Set("Authorization", "Bearer "+a.issuer.Token(a.t, c.tenant, c.subject, nil))
	if c.key != "" {
		r.Header.Set("Idempotency-Key", c.key)
	}
	rec := httptest.NewRecorder()
	a.handler.ServeHTTP(rec, r)
	return rec
}

func count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pgtest.Admin(t).QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func operations(t *testing.T, ten pgtest.Tenant) int {
	t.Helper()
	return count(t, "SELECT count(*) FROM platform.operation WHERE tenant_id = $1", ten.ID)
}

func TestIdempotentCreate(t *testing.T) {
	a := newApp(t, pgtest.Pool(t))
	ten := pgtest.NewTenant(t)
	c := call{method: "POST", path: "/create", key: "key-00000001", body: `{"x":1}`, tenant: ten.ID, subject: ten.Human.Subject}
	first := a.do(c)
	if first.Code != 201 || first.Header().Get("Idempotent-Replayed") != "" {
		t.Fatalf("%d %s", first.Code, first.Body)
	}
	replay := a.do(c)
	if replay.Code != 201 || replay.Header().Get("Idempotent-Replayed") != "true" ||
		replay.Header().Get("ETag") != `"1"` || replay.Header().Get("Location") != first.Header().Get("Location") {
		t.Fatalf("replay %d %v", replay.Code, replay.Header())
	}
	var x, y map[string]string
	if err := json.Unmarshal(first.Body.Bytes(), &x); err != nil {
		t.Fatalf("first body %s: %v", first.Body, err)
	}
	if err := json.Unmarshal(replay.Body.Bytes(), &y); err != nil {
		t.Fatalf("replayed body %s: %v", replay.Body, err)
	}
	if x["id"] == "" || x["id"] != y["id"] {
		t.Fatalf("replayed body %v vs %v", y, x)
	}
	if n := operations(t, ten); n != 1 {
		t.Fatalf("%d operations created", n)
	}
	// The audit entry was written in the same transaction, as the actor.
	if n := count(t, "SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'test.create' AND actor_id = $2", ten.ID, ten.Human.ID); n != 1 {
		t.Fatalf("%d audit events", n)
	}
	// The same key for a different body, or by another principal, is refused.
	if r := a.do(call{method: "POST", path: "/create", key: c.key, body: `{"x":2}`, tenant: ten.ID, subject: ten.Human.Subject}); r.Code != 422 {
		t.Fatalf("different body: %d", r.Code)
	}
	if r := a.do(call{method: "POST", path: "/create", key: c.key, body: c.body, tenant: ten.ID, subject: ten.Agent.Subject}); r.Code != 422 {
		t.Fatalf("other principal: %d", r.Code)
	}
	// Keys are per tenant.
	other := pgtest.NewTenant(t)
	if r := a.do(call{method: "POST", path: "/create", key: c.key, body: c.body, tenant: other.ID, subject: other.Human.Subject}); r.Code != 201 {
		t.Fatalf("other tenant: %d", r.Code)
	}
	// An expired key starts afresh.
	pgtest.Exec(t, "UPDATE platform.idempotency_key SET expires_at = now() - interval '1 second' WHERE tenant_id = $1", ten.ID)
	if r := a.do(c); r.Code != 201 || r.Header().Get("Idempotent-Replayed") != "" || operations(t, ten) != 2 {
		t.Fatalf("expired key: %d", r.Code)
	}
}

func TestErrorsKeepTheAnswerNotTheChanges(t *testing.T) {
	a := newApp(t, pgtest.Pool(t))
	ten := pgtest.NewTenant(t)
	c := call{method: "POST", path: "/conflict", key: "key-conflict-1", tenant: ten.ID, subject: ten.Human.Subject}
	if r := a.do(c); r.Code != 409 {
		t.Fatal(r.Code)
	}
	if r := a.do(c); r.Code != 409 || r.Header().Get("Idempotent-Replayed") != "true" || !strings.Contains(r.Body.String(), "test.conflict") {
		t.Fatalf("replayed 4xx: %d %s", r.Code, r.Body)
	}
	if n := operations(t, ten); n != 0 {
		t.Fatalf("4xx kept %d business rows", n)
	}
	// 5xx keeps nothing, so a retry runs again.
	c = call{method: "POST", path: "/crash", key: "key-crash-01", tenant: ten.ID, subject: ten.Human.Subject}
	for i := 0; i < 2; i++ {
		if r := a.do(c); r.Code != 500 || r.Header().Get("Idempotent-Replayed") != "" {
			t.Fatalf("5xx: %d %v", r.Code, r.Header())
		}
	}
	if n := count(t, "SELECT count(*) FROM platform.idempotency_key WHERE tenant_id = $1 AND key = 'key-crash-01'", ten.ID); n != 0 || operations(t, ten) != 0 {
		t.Fatal("5xx kept state")
	}
	// A request error (here 412) is not stored: the corrected retry with the same key runs.
	c = call{method: "POST", path: "/stale", key: "key-stale-01", tenant: ten.ID, subject: ten.Human.Subject}
	if r := a.do(c); r.Code != 412 {
		t.Fatal(r.Code)
	}
	if n := count(t, "SELECT count(*) FROM platform.idempotency_key WHERE tenant_id = $1 AND key = 'key-stale-01'", ten.ID); n != 0 || operations(t, ten) != 0 {
		t.Fatal("412 kept state")
	}
	c.path = "/create"
	if r := a.do(c); r.Code != 201 || r.Header().Get("Idempotent-Replayed") != "" {
		t.Fatalf("corrected retry: %d", r.Code)
	}
}

func TestSafeRequests(t *testing.T) {
	a := newApp(t, pgtest.Pool(t))
	ten := pgtest.NewTenant(t)
	r := a.do(call{method: "GET", path: "/read", tenant: ten.ID, subject: ten.Agent.Subject})
	var got map[string]string
	if err := json.Unmarshal(r.Body.Bytes(), &got); err != nil {
		t.Fatalf("%d %s: %v", r.Code, r.Body, err)
	}
	if r.Code != 200 || got["principal"] != ten.Agent.ID.String() || got["kind"] != "agent" || got["info"] != ten.Agent.ID.String() {
		t.Fatalf("%d %v", r.Code, got)
	}
	if r := a.do(call{method: "GET", path: "/missing", tenant: ten.ID, subject: ten.Human.Subject}); r.Code != 404 {
		t.Fatal(r.Code)
	}
}

func TestRequestValidation(t *testing.T) {
	a := newApp(t, pgtest.Pool(t))
	ten := pgtest.NewTenant(t)
	if r := a.do(call{method: "POST", path: "/create", tenant: ten.ID, subject: ten.Human.Subject}); r.Code != 428 {
		t.Fatalf("missing key: %d", r.Code)
	}
	if r := a.do(call{method: "POST", path: "/create", key: "short", tenant: ten.ID, subject: ten.Human.Subject}); r.Code != 400 {
		t.Fatalf("short key: %d", r.Code)
	}
	big := strings.Repeat("x", uow.MaxBody+1)
	if r := a.do(call{method: "POST", path: "/create", key: "key-big-0001", body: big, tenant: ten.ID, subject: ten.Human.Subject}); r.Code != 413 {
		t.Fatalf("large body: %d", r.Code)
	}
	// Without authentication in front, the unit of work refuses.
	rec := httptest.NewRecorder()
	a.mw.Wrap(http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest("GET", "/read", nil))
	if rec.Code != 401 {
		t.Fatal(rec.Code)
	}
}

func TestPrincipalResolution(t *testing.T) {
	a := newApp(t, pgtest.Pool(t))
	ten := pgtest.NewTenant(t)
	get := func(tenant uuid.UUID, subject string) int {
		return a.do(call{method: "GET", path: "/read", tenant: tenant, subject: subject}).Code
	}
	if c := get(uuid.New(), "anyone"); c != 401 {
		t.Fatalf("unknown tenant: %d", c)
	}
	if c := get(ten.ID, "nobody"); c != 403 {
		t.Fatalf("unknown subject: %d", c)
	}
	pgtest.Exec(t, "UPDATE identity.principal SET status = 'blocked', version = version + 1 WHERE id = $1", ten.Service.ID)
	if c := get(ten.ID, ten.Service.Subject); c != 403 {
		t.Fatalf("blocked principal: %d", c)
	}
	pgtest.Exec(t, "UPDATE tenancy.tenant SET status = 'suspended', version = version + 1 WHERE id = $1", ten.ID)
	if c := get(ten.ID, ten.Human.Subject); c != 403 {
		t.Fatalf("suspended tenant: %d", c)
	}
}

func TestConcurrentRetryWaitsThenGets409(t *testing.T) {
	pool, err := database.Open(context.Background(), pgtest.URL(t), database.Settings{LockTimeout: 200 * time.Millisecond, StatementTimeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	a := newApp(t, pool)
	ten := pgtest.NewTenant(t)
	c := call{method: "POST", path: "/slow", key: "key-slow-0001", tenant: ten.ID, subject: ten.Human.Subject}
	var wg sync.WaitGroup
	wg.Add(1)
	var first *httptest.ResponseRecorder
	go func() { defer wg.Done(); first = a.do(c) }()
	// Lets the first request finish; deferred too, so that a failed wait
	// does not leave it holding its connection while pool.Close waits.
	finish := sync.OnceFunc(func() { close(a.release); wg.Wait() })
	defer finish()
	// Wait until the first request holds the key.
	pgtest.Eventually(t, 10*time.Second, func() bool {
		return count(t, "SELECT count(*) FROM pg_locks l JOIN pg_stat_activity s USING (pid) WHERE s.query LIKE '%SAVEPOINT uow%'") > 0
	}, "the first request never held the key")
	second := a.do(c)
	if second.Code != 409 || second.Header().Get("Retry-After") == "" {
		t.Fatalf("concurrent retry: %d %v", second.Code, second.Header())
	}
	finish()
	if first.Code != 204 {
		t.Fatal(first.Code)
	}
	if third := a.do(c); third.Code != 204 || third.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("after completion: %d", third.Code)
	}
}

func TestFailuresAtTheEnd(t *testing.T) {
	a := newApp(t, pgtest.Pool(t))
	ten := pgtest.NewTenant(t)
	for path, want := range map[string]int{
		"/not-json":  500, // responses must be JSON to be stored
		"/bad-audit": 500, // an audit entry the trail rejects fails the request
		"/sabotage":  500, // the savepoint is gone
		"/deferred":  409, // the commit itself fails
	} {
		if r := a.do(call{method: "POST", path: path, key: "key-" + path[1:] + "-1", tenant: ten.ID, subject: ten.Human.Subject}); r.Code != want {
			t.Errorf("%s: %d %s", path, r.Code, r.Body)
		}
	}
}

// faultTx fails statements that start with a prefix, with err (default:
// errInjected).
type faultTx struct {
	pgx.Tx
	prefix string
	err    error
}

var errInjected = errors.New("injected failure")

func (f faultTx) fault() error {
	if f.err != nil {
		return f.err
	}
	return errInjected
}

func (f faultTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.HasPrefix(strings.TrimSpace(sql), f.prefix) {
		return pgconn.CommandTag{}, f.fault()
	}
	return f.Tx.Exec(ctx, sql, args...)
}

type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

func (f faultTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.HasPrefix(strings.TrimSpace(sql), f.prefix) {
		return errRow{f.fault()}
	}
	return f.Tx.QueryRow(ctx, sql, args...)
}

// TestLostConnectionIsRetryable checks that a connection lost while the
// audit entries are written is a 503 the client retries, not a 500.
func TestLostConnectionIsRetryable(t *testing.T) {
	pool := pgtest.Pool(t)
	a := newApp(t, pool)
	ten := pgtest.NewTenant(t)
	a.mw.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		tx, err := pool.Begin(ctx, s)
		return faultTx{Tx: tx, prefix: "INSERT INTO audit.chain_head", err: &pgconn.PgError{Code: "57P01", Message: "terminating connection"}}, err
	}
	r := a.do(call{method: "POST", path: "/create", key: "key-lost-connection-1", tenant: ten.ID, subject: ten.Human.Subject})
	if r.Code != 503 || r.Header().Get("Retry-After") != "1" || !strings.Contains(r.Body.String(), "connection was lost") {
		t.Fatalf("%d %v %s", r.Code, r.Header(), r.Body)
	}
}

func TestInjectedDatabaseFailures(t *testing.T) {
	pool := pgtest.Pool(t)
	a := newApp(t, pool)
	ten := pgtest.NewTenant(t)
	n := 0
	for _, prefix := range []string{
		"SELECT t.status",                   // principal resolution
		"SELECT set_config('app.actor_id'",  // actor
		"SAVEPOINT uow",                     // savepoint
		"UPDATE platform.idempotency_key",   // storing the response
		"SELECT principal_id, request_hash", // reading an existing key
	} {
		a.mw.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, s)
			return faultTx{Tx: tx, prefix: prefix}, err
		}
		n++
		key := "key-fault-" + string(rune('a'+n)) + "-xx"
		if prefix == "SELECT principal_id, request_hash" {
			a.mw.Begin = nil
			a.do(call{method: "POST", path: "/create", key: key, tenant: ten.ID, subject: ten.Human.Subject})
			a.mw.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
				tx, err := pool.Begin(ctx, s)
				return faultTx{Tx: tx, prefix: prefix}, err
			}
		}
		if r := a.do(call{method: "POST", path: "/create", key: key, tenant: ten.ID, subject: ten.Human.Subject}); r.Code != 500 {
			t.Errorf("%s: %d %s", prefix, r.Code, r.Body)
		}
	}
	a.mw.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errInjected }
	if r := a.do(call{method: "GET", path: "/read", tenant: ten.ID, subject: ten.Human.Subject}); r.Code != 500 {
		t.Fatalf("begin failure: %d", r.Code)
	}
}
