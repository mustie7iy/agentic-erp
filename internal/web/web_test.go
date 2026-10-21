package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"go.opentelemetry.io/otel/trace/noop"
	"golang.org/x/net/html"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/buildinfo"
	"github.com/mustie7iy/agentic-erp/internal/platform/telemetry"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// testServer is the workspace with the pseudo-locales on.
func testServer(t *testing.T) (*Server, *bytes.Buffer) {
	t.Helper()
	catalog, err := i18n.Load(true)
	if err != nil {
		t.Fatal(err)
	}
	assets, err := LoadAssets()
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	return &Server{Catalog: catalog, Assets: assets, Build: buildinfo.Info{Version: "v1.2.3", Revision: "0123abc"},
		Log: slog.New(slog.NewJSONHandler(&logs, nil))}, &logs
}

func get(t *testing.T, h http.Handler, path, lang string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	if lang != "" {
		r.Header.Set("Accept-Language", lang)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// pages are the workspace's pages and their statuses.
var pages = map[string]int{"/": http.StatusOK, "/health": http.StatusOK, "/no/such/page": http.StatusNotFound}

// routes are every kind of response: the pages, both static files and a
// static path that is none.
func routes(t *testing.T, s *Server) map[string]int {
	t.Helper()
	r := map[string]int{s.Assets.CSS: http.StatusOK, s.Assets.Script: http.StatusOK, "/static/css/workspace.css": http.StatusNotFound}
	for p, code := range pages {
		r[p] = code
	}
	return r
}

// TestSecurityHeaders: every response, page or file or not found, carries
// the CSP with a nonce and the other headers of ADR-0052 §8.
func TestSecurityHeaders(t *testing.T) {
	s, _ := testServer(t)
	h := s.Handler()
	cspRE := regexp.MustCompile(`^default-src 'self'; script-src 'self' 'nonce-[A-Z2-7]{26}'; style-src 'self'; img-src 'self' data:; ` +
		`connect-src 'self'; frame-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'$`)
	want := map[string]string{
		"Strict-Transport-Security":    "max-age=63072000; includeSubDomains",
		"X-Content-Type-Options":       "nosniff",
		"X-Frame-Options":              "DENY",
		"Referrer-Policy":              "same-origin",
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Permissions-Policy":           permissionsPolicy,
	}
	for path, code := range routes(t, s) {
		t.Run(path, func(t *testing.T) {
			rec := get(t, h, path, "")
			if rec.Code != code {
				t.Fatalf("status: got %d, want %d", rec.Code, code)
			}
			if got := rec.Header().Get("Content-Security-Policy"); !cspRE.MatchString(got) {
				t.Errorf("Content-Security-Policy: got %q", got)
			}
			for name, value := range want {
				if got := rec.Header().Get(name); got != value {
					t.Errorf("%s: got %q, want %q", name, got, value)
				}
			}
		})
	}
}

// TestCaching: pages are never stored, static files are cached for a
// year under their content-hashed names.
func TestCaching(t *testing.T) {
	s, _ := testServer(t)
	h := s.Handler()
	for path, code := range pages {
		t.Run(path, func(t *testing.T) {
			rec := get(t, h, path, "")
			if rec.Code != code {
				t.Fatalf("status: got %d, want %d", rec.Code, code)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control: got %q, want no-store", got)
			}
			if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
				t.Errorf("Content-Type: got %q", got)
			}
		})
	}
	for path, typ := range map[string]string{s.Assets.CSS: "text/css; charset=utf-8", s.Assets.Script: "text/javascript; charset=utf-8"} {
		t.Run(path, func(t *testing.T) {
			rec := get(t, h, path, "")
			if got := rec.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
				t.Errorf("Cache-Control: got %q", got)
			}
			if got := rec.Header().Get("Content-Type"); got != typ {
				t.Errorf("Content-Type: got %q, want %q", got, typ)
			}
		})
	}
}

// parse is a response's document.
func parse(t *testing.T, rec *httptest.ResponseRecorder) *html.Node {
	t.Helper()
	doc, err := html.Parse(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// walk calls fn on every element below n.
func walk(n *html.Node, fn func(*html.Node)) {
	if n.Type == html.ElementNode {
		fn(n)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walk(c, fn)
	}
}

func attr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

// TestScripts: every page's only script is htmx from this origin with the
// response's nonce, a fresh one per response; no element has an event
// handler attribute; htmx's configuration turns eval and script tags off.
func TestScripts(t *testing.T) {
	s, _ := testServer(t)
	h := s.Handler()
	nonceRE := regexp.MustCompile(`'nonce-([^']+)'`)
	seen := map[string]bool{}
	for path := range pages {
		t.Run(path, func(t *testing.T) {
			rec := get(t, h, path, "")
			nonce := nonceRE.FindStringSubmatch(rec.Header().Get("Content-Security-Policy"))[1]
			if seen[nonce] {
				t.Errorf("nonce %s: reused", nonce)
			}
			seen[nonce] = true
			var scripts []string
			walk(parse(t, rec), func(n *html.Node) {
				for _, a := range n.Attr {
					if strings.HasPrefix(a.Key, "on") || strings.HasPrefix(a.Key, "hx-on") {
						t.Errorf("<%s %s>: an event handler attribute", n.Data, a.Key)
					}
				}
				switch n.Data {
				case "script":
					src, _ := attr(n, "src")
					got, _ := attr(n, "nonce")
					if got != nonce || n.FirstChild != nil {
						t.Errorf("<script src=%q nonce=%q>: want the response's nonce %q and no inline code", src, got, nonce)
					}
					scripts = append(scripts, src)
				case "meta":
					if name, _ := attr(n, "name"); name == "htmx-config" {
						checkHTMXConfig(t, n)
					}
				}
			})
			if !slices.Equal(scripts, []string{s.Assets.Script}) {
				t.Errorf("scripts: got %v, want htmx alone (%s)", scripts, s.Assets.Script)
			}
		})
	}
}

func checkHTMXConfig(t *testing.T, n *html.Node) {
	t.Helper()
	content, _ := attr(n, "content")
	var cfg map[string]any
	if err := json.Unmarshal([]byte(content), &cfg); err != nil {
		t.Fatalf("htmx-config %q: %v", content, err)
	}
	for key, want := range map[string]any{"allowEval": false, "allowScriptTags": false, "selfRequestsOnly": true, "includeIndicatorStyles": false, "historyCacheSize": float64(0)} {
		if cfg[key] != want {
			t.Errorf("htmx-config %s: got %v, want %v", key, cfg[key], want)
		}
	}
}

// TestOnlyHTMX: the vendored htmx is the pinned file and the only script
// erp-web holds, and the repository holds no other JavaScript outside
// test tooling (ADR-0052 §10: no hand-written JavaScript).
func TestOnlyHTMX(t *testing.T) {
	body, err := fs.ReadFile(static, "static/"+HTMXFile)
	if err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(body); hex.EncodeToString(sum[:]) != HTMXSHA256 {
		t.Errorf("%s: SHA-256 %x, want %s", HTMXFile, sum, HTMXSHA256)
	}
	var served []string
	if err := fs.WalkDir(static, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Ext(p) == ".js" {
			served = append(served, p)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(served, []string{"static/" + HTMXFile}) {
		t.Errorf("embedded scripts: got %v, want htmx alone", served)
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"internal/web/static/vendor": true, "internal/web/e2e/testdata": true}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p) // p is below root
		if d.IsDir() {
			// Git's own files and the build's outputs (.gitignore) hold no source.
			if name := d.Name(); p != root && (strings.HasPrefix(name, ".") || slices.Contains([]string{"bin", "dist", "data"}, rel)) {
				return filepath.SkipDir
			}
			return nil
		}
		if ext := filepath.Ext(p); (ext == ".js" || ext == ".mjs" || ext == ".ts") && !allowed[filepath.ToSlash(filepath.Dir(rel))] {
			t.Errorf("%s: JavaScript outside the vendored htmx and test tooling (ADR-0052 §4)", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestLogicalProperties: the stylesheet uses no physical direction, so
// a right-to-left page mirrors (ADR-0052 §10).
func TestLogicalProperties(t *testing.T) {
	css, err := fs.ReadFile(static, "static/"+CSSFile)
	if err != nil {
		t.Fatal(err)
	}
	// Comments say "margin-left" when they explain the rule.
	body := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(string(css), "")
	decl := regexp.MustCompile(`([a-z-]+)\s*:\s*([^;{}]+)[;}]`)
	physical := regexp.MustCompile(`(^|-)(left|right)(-|$)`)
	found := 0
	for _, m := range decl.FindAllStringSubmatch(body, -1) {
		prop, value := m[1], strings.TrimSpace(m[2])
		found++
		if physical.MatchString(prop) {
			t.Errorf("%s: %s: a physical direction; use the logical property (inline-start, inline-end)", prop, value)
		}
		if slices.Contains([]string{"text-align", "float", "clear", "caption-side", "resize"}, prop) && regexp.MustCompile(`\b(left|right)\b`).MatchString(value) {
			t.Errorf("%s: %s: a physical direction; use start or end", prop, value)
		}
	}
	if found < 50 {
		t.Fatalf("found %d declarations: the check judged nothing", found)
	}
}

// texts are the text nodes and the text attributes a person meets, outside
// what is marked translate="no".
func texts(n *html.Node, out *[]string) {
	if n.Type == html.ElementNode {
		if v, _ := attr(n, "translate"); v == "no" || n.Data == "script" || n.Data == "style" {
			return
		}
		for _, key := range []string{"aria-label", "title", "alt", "placeholder"} {
			if v, ok := attr(n, key); ok {
				*out = append(*out, v)
			}
		}
	}
	if n.Type == html.TextNode && strings.TrimSpace(n.Data) != "" {
		*out = append(*out, strings.TrimSpace(n.Data))
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		texts(c, out)
	}
}

// TestPseudoLocales: in the long-text and the right-to-left pseudo-locale
// every text of every page is the catalog's, so no string bypasses it; the
// page says its language and direction.
func TestPseudoLocales(t *testing.T) {
	s, _ := testServer(t)
	h := s.Handler()
	for _, tc := range []struct {
		lang, dir, open, close string
	}{
		{"en-XA", "ltr", i18n.LongOpen, i18n.LongClose},
		{"ar-XB", "rtl", i18n.RTLOpen, i18n.RTLClose},
	} {
		for path := range pages {
			t.Run(tc.lang+path, func(t *testing.T) {
				rec := get(t, h, path, tc.lang)
				if got := rec.Header().Get("Content-Language"); got != tc.lang {
					t.Errorf("Content-Language: got %q, want %q", got, tc.lang)
				}
				doc := parse(t, rec)
				var root *html.Node
				walk(doc, func(n *html.Node) {
					if n.Data == "html" {
						root = n
					}
				})
				if lang, _ := attr(root, "lang"); lang != tc.lang {
					t.Errorf("lang: got %q, want %q", lang, tc.lang)
				}
				if dir, _ := attr(root, "dir"); dir != tc.dir {
					t.Errorf("dir: got %q, want %q", dir, tc.dir)
				}
				var all []string
				texts(doc, &all)
				if len(all) < 5 {
					t.Fatalf("texts: got %v, too few to judge", all)
				}
				for _, text := range all {
					if !strings.HasPrefix(text, tc.open) || !strings.HasSuffix(text, tc.close) {
						t.Errorf("text %q: not from the catalog (want it in %s…%s)", text, tc.open, tc.close)
					}
				}
			})
		}
	}
}

// TestLanguages: the shipped languages, by Accept-Language.
func TestLanguages(t *testing.T) {
	s, _ := testServer(t)
	h := s.Handler()
	for lang, want := range map[string]string{"": "Your workspace", "fr-CA,fr;q=0.9": "Votre espace de travail", "de": "Your workspace"} {
		t.Run(lang, func(t *testing.T) {
			rec := get(t, h, "/", lang)
			if body := rec.Body.String(); !strings.Contains(body, want) {
				t.Errorf("got a page without %q", want)
			}
			if got := rec.Header().Get("Vary"); got != "Accept-Language" {
				t.Errorf("Vary: got %q, want Accept-Language", got)
			}
		})
	}
}

// TestStatusPage: the health page names the build.
func TestStatusPage(t *testing.T) {
	s, _ := testServer(t)
	body := get(t, s.Handler(), "/health", "").Body.String()
	for _, want := range []string{"v1.2.3", "0123abc", "erp-web", "Running", "English"} {
		if !strings.Contains(body, want) {
			t.Errorf("health page: no %q", want)
		}
	}
}

// TestRenderFailure: a page that cannot be rendered is a clean 500,
// logged, never half a page.
func TestRenderFailure(t *testing.T) {
	s, logs := testServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	s.render(rec, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx), http.StatusOK, ui.Home(s.shell(i18n.HomeTitle, "/")))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status: got %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "<html") {
		t.Errorf("body: got %q, want no page", rec.Body.String())
	}
	if !strings.Contains(logs.String(), "web: render failed") {
		t.Errorf("logs: got %q, want the failure", logs.String())
	}
}

// brokenFS fails as told.
type brokenFS struct {
	fstest.MapFS
	readDir, readFile bool
}

func (b brokenFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if b.readDir {
		return nil, fs.ErrPermission
	}
	return b.MapFS.ReadDir(name)
}

func (b brokenFS) ReadFile(name string) ([]byte, error) {
	if b.readFile {
		return nil, fs.ErrPermission
	}
	return b.MapFS.ReadFile(name)
}

func TestLoadAssetsRefuses(t *testing.T) {
	htmx, err := fs.ReadFile(static, "static/"+HTMXFile)
	if err != nil {
		t.Fatal(err)
	}
	good := fstest.MapFS{CSSFile: {Data: []byte("a{}")}, HTMXFile: {Data: htmx}}
	with := func(name string, f *fstest.MapFile) fstest.MapFS {
		m := fstest.MapFS{}
		for k, v := range good {
			m[k] = v
		}
		if f == nil {
			delete(m, name)
		} else {
			m[name] = f
		}
		return m
	}
	for name, tc := range map[string]struct {
		fsys fs.FS
		want string
	}{
		"htmx changed":       {with(HTMXFile, &fstest.MapFile{Data: []byte("alert(1)")}), "want the pinned"},
		"another kind":       {with("img/logo.svg", &fstest.MapFile{Data: []byte("<svg/>")}), "only stylesheets and htmx"},
		"no stylesheet":      {with(CSSFile, nil), "want css/workspace.css and"},
		"no htmx":            {with(HTMXFile, nil), "want css/workspace.css and"},
		"unreadable listing": {brokenFS{MapFS: good, readDir: true}, "permission denied"},
		"unreadable file":    {brokenFS{MapFS: good, readFile: true}, "permission denied"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadAssets(tc.fsys); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want an error with %q", err, tc.want)
			}
		})
	}
	a, err := loadAssets(good)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^/static/css/workspace\.[0-9a-f]{12}\.css$`).MatchString(a.CSS) {
		t.Errorf("stylesheet path: got %q, want it content-hashed", a.CSS)
	}
}

// TestOperationLabels: the metrics and logs name a page by its route
// pattern, never its path (ADR-0043).
func TestOperationLabels(t *testing.T) {
	s, _ := testServer(t)
	m := telemetry.NewMetrics()
	h := telemetry.Middleware(slog.New(slog.DiscardHandler), m, noop.NewTracerProvider())(s.Handler())
	for _, path := range []string{"/health", "/no/such/page", "/", s.Assets.CSS} {
		get(t, h, path, "")
	}
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, want := range []string{`operation="GET /health"`, `operation="/"`, `operation="GET /{$}"`, `operation="GET /static/"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("metrics: no %s", want)
		}
	}
	if strings.Contains(rec.Body.String(), "/no/such/page") {
		t.Error("metrics: a path as a label")
	}
}

// TestAPIClient: the typed client sends nothing without the person's
// token (the call with one is in the end-to-end test, against the API).
func TestAPIClient(t *testing.T) {
	if _, err := NewAPI("http://[::1", http.DefaultClient); err == nil || !strings.Contains(err.Error(), "web: API client for") {
		t.Errorf("bad URL: got %v", err)
	}
	c, err := NewAPI("http://127.0.0.1:1", http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListLegalEntities(context.Background(), gen.ListLegalEntitiesParams{}); !errors.Is(err, ErrNoSession) {
		t.Errorf("without a token: got %v, want ErrNoSession before any request", err)
	}
	if got, err := (personToken{}).Oidc(WithToken(context.Background(), "t"), gen.ListLegalEntitiesOperation); err != nil || got.Token != "t" {
		t.Errorf("with a token: got %v, %v", got, err)
	}
}

func TestNew(t *testing.T) {
	api, err := url.Parse("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	s, err := New(Config{APIURL: api, PseudoLocales: true}, buildinfo.Info{Version: "v1"}, log)
	if err != nil {
		t.Fatal(err)
	}
	if s.API == nil || s.Catalog.Match("en-XA").Tag != i18n.Long || s.Assets.Script == "" || s.Build.Version != "v1" {
		t.Errorf("server: got %+v", s)
	}
	refused := errors.New("refused")
	for name, tc := range map[string]struct {
		cfg     Config
		catalog func(bool) (*i18n.Catalog, error)
		assets  func() (*Assets, error)
		want    string
	}{
		"catalog": {Config{APIURL: api}, func(bool) (*i18n.Catalog, error) { return nil, refused }, LoadAssets, "refused"},
		"assets":  {Config{APIURL: api}, i18n.Load, func() (*Assets, error) { return nil, refused }, "refused"},
		"API":     {Config{APIURL: &url.URL{Scheme: "http", Host: "[::1"}}, i18n.Load, LoadAssets, "web: API client for"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := newServer(tc.cfg, buildinfo.Info{}, log, tc.catalog, tc.assets); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want an error with %q", err, tc.want)
			}
		})
	}
}

// TestAPIDial: with a dial address the client connects there, whatever
// the URL's host, which stays the request's.
func TestAPIDial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(r.Host)) }))
	t.Cleanup(srv.Close)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://api.example.invalid/v1/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := apiClient(srv.Listener.Addr().String()).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }() // the body is read below; a close error changes nothing asserted
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "api.example.invalid" {
		t.Errorf("Host at the dialled server: got %q, want the URL's host", body)
	}
}
