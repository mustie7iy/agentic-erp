// Package web is erp-web, the workspace for finance users (ADR-0052): a
// backend-for-frontend that renders pages on the server with templ
// (internal/web/ui) and calls the public API with the typed ogen client
// (api/gen) as the signed-in person, never as itself (§7). It holds no
// business logic and no business data: its own store
// (internal/web/session) keeps who signed in, with their tokens sealed, in
// the directory database (§6). Sign-in is the authorization code flow at
// the tenant's provider (signin.go, oidc.go); every API call carries the
// person's own access token.
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
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/buildinfo"
	"github.com/mustie7iy/agentic-erp/internal/platform/secrets"
	"github.com/mustie7iy/agentic-erp/internal/platform/telemetry"
	"github.com/mustie7iy/agentic-erp/internal/platform/tlsconf"
	"github.com/mustie7iy/agentic-erp/internal/platform/worker"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/session"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// Server serves the workspace's pages.
type Server struct {
	Catalog *i18n.Catalog
	Assets  *Assets
	Build   buildinfo.Info
	Log     *slog.Logger
	// API is the public API's typed client, called with the signed-in
	// person's token (WithToken, put on each request by signedIn).
	API *gen.Client
	// Store keeps the sessions, Clients finds the web clients and OIDC
	// signs people in at their providers; a nil Store: no one can sign in
	// (development without a directory).
	Store   *session.Store
	Clients *session.Clients
	OIDC    *OIDC
	Now     func() time.Time // nil: the wall clock

	nameCache nameCache // principals' names per session (ADR-0052 G3)
}

// New is the workspace for cfg: its catalog (the pseudo-locales outside
// production), its static files and the API's client, which reaches the
// API within the router's own limits.
func New(ctx context.Context, cfg Config, build buildinfo.Info, log *slog.Logger) (*Server, error) {
	s, err := newServer(cfg, build, log, i18n.Load, LoadAssets)
	if err != nil {
		return nil, err
	}
	return s, s.signIn(ctx, cfg)
}

// signIn opens the session store and makes the OIDC client, with a
// directory; without one (development) no one can sign in.
func (s *Server) signIn(ctx context.Context, cfg Config) error {
	if cfg.DirectoryURL == "" {
		return nil
	}
	sealer, err := session.NewSealer(cfg.SessionSecrets...)
	if err != nil {
		return err
	}
	if s.Store, err = session.Open(ctx, cfg.DirectoryURL, cfg.DirectoryPassword, sealer); err != nil {
		return err
	}
	public := strings.TrimSuffix(cfg.PublicURL.String(), "/")
	s.Clients = &session.Clients{Store: s.Store}
	s.OIDC = &OIDC{Providers: &authn.Providers{Production: cfg.Environment == string(authn.Production)},
		Credentials: secrets.Local{}, Prefix: cfg.CredentialPrefix, Callback: public + "/callback", SignedOut: public + "/signed-out"}
	return nil
}

// Close releases the session store's connections.
func (s *Server) Close() {
	if s.Store != nil {
		s.Store.Close()
	}
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
	if s.API, err = NewAPI(cfg.APIURL.String(), apiClient(cfg.APIDial, nil, cfg.Identity)); err != nil {
		return nil, err
	}
	return s, nil
}

// apiClient reaches the API, at dial when it is set: in the cluster the
// router's Service, reached directly rather than through the internet and
// its load balancer, still verified as the API's public name; trust is
// the system's roots unless tlsConfig (tests) says otherwise. With an
// identity, each connection presents erp-web's SPIFFE certificate as read
// last, so the router believes the person's address each call forwards.
func apiClient(dial string, tlsConfig *tls.Config, identity *tlsconf.Store) *http.Client {
	if identity != nil {
		if tlsConfig == nil {
			tlsConfig = &tls.Config{MinVersion: tlsconf.External}
		}
		tlsConfig.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			cert, err := identity.Certificate()
			return &cert, err // the handshake reports it; a store read has the certificate LoadConfig checked
		}
	}
	t := &http.Transport{TLSClientConfig: tlsConfig, ForceAttemptHTTP2: true, MaxIdleConns: 100, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second}
	if dial != "" {
		dialer := &net.Dialer{Timeout: 10 * time.Second}
		t.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, dial)
		}
	}
	// One API call, under the router's 60 s write timeout.
	return &http.Client{Timeout: 30 * time.Second, Transport: forwardAddress{next: t}}
}

// Handler is the workspace: its pages, its static files, and a page for
// every other address, each response with the security headers. No route
// changes state on GET, HEAD or OPTIONS, which CrossOriginProtection lets
// through from anywhere (ADR-0052 §5), but for the sign-in's own two: the
// start (a sign-in's cookies, nothing a person owns) and the callback,
// which the provider's redirect reaches as a GET and which the sign-in's
// state binds to the browser that started it (RFC 6749 §10.12).
func (s *Server) Handler() http.Handler {
	cop := http.NewCrossOriginProtection()
	cop.SetDenyHandler(http.HandlerFunc(s.refused))
	return secure(s.localized(cop.Handler(s.mux())))
}

// mux routes the workspace's pages; Screens lists the API operations each
// calls.
func (s *Server) mux() *http.ServeMux {
	mux := http.NewServeMux()
	route := func(pattern string, h http.HandlerFunc) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			telemetry.SetOperation(r.Context(), pattern)
			h(w, r)
		})
	}
	route("GET /{$}", s.signedIn(s.home))
	route("GET /login", s.loginPage)
	route("POST /login", s.loginForm)
	route("GET /login/{code}", s.login)
	route("GET /callback", s.callback)
	route("POST /logout", s.signedIn(s.csrf(s.logout)))
	route("GET "+ui.ApprovalsPath, s.signedIn(s.inbox))
	route("GET "+ui.ApprovalsPath+"/{id}", s.signedIn(s.request))
	for verb := range decideOps {
		route("POST "+ui.ApprovalsPath+"/{id}/"+verb, s.signedIn(s.csrf(s.decide(verb))))
	}
	route("GET "+ui.InvoicesPath, s.signedIn(s.invoices))
	route("GET "+ui.InvoicesPath+"/{id}", s.signedIn(s.invoice))
	route("GET "+ui.InvoicesPath+"/{id}/order", s.signedIn(s.order))
	route("GET "+ui.InvoicesPath+"/{id}/document", s.signedIn(s.document))
	for verb := range actionOps {
		route("POST "+ui.InvoicesPath+"/{id}/"+verb, s.signedIn(s.csrf(s.act(verb))))
	}
	route("POST "+ui.InvoicesPath+"/{id}/recommendations/{rec}/decide", s.signedIn(s.csrf(s.decideRecommendation)))
	route("GET "+ui.BankPath, s.signedIn(s.statements))
	route("POST "+ui.UploadPath, s.signedIn(s.csrfUpload(s.importStatement)))
	route("GET "+ui.UploadPath+"/{id}", s.signedIn(s.statement))
	for verb := range lineOps {
		route("POST "+ui.UploadPath+"/{id}/"+verb, s.signedIn(s.csrf(s.decideLine(verb))))
	}
	route("GET "+ui.BankPath+"/house-banks/{id}/reconciliation", s.signedIn(s.reconciliation))
	s.paymentRoutes(route)
	s.closeRoutes(route)
	s.journalRoutes(route)
	s.reportRoutes(route)
	route("GET /signed-out", s.signedOut)
	route("GET /health", func(w http.ResponseWriter, r *http.Request) {
		st := ui.Status{Version: s.Build.Version, Revision: s.Build.Revision}
		s.render(w, r, http.StatusOK, ui.StatusPage(s.shell(i18n.StatusTitle, "/health"), st))
	})
	route("GET /static/", s.Assets.ServeHTTP)
	route("/", s.anyone(s.notFound))
	return mux
}

// paymentRoutes routes the payment runs' pages (T73f).
func (s *Server) paymentRoutes(route func(string, http.HandlerFunc)) {
	route("GET "+ui.PaymentsPath, s.signedIn(s.runs))
	route("POST "+ui.ProposePath, s.signedIn(s.csrf(s.propose)))
	route("GET "+ui.PaymentsPath+"/{id}", s.signedIn(s.run))
	route("GET "+ui.PaymentsPath+"/{id}/progress", s.signedIn(s.progress))
	route("GET "+ui.PaymentsPath+"/{id}/files", s.signedIn(s.files))
	route("POST "+ui.PaymentsPath+"/{id}/cancel", s.signedIn(s.csrf(s.cancelRunOperation)))
	for verb := range runOps {
		route("POST "+ui.PaymentsPath+"/{id}/"+verb, s.signedIn(s.csrf(s.decideRun(verb))))
	}
}

// closeRoutes routes the period close's pages (T73i).
func (s *Server) closeRoutes(route func(string, http.HandlerFunc)) {
	route("GET "+ui.ClosePath, s.signedIn(s.periods))
	route("GET "+ui.PeriodsBase+"/{id}", s.signedIn(s.period))
	for verb := range periodOps {
		route("POST "+ui.PeriodsBase+"/{id}/"+verb, s.signedIn(s.csrf(s.decidePeriod(verb))))
	}
	route("GET "+ui.ChecksPath, s.signedIn(s.checks))
	route("GET "+ui.RevaluationPath, s.signedIn(s.revaluation))
	route("POST "+ui.PreviewPath, s.signedIn(s.csrf(s.previewRevaluation)))
	route("POST "+ui.RevaluePostPath, s.signedIn(s.csrf(s.postRevaluation)))
	route("GET "+ui.RevaluationPath+"/{id}", s.signedIn(s.revaluationRun))
}

// journalRoutes routes the manual journal entries' pages (T73h).
func (s *Server) journalRoutes(route func(string, http.HandlerFunc)) {
	route("GET "+ui.JournalPath, s.signedIn(s.parkedEntries))
	route("GET "+ui.NewEntryPath, s.signedIn(s.newEntry))
	route("POST "+ui.NewEntryPath, s.signedIn(s.csrf(s.parkEntry)))
	route("POST "+ui.NewEntryFile, s.signedIn(s.csrfUpload(s.parkFile)))
	route("GET "+ui.ParkedBase+"/{id}", s.signedIn(s.parkedEntry))
	route("GET "+ui.ParkedBase+"/{id}/edit", s.signedIn(s.editEntry))
	route("POST "+ui.ParkedBase+"/{id}/edit", s.signedIn(s.csrf(s.saveEntry)))
	route("POST "+ui.ParkedBase+"/{id}/lines", s.signedIn(s.csrfUpload(s.uploadLines)))
	for verb := range parkedOps {
		route("POST "+ui.ParkedBase+"/{id}/"+verb, s.signedIn(s.csrf(s.decideParked(verb))))
	}
	route("GET "+ui.EntriesPath, s.signedIn(s.journalEntries))
	route("GET "+ui.EntriesPath+"/{id}", s.signedIn(s.journalEntry))
	route("POST "+ui.EntriesPath+"/{id}/reverse", s.signedIn(s.csrf(s.reverseEntry)))
}

// reportRoutes routes the reports' pages and downloads (T73j).
func (s *Server) reportRoutes(route func(string, http.HandlerFunc)) {
	route("GET "+ui.ReportsPath, s.signedIn(s.reports))
	route("GET "+ui.TrialBalancePath, s.signedIn(s.trialBalance))
	route("GET "+ui.TrialBalancePath+ui.CSVSuffix, s.signedIn(s.trialBalanceCSV))
	route("GET "+ui.OpenItemsPath, s.signedIn(s.openItems))
	route("GET "+ui.OpenItemsPath+ui.CSVSuffix, s.signedIn(s.openItemsCSV))
	route("GET "+ui.GRIRPath, s.signedIn(s.grirReport))
	route("GET "+ui.GRIRPath+ui.CSVSuffix, s.signedIn(s.grirCSV))
	route("GET "+ui.LedgerReconPath, s.signedIn(s.ledgerRecon))
	route("GET "+ui.ReconEntriesPath, s.signedIn(s.reconEntries))
	route("GET "+ui.AuditPath, s.signedIn(s.audit))
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

// SweepInterval is how often ended sessions nobody came back to are
// deleted (ADR-0052 §6).
const SweepInterval = time.Hour

// RunSweeper deletes ended sessions every SweepInterval until ctx ends;
// without a store there is nothing to sweep.
func (s *Server) RunSweeper(ctx context.Context) {
	if s.Store == nil {
		return
	}
	worker.Loop(ctx, SweepInterval, SweepInterval, s.sweep, s.Log, "web: the session sweep failed")
}

// sweep is one round of the sweeper.
func (s *Server) sweep(ctx context.Context) (bool, error) {
	n, err := s.Store.Sweep(ctx)
	if n > 0 {
		s.Log.InfoContext(ctx, "web: ended sessions deleted", "sessions", n)
	}
	return false, err
}
