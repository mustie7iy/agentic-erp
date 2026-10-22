package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
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

// stack is a running erp-web in front of the in-process API.
type stack struct {
	api *apitest.API
	web *web.Server
	url string
}

func newStack(t *testing.T) stack {
	t.Helper()
	api := apitest.New(t)
	apiSrv := httptest.NewServer(api.Handler)
	t.Cleanup(apiSrv.Close)
	// As in the cluster: the API's public name, reached at the router's
	// Service (here the in-process API's listener).
	apiURL, err := url.Parse("http://api.example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	cfg := web.Config{APIURL: apiURL, APIDial: apiSrv.Listener.Addr().String(), PseudoLocales: true}
	srv, err := web.New(cfg, buildinfo.Get(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	webSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(webSrv.Close)
	return stack{api: api, web: srv, url: webSrv.URL}
}

// TestAPIClientAgainstTheAPI: the typed client erp-web holds reaches the
// real API as the person whose token it carries, gets the API's own
// refusal for a person without the permission, and sends nothing without
// a token.
func TestAPIClientAgainstTheAPI(t *testing.T) {
	s := newStack(t)
	allowed, refused := pgtest.NewTenant(t, "tenancy.org.read"), pgtest.NewTenant(t)
	call := func(ten pgtest.Tenant) (gen.ListLegalEntitiesRes, error) {
		ctx := web.WithToken(t.Context(), s.api.Issuer.Token(t, ten.ID, ten.Human.Subject, nil))
		return s.web.API.ListLegalEntities(ctx, gen.ListLegalEntitiesParams{})
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
	if _, err := s.web.API.ListLegalEntities(t.Context(), gen.ListLegalEntitiesParams{}); err == nil || !strings.Contains(err.Error(), web.ErrNoSession.Error()) {
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
	opts := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(path))
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

// TestPagesInTheBrowser: every page, in English and both pseudo-locales,
// at a desktop's and a phone's width: no error in the browser (a CSP
// violation is one), htmx loaded with eval and script tags off, the
// locale's language and direction, no sideways scrolling, the skip link
// first in the tab order, and axe-core at zero violations.
func TestPagesInTheBrowser(t *testing.T) {
	browserCtx := browse(t)
	s := newStack(t)
	src := axe(t)
	for _, lang := range []struct{ header, lang, dir string }{{"", "en", "ltr"}, {"en-XA", "en-XA", "ltr"}, {"ar-XB", "ar-XB", "rtl"}, {"fr-CA", "fr-CA", "ltr"}} {
		for _, width := range []int64{1280, 390} {
			for _, path := range []string{"/", "/health", "/no/such/page"} {
				t.Run(fmt.Sprintf("%s %d %s", lang.lang, width, path), func(t *testing.T) {
					ctx, c := tab(t, browserCtx, lang.header, width)
					if err := chromedp.Do(ctx,
						chromedp.Navigate(s.url+path),
						chromedp.WaitReady(chromedp.CSS("main")),
						chromedp.KeyEvent("\t"),
					); err != nil {
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
					if got.Lang != lang.lang || got.Dir != lang.dir {
						t.Errorf("document: lang %q dir %q, want %q %q", got.Lang, got.Dir, lang.lang, lang.dir)
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
					for _, e := range c.errors() {
						// The not-found page's own 404 is the browser's to log.
						if path == "/no/such/page" && strings.HasSuffix(e, " "+s.url+path) && strings.Contains(e, "status of 404") {
							continue
						}
						t.Errorf("browser: %s", e)
					}
				})
			}
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
		chromedp.Navigate(s.url+"/"),
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
