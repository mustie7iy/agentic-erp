package e2e

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/directory"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn/authntest"
	"github.com/mustie7iy/agentic-erp/internal/platform/buildinfo"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/web"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// The vendored axe-core: an upgrade replaces the file, its licence files
// and these constants in one reviewed change.
const (
	axeFile    = "testdata/axe-4.14.0.min.js"
	axeSHA256  = "20c09fe157a8a34a30e241aaa1fcdade657734f08ab379ecfbeb7d45cc46e878"
	axeLicence = "testdata/axe-4.14.0.LICENSE"
)

// axe reads the vendored axe-core, checked against its pin.
func axe(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile(axeFile)
	if err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(src); hex.EncodeToString(sum[:]) != axeSHA256 {
		t.Fatalf("%s: SHA-256 %x, want the pinned %s", axeFile, sum, axeSHA256)
	}
	return string(src)
}

// TestVendoredLicences: the vendored scripts keep their licences beside
// them: htmx's Zero-Clause BSD, axe-core's MPL-2.0 and the licences of the
// code axe-core bundles (ADR-0052, recorded there).
func TestVendoredLicences(t *testing.T) {
	for file, want := range map[string]string{
		"../static/vendor/htmx-2.0.11.LICENSE":      "Zero-Clause BSD",
		axeLicence:                                  "Mozilla Public License, version 2.0",
		"testdata/axe-4.14.0.LICENSE-3RD-PARTY.txt": "MIT License",
	} {
		t.Run(file, func(t *testing.T) {
			b, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(b), want) {
				t.Errorf("%s: no %q", file, want)
			}
		})
	}
}

// browser is the Chromium to drive, or "" when there is none.
func browser() string {
	if p := os.Getenv("ERP_E2E_BROWSER"); p != "" {
		return p
	}
	for _, name := range []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", "/Applications/Chromium.app/Contents/MacOS/Chromium",
		"chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "headless-shell",
	} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// stack is a running erp-web, over TLS, in front of the in-process API,
// signing people in at a tenant's identity provider: authntest's
// authorization server, which the cell's registry holds with the
// tenant's session rule, and whose web client the directory database
// projects (ADR-0052 §5).
type stack struct {
	api *apitest.API
	web *web.Server
	url string
	is  *authntest.Issuer
	ten pgtest.Tenant
}

// signInAs makes a the person the provider signs in next.
func (s stack) signInAs(a pgtest.Actor) {
	s.is.SignIn(authntest.User{Tenant: s.ten.ID, Subject: a.Subject})
}

// loginCode is the stack's tenant's login code.
const loginCode = "acme"

func newStack(t *testing.T) stack {
	t.Helper()
	ten := pgtest.NewTenant(t, "tenancy.org.read")
	ten.AllCompanies(t, ten.Human)
	pgtest.Exec(t, `INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by)
		VALUES ($1, gen_random_uuid(), '2000', 'Acme GmbH', 'DE', 'EUR', gen_random_uuid(), 'active', $2, $2)`, ten.ID, ten.Human.ID)
	return stackFor(t, ten, ten.Human)
}

// stackFor is the stack of a tenant whose principals the test made, with
// person signing in first.
func stackFor(t *testing.T, ten pgtest.Tenant, person pgtest.Actor) stack {
	t.Helper()
	return stackWith(t, ten, person, nil)
}

// stackWith is stackFor with the API's configuration changed by configure
// (nil: as it is).
func stackWith(t *testing.T, ten pgtest.Tenant, person pgtest.Actor, configure func(*app.Config)) stack {
	t.Helper()
	ctx := context.Background()
	is := authntest.NewSeparate(t)
	pgtest.Exec(t, `UPDATE identity.principal SET external_issuer = $2, version = version + 1 WHERE tenant_id = $1`, ten.ID, is.URL)
	reg := registry{authn.Registration{TenantID: ten.ID, Issuer: is.URL, Audience: authntest.Audience, SessionMaxAge: 12 * time.Hour, IdleTimeout: time.Hour}}
	api := apitest.NewWith(t, func(cfg *app.Config) {
		v, err := authn.NewRegistered(ctx, authn.Development, reg, &authn.Providers{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Verifier = v
		if configure != nil {
			configure(cfg)
		}
	})
	apiSrv := httptest.NewServer(api.Handler)
	t.Cleanup(apiSrv.Close)
	webSrv := httptest.NewUnstartedServer(nil)
	t.Cleanup(webSrv.Close)
	public := "https://" + webSrv.Listener.Addr().String()
	creds := t.TempDir()
	key := clientKey(t, filepath.Join(creds, "acme.pem"))
	dirURL := directoryWith(t, directory.Issuer{Issuer: is.URL, Audience: authntest.Audience, TenantID: ten.ID, Cell: "eu-1", ProviderID: uuid.New(),
		Web: &directory.WebClient{LoginCode: loginCode, ClientID: "erp-web", AuthMethod: "private_key_jwt", CredentialRef: "file:" + filepath.Join(creds, "acme.pem"), Scope: "openid erp-api"}})
	is.RegisterClient(authntest.Client{ID: "erp-web", RedirectURI: public + "/callback", PostLogoutRedirectURI: public + "/signed-out", Key: &key.PublicKey, Refresh: true})
	is.SignIn(authntest.User{Tenant: ten.ID, Subject: person.Subject})
	// As in the cluster: the API's public name, reached at the router's
	// Service (here the in-process API's listener).
	apiURL, _ := url.Parse("http://api.example.invalid")
	publicURL, _ := url.Parse(public)
	cfg := web.Config{APIURL: apiURL, APIDial: apiSrv.Listener.Addr().String(), PublicURL: publicURL, PseudoLocales: true, Environment: "development",
		SignIn: web.SignIn{DirectoryURL: dirURL, SessionSecrets: [][]byte{[]byte(strings.Repeat("k", 32))}, CredentialPrefix: "file:" + creds + string(filepath.Separator)}}
	srv, err := web.New(ctx, cfg, buildinfo.Get(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	webSrv.Config.Handler = srv.Handler()
	webSrv.StartTLS()
	return stack{api: api, web: srv, url: public, is: is, ten: ten}
}

// registry is the cell's registry of the tenant's one provider.
type registry struct{ reg authn.Registration }

func (r registry) Resolve(_ context.Context, issuer string, audiences []string) (authn.Registration, error) {
	if issuer == r.reg.Issuer && slices.Contains(audiences, r.reg.Audience) {
		return r.reg, nil
	}
	return authn.Registration{}, authn.ErrUnknownIssuer
}

// directoryWith is a directory database of the test's own, with the web
// schema and the claim.
func directoryWith(t *testing.T, is directory.Issuer) string {
	t.Helper()
	ctx := context.Background()
	dirURL := pgtest.EmptyDatabase(t)
	dir, err := directory.Open(ctx, dirURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dir.Close)
	if err := dir.ClaimIssuer(ctx, is); err != nil {
		t.Fatal(err)
	}
	if err := dir.ClaimWebClient(ctx, is); err != nil {
		t.Fatal(err)
	}
	return dirURL
}

// clientKey writes the web client's private key, PEM, and returns it.
func clientKey(t *testing.T, path string) *rsa.PrivateKey {
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

// TestAPIClientAgainstTheAPI: the typed client erp-web holds reaches the
// real API as the person whose token it carries, gets the API's own
// refusal for a person without the permission, and sends nothing without
// a token.
func TestAPIClientAgainstTheAPI(t *testing.T) {
	api := apitest.New(t)
	apiSrv := httptest.NewServer(api.Handler)
	t.Cleanup(apiSrv.Close)
	client, err := web.NewAPI(apiSrv.URL, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	allowed, refused := pgtest.NewTenant(t, "tenancy.org.read"), pgtest.NewTenant(t)
	call := func(ten pgtest.Tenant) (gen.ListLegalEntitiesRes, error) {
		ctx := web.WithToken(t.Context(), api.Issuer.Token(t, ten.ID, ten.Human.Subject, nil))
		return client.ListLegalEntities(ctx, gen.ListLegalEntitiesParams{})
	}
	if res, err := call(allowed); err != nil {
		t.Errorf("allowed: %v", err)
	} else if _, ok := res.(*gen.LegalEntityPageHeaders); !ok {
		t.Errorf("allowed: got %T, want a page of legal entities", res)
	}
	if res, err := call(refused); err != nil {
		t.Errorf("refused: %v", err)
	} else if _, ok := res.(*gen.ForbiddenHeaders); !ok {
		t.Errorf("refused: got %T, want the API's 403", res)
	}
	if _, err := client.ListLegalEntities(t.Context(), gen.ListLegalEntitiesParams{}); err == nil || !strings.Contains(err.Error(), web.ErrNoSession.Error()) {
		t.Errorf("no token: got %v, want ErrNoSession", err)
	}
}

// console collects what a tab reports as an error: exceptions, console
// errors and the browser's own log, where CSP violations appear.
type console struct {
	mu   sync.Mutex
	errs []string
}

// listen collects the tab's errors until its context ends.
func (c *console) listen(ctx context.Context) {
	messages := chromedp.Console(ctx)
	go func() {
		for m, err := range messages {
			if err != nil {
				return // the tab's context ended
			}
			if m.Type == chromedp.ConsoleError || m.Type == chromedp.ConsoleException {
				c.mu.Lock()
				c.errs = append(c.errs, fmt.Sprintf("%s %s: %s %s", m.Type, m.Source, m.Text, m.URL))
				c.mu.Unlock()
			}
		}
	}()
}

func (c *console) errors() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.errs...)
}

// browse opens a browser for the test, or skips it without one.
func browse(t *testing.T) context.Context {
	t.Helper()
	if testing.Short() {
		t.Skip("end-to-end: drives a browser")
	}
	path := browser()
	if path == "" {
		if os.Getenv("ERP_E2E_REQUIRE_BROWSER") != "" {
			t.Fatal("ERP_E2E_REQUIRE_BROWSER is set and no Chromium was found: set ERP_E2E_BROWSER")
		}
		t.Skip("no Chromium-based browser (ERP_E2E_BROWSER)")
	}
	// The workspace serves TLS (its cookies are __Host-, Secure) with the
	// test server's own certificate.
	opts := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(path), chromedp.Flag("ignore-certificate-errors", true))
	if os.Getenv("CI") != "" {
		// CI's runners refuse Chromium's sandbox its user namespaces; the
		// browser loads this test's own pages alone.
		opts = append(opts, chromedp.NoSandbox)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	alloc, cancelAlloc := chromedp.NewExecAllocator(ctx, opts...)
	t.Cleanup(cancelAlloc)
	browserCtx, cancelBrowser := chromedp.NewContext(alloc)
	t.Cleanup(cancelBrowser)
	if err := chromedp.Do(browserCtx); err != nil {
		t.Fatalf("start %s: %v", path, err)
	}
	return browserCtx
}

// tab is a new tab of the browser at width, asking for lang, its errors
// collected.
func tab(t *testing.T, browserCtx context.Context, lang string, width int64) (context.Context, *console) {
	t.Helper()
	ctx, cancel := chromedp.NewContext(browserCtx)
	t.Cleanup(cancel)
	c := &console{}
	c.listen(ctx)
	if err := chromedp.Do(ctx, chromedp.EmulateViewport(width, 900)); err != nil {
		t.Fatal(err)
	}
	if lang != "" {
		if _, err := chromedp.Call(ctx, network.SetExtraHTTPHeaders, network.SetExtraHTTPHeadersParams{Headers: network.Headers{"Accept-Language": lang}}); err != nil {
			t.Fatal(err)
		}
	}
	return ctx, c
}

// violation is one axe rule a page breaks.
type violation struct {
	ID    string `json:"id"`
	Help  string `json:"help"`
	Nodes []struct {
		Target []string `json:"target"`
	} `json:"nodes"`
}

// page is what a test reads from a loaded page.
type page struct {
	Dir      string `json:"dir"`
	Lang     string `json:"lang"`
	HTMX     bool   `json:"htmx"`
	Overflow bool   `json:"overflow"`
	Focus    string `json:"focus"`
}

const inspect = `({
	dir: document.documentElement.dir,
	lang: document.documentElement.lang,
	htmx: typeof htmx === "object" && htmx.version === "2.0.11" && htmx.config.allowEval === false &&
		htmx.config.allowScriptTags === false && htmx.config.historyCacheSize === 0 && htmx.config.selfRequestsOnly === true,
	overflow: document.documentElement.scrollWidth > window.innerWidth,
	focus: (document.activeElement && document.activeElement.className) || ""
})`

// The WCAG 2.2 A and AA rules of axe-core (ADR-0052 §10).
const runAxe = `axe.run(document, {runOnly: {type: "tag", values: ["wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22aa"]}, resultTypes: ["violations"]}).then(r => r.violations)`

// TestPagesInTheBrowser: every page, before and after sign-in, in
// English, French and both pseudo-locales, at a desktop's and a phone's
// width: no error in the browser (a CSP violation is one), htmx loaded
// with eval and script tags off, the locale's language and direction, no
// sideways scrolling, the skip link first in the tab order, and axe-core
// at zero violations.
func TestPagesInTheBrowser(t *testing.T) {
	browserCtx := browse(t)
	s := newStack(t)
	src := axe(t)
	// Pages a person sees before signing in, then after; the 404s are
	// pages too (the browser logs their status).
	before := []string{"/login", "/login/nobody", "/signed-out", "/health", "/no/such/page"}
	after := []string{"/", "/login?other", "/health"}
	langs := []struct{ header, lang, dir string }{{"", "en", "ltr"}, {"en-XA", "en-XA", "ltr"}, {"ar-XB", "ar-XB", "rtl"}, {"fr-CA", "fr-CA", "ltr"}}
	for _, paths := range [][]string{before, after} {
		if paths[0] == "/" {
			signIn(t, browserCtx, s)
		}
		for _, lang := range langs {
			for _, width := range []int64{1280, 390} {
				for _, path := range paths {
					t.Run(fmt.Sprintf("%s %d %s", lang.lang, width, path), func(t *testing.T) {
						checkPage(t, browserCtx, s, src, lang.header, lang.lang, lang.dir, width, path)
					})
				}
			}
		}
	}
}

// checkPage loads path and checks it as TestPagesInTheBrowser says.
func checkPage(t *testing.T, browserCtx context.Context, s stack, src, header, lang, dir string, width int64, path string) {
	t.Helper()
	ctx, c := tab(t, browserCtx, header, width)
	if err := chromedp.Do(ctx,
		chromedp.Navigate(s.url+path),
		chromedp.WaitReady(chromedp.CSS("main")),
	); err != nil {
		t.Fatal(err)
	}
	audit(t, ctx, src, lang, dir, width)
	for _, e := range c.errors() {
		// A page answered 4xx is the browser's to log.
		if strings.HasSuffix(e, " "+s.url+path) && (strings.Contains(e, "status of 404") || strings.Contains(e, "status of 403")) {
			continue
		}
		t.Errorf("browser: %s", e)
	}
}

// audit checks the page the tab shows as TestPagesInTheBrowser says: its
// language and direction, htmx, no sideways scrolling at width, the skip
// link as the first tab stop, and axe-core at zero violations.
func audit(t *testing.T, ctx context.Context, src, lang, dir string, width int64) {
	t.Helper()
	if err := chromedp.Do(ctx, chromedp.KeyEvent("\t")); err != nil {
		t.Fatal(err)
	}
	got, err := chromedp.Run(ctx, chromedp.Evaluate[page](inspect))
	if err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Do(ctx, chromedp.Evaluate[chromedp.Void](src)); err != nil {
		t.Fatal(err)
	}
	violations, err := chromedp.Run(ctx, chromedp.Evaluate[[]violation](runAxe, chromedp.EvalAwaitPromise))
	if err != nil {
		t.Fatal(err)
	}
	if got.Lang != lang || got.Dir != dir {
		t.Errorf("document: lang %q dir %q, want %q %q", got.Lang, got.Dir, lang, dir)
	}
	if !got.HTMX {
		t.Error("htmx: not loaded with the workspace's configuration")
	}
	if got.Overflow {
		t.Errorf("the page scrolls sideways at %d px", width)
	}
	if got.Focus != "skip-link" {
		t.Errorf("first tab stop: got %q, want the skip link", got.Focus)
	}
	for _, v := range violations {
		t.Errorf("axe %s: %s at %v", v.ID, v.Help, v.Nodes)
	}
}

// signIn signs the browser in through the provider, as a person does:
// the login code page, the provider, the continue page, the home page.
func signIn(t *testing.T, browserCtx context.Context, s stack) {
	t.Helper()
	ctx, _ := tab(t, browserCtx, "", 1280)
	if err := chromedp.Do(ctx,
		chromedp.Navigate(s.url+"/"),
		chromedp.WaitVisible(chromedp.CSS("#code")),
		chromedp.SendKeys(chromedp.CSS("#code"), loginCode),
		chromedp.Click(chromedp.CSS("main button[type=submit]")),
	); err != nil {
		t.Fatal(err)
	}
	var last string
	pgtest.Eventually(t, 20*time.Second, func() bool {
		text, err := chromedp.Run(ctx, chromedp.Evaluate[string](`location.href + " " + document.body.innerText`))
		last = text
		// Home, with the companies the person may see, or none.
		return err == nil && strings.HasPrefix(text, s.url+"/ ") && (strings.Contains(text, "Acme GmbH") || strings.Contains(text, "no company to work in"))
	}, "the browser did not reach the home page signed in: %s", lastSeen{&last})
}

// lastSeen is what the browser showed last, said when a wait fails.
type lastSeen struct{ text *string }

func (l lastSeen) String() string { return *l.text }

// TestSignInInTheBrowser is sign-in and sign-out as a person goes through
// them (ADR-0052 §5): the session cookie is HttpOnly, Secure and
// SameSite=Strict, out of the page's scripts' reach, and signing out ends
// the provider's session too, back on the signed-out page.
func TestSignInInTheBrowser(t *testing.T) {
	browserCtx := browse(t)
	s := newStack(t)
	signIn(t, browserCtx, s)
	ctx, c := tab(t, browserCtx, "", 1280)
	if err := chromedp.Do(ctx, chromedp.Navigate(s.url+"/"), chromedp.WaitReady(chromedp.CSS("main"))); err != nil {
		t.Fatal(err)
	}
	got, err := chromedp.Call(ctx, network.GetCookies, network.GetCookiesParams{})
	if err != nil {
		t.Fatal(err)
	}
	cookies := got.Cookies
	found := false
	for _, ck := range cookies {
		if ck.Name == "__Host-Http-erp-session" {
			found = true
			if !ck.HTTPOnly || !ck.Secure || ck.SameSite != network.CookieSameSiteStrict || ck.Path != "/" {
				t.Errorf("session cookie: got %+v", ck)
			}
		}
	}
	if !found {
		t.Errorf("cookies: got %v, want the session's", cookies)
	}
	if seen, err := chromedp.Run(ctx, chromedp.Evaluate[string](`document.cookie`)); err != nil || strings.Contains(seen, "erp-session") {
		t.Errorf("document.cookie: got %q, want the session out of reach", seen)
	}
	if err := chromedp.Do(ctx, chromedp.Click(chromedp.CSS("header form button"))); err != nil {
		t.Fatal(err)
	}
	pgtest.Eventually(t, 20*time.Second, func() bool {
		text, err := chromedp.Run(ctx, chromedp.Evaluate[string](`location.pathname + " " + document.body.innerText`))
		return err == nil && strings.HasPrefix(text, "/signed-out ") && strings.Contains(text, "You are signed out")
	}, "the browser did not reach the signed-out page")
	if n := len(s.is.Logouts()); n != 1 {
		t.Errorf("the provider's logouts: got %d, want 1", n)
	}
	for _, e := range c.errors() {
		t.Errorf("browser: %s", e)
	}
	after, err := chromedp.Call(ctx, network.GetCookies, network.GetCookiesParams{})
	if err != nil {
		t.Fatal(err)
	}
	for _, ck := range after.Cookies {
		if ck.Name == "__Host-Http-erp-session" {
			t.Errorf("after sign-out: the session cookie is still there (%+v)", ck)
		}
	}
}

// TestCSPEnforced: the browser refuses a script the page did not carry
// with its nonce, inline or from this origin (htmx's own file, planted
// again without the nonce), so a planted script cannot run.
func TestCSPEnforced(t *testing.T) {
	browserCtx := browse(t)
	s := newStack(t)
	ctx, c := tab(t, browserCtx, "", 1280)
	if err := chromedp.Do(ctx,
		chromedp.Navigate(s.url+"/health"),
		chromedp.WaitReady(chromedp.CSS("main")),
		chromedp.Evaluate[chromedp.Void](`(() => { const s = document.createElement("script"); s.textContent = "window.planted = true"; document.body.append(s) })()`),
		chromedp.Evaluate[chromedp.Void](`(() => { window.htmx = undefined; const s = document.createElement("script"); s.src = "`+s.web.Assets.Script+`"; document.body.append(s) })()`),
	); err != nil {
		t.Fatal(err)
	}
	// The browser reports the refused file; until then it might still run.
	pgtest.Eventually(t, 10*time.Second, func() bool {
		return slices.ContainsFunc(c.errors(), func(e string) bool { return strings.Contains(e, s.web.Assets.Script) })
	}, "the browser did not report refusing %s", s.web.Assets.Script)
	if again, err := chromedp.Run(ctx, chromedp.Evaluate[bool](`typeof window.htmx === "object"`)); err != nil || again {
		t.Errorf("a same-origin script without the nonce ran (%v)", err)
	}
	ran, err := chromedp.Run(ctx, chromedp.Evaluate[bool](`window.planted === true`))
	if err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Error("a script without the nonce ran")
	}
	refused := false
	for _, e := range c.errors() {
		refused = refused || strings.Contains(e, "Content Security Policy")
	}
	if !refused {
		t.Errorf("browser errors: got %v, want the CSP's refusal", c.errors())
	}
}
