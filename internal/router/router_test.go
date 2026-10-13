package router_test

import (
	"context"
	"encoding/base64"
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

	"github.com/mustie7iy/agentic-erp/internal/directory"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn/authntest"
	"github.com/mustie7iy/agentic-erp/internal/router"
)

// memory is an in-memory directory.
type memory struct {
	entries map[uuid.UUID]directory.Entry
	err     error
}

func (m *memory) Lookup(context.Context, uuid.UUID) (directory.Entry, error) { panic("unused") }
func (m *memory) Upsert(context.Context, directory.Entry) error              { panic("unused") }

// lookup is the store the router sees: entries by tenant, or a failure.
type lookup struct{ m *memory }

func (l lookup) Lookup(_ context.Context, tenant uuid.UUID) (directory.Entry, error) {
	if l.m.err != nil {
		return directory.Entry{}, l.m.err
	}
	e, ok := l.m.entries[tenant]
	if !ok {
		return directory.Entry{}, directory.ErrUnknown
	}
	return e, nil
}
func (l lookup) Upsert(_ context.Context, e directory.Entry) error {
	l.m.entries[e.TenantID] = e
	return nil
}

func TestLoadConfig(t *testing.T) {
	valid := map[string]string{"ERP_DIRECTORY_URL": "postgres://x", "ERP_OIDC_ISSUERS": `[{"issuer":"https://idp.example.com","audience":"erp-api"}]`}
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	c, err := router.LoadConfig(env(valid))
	if err != nil || c.Addr != ":8081" || c.CacheTTL != 30*time.Second || c.ShutdownTimeout != 20*time.Second || len(c.Issuers) != 1 || c.LogLevel != slog.LevelInfo {
		t.Fatalf("%+v %v", c, err)
	}
	for name, bad := range map[string]map[string]string{
		"no directory": {"ERP_DIRECTORY_URL": ""},
		"no issuers":   {"ERP_OIDC_ISSUERS": ""},
		"bad issuers":  {"ERP_OIDC_ISSUERS": "[]"},
		"bad cache":    {"ERP_DIRECTORY_CACHE": "0"},
		"bad shutdown": {"ERP_SHUTDOWN_TIMEOUT": "soon"},
		"bad level":    {"ERP_LOG_LEVEL": "loud"},
	} {
		m := map[string]string{}
		for k, v := range valid {
			m[k] = v
		}
		for k, v := range bad {
			m[k] = v
		}
		if _, err := router.LoadConfig(env(m)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	m := map[string]string{}
	for k, v := range valid {
		m[k] = v
	}
	m["ERP_ROUTER_ADDR"], m["ERP_DIRECTORY_CACHE"], m["ERP_LOG_LEVEL"] = "127.0.0.1:0", "5s", "debug"
	if c, err := router.LoadConfig(env(m)); err != nil || c.Addr != "127.0.0.1:0" || c.AdminAddr != ":9091" || c.CacheTTL != 5*time.Second || c.LogLevel != slog.LevelDebug || c.OperatorIssuers != nil {
		t.Fatalf("%+v %v", c, err)
	}
	// The global control plane needs operator issuers and a page-token key.
	m["ERP_OPERATOR_ISSUERS"], m["ERP_ROUTER_ADMIN_ADDR"] = `[{"issuer":"https://ops.example.com","audience":"erp-control"}]`, "127.0.0.1:0"
	if _, err := router.LoadConfig(env(m)); err == nil || !strings.Contains(err.Error(), "ERP_PAGE_TOKEN_KEY") {
		t.Fatalf("operators without a page key: %v", err)
	}
	m["ERP_PAGE_TOKEN_KEY"] = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))
	if c, err := router.LoadConfig(env(m)); err != nil || len(c.OperatorIssuers) != 1 || len(c.PageTokenKey) != 32 || c.AdminAddr != "127.0.0.1:0" {
		t.Fatalf("%+v %v", c, err)
	}
	m["ERP_OPERATOR_ISSUERS"] = "[]"
	if _, err := router.LoadConfig(env(m)); err == nil || !strings.Contains(err.Error(), "ERP_OPERATOR_ISSUERS") {
		t.Fatalf("empty operators: %v", err)
	}
}

func TestRouting(t *testing.T) {
	is := authntest.New(t)
	v, err := authn.New(context.Background(), []authn.Issuer{is.Config()})
	if err != nil {
		t.Fatal(err)
	}
	// Two cells echoing what they receive.
	cell := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			// A failed write reaches the router as a broken answer, which the client asserts on.
			_ = json.NewEncoder(w).Encode(map[string]any{"cell": name, "method": r.Method, "path": r.URL.RequestURI(), "host": r.Host,
				"forwarded": r.Header.Get("X-Forwarded-Host"), "proto": r.Header.Get("X-Forwarded-Proto"), "auth": r.Header.Get("Authorization"), "body": string(body)})
		}))
	}
	eu1, eu2 := cell("eu-1"), cell("eu-2")
	defer eu1.Close()
	defer eu2.Close()
	a, b, moving, suspended, broken, down := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	m := &memory{entries: map[uuid.UUID]directory.Entry{
		a:         {TenantID: a, Cell: "eu-1", APIURL: eu1.URL, Status: directory.Active},
		b:         {TenantID: b, Cell: "eu-2", APIURL: eu2.URL, Status: directory.Active},
		moving:    {TenantID: moving, Cell: "eu-2", APIURL: eu2.URL, Status: directory.ReadOnly},
		suspended: {TenantID: suspended, Cell: "eu-1", APIURL: eu1.URL, Status: directory.Suspended},
		broken:    {TenantID: broken, Cell: "eu-9", APIURL: "not a url", Status: directory.Active},
		down:      {TenantID: down, Cell: "eu-0", APIURL: deadURL, Status: directory.Active},
	}}
	rt := &router.Router{Verifier: v, Directory: lookup{m}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv := httptest.NewServer(rt)
	defer srv.Close()
	do := func(method, path, token, body string) (int, map[string]any, http.Header) {
		req, _ := http.NewRequestWithContext(t.Context(), method, srv.URL+path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }() // a read body has nothing left to lose on close
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("%s %s: %d, not JSON: %v", method, path, resp.StatusCode, err)
		}
		return resp.StatusCode, out, resp.Header
	}
	// Each tenant reaches its own cell, request intact; the answer names the cell.
	code, out, h := do("POST", "/v1/journal-entries?dry_run=true", is.Token(t, a, "ada", nil), `{"x":1}`)
	if code != 200 || out["cell"] != "eu-1" || out["method"] != "POST" || out["path"] != "/v1/journal-entries?dry_run=true" || out["body"] != `{"x":1}` ||
		out["auth"] == "" || out["forwarded"] == "" || out["proto"] != "http" || !strings.Contains(out["host"].(string), "127.0.0.1") || h.Get("X-Erp-Cell") != "eu-1" {
		t.Fatalf("%d %v %v", code, out, h)
	}
	if code, out, _ := do("GET", "/v1/balances", is.Token(t, b, "bob", nil), ""); code != 200 || out["cell"] != "eu-2" {
		t.Fatalf("%d %v", code, out)
	}
	// A moving tenant reads but does not write; other statuses are refused; unknown tenants have no cell.
	if code, out, _ := do("GET", "/v1/balances", is.Token(t, moving, "m", nil), ""); code != 200 || out["cell"] != "eu-2" {
		t.Fatalf("moving read: %d %v", code, out)
	}
	if code, out, h := do("POST", "/v1/journal-entries", is.Token(t, moving, "m", nil), "{}"); code != 503 || h.Get("Retry-After") != "60" || out["status"] != float64(503) {
		t.Fatalf("moving write: %d %v %v", code, out, h)
	}
	if code, out, _ := do("GET", "/v1/balances", is.Token(t, suspended, "s", nil), ""); code != 403 || out["code"] != "tenancy.tenant_suspended" {
		t.Fatalf("suspended: %d %v", code, out)
	}
	if code, out, _ := do("GET", "/v1/balances", is.Token(t, uuid.New(), "n", nil), ""); code != 404 {
		t.Fatalf("unknown: %d %v", code, out)
	}
	if code, out, _ := do("GET", "/v1/balances", is.Token(t, broken, "x", nil), ""); code != 503 {
		t.Fatalf("broken entry: %d %v", code, out)
	}
	if code, out, _ := do("GET", "/v1/balances", is.Token(t, down, "x", nil), ""); code != 502 || out["code"] != "platform.cell_unreachable" {
		t.Fatalf("cell down: %d %v", code, out)
	}
	// Tokens: none, invalid, an operator's (no tenant).
	if code, _, _ := do("GET", "/v1/balances", "", ""); code != 401 {
		t.Fatalf("no token: %d", code)
	}
	if code, _, _ := do("GET", "/v1/balances", "not.a.token", ""); code != 401 {
		t.Fatalf("bad token: %d", code)
	}
	if code, _, _ := do("GET", "/v1/balances", is.Token(t, uuid.Nil, "ops", map[string]any{"tenant_id": nil}), ""); code != 401 {
		t.Fatalf("operator token: %d", code)
	}
	// The directory failing is a 503 with retry; liveness needs no token.
	m.err = errors.New("directory down")
	if code, out, h := do("GET", "/v1/balances", is.Token(t, a, "ada", nil), ""); code != 503 || h.Get("Retry-After") != "5" || out["status"] != float64(503) {
		t.Fatalf("directory down: %d %v %v", code, out, h)
	}
	m.err = nil
	if code, out, _ := do("GET", "/livez", "", ""); code != 200 || out["status"] != "ok" {
		t.Fatalf("livez: %d %v", code, out)
	}
}
