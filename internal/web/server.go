// Package web is erp-web, the workspace for finance users (ADR-0052): a
// backend-for-frontend that renders pages on the server with templ
// (internal/web/ui) and calls the public API with the typed ogen client
// (api/gen) as the signed-in person, never as itself (§7). It holds no
// business logic and no business data; sign-in and its sealed sessions
// arrive with T73b.
//
// Every response carries a strict Content Security Policy with a fresh
// nonce, HSTS and the other headers of ADR-0052 §8; the only script is the
// vendored htmx, pinned by its SHA-256; every string goes through the
// message catalog (internal/web/i18n). internal/layout holds the package
// to the platform packages without database or tenant scope: no context,
// unit of work or database package may be imported here.
package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/a-h/templ"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/buildinfo"
	"github.com/mustie7iy/agentic-erp/internal/platform/telemetry"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// Server serves the workspace's pages.
type Server struct {
	Catalog *i18n.Catalog
	Assets  *Assets
	Build   buildinfo.Info
	Log     *slog.Logger
	// API is the public API's typed client. The jobs' screens call it with
	// the signed-in person's token (WithToken, T73b); nothing calls it yet.
	API *gen.Client
}

// New is the workspace for cfg: its catalog (the pseudo-locales outside
// production), its static files and the API's client, which reaches the
// API within the router's own limits.
func New(cfg Config, build buildinfo.Info, log *slog.Logger) (*Server, error) {
	return newServer(cfg, build, log, i18n.Load, LoadAssets)
}

func newServer(cfg Config, build buildinfo.Info, log *slog.Logger, catalog func(pseudo bool) (*i18n.Catalog, error), assets func() (*Assets, error)) (*Server, error) {
	s := &Server{Build: build, Log: log}
	var err error
	if s.Catalog, err = catalog(cfg.PseudoLocales); err != nil {
		return nil, err
	}
	if s.Assets, err = assets(); err != nil {
		return nil, err
	}
	if s.API, err = NewAPI(cfg.APIURL.String(), apiClient(cfg.APIDial, nil)); err != nil {
		return nil, err
	}
	return s, nil
}

// apiClient reaches the API, at dial when it is set: in the cluster the
// router's Service, reached directly rather than through the internet and
// its load balancer, still verified as the API's public name; trust is
// the system's roots unless tlsConfig (tests) says otherwise.
func apiClient(dial string, tlsConfig *tls.Config) *http.Client {
	t := &http.Transport{TLSClientConfig: tlsConfig, ForceAttemptHTTP2: true, MaxIdleConns: 100, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second}
	if dial != "" {
		dialer := &net.Dialer{Timeout: 10 * time.Second}
		t.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, dial)
		}
	}
	// One API call, under the router's 60 s write timeout.
	return &http.Client{Timeout: 30 * time.Second, Transport: t}
}

// Handler is the workspace: its pages, its static files, and a page for
// every other address, each response with the security headers.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	route := func(pattern string, h http.HandlerFunc) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			telemetry.SetOperation(r.Context(), pattern)
			h(w, r)
		})
	}
	route("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		s.render(w, r, http.StatusOK, ui.Home(s.shell(i18n.HomeTitle, "/")))
	})
	route("GET /health", func(w http.ResponseWriter, r *http.Request) {
		st := ui.Status{Version: s.Build.Version, Revision: s.Build.Revision}
		s.render(w, r, http.StatusOK, ui.StatusPage(s.shell(i18n.StatusTitle, "/health"), st))
	})
	route("GET /static/", s.Assets.ServeHTTP)
	route("/", func(w http.ResponseWriter, r *http.Request) {
		s.render(w, r, http.StatusNotFound, ui.NotFound(s.shell(i18n.NotFoundTitle, "")))
	})
	return secure(s.localized(mux))
}

func (s *Server) shell(title i18n.Key, current string) ui.Shell {
	return ui.Shell{Title: title, Current: current, CSS: s.Assets.CSS, Script: s.Assets.Script}
}

// localized gives the request the person's locale, from Accept-Language
// until the person's preference exists (ADR-0052 §9, G2).
func (s *Server) localized(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l := s.Catalog.Match(r.Header.Get("Accept-Language"))
		w.Header().Set("Content-Language", l.Tag.String())
		w.Header().Add("Vary", "Accept-Language")
		next.ServeHTTP(w, r.WithContext(i18n.With(r.Context(), l)))
	})
}

// render writes a page, rendered in full first so that a failure is a
// clean 500 rather than half a page. Pages are never stored: each holds
// what one person may see (ADR-0052 §6).
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page templ.Component) {
	var body bytes.Buffer
	if err := page.Render(r.Context(), &body); err != nil {
		s.Log.ErrorContext(r.Context(), "web: render failed", "error", err)
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(body.Bytes())
}

// Permissions the workspace never uses, denied to it and to any frame.
const permissionsPolicy = "accelerometer=(), camera=(), display-capture=(), geolocation=(), gyroscope=(), hid=(), " +
	"magnetometer=(), microphone=(), midi=(), payment=(), serial=(), usb=()"

// CSP is the Content Security Policy of a response with the given nonce
// (ADR-0052 §8): a script only with the response's nonce, even from this
// origin (so a planted <script src> of a same-origin URL does not run),
// styles and everything else from this origin only, no plugins, no base
// element, no framing by anyone, forms posted here only.
func CSP(nonce string) string {
	return "default-src 'self'; script-src 'nonce-" + nonce + "'; style-src 'self'; img-src 'self' data:; " +
		"connect-src 'self'; frame-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
}

// secure sets the security headers on every response and gives the
// request a fresh nonce, which templ puts on the page's script element.
func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nonce := rand.Text() // 128 random bits; crypto/rand does not fail
		h := w.Header()
		h.Set("Content-Security-Policy", CSP(nonce))
		// Two years, with every subdomain: the workspace is HTTPS only.
		h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", permissionsPolicy)
		next.ServeHTTP(w, r.WithContext(templ.WithNonce(r.Context(), nonce)))
	})
}
