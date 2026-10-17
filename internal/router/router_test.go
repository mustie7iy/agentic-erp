package router_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api"

	"github.com/mustie7iy/agentic-erp/internal/directory"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn/authntest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/tlsconf"
	"github.com/mustie7iy/agentic-erp/internal/platform/tlsconf/tlstest"
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
	t.Setenv("PGSSLMODE", "") // pgx reads the process's PG* settings, not the injected environment
	valid := map[string]string{"ERP_DIRECTORY_URL": "postgres://x", "ERP_ENV": "development", "ERP_OIDC_ISSUERS": `[{"issuer":"https://idp.example.com","audience":"erp-api"}]`}
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	check := func(what string, got, want any) {
		t.Helper()
		if got != want {
			t.Errorf("%s: got %v, want %v", what, got, want)
		}
	}
	c, err := router.LoadConfig(env(valid))
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	check("Addr", c.Addr, ":8081")
	check("CacheTTL", c.CacheTTL, 30*time.Second)
	check("ShutdownTimeout", c.ShutdownTimeout, 20*time.Second)
	check("len(Issuers)", len(c.Issuers), 1)
	check("LogLevel", c.LogLevel, slog.LevelInfo)
	check("Environment", c.Environment, "development")
	// Tenants' tokens may come from registered identity providers alone
	// (ADR-0031): the router resolves them through the directory.
	noIssuers := map[string]string{}
	for k, v := range valid {
		noIssuers[k] = v
	}
	delete(noIssuers, "ERP_OIDC_ISSUERS")
	if c, err := router.LoadConfig(env(noIssuers)); err != nil || len(c.Issuers) != 0 {
		t.Errorf("no issuers: got %d issuers, %v, want none and no error", len(c.Issuers), err)
	}
	for name, bad := range map[string]map[string]string{
		"no environment":      {"ERP_ENV": ""},
		"unknown environment": {"ERP_ENV": "staging"},
		"no directory":        {"ERP_DIRECTORY_URL": ""},
		"bad issuers":         {"ERP_OIDC_ISSUERS": "[]"},
		"bad cache":           {"ERP_DIRECTORY_CACHE": "0"},
		"bad shutdown":        {"ERP_SHUTDOWN_TIMEOUT": "soon"},
		"bad level":           {"ERP_LOG_LEVEL": "loud"},
	} {
		t.Run(name, func(t *testing.T) {
			m := map[string]string{}
			for k, v := range valid {
				m[k] = v
			}
			for k, v := range bad {
				m[k] = v
			}
			if _, err := router.LoadConfig(env(m)); err == nil {
				t.Errorf("got no error for %v, want one", bad)
			}
		})
	}
	m := map[string]string{}
	for k, v := range valid {
		m[k] = v
	}
	m["ERP_ROUTER_ADDR"], m["ERP_DIRECTORY_CACHE"], m["ERP_LOG_LEVEL"] = "127.0.0.1:0", "5s", "debug"
	if c, err = router.LoadConfig(env(m)); err != nil {
		t.Fatalf("overrides: %v", err)
	}
	check("overridden Addr", c.Addr, "127.0.0.1:0")
	check("AdminAddr", c.AdminAddr, ":9091")
	check("overridden CacheTTL", c.CacheTTL, 5*time.Second)
	check("overridden LogLevel", c.LogLevel, slog.LevelDebug)
	if c.OperatorIssuers != nil {
		t.Errorf("OperatorIssuers: got %v, want nil", c.OperatorIssuers)
	}
	// The global control plane needs operator issuers and a page-token key.
	m["ERP_OPERATOR_ISSUERS"], m["ERP_ROUTER_ADMIN_ADDR"] = `[{"issuer":"https://ops.example.com","audience":"erp-control"}]`, "127.0.0.1:0"
	_, err = router.LoadConfig(env(m))
	pgtest.WantErr(t, err, "ERP_PAGE_TOKEN_KEY")
	m["ERP_PAGE_TOKEN_KEY"] = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))
	if c, err = router.LoadConfig(env(m)); err != nil {
		t.Fatalf("operators: %v", err)
	}
	check("len(OperatorIssuers)", len(c.OperatorIssuers), 1)
	check("len(PageTokenKey)", len(c.PageTokenKey), 32)
	check("operator AdminAddr", c.AdminAddr, "127.0.0.1:0")
	m["ERP_OPERATOR_ISSUERS"] = "[]"
	_, err = router.LoadConfig(env(m))
	pgtest.WantErr(t, err, "ERP_OPERATOR_ISSUERS")
	// Production trusts tenants' registered identity providers only
	// (ADR-0031): issuers for every tenant are refused.
	prodEnv := map[string]string{}
	for k, v := range valid {
		prodEnv[k] = v
	}
	prodEnv["ERP_ENV"] = "production"
	_, err = router.LoadConfig(env(prodEnv))
	pgtest.WantErr(t, err, "ERP_OIDC_ISSUERS must not be set with ERP_ENV=production")
	delete(prodEnv, "ERP_OIDC_ISSUERS")
	// Production reaches the directory over verified TLS only (ADR-0033).
	_, err = router.LoadConfig(env(prodEnv))
	pgtest.WantErr(t, err, "ERP_DIRECTORY_URL: sslmode must be verify-full")
	prodEnv["ERP_DIRECTORY_URL"] = "postgres://u:p@dir.example.com/erp?sslmode=verify-full"
	// Its own TLS material is the rest of what production requires
	// (TestLoadConfigTLS).
	_, err = router.LoadConfig(env(prodEnv))
	pgtest.WantErr(t, err, "ERP_TLS_CERT and ERP_TLS_KEY are required with ERP_ENV=production")
}

func TestRouting(t *testing.T) {
	is := authntest.New(t)
	v, err := authn.New(context.Background(), authn.Development, []authn.Issuer{is.Config()})
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
	check := func(what string, got, want any) {
		t.Helper()
		if got != want {
			t.Errorf("%s: got %v, want %v", what, got, want)
		}
	}
	// Each tenant reaches its own cell, request intact; the answer names the cell.
	code, out, h := do("POST", "/v1/journal-entries?dry_run=true", is.Token(t, a, "ada", nil), `{"x":1}`)
	if code != 200 {
		t.Fatalf("status: got %d, want 200 (%v)", code, out)
	}
	check("cell", out["cell"], "eu-1")
	check("method", out["method"], "POST")
	check("path", out["path"], "/v1/journal-entries?dry_run=true")
	check("body", out["body"], `{"x":1}`)
	if out["auth"] == "" {
		t.Error("auth: got empty, want the bearer token passed on")
	}
	if out["forwarded"] == "" {
		t.Error("X-Forwarded-Host: got empty, want the client's host")
	}
	check("proto", out["proto"], "http")
	if host, _ := out["host"].(string); !strings.Contains(host, "127.0.0.1") {
		t.Errorf("host: got %q, want the cell's 127.0.0.1 address", host)
	}
	check("X-Erp-Cell", h.Get("X-Erp-Cell"), "eu-1")
	code, out, _ = do("GET", "/v1/balances", is.Token(t, b, "bob", nil), "")
	check("other tenant status", code, 200)
	check("other tenant cell", out["cell"], "eu-2")
	// A moving tenant reads but does not write; other statuses are refused; unknown tenants have no cell.
	code, out, _ = do("GET", "/v1/balances", is.Token(t, moving, "m", nil), "")
	check("moving read status", code, 200)
	check("moving read cell", out["cell"], "eu-2")
	code, out, h = do("POST", "/v1/journal-entries", is.Token(t, moving, "m", nil), "{}")
	check("moving write status", code, 503)
	check("moving write Retry-After", h.Get("Retry-After"), "60")
	check("moving write problem status", out["status"], float64(503))
	code, out, _ = do("GET", "/v1/balances", is.Token(t, suspended, "s", nil), "")
	check("suspended status", code, 403)
	check("suspended code", out["code"], "tenancy.tenant_suspended")
	if code, out, _ := do("GET", "/v1/balances", is.Token(t, uuid.New(), "n", nil), ""); code != 404 {
		t.Fatalf("unknown: %d %v", code, out)
	}
	if code, out, _ := do("GET", "/v1/balances", is.Token(t, broken, "x", nil), ""); code != 503 {
		t.Fatalf("broken entry: %d %v", code, out)
	}
	code, out, _ = do("GET", "/v1/balances", is.Token(t, down, "x", nil), "")
	check("cell down status", code, 502)
	check("cell down code", out["code"], "platform.cell_unreachable")
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
	code, out, h = do("GET", "/v1/balances", is.Token(t, a, "ada", nil), "")
	check("directory down status", code, 503)
	check("directory down Retry-After", h.Get("Retry-After"), "5")
	check("directory down problem status", out["status"], float64(503))
	m.err = nil
	code, out, _ = do("GET", "/livez", "", "")
	check("livez status", code, 200)
	check("livez body status", out["status"], "ok")
}

// TestRoutingRegisteredIssuers: a token of a tenant's registered identity
// provider is routed to that tenant's cell by its issuer and audience in
// the directory, whatever tenant it claims; one no tenant registered is
// refused before any cell (ADR-0031).
func TestRoutingRegisteredIssuers(t *testing.T) {
	ctx := context.Background()
	dir, err := directory.Open(ctx, pgtest.EmptyDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	is, other := authntest.NewSeparate(t), authntest.NewSeparate(t)
	tenant := uuid.New()
	if err := dir.ClaimIssuer(ctx, directory.Issuer{Issuer: is.URL, Audience: authntest.Audience, TenantID: tenant, Cell: "eu-1", ProviderID: uuid.New()}); err != nil {
		t.Fatal(err)
	}
	cell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"cell": "eu-1"}`)) // the client asserts on what arrives
	}))
	defer cell.Close()
	if err := dir.Upsert(ctx, directory.Entry{TenantID: tenant, Region: "eu-west", Cell: "eu-1", APIURL: cell.URL, Status: directory.Active}); err != nil {
		t.Fatal(err)
	}
	v, err := authn.NewRegistered(ctx, authn.Development, &authn.RegistryCache{Registry: directory.IssuerRegistry{Directory: dir}}, &authn.Providers{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(&router.Router{Verifier: v, Directory: dir, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer srv.Close()
	get := func(token string) int {
		req, _ := http.NewRequestWithContext(t.Context(), "GET", srv.URL+"/v1/roles", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close() // the status is what this test reads
		return resp.StatusCode
	}
	if got := get(is.Token(t, uuid.New(), "ada", nil)); got != 200 {
		t.Errorf("a registered issuer's token claiming another tenant: got %d, want 200 from the registration's cell", got)
	}
	if got := get(other.Token(t, tenant, "mallory", nil)); got != 401 {
		t.Errorf("an unregistered issuer's token: got %d, want 401", got)
	}
}

// TestRefusesPlaintextCells: with RequireTLS (production) a tenant whose
// cell's API URL is not https is not forwarded: 503 (ADR-0033).
func TestRefusesPlaintextCells(t *testing.T) {
	is := authntest.New(t)
	v, err := authn.New(context.Background(), authn.Development, []authn.Issuer{is.Config()})
	if err != nil {
		t.Fatal(err)
	}
	tenant := uuid.New()
	m := &memory{entries: map[uuid.UUID]directory.Entry{tenant: {TenantID: tenant, Cell: "eu-1", APIURL: "http://eu1.internal", Status: directory.Active}}}
	rt := &router.Router{Verifier: v, Directory: lookup{m}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), RequireTLS: true}
	req := httptest.NewRequest(http.MethodGet, "/v1/balances", nil)
	req.Header.Set("Authorization", "Bearer "+is.Token(t, tenant, "ada", nil))
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("plaintext cell: got %d, want 503", rec.Code)
	}
}

// TestLoadConfigTLS: the router reads its identity (ERP_TLS_*), the public
// listener's certificate (ERP_ROUTER_PUBLIC_*) and the trust domain, from
// env: and file: references; production requires all three (ADR-0033).
func TestLoadConfigTLS(t *testing.T) {
	t.Setenv("PGSSLMODE", "") // pgx reads the process's PG* settings, not the injected environment
	ca, dir := tlstest.NewCA(t), t.TempDir()
	leaf, public := ca.Issue(t, tlstest.Options{URIs: []string{"spiffe://erp.example/global/erp-router"}}), ca.Issue(t, tlstest.Options{DNS: []string{"api.example.com"}})
	files := map[string]string{
		"ERP_TLS_CA":             "file:" + tlstest.WriteFile(t, dir, "ca.crt", ca.PEM),
		"ERP_TLS_CERT":           "file:" + tlstest.WriteFile(t, dir, "tls.crt", leaf.CertPEM),
		"ERP_TLS_KEY":            "file:" + tlstest.WriteFile(t, dir, "tls.key", leaf.KeyPEM),
		"ERP_ROUTER_PUBLIC_CERT": "file:" + tlstest.WriteFile(t, dir, "public.crt", public.CertPEM),
		"ERP_ROUTER_PUBLIC_KEY":  "file:" + tlstest.WriteFile(t, dir, "public.key", public.KeyPEM),
		"ERP_TRUST_DOMAIN":       "erp.example",
	}
	base := map[string]string{"ERP_DIRECTORY_URL": "postgres://u:p@dir.example.com/erp?sslmode=verify-full", "ERP_ENV": "production"}
	env := func(set ...map[string]string) func(string) string {
		m := maps.Clone(base)
		for _, s := range set {
			maps.Copy(m, s)
		}
		return func(k string) string { return m[k] }
	}
	c, err := router.LoadConfig(env(files))
	if err != nil {
		t.Fatal(err)
	}
	if c.Identity == nil || !c.Identity.HasCertificate() || c.Public == nil || !c.Public.HasCertificate() || c.TrustDomain != "erp.example" {
		t.Errorf("TLS: got identity %v, public %v, trust domain %q, want all read", c.Identity, c.Public, c.TrustDomain)
	}
	for _, bad := range []struct {
		name string
		set  map[string]string
		want string
	}{
		{"production without an identity", map[string]string{"ERP_TLS_CERT": "", "ERP_TLS_KEY": ""}, "ERP_TLS_CERT and ERP_TLS_KEY are required with ERP_ENV=production"},
		{"production without a public certificate", map[string]string{"ERP_ROUTER_PUBLIC_CERT": "", "ERP_ROUTER_PUBLIC_KEY": ""}, "ERP_ROUTER_PUBLIC_CERT and ERP_ROUTER_PUBLIC_KEY are required with ERP_ENV=production"},
		{"production without a trust domain", map[string]string{"ERP_TRUST_DOMAIN": ""}, "ERP_TRUST_DOMAIN is required with ERP_ENV=production"},
		{"a bad trust domain", map[string]string{"ERP_TRUST_DOMAIN": "Erp"}, "ERP_TRUST_DOMAIN must be"},
		{"an identity without a trust domain", map[string]string{"ERP_ENV": "development", "ERP_TRUST_DOMAIN": ""}, "ERP_TLS_* needs ERP_TRUST_DOMAIN"},
		{"a bundle without a trust domain", map[string]string{"ERP_ENV": "development", "ERP_TRUST_DOMAIN": "", "ERP_TLS_CERT": "", "ERP_TLS_KEY": ""}, "ERP_TLS_* needs ERP_TRUST_DOMAIN"},
		{"an unreadable identity", map[string]string{"ERP_TLS_CA": "file:/nonexistent/ca.crt"}, "ERP_TLS_CA, ERP_TLS_CERT, ERP_TLS_KEY: tlsconf: CA bundle"},
		{"a public key without its certificate", map[string]string{"ERP_ROUTER_PUBLIC_CERT": ""}, "ERP_ROUTER_PUBLIC_CERT, ERP_ROUTER_PUBLIC_KEY: tlsconf: the certificate and key go together"},
	} {
		t.Run(bad.name, func(t *testing.T) {
			_, err := router.LoadConfig(env(files, bad.set))
			pgtest.WantErr(t, err, bad.want)
		})
	}
	// Development needs none of them.
	if c, err := router.LoadConfig(env(map[string]string{"ERP_ENV": "development"})); err != nil || c.Identity != nil || c.Public != nil {
		t.Errorf("development: got %v, %v, %v, want no TLS and no error", c.Identity, c.Public, err)
	}
}

// roundTrip is a transport made of a function.
type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestPeerOfTheNamedCell: with PeerOf the request to the cell names the
// identity of the cell the directory names, which alone the connection
// accepts (ADR-0033).
func TestPeerOfTheNamedCell(t *testing.T) {
	is := authntest.New(t)
	v, err := authn.New(context.Background(), authn.Development, []authn.Issuer{is.Config()})
	if err != nil {
		t.Fatal(err)
	}
	tenant := uuid.New()
	m := &memory{entries: map[uuid.UUID]directory.Entry{tenant: {TenantID: tenant, Cell: "eu-1", APIURL: "https://eu1.internal", Status: directory.Active}}}
	var named string
	transport := roundTrip(func(r *http.Request) (*http.Response, error) {
		named, _ = tlsconf.ExpectedPeer(r.Context())
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}}, nil
	})
	rt := &router.Router{Verifier: v, Directory: lookup{m}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Transport: transport,
		PeerOf: func(cell string) string { return "spiffe://erp.example/" + cell + "/erp-server" }}
	req := httptest.NewRequest(http.MethodGet, "/v1/balances", nil)
	req.Header.Set("Authorization", "Bearer "+is.Token(t, tenant, "ada", nil))
	rt.ServeHTTP(httptest.NewRecorder(), req)
	if named != "spiffe://erp.example/eu-1/erp-server" {
		t.Errorf("named peer: got %q, want the directory's cell eu-1", named)
	}
}

// TestForwardsTheClientCertificate: the router forwards the thumbprint of
// the client certificate a request was made with, and never one the
// client sent itself (ADR-0033).
func TestForwardsTheClientCertificate(t *testing.T) {
	is := authntest.New(t)
	v, err := authn.New(context.Background(), authn.Development, []authn.Issuer{is.Config()})
	if err != nil {
		t.Fatal(err)
	}
	tenant := uuid.New()
	m := &memory{entries: map[uuid.UUID]directory.Entry{tenant: {TenantID: tenant, Cell: "eu-1", APIURL: "https://eu1.internal", Status: directory.Active}}}
	var forwarded string
	transport := roundTrip(func(r *http.Request) (*http.Response, error) {
		forwarded = r.Header.Get(tlsconf.ClientCertHeader)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}}, nil
	})
	rt := &router.Router{Verifier: v, Directory: lookup{m}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Transport: transport}
	cert := &x509.Certificate{Raw: []byte("the agent's certificate")}
	for _, c := range []struct {
		name string
		tls  *tls.ConnectionState
		want string
	}{
		{"with a client certificate", &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}, tlsconf.Thumbprint(cert)},
		{"without one", &tls.ConnectionState{}, ""},
		{"over plaintext", nil, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/balances", nil)
			req.Header.Set("Authorization", "Bearer "+is.Token(t, tenant, "ada", nil))
			req.Header.Set(tlsconf.ClientCertHeader, "forged")
			req.TLS = c.tls
			rt.ServeHTTP(httptest.NewRecorder(), req)
			if forwarded != c.want {
				t.Errorf("forwarded thumbprint: got %q, want %q", forwarded, c.want)
			}
		})
	}
}

// deadlineWriter records the write deadline asked of it.
type deadlineWriter struct {
	*httptest.ResponseRecorder
	deadline time.Time
}

func (d *deadlineWriter) SetWriteDeadline(t time.Time) error { d.deadline = t; return nil }

// TestModelCallsMayTakeTheirTime: a model call through the router may be
// answered for the gateway's call time, not the listener's write timeout,
// which would cut a long answer and have the SDK call again, billed again
// (ADR-0036); other requests keep the listener's.
func TestModelCallsMayTakeTheirTime(t *testing.T) {
	is := authntest.New(t)
	v, err := authn.New(context.Background(), authn.Development, []authn.Issuer{is.Config()})
	if err != nil {
		t.Fatal(err)
	}
	tenant := uuid.New()
	m := &memory{entries: map[uuid.UUID]directory.Entry{tenant: {TenantID: tenant, Cell: "eu-1", APIURL: "https://eu1.internal", Status: directory.Active}}}
	ok := roundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}}, nil
	})
	// The router holds the answer longer than the cell's call may take, so
	// the cell, not the router, is the one to cut a call that overruns.
	if router.ModelTimeout <= api.ModelCallTimeout {
		t.Errorf("router's model timeout: got %v, want more than the call's %v", router.ModelTimeout, api.ModelCallTimeout)
	}
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	rt := &router.Router{Verifier: v, Directory: lookup{m}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Transport: ok, Now: func() time.Time { return now }}
	for _, c := range []struct {
		path string
		want time.Time
	}{{"/v1/model/v1/messages", now.Add(router.ModelTimeout)}, {"/v1/balances", time.Time{}}} {
		t.Run(c.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, c.path, strings.NewReader("{}"))
			req.Header.Set("Authorization", "Bearer "+is.Token(t, tenant, "ada", nil))
			w := &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
			rt.ServeHTTP(w, req)
			if !w.deadline.Equal(c.want) {
				t.Errorf("write deadline: got %v, want %v", w.deadline, c.want)
			}
		})
	}
}
