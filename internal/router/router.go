// Package router is the global router of 05-scalability §5.4 (ADR-0022):
// the one entry point of the public API across cells. It verifies the
// bearer token, reads the tenant claim, finds the tenant's cell in the
// directory and forwards the request there unchanged; the cell verifies the
// token again and serves it. It holds no state but a cache of the directory.
package router

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/directory"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/secrets"
	"github.com/mustie7iy/agentic-erp/internal/platform/tlsconf"
)

// Config is the router's configuration, from the environment.
type Config struct {
	Addr            string            // ERP_ROUTER_ADDR (default :8081)
	AdminAddr       string            // ERP_ROUTER_ADMIN_ADDR (default :9091): liveness, metrics, and the global control plane with operator issuers
	DirectoryURL    string            // ERP_DIRECTORY_URL (required)
	Environment     string            // ERP_ENV (required, no default): development or production, as for the cells (internal/platform/config)
	Issuers         []authn.Issuer    // ERP_OIDC_ISSUERS, the same list the cells trust (optional; tenants' registered providers resolve through the directory)
	OperatorIssuers []authn.Issuer    // ERP_OPERATOR_ISSUERS, for the global control plane on the admin listener; empty leaves it off
	PageTokenKey    []byte            // ERP_PAGE_TOKEN_KEY, base64, at least 32 bytes (required with ERP_OPERATOR_ISSUERS)
	CacheTTL        time.Duration     // ERP_DIRECTORY_CACHE, how long a lookup is remembered (default 30s)
	LogLevel        slog.Level        // ERP_LOG_LEVEL (default info)
	ShutdownTimeout time.Duration     // ERP_SHUTDOWN_TIMEOUT (default 20s)
	Static          []authn.Static    // trusted issuers with known keys (tests)
	Transport       http.RoundTripper // nil: the default transport, or Identity's to the cells
	Identity        *tlsconf.Store    // ERP_TLS_CA, ERP_TLS_CERT, ERP_TLS_KEY (env: or file:): the router's SPIFFE identity, to the cells and on the admin listener; nil without (development)
	Public          *tlsconf.Store    // ERP_ROUTER_PUBLIC_CERT, ERP_ROUTER_PUBLIC_KEY (env: or file:): the public listener's certificate, a public CA's; nil: plaintext (development)
	TrustDomain     string            // ERP_TRUST_DOMAIN: the cells the router reaches are spiffe://<domain>/<cell>/erp-server (ADR-0033)
}

// LoadConfig reads and validates the configuration.
func LoadConfig(getenv func(string) string) (Config, error) {
	var errs []error
	get := func(name, def string) string {
		if v := getenv(name); v != "" {
			return v
		}
		return def
	}
	c := Config{Addr: get("ERP_ROUTER_ADDR", ":8081"), AdminAddr: get("ERP_ROUTER_ADMIN_ADDR", ":9091"), DirectoryURL: getenv("ERP_DIRECTORY_URL"), Environment: getenv("ERP_ENV")}
	if err := config.CheckEnvironment(c.Environment); err != nil {
		errs = append(errs, err)
	}
	if c.DirectoryURL == "" {
		errs = append(errs, errors.New("ERP_DIRECTORY_URL is required: the directory database"))
	} else if c.Environment == config.Production {
		if err := config.CheckDatabaseTLS("ERP_DIRECTORY_URL", c.DirectoryURL); err != nil {
			errs = append(errs, err)
		}
	}
	if raw := getenv("ERP_OIDC_ISSUERS"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &c.Issuers); err != nil || len(c.Issuers) == 0 {
			errs = append(errs, errors.New(`ERP_OIDC_ISSUERS must be a JSON list of {"issuer", "audience", "tenant_claim"} when set`))
		}
	}
	if c.Environment == config.Production && len(c.Issuers) > 0 {
		errs = append(errs, errors.New("ERP_OIDC_ISSUERS must not be set with ERP_ENV=production: it trusts its issuers for every tenant; tenants' registered identity providers resolve through the directory (ADR-0031)"))
	}
	if raw := getenv("ERP_OPERATOR_ISSUERS"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &c.OperatorIssuers); err != nil || len(c.OperatorIssuers) == 0 {
			errs = append(errs, errors.New(`ERP_OPERATOR_ISSUERS must be a JSON list of {"issuer", "audience"}`))
		}
		var err error
		if c.PageTokenKey, err = base64.StdEncoding.DecodeString(getenv("ERP_PAGE_TOKEN_KEY")); err != nil || len(c.PageTokenKey) < 32 {
			errs = append(errs, errors.New("ERP_OPERATOR_ISSUERS needs ERP_PAGE_TOKEN_KEY, base64 of at least 32 random bytes, the same on every router"))
		}
	}
	var err error
	if c.CacheTTL, err = time.ParseDuration(get("ERP_DIRECTORY_CACHE", "30s")); err != nil || c.CacheTTL <= 0 {
		errs = append(errs, errors.New("ERP_DIRECTORY_CACHE must be a positive duration such as 30s"))
	}
	if c.ShutdownTimeout, err = time.ParseDuration(get("ERP_SHUTDOWN_TIMEOUT", "20s")); err != nil || c.ShutdownTimeout <= 0 {
		errs = append(errs, errors.New("ERP_SHUTDOWN_TIMEOUT must be a positive duration such as 20s"))
	}
	if err := c.LogLevel.UnmarshalText([]byte(get("ERP_LOG_LEVEL", "info"))); err != nil {
		errs = append(errs, errors.New("ERP_LOG_LEVEL must be debug, info, warn or error"))
	}
	errs = append(errs, c.loadTLS(getenv)...)
	return c, errors.Join(errs...)
}

// loadTLS reads the router's TLS material and trust domain (ADR-0033):
// references to mounted files or the environment, read now and again
// while the router runs. Production requires all of it.
func (c *Config) loadTLS(getenv func(string) string) []error {
	var errs []error
	if c.TrustDomain = getenv("ERP_TRUST_DOMAIN"); !config.ValidTrustDomain(c.TrustDomain) {
		errs = append(errs, config.ErrTrustDomain)
	}
	local := secrets.Local{Getenv: getenv}
	read := func(names string, src tlsconf.Source) *tlsconf.Store {
		if !src.Enabled() {
			return nil
		}
		s := &tlsconf.Store{Source: src, Resolver: local}
		if err := s.Refresh(context.Background()); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", names, err))
			return nil
		}
		return s
	}
	c.Identity = read("ERP_TLS_CA, ERP_TLS_CERT, ERP_TLS_KEY", tlsconf.Source{CA: getenv("ERP_TLS_CA"), Cert: getenv("ERP_TLS_CERT"), Key: getenv("ERP_TLS_KEY")})
	c.Public = read("ERP_ROUTER_PUBLIC_CERT, ERP_ROUTER_PUBLIC_KEY", tlsconf.Source{Cert: getenv("ERP_ROUTER_PUBLIC_CERT"), Key: getenv("ERP_ROUTER_PUBLIC_KEY")})
	if c.Identity != nil && c.TrustDomain == "" {
		errs = append(errs, errors.New("ERP_TLS_* needs ERP_TRUST_DOMAIN: with them the router reaches cells over TLS only, accepting only cells' identities in the trust domain (ADR-0033)"))
	}
	if c.Environment != config.Production {
		return errs
	}
	if c.Identity == nil || !c.Identity.HasCertificate() {
		errs = append(errs, errors.New("ERP_TLS_CERT and ERP_TLS_KEY are required with ERP_ENV=production: the router reaches the cells as its SPIFFE identity (ADR-0033)"))
	}
	if c.Public == nil {
		errs = append(errs, errors.New("ERP_ROUTER_PUBLIC_CERT and ERP_ROUTER_PUBLIC_KEY are required with ERP_ENV=production: the public listener serves TLS only (ADR-0033)"))
	}
	if c.TrustDomain == "" {
		errs = append(errs, errors.New("ERP_TRUST_DOMAIN is required with ERP_ENV=production: the cells are reached by their SPIFFE identities (ADR-0033)"))
	}
	return errs
}

// Router forwards each request to its tenant's cell.
type Router struct {
	Verifier  *authn.Verifier
	Directory directory.Store // a Cache in production
	Log       *slog.Logger
	Transport http.RoundTripper // nil: http.DefaultTransport
	// RequireTLS refuses a cell whose API URL is not https (production,
	// ADR-0033).
	RequireTLS bool
	// PeerOf is the SPIFFE ID the tenant's cell must present; nil: the
	// transport's own rule.
	PeerOf func(cell string) string
}

// ServeHTTP implements http.Handler. /livez answers without a token; every
// other request needs a tenant token and a routable directory entry.
func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/livez" {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
		return
	}
	target, err := rt.route(r)
	if err != nil {
		problem.Write(w, r, problem.From(err))
		return
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target.url)
			pr.Out.Host = target.url.Host
			pr.SetXForwarded()
			forwardClientCert(pr)
		},
		Transport: rt.Transport,
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Set("X-Erp-Cell", target.cell)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			rt.Log.ErrorContext(r.Context(), "cell unreachable", "cell", target.cell, "error", err)
			problem.Write(w, r, problem.New(http.StatusBadGateway, "platform.cell_unreachable", "Cell unreachable",
				"The tenant's cell did not answer; try again."))
		},
	}
	if rt.PeerOf != nil {
		r = r.WithContext(tlsconf.ExpectPeer(r.Context(), rt.PeerOf(target.cell)))
	}
	proxy.ServeHTTP(w, r)
}

// forwardClientCert tells the cell the thumbprint of the client
// certificate the request was made with (the TLS handshake proved the
// client holds its key), and never one the client sent itself: the cell
// checks a certificate-bound token against it (RFC 8705, ADR-0033).
func forwardClientCert(pr *httputil.ProxyRequest) {
	pr.Out.Header.Del(tlsconf.ClientCertHeader)
	if cs := pr.In.TLS; cs != nil && len(cs.PeerCertificates) > 0 {
		pr.Out.Header.Set(tlsconf.ClientCertHeader, tlsconf.Thumbprint(cs.PeerCertificates[0]))
	}
}

type target struct {
	url  *url.URL
	cell string
}

// route verifies the token and resolves the tenant's cell.
func (rt *Router) route(r *http.Request) (target, error) {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || raw == "" {
		return target{}, problem.Unauthorized("A bearer token is required.")
	}
	claims, err := rt.Verifier.Verify(r.Context(), raw)
	if err != nil {
		return target{}, err
	}
	e, err := rt.Directory.Lookup(r.Context(), claims.TenantID)
	if errors.Is(err, directory.ErrUnknown) {
		return target{}, problem.NotFound("No cell serves this tenant.")
	}
	if err != nil {
		rt.Log.ErrorContext(r.Context(), "directory lookup failed", "tenant_id", claims.TenantID, "error", err)
		return target{}, problem.Unavailable("The tenant directory did not answer; try again.", 5)
	}
	switch e.Status {
	case directory.Active:
	case directory.ReadOnly:
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			return target{}, problem.Unavailable("The tenant is being moved between cells; writes resume shortly.", 60)
		}
	default:
		return target{}, problem.Forbidden("tenancy.tenant_"+e.Status, fmt.Sprintf("The tenant is %s.", e.Status))
	}
	u, err := url.Parse(e.APIURL)
	if err != nil || u.Host == "" || (rt.RequireTLS && u.Scheme != "https") {
		rt.Log.ErrorContext(r.Context(), "directory entry has no usable API URL", "tenant_id", claims.TenantID, "cell", e.Cell, "api_url", e.APIURL)
		return target{}, problem.Unavailable("The tenant's cell has no usable address; try again later.", 60)
	}
	return target{url: u, cell: e.Cell}, nil
}
