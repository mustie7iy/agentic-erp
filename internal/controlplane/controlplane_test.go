package controlplane_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api"
	"github.com/mustie7iy/agentic-erp/internal/controlplane"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn/authntest"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/provisioning"
)

func service(t *testing.T) *controlplane.Service {
	t.Helper()
	pool := pgtest.Pool(t)
	pages, err := pagination.NewCodec([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	keys := apitest.FieldKeys()
	return &controlplane.Service{Pool: pool, AppRole: "erp_app", ControlRole: "erp_control", Pages: pages, Keys: keys,
		Provisioner: &provisioning.Provisioner{Pool: pool, AppRole: "erp_app", Keys: keys, Configuration: httpapi.Configuration(httpapi.Config{FieldKeys: keys})}}
}

// plane is the control plane on a listener with an operator identity provider.
type plane struct {
	t   *testing.T
	s   *controlplane.Service
	is  *authntest.Issuer
	srv *httptest.Server
}

func newPlane(t *testing.T) *plane {
	t.Helper()
	is := authntest.New(t)
	v, err := authn.New(context.Background(), []authn.Issuer{is.Config()})
	if err != nil {
		t.Fatal(err)
	}
	s := service(t)
	srv := httptest.NewServer(controlplane.Handler(s, v, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(srv.Close)
	return &plane{t: t, s: s, is: is, srv: srv}
}

// operator mints an operator token: the issuer's token without a tenant.
func (p *plane) operator(subject string) string {
	return p.is.Token(p.t, uuid.Nil, subject, map[string]any{"tenant_id": nil})
}

// do performs a call with the given headers (Authorization included when
// present) and returns the status, the decoded body and the headers.
func (p *plane) do(method, path, body string, headers map[string]string) (int, map[string]any, http.Header) {
	p.t.Helper()
	req, err := http.NewRequest(method, p.srv.URL+path, strings.NewReader(body))
	if err != nil {
		p.t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		p.t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	raw, _ := io.ReadAll(resp.Body)
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &m); err != nil {
			p.t.Fatalf("%s %s: %d, not JSON: %s", method, path, resp.StatusCode, raw)
		}
	}
	return resp.StatusCode, m, resp.Header
}

// as performs a call as the operator with extra headers.
func (p *plane) as(subject, method, path, body string, extra ...string) (int, map[string]any, http.Header) {
	p.t.Helper()
	h := map[string]string{"Authorization": "Bearer " + p.operator(subject), "Content-Type": "application/json"}
	for i := 0; i+1 < len(extra); i += 2 {
		h[extra[i]] = extra[i+1]
	}
	return p.do(method, path, body, h)
}

// must performs a call that has to answer with code.
func (p *plane) must(code int, subject, method, path, body string, extra ...string) (map[string]any, http.Header) {
	p.t.Helper()
	got, m, h := p.as(subject, method, path, body, extra...)
	if got != code {
		p.t.Fatalf("%s %s: %d %v, want %d", method, path, got, m, code)
	}
	return m, h
}

// problem performs a call that has to fail with the status and problem code.
func (p *plane) problem(status int, code, subject, method, path, body string, extra ...string) map[string]any {
	p.t.Helper()
	got, m, _ := p.as(subject, method, path, body, extra...)
	if got != status || m["code"] != code {
		p.t.Errorf("%s %s: %d %v, want %d %s", method, path, got, m, status, code)
	}
	return m
}

// tenantBody is a valid TenantCreate with the fixture configuration.
func tenantBody(t *testing.T, name string) string {
	t.Helper()
	doc, _ := json.Marshal(fixture.Document(t))
	return `{"name": "` + name + `", "region": "eu-west", "group_currency": "EUR", "administrator": {"subject": "idp|ada", "display_name": "Ada", "email": "ada@example.com"},
		"configuration": ` + string(doc) + `}`
}

const tenants = controlplane.Base + "/tenants"

func TestTenantsLifecycleThroughTheControlPlane(t *testing.T) {
	p := newPlane(t)
	ctx := context.Background()

	// Only operators get in: no token, a tenant's token and a token from an untrusted issuer are refused.
	if code, m, _ := p.do("GET", tenants, "", nil); code != 401 || m["code"] != "authn.invalid_token" {
		t.Fatalf("no token: %d %v", code, m)
	}
	if code, _, _ := p.do("GET", tenants, "", map[string]string{"Authorization": "Bearer " + p.is.Token(t, uuid.New(), "idp|ada", nil)}); code != 401 {
		t.Fatalf("tenant token: %d", code)
	}
	if code, _, _ := p.do("GET", tenants, "", map[string]string{"Authorization": "Bearer " + authntest.New(t).Token(t, uuid.Nil, "ops", map[string]any{"tenant_id": nil})}); code != 401 {
		t.Fatalf("untrusted operator: %d", code)
	}

	// Provisioning: the tenant is active with everything made for it; the Idempotency-Key replays it.
	key := "key-" + uuid.NewString()
	body := tenantBody(t, "Acme")
	created, h := p.must(201, "ops|alice", "POST", tenants, body, "Idempotency-Key", key)
	id := created["id"].(string)
	prov := created["provisioned"].(map[string]any)
	if created["status"] != "active" || h.Get("ETag") != `"1"` || h.Get("Location") != tenants+"/"+id || prov["configuration_version"] != float64(1) ||
		len(prov["legal_entities"].(map[string]any)) != 2 || prov["tenant_id"] != id || h.Get("Idempotent-Replayed") != "" {
		t.Fatalf("created: %v %v", created, h)
	}
	again, h := p.must(201, "ops|alice", "POST", tenants, body, "Idempotency-Key", key)
	if again["id"] != id || h.Get("Idempotent-Replayed") != "true" || again["provisioned"].(map[string]any)["administrator_id"] != prov["administrator_id"] {
		t.Fatalf("replay: %v %v", again, h)
	}
	p.problem(422, "request.idempotency_key_reused", "ops|alice", "POST", tenants, tenantBody(t, "Other"), "Idempotency-Key", key)
	p.problem(422, "request.idempotency_key_reused", "ops|bob", "POST", tenants, body, "Idempotency-Key", key)
	// The request's rules.
	p.problem(428, "request.precondition_required", "ops|alice", "POST", tenants, body)
	p.problem(400, "request.invalid_idempotency_key", "ops|alice", "POST", tenants, body, "Idempotency-Key", "short")
	p.problem(400, "request.invalid_body", "ops|alice", "POST", tenants, `{"name": `, "Idempotency-Key", "key-"+uuid.NewString())
	p.problem(400, "request.invalid_body", "ops|alice", "POST", tenants, `{"name": "x", "bogus": 1}`, "Idempotency-Key", "key-"+uuid.NewString())
	p.problem(413, "request.body_too_large", "ops|alice", "POST", tenants, `{"name": "`+strings.Repeat("x", 5<<20)+`"}`, "Idempotency-Key", "key-"+uuid.NewString())
	invalid := p.problem(422, "tenancy.invalid_tenant", "ops|alice", "POST", tenants, `{"name": "Acme", "region": "eu-west", "group_currency": "EUR",
		"administrator": {"subject": "idp|ada", "display_name": "Ada"}, "configuration": {"bogus": 1}}`, "Idempotency-Key", "key-"+uuid.NewString())
	if errs := invalid["errors"].([]any); errs[0].(map[string]any)["field"] != "configuration" {
		t.Fatalf("configuration error: %v", invalid)
	}
	noDoc := p.problem(422, "tenancy.invalid_tenant", "ops|alice", "POST", tenants, `{"name": "Acme", "region": "eu-west", "group_currency": "EUR",
		"administrator": {"subject": "idp|ada", "display_name": "Ada"}}`, "Idempotency-Key", "key-"+uuid.NewString())
	if errs := noDoc["errors"].([]any); errs[0].(map[string]any)["field"] != "document" {
		t.Fatalf("missing document: %v", noDoc)
	}

	// Listing: filters, order and pages across the cell.
	second, _ := p.must(201, "ops|alice", "POST", tenants, tenantBody(t, "Beta"), "Idempotency-Key", "key-"+uuid.NewString())
	page, _ := p.must(200, "ops|alice", "GET", tenants+"?status=active&page_size=1", "")
	if items := page["items"].([]any); len(items) != 1 || page["next_page_token"] == nil {
		t.Fatalf("first page: %v", page)
	}
	seen := map[string]bool{}
	next, pages := "", 0
	for {
		path := tenants + "?status=active&page_size=1"
		if next != "" {
			path += "&page_token=" + next
		}
		page, _ := p.must(200, "ops|alice", "GET", path, "")
		for _, it := range page["items"].([]any) {
			seen[it.(map[string]any)["id"].(string)] = true
		}
		pages++
		if page["next_page_token"] == nil {
			break
		}
		next = page["next_page_token"].(string)
	}
	if !seen[id] || !seen[second["id"].(string)] || pages < 2 {
		t.Fatalf("paged %d pages, saw %v", pages, seen)
	}
	if first, _ := p.must(200, "ops|alice", "GET", tenants+"?order_by=name%20asc&page_size=1", ""); first["next_page_token"] == nil {
		t.Fatalf("by name, one per page: %v", first)
	}
	byName, _ := p.must(200, "ops|alice", "GET", tenants+"?order_by=name%20asc&page_size=500", "")
	names := []string{}
	for _, it := range byName["items"].([]any) {
		names = append(names, it.(map[string]any)["name"].(string))
	}
	if len(names) < 2 || names[0] > names[1] {
		t.Fatalf("by name: %v", names)
	}
	p.problem(400, "request.invalid_parameter", "ops|alice", "GET", tenants+"?status=deleted", "")
	p.problem(400, "request.invalid_parameter", "ops|alice", "GET", tenants+"?page_size=many", "")
	p.problem(400, "request.invalid_page_size", "ops|alice", "GET", tenants+"?page_size=0", "")
	p.problem(400, "request.invalid_order_by", "ops|alice", "GET", tenants+"?order_by=region", "")
	p.problem(400, "request.invalid_page_token", "ops|alice", "GET", tenants+"?page_token=x.y", "")

	// Reading one.
	got, h := p.must(200, "ops|alice", "GET", tenants+"/"+id, "")
	if got["name"] != "Acme" || h.Get("ETag") != `"1"` {
		t.Fatalf("get: %v %v", got, h)
	}
	p.problem(404, "request.not_found", "ops|alice", "GET", tenants+"/"+uuid.NewString(), "")
	p.problem(400, "request.invalid_parameter", "ops|alice", "GET", tenants+"/not-a-uuid", "")
	p.problem(404, "request.not_found", "ops|alice", "GET", controlplane.Base+"/nothing", "")
	if code, m, h := p.as("ops|alice", "PUT", tenants, ""); code != 405 || h.Get("Allow") != "GET, POST" || m["code"] != "request.method_not_allowed" {
		t.Fatalf("PUT: %d %v %v", code, m, h)
	}
	if code, _, h := p.as("ops|alice", "POST", tenants+"/"+id, ""); code != 405 || h.Get("Allow") != "GET" {
		t.Fatalf("POST item: %d %v", code, h)
	}
	if code, _, h := p.as("ops|alice", "GET", tenants+"/"+id+":suspend", ""); code != 405 || h.Get("Allow") != "POST" {
		t.Fatalf("GET custom: %d %v", code, h)
	}

	// Transitions: guarded by If-Match, refused in the wrong status, felt at the public API's door.
	ten := pgtest.Tenant{ID: uuid.MustParse(id)}
	admin := pgtest.Actor{ID: uuid.MustParse(prov["administrator_id"].(string)), Subject: "idp|ada", Kind: "human"}
	public := apitest.Client{T: t, API: apitest.New(t), Tenant: ten, As: admin}
	public.Must(200, "GET", "/v1/roles", "", "")
	p.problem(428, "request.precondition_required", "ops|alice", "POST", tenants+"/"+id+":suspend", "")
	p.problem(412, "request.version_mismatch", "ops|alice", "POST", tenants+"/"+id+":suspend", "", "If-Match", `"9"`)
	p.problem(404, "request.not_found", "ops|alice", "POST", tenants+"/"+id+":destroy", "", "If-Match", `"1"`)
	p.problem(404, "request.not_found", "ops|alice", "POST", tenants+"/"+uuid.NewString()+":suspend", "", "If-Match", `"1"`)
	p.problem(409, "tenancy.invalid_transition", "ops|alice", "POST", tenants+"/"+id+":resume", "", "If-Match", `"1"`)
	suspended, h := p.must(200, "ops|alice", "POST", tenants+"/"+id+":suspend", "", "If-Match", `"1"`)
	if suspended["status"] != "suspended" || suspended["version"] != float64(2) || h.Get("ETag") != `"2"` {
		t.Fatalf("suspend: %v %v", suspended, h)
	}
	public.Problem(403, "tenancy.tenant_inactive", "GET", "/v1/roles", "", "")
	p.problem(409, "tenancy.invalid_transition", "ops|alice", "POST", tenants+"/"+id+":suspend", "", "If-Match", `"2"`)
	resumed, _ := p.must(200, "ops|alice", "POST", tenants+"/"+id+":resume", "", "If-Match", `"2"`)
	if resumed["status"] != "active" {
		t.Fatalf("resume: %v", resumed)
	}
	public.Must(200, "GET", "/v1/roles", "", "")
	offboarded, _ := p.must(200, "ops|bob", "POST", tenants+"/"+id+":offboard", "", "If-Match", "*")
	if offboarded["status"] != "offboarded" || offboarded["version"] != float64(4) {
		t.Fatalf("offboard: %v", offboarded)
	}
	public.Problem(403, "tenancy.tenant_inactive", "GET", "/v1/roles", "", "")
	p.problem(409, "tenancy.invalid_transition", "ops|alice", "POST", tenants+"/"+id+":resume", "", "If-Match", `"4"`)
	p.problem(409, "tenancy.invalid_transition", "ops|alice", "POST", tenants+"/"+id+":offboard", "", "If-Match", `"4"`)
	p.problem(409, "tenancy.invalid_transition", "ops|alice", "POST", tenants+"/"+id+":suspend", "", "If-Match", `"4"`)
	filtered, _ := p.must(200, "ops|alice", "GET", tenants+"?status=offboarded", "")
	if items := filtered["items"].([]any); len(items) != 1 || items[0].(map[string]any)["id"] != id {
		t.Fatalf("offboarded: %v", filtered)
	}

	// Every change was published and recorded in the tenant's chain as the operator, kind system.
	events := apitest.Published(t, ten, outbox.Event{Context: "tenancy", Name: "tenant_status_changed"})
	statuses := []string{}
	for _, e := range events {
		statuses = append(statuses, e["previous_status"].(string)+">"+e["status"].(string))
	}
	if strings.Join(statuses, " ") != "provisioning>active active>suspended suspended>active active>offboarded" {
		t.Fatalf("events: %v", statuses)
	}
	conn := pgtest.Admin(t)
	rows, _ := conn.Query(ctx, `SELECT actor_id, actor_kind, action, detail->>'operator' FROM audit.audit_event WHERE tenant_id = $1 AND action LIKE 'tenancy.tenant.%' ORDER BY seq`, ten.ID)
	type entry struct {
		Actor    uuid.UUID
		Kind     string
		Action   string
		Operator string
	}
	entries, err := pgx.CollectRows(rows, pgx.RowToStructByPos[entry])
	if err != nil || len(entries) != 4 {
		t.Fatalf("audit: %v %v", entries, err)
	}
	alice := controlplane.OperatorFrom(reqctx.Claims{Issuer: p.is.URL, Subject: "ops|alice"})
	if entries[0].Kind != "human" || entries[0].Operator != "ops|alice" || entries[0].Action != "tenancy.tenant.provision" {
		t.Fatalf("provision entry: %+v", entries[0])
	}
	for i, want := range []string{"suspend", "resume", "offboard"} {
		e := entries[i+1]
		if e.Kind != "system" || e.Action != "tenancy.tenant."+want || (want != "offboard" && (e.Actor != alice.ID || e.Operator != "ops|alice")) {
			t.Errorf("%s: %+v", want, e)
		}
	}
	if entries[3].Actor == alice.ID || entries[3].Operator != "ops|bob" {
		t.Errorf("offboard by bob: %+v", entries[3])
	}
	tx, _ := conn.Begin(ctx)
	defer tx.Rollback(ctx)
	if res, err := audit.Verify(ctx, tx, ten.ID); err != nil || res.BrokenAt != 0 || res.Checked < 4 {
		t.Fatalf("chain: %+v %v", res, err)
	}
}

// TestContractIsServed checks that every operation of api/control.yaml is
// routed: no operation answers "no such endpoint" or "method not allowed".
func TestContractIsServed(t *testing.T) {
	p := newPlane(t)
	ops, err := api.ControlOperations()
	if err != nil {
		t.Fatal(err)
	}
	for id, op := range ops {
		path := strings.ReplaceAll(op.Path, "{id}", uuid.NewString())
		code, m, _ := p.as("ops|alice", op.Method, path, "")
		if code == 405 || (code == 404 && m["detail"] == "No such endpoint.") {
			t.Errorf("%s (%s %s): %d %v", id, op.Method, path, code, m)
		}
	}
}

// TestControlRoleIsReadOnly: the control role reads tenants and idempotency
// keys and nothing else; status changes go through the application role.
func TestControlRoleIsReadOnly(t *testing.T) {
	pool := pgtest.Pool(t)
	ctx := context.Background()
	as := func(sql string) error {
		return pool.InTx(ctx, database.Scope{Role: "erp_control"}, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql)
			return err
		})
	}
	if err := as(`SELECT count(*) FROM tenancy.tenant`); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{`UPDATE tenancy.tenant SET name = name, version = version + 1`, `SELECT count(*) FROM identity.principal`,
		`INSERT INTO platform.idempotency_key (tenant_id, key, principal_id, request_hash, status, expires_at) VALUES (gen_random_uuid(), 'control|x', gen_random_uuid(), '', 'completed', now())`} {
		if err := as(sql); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("%s: %v", sql, err)
		}
	}
}

func TestOperatorIdentity(t *testing.T) {
	a := controlplane.OperatorFrom(reqctx.Claims{Issuer: "https://ops", Subject: "alice"})
	if b := controlplane.OperatorFrom(reqctx.Claims{Issuer: "https://ops", Subject: "alice"}); b != a {
		t.Fatal("not stable")
	}
	if b := controlplane.OperatorFrom(reqctx.Claims{Issuer: "https://ops", Subject: "bob"}); b.ID == a.ID {
		t.Fatal("subjects share an id")
	}
	if b := controlplane.OperatorFrom(reqctx.Claims{Issuer: "https://other", Subject: "alice"}); b.ID == a.ID {
		t.Fatal("issuers share an id")
	}
}

// everyFailure runs fn with its first, second, ... database statement
// failing, and checks that every failure is reported as such.
func everyFailure(t *testing.T, s *controlplane.Service, name string, fn func(ctx context.Context) error) {
	t.Helper()
	pool := s.Pool
	defer func() { s.Begin = nil }()
	for n := 1; n < 100; n++ {
		s.Begin = func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, scope)
			if err != nil {
				return nil, err
			}
			return &pgtest.FailingTx{Tx: tx, FailAt: n}, nil
		}
		err := fn(context.Background())
		if err == nil {
			if n == 1 {
				t.Errorf("%s: succeeded without a statement to fail", name)
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

func TestDatabaseFailuresAreReported(t *testing.T) {
	s := service(t)
	ctx := context.Background()
	op := controlplane.OperatorFrom(reqctx.Claims{Issuer: "https://ops", Subject: "alice"})
	doc := fixture.Document(t)
	request := func(key string) provisioning.Request {
		return provisioning.Request{Name: "Acme", Region: "eu-west", GroupCurrency: "EUR", Document: doc,
			Administrator: provisioning.Administrator{Subject: "idp|ada", DisplayName: "Ada"}, Idempotency: &provisioning.Idempotency{Key: key, RequestHash: []byte("h")}}
	}
	created, replayed, err := s.Create(ctx, op, request("key-first"))
	if err != nil || replayed || created.Status != controlplane.Active {
		t.Fatalf("%+v %v %v", created, replayed, err)
	}
	everyFailure(t, s, "list", func(ctx context.Context) error {
		_, _, err := s.List(ctx, "", pagination.Request{})
		return err
	})
	everyFailure(t, s, "get", func(ctx context.Context) error {
		_, err := s.Get(ctx, created.ID)
		return err
	})
	everyFailure(t, s, "replay", func(ctx context.Context) error {
		_, _, err := s.Create(ctx, op, request("key-first"))
		return err
	})
	everyFailure(t, s, "create", func(ctx context.Context) error {
		_, _, err := s.Create(ctx, op, request("key-"+uuid.NewString()))
		return err
	})
	everyFailure(t, s, "suspend", func(ctx context.Context) error {
		_, err := s.Transition(ctx, op, created.ID, "suspend", "*")
		return err
	})
	// The transaction itself failing to start.
	s.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errors.New("database down") }
	for name, call := range map[string]func() error{
		"list":   func() error { _, _, err := s.List(ctx, "", pagination.Request{}); return err },
		"get":    func() error { _, err := s.Get(ctx, created.ID); return err },
		"replay": func() error { _, _, err := s.Create(ctx, op, request("key-first")); return err },
		"resume": func() error { _, err := s.Transition(ctx, op, created.ID, "resume", "*"); return err },
	} {
		if err := call(); err == nil || !strings.Contains(err.Error(), "database down") {
			t.Errorf("%s: %v", name, err)
		}
	}
	s.Begin = nil
	// A commit that fails is reported too (the connection is gone).
	s.Begin = func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
		tx, err := s.Pool.Begin(ctx, scope)
		if err != nil {
			return nil, err
		}
		return &brokenCommit{Tx: tx}, nil
	}
	if _, err := s.Transition(ctx, op, created.ID, "resume", "*"); err == nil || !strings.Contains(err.Error(), "commit failed") {
		t.Fatalf("commit: %v", err)
	}
	s.Begin = nil
	if got, err := s.Get(ctx, created.ID); err != nil || got.Status != controlplane.Suspended {
		t.Fatalf("after the failed commit: %+v %v", got, err)
	}
}

// brokenCommit is a transaction whose commit fails.
type brokenCommit struct{ pgx.Tx }

func (b *brokenCommit) Commit(ctx context.Context) error {
	_ = b.Tx.Rollback(ctx)
	return errors.New("commit failed")
}

// TestProvisioningKeyUniqueness: two provisioning requests with the same
// key, racing past the replay check, create one tenant; the loser gets 409.
func TestProvisioningKeyUniqueness(t *testing.T) {
	s := service(t)
	ctx := context.Background()
	op := controlplane.OperatorFrom(reqctx.Claims{Issuer: "https://ops", Subject: "alice"})
	req := provisioning.Request{Name: "Acme", Region: "eu-west", GroupCurrency: "EUR", Document: fixture.Document(t),
		Administrator: provisioning.Administrator{Subject: "idp|ada", DisplayName: "Ada"}, Operator: "alice",
		Idempotency: &provisioning.Idempotency{Key: "race-" + uuid.NewString(), RequestHash: []byte("h"), ActorID: op.ID}}
	if _, err := s.Provisioner.Provision(ctx, req); err != nil {
		t.Fatal(err)
	}
	_, err := s.Provisioner.Provision(ctx, req) // the replay check was skipped: straight to the insert
	if p := problem.From(err); p.Status != 409 || p.Code != "request.idempotency_key_in_use" || p.RetryAfter != 1 {
		t.Fatalf("second: %v", err)
	}
	var n int
	pgtest.Admin(t).QueryRow(ctx, `SELECT count(*) FROM platform.idempotency_key WHERE key = $1`, provisioning.KeyPrefix+req.Idempotency.Key).Scan(&n)
	if n != 1 {
		t.Fatalf("%d keys", n)
	}
}

// TestKeyRotationThroughTheControlPlane: a provisioned tenant has its own
// key; rotation gives it a new one and re-seals its house banks.
func TestKeyRotationThroughTheControlPlane(t *testing.T) {
	p := newPlane(t)
	ctx := context.Background()
	created, _ := p.must(201, "ops|alice", "POST", tenants, tenantBody(t, "Keyed"), "Idempotency-Key", "key-"+uuid.NewString())
	id := created["id"].(string)
	var keys []string
	rows, _ := pgtest.Admin(t).Query(ctx, `SELECT status FROM tenancy.tenant_key WHERE tenant_id = $1`, uuid.MustParse(id))
	keys, _ = pgx.CollectRows(rows, pgx.RowTo[string])
	if len(keys) != 1 || keys[0] != "active" {
		t.Fatalf("provisioned tenant keys: %v", keys)
	}
	var sealed []byte
	pgtest.Admin(t).QueryRow(ctx, `SELECT iban_cipher FROM payments.house_bank WHERE tenant_id = $1 LIMIT 1`, uuid.MustParse(id)).Scan(&sealed)
	if sealed[0] != 2 {
		t.Fatalf("house bank sealed with format %d", sealed[0])
	}
	p.problem(428, "request.precondition_required", "ops|alice", "POST", tenants+"/"+id+":rotateKey", "")
	p.problem(412, "request.version_mismatch", "ops|alice", "POST", tenants+"/"+id+":rotateKey", "", "If-Match", `"7"`)
	p.problem(404, "request.not_found", "ops|alice", "POST", tenants+"/"+uuid.NewString()+":rotateKey", "", "If-Match", "*")
	at := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	p.s.Now = func() time.Time { return at } // the audit entry carries the service's clock
	got, h := p.must(200, "ops|alice", "POST", tenants+"/"+id+":rotateKey", "", "If-Match", `"1"`)
	p.s.Now = nil
	rot := got["rotation"].(map[string]any)
	if got["version"] != float64(2) || h.Get("ETag") != `"2"` || rot["resealed"] != float64(2) || rot["master_key_id"] != "test" ||
		rot["previous_key_id"] == uuid.Nil.String() || rot["key_id"] == rot["previous_key_id"] {
		t.Fatalf("rotation: %v", got)
	}
	var resealed []byte
	pgtest.Admin(t).QueryRow(ctx, `SELECT iban_cipher FROM payments.house_bank WHERE tenant_id = $1 LIMIT 1`, uuid.MustParse(id)).Scan(&resealed)
	if string(resealed) == string(sealed) || string(resealed[1:17]) != uuid.MustParse(rot["key_id"].(string)).String()[:0]+string(mustBytes(rot["key_id"].(string))) {
		t.Fatalf("house bank not re-sealed under the new key")
	}
	// The tenant's administrator still reads its house banks through the API (opened with the new key).
	admin := pgtest.Actor{ID: uuid.MustParse(created["provisioned"].(map[string]any)["administrator_id"].(string)), Subject: "idp|ada", Kind: "human"}
	public := apitest.Client{T: t, API: apitest.New(t), Tenant: pgtest.Tenant{ID: uuid.MustParse(id)}, As: admin}
	public.Must(200, "GET", "/v1/config-versions?is_active=true", "", "")
	// Recorded in the tenant's chain as the operator.
	var action, operator string
	pgtest.Admin(t).QueryRow(ctx, `SELECT action, detail->>'operator' FROM audit.audit_event WHERE tenant_id = $1 AND action = 'tenancy.tenant.rotate_key'`, uuid.MustParse(id)).Scan(&action, &operator)
	if operator != "ops|alice" {
		t.Fatalf("audit: %s %s", action, operator)
	}
	// A tenant on the legacy key gets its first key; an offboarded tenant is left alone; without masters, 503.
	legacy := pgtest.NewTenant(t)
	got, _ = p.must(200, "ops|alice", "POST", tenants+"/"+legacy.ID.String()+":rotateKey", "", "If-Match", "*")
	if got["rotation"].(map[string]any)["previous_key_id"] != uuid.Nil.String() {
		t.Fatalf("legacy rotation: %v", got)
	}
	p.must(200, "ops|alice", "POST", tenants+"/"+id+":offboard", "", "If-Match", "*")
	p.problem(409, "tenancy.invalid_transition", "ops|alice", "POST", tenants+"/"+id+":rotateKey", "", "If-Match", "*")
	p.s.Keys = nil
	p.problem(503, "platform.unavailable", "ops|alice", "POST", tenants+"/"+legacy.ID.String()+":rotateKey", "", "If-Match", "*")
	p.s.Keys = apitest.FieldKeys()
	op := controlplane.OperatorFrom(reqctx.Claims{Issuer: p.is.URL, Subject: "ops|alice"})
	everyFailure(t, p.s, "rotate", func(ctx context.Context) error {
		_, _, err := p.s.RotateKey(ctx, op, legacy.ID, "*")
		return err
	})
	p.s.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errors.New("database down") }
	if _, _, err := p.s.RotateKey(ctx, op, legacy.ID, "*"); err == nil || !strings.Contains(err.Error(), "database down") {
		t.Fatalf("begin: %v", err)
	}
	p.s.Begin = func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
		tx, err := p.s.Pool.Begin(ctx, scope)
		if err != nil {
			return nil, err
		}
		return &brokenCommit{Tx: tx}, nil
	}
	if _, _, err := p.s.RotateKey(ctx, op, legacy.ID, "*"); err == nil || !strings.Contains(err.Error(), "commit failed") {
		t.Fatalf("commit: %v", err)
	}
	p.s.Begin = nil
}

func mustBytes(id string) []byte {
	u := uuid.MustParse(id)
	return u[:]
}
