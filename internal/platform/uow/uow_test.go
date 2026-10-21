package uow_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
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
	entered chan struct{} // closed when a request enters /slow: it holds its idempotency key
	slow    atomic.Int32  // requests that entered /slow
	commits atomic.Int32  // creates whose commit ran their OnCommit
}

func newApp(t *testing.T, pool *database.Pool, also ...*authntest.Issuer) *app {
	t.Helper()
	is := authntest.New(t)
	trusted := []authn.Issuer{is.Config()}
	for _, o := range also {
		trusted = append(trusted, o.Config())
	}
	v, err := authn.New(context.Background(), authn.Development, trusted)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{t: t, issuer: is, release: make(chan struct{}), entered: make(chan struct{})}
	mux := http.NewServeMux()
	create := func(w http.ResponseWriter, r *http.Request) (uuid.UUID, error) {
		tx, _ := reqctx.TxFrom(r.Context())
		p, _ := reqctx.PrincipalFrom(r.Context())
		id := uuid.Must(uuid.NewV7())
		reqctx.OnCommit(r.Context(), func() { a.commits.Add(1) })
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
	mux.HandleFunc("POST /refuse", func(w http.ResponseWriter, r *http.Request) {
		// A write refused by a rule: its work and entries roll back, its
		// refusal is kept with the answer (ADR-0046).
		id, err := create(w, r)
		a.check(err)
		a.check(audit.Add(r.Context(), audit.Entry{Action: "test.create", ObjectType: "platform.operation", ObjectID: id, Outcome: "succeeded"}))
		a.check(audit.Refuse(r.Context(), audit.Entry{Action: "test.create", ObjectType: "platform.operation", Outcome: "refused",
			Detail: map[string]any{"source": "rule"}}))
		problem.Write(w, r, problem.Unprocessable("request.rule_refused", "no"))
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
		// One request per app may wait here. A second means the key did not
		// hold it off; waiting for release would hang the test, so it fails.
		if n := a.slow.Add(1); n > 1 {
			a.check(fmt.Errorf("/slow: request %d ran the handler while the key should hold it off", n))
			w.WriteHeader(500)
			return
		}
		close(a.entered)
		<-a.release
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /secret", func(w http.ResponseWriter, r *http.Request) {
		uow.OmitOnReplay(r.Context(), "secret")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		a.check(json.NewEncoder(w).Encode(map[string]string{"id": "x", "secret": "whsec_shown_once"}))
	})
	mux.HandleFunc("POST /secret-array", func(w http.ResponseWriter, r *http.Request) {
		uow.OmitOnReplay(r.Context(), "secret")
		w.Write([]byte(`["whsec_shown_once"]`))
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
	isolation := func(w http.ResponseWriter, r *http.Request) {
		tx, _ := reqctx.TxFrom(r.Context()) // the unit of work always sets it before the handler runs
		var level string
		a.check(tx.QueryRow(r.Context(), `SHOW transaction_isolation`).Scan(&level))
		w.Header().Set("Content-Type", "application/json")
		a.check(json.NewEncoder(w).Encode(level))
	}
	mux.HandleFunc("GET /isolation", isolation)
	mux.HandleFunc("GET /snapshot", isolation)
	mux.HandleFunc("POST /snapshot", isolation)
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

// in makes the app report to the subtest t until t ends, so a failure in a
// handler or a request fails that subtest rather than its parent. The
// handlers hold a, not a copy, so the app is switched in place; subtests
// using it run one at a time.
func (a *app) in(t *testing.T) *app {
	t.Helper()
	parent := a.t
	a.t = t
	t.Cleanup(func() { a.t = parent })
	return a
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
	pgtest.Scan(t, sql, args, &n) // a connection per call, closed after: count runs in polling waits
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
	if first.Code != 201 {
		t.Fatalf("first status: got %d, want 201 (%s)", first.Code, first.Body)
	}
	if got := first.Header().Get("Idempotent-Replayed"); got != "" {
		t.Fatalf("first Idempotent-Replayed: got %q, want empty", got)
	}
	replay := a.do(c)
	if replay.Code != 201 {
		t.Fatalf("replay status: got %d, want 201 (%s)", replay.Code, replay.Body)
	}
	if got := replay.Header().Get("Idempotent-Replayed"); got != "true" {
		t.Fatalf("replay Idempotent-Replayed: got %q, want true", got)
	}
	if got := replay.Header().Get("ETag"); got != `"1"` {
		t.Fatalf("replay ETag: got %s, want \"1\"", got)
	}
	if got, want := replay.Header().Get("Location"), first.Header().Get("Location"); got != want {
		t.Fatalf("replay Location: got %q, want %q", got, want)
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
	r := a.do(c)
	if r.Code != 201 {
		t.Fatalf("expired key status: got %d, want 201 (%s)", r.Code, r.Body)
	}
	if got := r.Header().Get("Idempotent-Replayed"); got != "" {
		t.Fatalf("expired key Idempotent-Replayed: got %q, want empty", got)
	}
	if n := operations(t, ten); n != 2 {
		t.Fatalf("operations after expired key: got %d, want 2", n)
	}
}

// TestRefusalKeptWithTheAnswer: a business outcome keeps a refusal's
// audit, and only it, with the stored answer; the replayed answer records
// no second refusal.
func TestRefusalKeptWithTheAnswer(t *testing.T) {
	a := newApp(t, pgtest.Pool(t))
	ten := pgtest.NewTenant(t)
	c := call{method: "POST", path: "/refuse", key: "key-refuse-1", tenant: ten.ID, subject: ten.Human.Subject}
	for i := range 2 {
		if r := a.do(c); r.Code != 422 {
			t.Fatalf("attempt %d status: got %d, want 422 (%s)", i+1, r.Code, r.Body)
		}
	}
	audited := func(outcome string) int {
		return count(t, "SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'test.create' AND outcome = $2", ten.ID, outcome)
	}
	if n := audited("refused"); n != 1 {
		t.Errorf("refusals audited: got %d, want 1", n)
	}
	if n := audited("succeeded"); n != 0 {
		t.Errorf("rolled-back entries audited: got %d, want 0", n)
	}
	if n := operations(t, ten); n != 0 {
		t.Errorf("rows kept: got %d, want 0", n)
	}
}

func TestErrorsKeepTheAnswerNotTheChanges(t *testing.T) {
	a := newApp(t, pgtest.Pool(t))
	ten := pgtest.NewTenant(t)
	c := call{method: "POST", path: "/conflict", key: "key-conflict-1", tenant: ten.ID, subject: ten.Human.Subject}
	if r := a.do(c); r.Code != 409 {
		t.Fatal(r.Code)
	}
	r := a.do(c)
	if r.Code != 409 {
		t.Fatalf("replayed 4xx status: got %d, want 409 (%s)", r.Code, r.Body)
	}
	if got := r.Header().Get("Idempotent-Replayed"); got != "true" {
		t.Fatalf("replayed 4xx Idempotent-Replayed: got %q, want true", got)
	}
	if !strings.Contains(r.Body.String(), "test.conflict") {
		t.Fatalf("replayed 4xx body: got %s, want test.conflict", r.Body)
	}
	if n := operations(t, ten); n != 0 {
		t.Fatalf("4xx kept %d business rows", n)
	}
	// 5xx keeps nothing, so a retry runs again.
	c = call{method: "POST", path: "/crash", key: "key-crash-01", tenant: ten.ID, subject: ten.Human.Subject}
	for i := 0; i < 2; i++ {
		r := a.do(c)
		if r.Code != 500 {
			t.Fatalf("5xx attempt %d status: got %d, want 500", i+1, r.Code)
		}
		if got := r.Header().Get("Idempotent-Replayed"); got != "" {
			t.Fatalf("5xx attempt %d Idempotent-Replayed: got %q, want empty", i+1, got)
		}
	}
	if n := count(t, "SELECT count(*) FROM platform.idempotency_key WHERE tenant_id = $1 AND key = 'key-crash-01'", ten.ID); n != 0 {
		t.Fatalf("5xx kept keys: got %d, want 0", n)
	}
	if n := operations(t, ten); n != 0 {
		t.Fatalf("5xx kept operations: got %d, want 0", n)
	}
	// A request error (here 412) is not stored: the corrected retry with the same key runs.
	c = call{method: "POST", path: "/stale", key: "key-stale-01", tenant: ten.ID, subject: ten.Human.Subject}
	if r := a.do(c); r.Code != 412 {
		t.Fatal(r.Code)
	}
	if n := count(t, "SELECT count(*) FROM platform.idempotency_key WHERE tenant_id = $1 AND key = 'key-stale-01'", ten.ID); n != 0 {
		t.Fatalf("412 kept keys: got %d, want 0", n)
	}
	if n := operations(t, ten); n != 0 {
		t.Fatalf("412 kept operations: got %d, want 0", n)
	}
	c.path = "/create"
	r = a.do(c)
	if r.Code != 201 {
		t.Fatalf("corrected retry status: got %d, want 201", r.Code)
	}
	if got := r.Header().Get("Idempotent-Replayed"); got != "" {
		t.Fatalf("corrected retry Idempotent-Replayed: got %q, want empty", got)
	}
}

// TestOnCommit: what a request adds with reqctx.OnCommit runs once its
// work commits, and not for a 4xx or 5xx whose changes are undone.
func TestOnCommit(t *testing.T) {
	a := newApp(t, pgtest.Pool(t))
	ten := pgtest.NewTenant(t)
	for _, c := range []struct {
		path string
		want int32
	}{{"/conflict", 0}, {"/crash", 0}, {"/create", 1}} {
		t.Run(c.path, func(t *testing.T) {
			a.in(t).do(call{method: "POST", path: c.path, key: "key-commit" + strings.ReplaceAll(c.path, "/", "-"), tenant: ten.ID, subject: ten.Human.Subject})
			if got := a.commits.Load(); got != c.want {
				t.Errorf("commit hooks run: got %d, want %d", got, c.want)
			}
		})
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
	if r.Code != 200 {
		t.Fatalf("status: got %d, want 200 (%v)", r.Code, got)
	}
	if want := ten.Agent.ID.String(); got["principal"] != want {
		t.Errorf("principal: got %s, want %s", got["principal"], want)
	}
	if got["kind"] != "agent" {
		t.Errorf("kind: got %s, want agent", got["kind"])
	}
	if want := ten.Agent.ID.String(); got["info"] != want {
		t.Errorf("info: got %s, want %s", got["info"], want)
	}
	if r := a.do(call{method: "GET", path: "/missing", tenant: ten.ID, subject: ten.Human.Subject}); r.Code != 404 {
		t.Fatal(r.Code)
	}
}

// TestSnapshotRequests: a safe request the Snapshot hook marks reads at
// REPEATABLE READ, any other request at READ COMMITTED; an unsafe request
// is never marked, whatever the hook says.
func TestSnapshotRequests(t *testing.T) {
	a := newApp(t, pgtest.Pool(t))
	a.mw.Snapshot = func(r *http.Request) bool { return r.URL.Path == "/snapshot" }
	ten := pgtest.NewTenant(t)
	for _, c := range []struct {
		name, method, path, want string
	}{
		{"marked", "GET", "/snapshot", "repeatable read"},
		{"not marked", "GET", "/isolation", "read committed"},
		{"unsafe", "POST", "/snapshot", "read committed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := a.in(t).do(call{method: c.method, path: c.path, key: "snapshot-" + uuid.NewString(), tenant: ten.ID, subject: ten.Human.Subject})
			if got := strings.TrimSpace(r.Body.String()); r.Code != 200 || got != `"`+c.want+`"` {
				t.Errorf("isolation: got %d %q, want 200 %q", r.Code, got, c.want)
			}
		})
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

// TestPrincipalsBoundToIssuer: a principal is its issuer's subject, not any
// trusted issuer's (ADR-0010, F7). Another trusted issuer, say another
// tenant's identity provider, that names the same subject in this tenant
// never resolves to this tenant's principal.
func TestPrincipalsBoundToIssuer(t *testing.T) {
	other := authntest.NewSeparate(t)
	a := newApp(t, pgtest.Pool(t), other)
	ten := pgtest.NewTenant(t)
	read := func(is *authntest.Issuer, tenant uuid.UUID, subject string) (int, map[string]string) {
		r := httptest.NewRequest("GET", "/read", nil)
		r.Header.Set("Authorization", "Bearer "+is.Token(t, tenant, subject, nil))
		rec := httptest.NewRecorder()
		a.handler.ServeHTTP(rec, r)
		var body map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &body) // the status is checked first; a non-JSON body leaves the map empty
		return rec.Code, body
	}
	code, body := read(a.issuer, ten.ID, ten.Human.Subject)
	if code != 200 {
		t.Fatalf("own issuer: got %d %v, want 200", code, body)
	}
	if body["principal"] != ten.Human.ID.String() {
		t.Fatalf("own issuer: got principal %v, want %s", body["principal"], ten.Human.ID)
	}
	code, body = read(other, ten.ID, ten.Human.Subject)
	if code != 403 {
		t.Fatalf("other issuer, same subject: got %d %v, want 403", code, body)
	}
	if body["code"] != "identity.principal_unknown" {
		t.Errorf("other issuer, same subject: got problem %v, want identity.principal_unknown", body["code"])
	}
	// The other issuer's own tenant has a principal with the same subject:
	// each issuer's token resolves to its own principal, never the other's.
	ten2 := pgtest.NewTenant(t)
	theirs := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO identity.principal (tenant_id, id, kind, external_issuer, external_subject, display_name, status, created_by, updated_by)
		VALUES ($1, $2, 'human', $3, $4, 'Theirs', 'active', $2, $2)`, ten2.ID, theirs, other.URL, ten.Human.Subject)
	code, body = read(other, ten2.ID, ten.Human.Subject)
	if code != 200 {
		t.Fatalf("other issuer in its tenant: got %d %v, want 200", code, body)
	}
	if body["principal"] != theirs.String() {
		t.Errorf("other issuer in its tenant: got principal %v, want %s", body["principal"], theirs)
	}
	if code, _ := read(a.issuer, ten2.ID, ten.Human.Subject); code != 403 {
		t.Errorf("own issuer in the other tenant: got %d, want 403", code)
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
	// The unit of work claims the key before it runs the handler, so the
	// first request holds the key once it is in /slow. (A wait on
	// pg_stat_activity matched its own query text and other databases'
	// sessions, and ended at once.)
	select {
	case <-a.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the first request never reached the handler")
	}
	second := a.do(c)
	if second.Code != 409 {
		t.Fatalf("concurrent retry status: got %d, want 409", second.Code)
	}
	if second.Header().Get("Retry-After") == "" {
		t.Fatalf("concurrent retry Retry-After: got empty, want set (%v)", second.Header())
	}
	finish()
	if first.Code != 204 {
		t.Fatalf("first request status: got %d, want 204", first.Code)
	}
	third := a.do(c)
	if third.Code != 204 {
		t.Fatalf("after completion status: got %d, want 204", third.Code)
	}
	if got := third.Header().Get("Idempotent-Replayed"); got != "true" {
		t.Fatalf("after completion Idempotent-Replayed: got %q, want true", got)
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
		t.Run(path, func(t *testing.T) {
			if r := a.in(t).do(call{method: "POST", path: path, key: "key-" + path[1:] + "-1", tenant: ten.ID, subject: ten.Human.Subject}); r.Code != want {
				t.Errorf("got %d, want %d (%s)", r.Code, want, r.Body)
			}
		})
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
	if r.Code != 503 {
		t.Fatalf("status: got %d, want 503 (%s)", r.Code, r.Body)
	}
	if got := r.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After: got %q, want 1", got)
	}
	if !strings.Contains(r.Body.String(), "connection was lost") {
		t.Errorf("body: got %s, want it to mention the lost connection", r.Body)
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
		n++
		key := "key-fault-" + string(rune('a'+n)) + "-xx"
		t.Run(prefix, func(t *testing.T) {
			a := a.in(t)
			a.mw.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
				tx, err := pool.Begin(ctx, s)
				return faultTx{Tx: tx, prefix: prefix}, err
			}
			if prefix == "SELECT principal_id, request_hash" {
				a.mw.Begin = nil
				a.do(call{method: "POST", path: "/create", key: key, tenant: ten.ID, subject: ten.Human.Subject})
				a.mw.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
					tx, err := pool.Begin(ctx, s)
					return faultTx{Tx: tx, prefix: prefix}, err
				}
			}
			if r := a.do(call{method: "POST", path: "/create", key: key, tenant: ten.ID, subject: ten.Human.Subject}); r.Code != 500 {
				t.Errorf("got %d, want 500 (%s)", r.Code, r.Body)
			}
		})
	}
	a.mw.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errInjected }
	if r := a.do(call{method: "GET", path: "/read", tenant: ten.ID, subject: ten.Human.Subject}); r.Code != 500 {
		t.Fatalf("begin failure: %d", r.Code)
	}
}

// TestOmitOnReplay: a field the handler omits is answered once and not kept
// with the idempotency key, so a replay answers without it (ADR-0041).
func TestOmitOnReplay(t *testing.T) {
	a := newApp(t, pgtest.Pool(t))
	ten := pgtest.NewTenant(t)
	c := call{method: "POST", path: "/secret", key: "key-secret01", tenant: ten.ID, subject: ten.Human.Subject}
	first := a.do(c)
	if !strings.Contains(first.Body.String(), "whsec_shown_once") {
		t.Fatalf("first answer: got %s, want the secret", first.Body)
	}
	if n := count(t, `SELECT count(*) FROM platform.idempotency_key WHERE tenant_id = $1 AND response_body::text LIKE '%whsec_%'`, ten.ID); n != 0 {
		t.Errorf("stored answers with the secret: got %d, want 0", n)
	}
	replay := a.do(c)
	if replay.Code != 201 || replay.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: got %d %v, want a replayed 201", replay.Code, replay.Header())
	}
	var got map[string]string
	if err := json.Unmarshal(replay.Body.Bytes(), &got); err != nil {
		t.Fatalf("replay body %s: %v", replay.Body, err)
	}
	if _, ok := got["secret"]; ok || got["id"] != "x" {
		t.Errorf("replay body: got %v, want the id without the secret", got)
	}
	// A body that is not an object cannot have the field taken out: the
	// request fails rather than keep the secret.
	if r := a.do(call{method: "POST", path: "/secret-array", key: "key-secret02", tenant: ten.ID, subject: ten.Human.Subject}); r.Code != 500 {
		t.Errorf("array body: got %d, want 500", r.Code)
	}
}
