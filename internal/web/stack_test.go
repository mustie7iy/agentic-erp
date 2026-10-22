package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/net/html"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/directory"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn/authntest"
	"github.com/mustie7iy/agentic-erp/internal/platform/buildinfo"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// registry is the cell's registry of the tenant's one provider, which a
// test may change (its multi-factor rule).
type registry struct {
	mu  sync.Mutex
	reg authn.Registration
}

func (r *registry) Resolve(_ context.Context, issuer string, audiences []string) (authn.Registration, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range audiences {
		if issuer == r.reg.Issuer && a == r.reg.Audience {
			return r.reg, nil
		}
	}
	return authn.Registration{}, authn.ErrUnknownIssuer
}

func (r *registry) set(fn func(*authn.Registration)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(&r.reg)
}

// testClock is erp-web's settable clock.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// stack is erp-web signing people in at a tenant's provider (a real OIDC
// authorization server in process, authntest) in front of the real API,
// whose cell knows the provider with its session rule, and the directory
// database with the web schema and the provider's web client.
type stack struct {
	t      *testing.T
	is     *authntest.Issuer
	ten    pgtest.Tenant
	reg    *registry
	api    *httptest.Server
	srv    *Server
	web    *httptest.Server
	dir    *directory.Postgres
	dirURL string
	clock  *testClock
	key    *rsa.PrivateKey
	public string
	logs   *pgtest.SyncBuffer
	client authntest.Client
}

// stackOptions change the stack before it starts.
type stackOptions struct {
	method     string // the web client's auth method; "" private_key_jwt
	credential string // its credential reference; "" the stack's key under the prefix
	discovery  []byte // the provider's discovery document in place of its own
}

const loginCodeOf = "acme"

func newStack(t *testing.T, opts stackOptions) *stack {
	t.Helper()
	ctx := context.Background()
	s := &stack{t: t, is: authntest.NewSeparate(t), clock: &testClock{now: time.Now()}, logs: &pgtest.SyncBuffer{}}
	if opts.discovery != nil {
		s.is.Serve(authntest.DiscoveryPath, bytes.ReplaceAll(opts.discovery, []byte("ISSUER"), []byte(s.is.URL)))
	}
	s.ten = pgtest.NewTenant(t, "tenancy.org.read")
	s.ten.AllCompanies(t, s.ten.Human)
	pgtest.Exec(t, `INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by)
		VALUES ($1, gen_random_uuid(), '2000', 'Acme GmbH', 'DE', 'EUR', gen_random_uuid(), 'active', $2, $2)`, s.ten.ID, s.ten.Human.ID)
	pgtest.Exec(t, `UPDATE identity.principal SET external_issuer = $2, version = version + 1 WHERE tenant_id = $1`, s.ten.ID, s.is.URL)
	s.reg = &registry{reg: authn.Registration{TenantID: s.ten.ID, Issuer: s.is.URL, Audience: authntest.Audience, SessionMaxAge: 12 * time.Hour, IdleTimeout: time.Hour}}
	api := apitest.NewWith(t, func(cfg *app.Config) {
		v, err := authn.NewRegistered(ctx, authn.Development, s.reg, &authn.Providers{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Verifier = v
		cfg.Now = s.clock.Now // the cell's sessions age with erp-web's
	})
	s.api = httptest.NewServer(api.Handler)
	t.Cleanup(s.api.Close)
	s.web = httptest.NewUnstartedServer(nil)
	t.Cleanup(s.web.Close)
	s.public = "https://" + s.web.Listener.Addr().String()
	creds := t.TempDir()
	s.key = writeKey(t, filepath.Join(creds, "acme.pem"))
	if err := os.WriteFile(filepath.Join(creds, "acme.secret"), []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.directory(opts, creds)
	s.client = authntest.Client{ID: "erp-web", RedirectURI: s.public + "/callback", PostLogoutRedirectURI: s.public + "/signed-out", Key: &s.key.PublicKey, Refresh: true}
	if opts.method == "client_secret_basic" {
		s.client.Key, s.client.Secret = nil, "s3cret"
	}
	s.is.RegisterClient(s.client)
	s.is.SignIn(authntest.User{Tenant: s.ten.ID, Subject: s.ten.Human.Subject})
	s.start(ctx, creds)
	return s
}

// directory makes the directory database with the provider's web client.
func (s *stack) directory(opts stackOptions, creds string) {
	ctx := context.Background()
	dirURL := pgtest.EmptyDatabase(s.t)
	dir, err := directory.Open(ctx, dirURL)
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(dir.Close)
	s.dir, s.dirURL = dir, dirURL
	method, ref := opts.method, opts.credential
	if method == "" {
		method = "private_key_jwt"
	}
	if ref == "" {
		ref = "file:" + filepath.Join(creds, "acme.pem")
		if method == "client_secret_basic" {
			ref = "file:" + filepath.Join(creds, "acme.secret")
		}
	}
	is := directory.Issuer{Issuer: s.is.URL, Audience: authntest.Audience, TenantID: s.ten.ID, Cell: "eu-1", ProviderID: uuid.New(),
		Web: &directory.WebClient{LoginCode: loginCodeOf, ClientID: "erp-web", AuthMethod: method, CredentialRef: ref, Scope: "openid erp-api offline_access"}}
	if err := dir.ClaimIssuer(ctx, is); err != nil {
		s.t.Fatal(err)
	}
	if err := dir.ClaimWebClient(ctx, is); err != nil {
		s.t.Fatal(err)
	}
}

// start starts erp-web.
func (s *stack) start(ctx context.Context, creds string) {
	apiURL, _ := url.Parse(s.api.URL)
	public, _ := url.Parse(s.public)
	cfg := Config{APIURL: apiURL, PublicURL: public, PseudoLocales: true, Environment: "development",
		SignIn: SignIn{DirectoryURL: s.dirURL, SessionSecrets: [][]byte{[]byte(strings.Repeat("k", 32))}, CredentialPrefix: "file:" + creds + string(filepath.Separator)}}
	srv, err := New(ctx, cfg, buildinfo.Info{Version: "v1"}, slog.New(slog.NewTextHandler(s.logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(srv.Close)
	srv.Now, srv.Store.Now, srv.OIDC.Now = s.clock.Now, s.clock.Now, s.clock.Now
	s.srv = srv
	s.web.Config.Handler = srv.Handler()
	s.web.StartTLS()
}

func writeKey(t *testing.T, path string) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return key
}

// browser is a client with its own cookies that follows no redirect.
type browser struct {
	s  *stack
	hc *http.Client
}

func (s *stack) browser() *browser {
	jar, _ := cookiejar.New(nil) // with no options it cannot fail
	hc := new(http.Client)
	*hc = *s.web.Client() // a copy: the server hands out one client, and each browser has its own cookies
	hc.Jar = jar
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &browser{s: s, hc: hc}
}

// response is an answer, read.
type response struct {
	status int
	header http.Header
	body   string
	url    *url.URL
}

func (r response) location() string { return r.header.Get("Location") }

// do sends a request; header holds extra headers, as name, value pairs.
func (b *browser) do(method, u string, body url.Values, header ...string) response {
	b.s.t.Helper()
	if strings.HasPrefix(u, "/") {
		u = b.s.public + u
	}
	var rd io.Reader
	if body != nil {
		rd = strings.NewReader(body.Encode())
	}
	req, err := http.NewRequestWithContext(context.Background(), method, u, rd)
	if err != nil {
		b.s.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	// A same-origin request, as the browser sends it, unless the test says
	// otherwise.
	if strings.HasPrefix(u, b.s.public) && method != http.MethodGet && method != http.MethodHead {
		req.Header.Set("Origin", b.s.public)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := b.hc.Do(req)
	if err != nil {
		b.s.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }() // read in full below
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		b.s.t.Fatal(err)
	}
	return response{status: resp.StatusCode, header: resp.Header, body: string(data), url: req.URL}
}

func (b *browser) get(u string, header ...string) response {
	b.s.t.Helper()
	return b.do(http.MethodGet, u, nil, header...)
}

// follow GETs u and follows redirects, at most ten, returning the last
// answer.
func (b *browser) follow(u string) response {
	b.s.t.Helper()
	r := b.get(u)
	for i := 0; i < 10 && r.status >= 300 && r.status < 400; i++ {
		next, err := r.url.Parse(r.location())
		if err != nil {
			b.s.t.Fatal(err)
		}
		r = b.get(next.String())
	}
	return r
}

// signIn signs the browser in and continues to where the continue page
// leads; it returns the continue page.
func (b *browser) signIn() response {
	b.s.t.Helper()
	r := b.follow("/login/" + loginCodeOf + "?return=/")
	if r.status != http.StatusOK || continueTarget(b.s.t, r.body) == "" {
		b.s.t.Fatalf("sign-in: got %d %s, want the continue page", r.status, r.body)
	}
	return r
}

// cookie is the browser's cookie of the name, "" without one.
func (b *browser) cookie(name string) string {
	u, _ := url.Parse(b.s.public)
	for _, c := range b.hc.Jar.Cookies(u) {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

// setCookie sets a cookie in the browser.
func (b *browser) setCookie(name, value string) {
	u, _ := url.Parse(b.s.public)
	b.hc.Jar.SetCookies(u, []*http.Cookie{{Name: name, Value: value, Path: "/", Secure: true}})
}

// csrf is the CSRF token of a signed-in page, from its sign-out form.
func (b *browser) csrf() string {
	b.s.t.Helper()
	r := b.get("/")
	if r.status != http.StatusOK {
		b.s.t.Fatalf("home: got %d %s", r.status, r.body)
	}
	token := ""
	walkHTML(r.body, func(n *html.Node) {
		if n.Data == "input" && attrOf(n, "name") == "csrf_token" {
			token = attrOf(n, "value")
		}
	})
	if token == "" {
		b.s.t.Fatal("home: no CSRF token")
	}
	return token
}

func walkHTML(body string, fn func(*html.Node)) {
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		return
	}
	walk(doc, fn)
}

func attrOf(n *html.Node, key string) string {
	v, _ := attr(n, key)
	return v
}

// continueTarget is where a continue page goes on to, "" for another page.
func continueTarget(t *testing.T, body string) string {
	t.Helper()
	target := ""
	walkHTML(body, func(n *html.Node) {
		if n.Data == "meta" && attrOf(n, "http-equiv") == "refresh" {
			target = strings.TrimPrefix(attrOf(n, "content"), "0;url=")
		}
	})
	return target
}

// sessions counts erp-web's sessions.
func (s *stack) sessions() int {
	s.t.Helper()
	var n int
	if err := s.dir.Pool.InTx(context.Background(), database.Scope{}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM web.session`).Scan(&n)
	}); err != nil {
		s.t.Fatal(err)
	}
	return n
}

// cellSessions lists the cell's sessions of the person: their end reasons
// ("" while live).
func (s *stack) cellSessions() []string {
	s.t.Helper()
	var out []string
	pgtest.Scan(s.t, `SELECT coalesce(array_agg(coalesce(end_reason, '') ORDER BY created_at), '{}') FROM identity.session WHERE tenant_id = $1 AND principal_id = $2`,
		[]any{s.ten.ID, s.ten.Human.ID}, &out)
	return out
}
