package web

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/proxyproto"
	"github.com/mustie7iy/agentic-erp/internal/platform/rdsiam"
	"github.com/mustie7iy/agentic-erp/internal/platform/secrets"
	"github.com/mustie7iy/agentic-erp/internal/platform/tlsconf"
)

// Config is erp-web's configuration, from the environment.
type Config struct {
	Addr            string         // ERP_WEB_ADDR (default :8082): the workspace, behind its load balancer
	AdminAddr       string         // ERP_WEB_ADMIN_ADDR (default :9092): liveness, metrics and the build's version, for the kubelet alone
	Environment     string         // ERP_ENV (required, no default): development or production
	PublicURL       *url.URL       // ERP_WEB_PUBLIC_URL: the workspace's own address, https://app.<domain> (required in production)
	APIURL          *url.URL       // ERP_WEB_API_URL (required): the public API, the router's address (https in production)
	APIDial         string         // ERP_WEB_API_DIAL, host:port: where to connect for the API instead of the URL's host (the router's Service in the cluster); TLS still verifies the URL's host
	Public          *tlsconf.Store // ERP_WEB_PUBLIC_CERT, ERP_WEB_PUBLIC_KEY (env: or file:): a public CA's certificate; nil: plaintext (development)
	ProxyFrom       []netip.Prefix // ERP_WEB_PROXY_FROM, CIDRs: the load balancer's subnets, whose connections begin with a PROXY v2 header
	LogLevel        slog.Level     // ERP_LOG_LEVEL (default info)
	ShutdownTimeout time.Duration  // ERP_SHUTDOWN_TIMEOUT (default 20s)
	PseudoLocales   bool           // the pseudo-locales en-XA and ar-XB, offered outside production only
	// Identity is ERP_TLS_CERT and ERP_TLS_KEY (env: or file:): erp-web's
	// SPIFFE identity, spiffe://<domain>/global/erp-web, which it presents
	// to the router, and from which alone the router believes the
	// person's address erp-web forwards (Erp-Client-Address, ADR-0033);
	// required in production. nil: none (development), and the API's
	// audit trail records erp-web's own address.
	Identity *tlsconf.Store
	SignIn
}

// SignIn is sign-in's configuration (ADR-0052 §5, §11). Without a
// directory (development only) no one can sign in.
type SignIn struct {
	// DirectoryURL is ERP_WEB_DIRECTORY_URL: the global directory's
	// database, where sessions are kept (sslmode=verify-full in production).
	DirectoryURL string
	// DirectoryPassword is what the login presents for each new
	// connection in place of the URL's password: in production an RDS IAM
	// token (ERP_WEB_DIRECTORY_REGION, ERP_AWS_STS_ENDPOINT and erp-web's
	// IRSA role, rdsiam); in development ERP_WEB_DIRECTORY_PASSWORD, a
	// reference (env:, file:), read again each time; nil: the URL's.
	DirectoryPassword func(context.Context) (string, error)
	// SessionSecrets are ERP_WEB_SESSION_KEY, then
	// ERP_WEB_SESSION_KEY_PREVIOUS while a rotation runs (ADR-0028),
	// references to secrets of at least 32 bytes that seal sessions.
	SessionSecrets [][]byte
	// CredentialPrefix is ERP_WEB_CLIENT_CREDENTIALS: the reference prefix
	// the web clients' credentials must start with (file:/etc/erp/web-clients/).
	CredentialPrefix string
}

// LoadConfig reads and validates the configuration. In production a
// missing or unsafe setting fails startup (CLAUDE.md: no silent
// fallbacks).
func LoadConfig(getenv func(string) string) (Config, error) {
	var errs []error
	get := func(name, def string) string {
		if v := getenv(name); v != "" {
			return v
		}
		return def
	}
	c := Config{Addr: get("ERP_WEB_ADDR", ":8082"), AdminAddr: get("ERP_WEB_ADMIN_ADDR", ":9092"), Environment: getenv("ERP_ENV")}
	if err := authn.CheckEnvironment(c.Environment); err != nil {
		errs = append(errs, err)
	}
	production := c.Environment == string(authn.Production)
	c.PseudoLocales = !production
	var err error
	if c.PublicURL, err = address("ERP_WEB_PUBLIC_URL", getenv("ERP_WEB_PUBLIC_URL"), production, production); err != nil {
		errs = append(errs, err)
	}
	if c.APIURL, err = address("ERP_WEB_API_URL", getenv("ERP_WEB_API_URL"), true, production); err != nil {
		errs = append(errs, err)
	}
	if c.APIDial = getenv("ERP_WEB_API_DIAL"); c.APIDial != "" {
		if _, _, err := net.SplitHostPort(c.APIDial); err != nil {
			errs = append(errs, fmt.Errorf("ERP_WEB_API_DIAL must be host:port, not %q", c.APIDial))
		}
	}
	if c.ShutdownTimeout, err = time.ParseDuration(get("ERP_SHUTDOWN_TIMEOUT", "20s")); err != nil || c.ShutdownTimeout <= 0 {
		errs = append(errs, errors.New("ERP_SHUTDOWN_TIMEOUT must be a positive duration such as 20s"))
	}
	if err := c.LogLevel.UnmarshalText([]byte(get("ERP_LOG_LEVEL", "info"))); err != nil {
		errs = append(errs, errors.New("ERP_LOG_LEVEL must be debug, info, warn or error"))
	}
	if c.ProxyFrom, err = proxyproto.ParseFrom("ERP_WEB_PROXY_FROM", getenv("ERP_WEB_PROXY_FROM")); err != nil {
		errs = append(errs, err)
	}
	errs = append(errs, c.loadTLS(getenv, production)...)
	errs = append(errs, c.loadSignIn(getenv, production)...)
	return c, errors.Join(errs...)
}

// address parses an absolute http(s) URL setting with no path beyond
// "/", required when required and https when secure.
func address(name, raw string, required, secure bool) (*url.URL, error) {
	if raw == "" {
		if required {
			return nil, fmt.Errorf("%s is required", name)
		}
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, fmt.Errorf("%s must be an absolute http(s) URL with no path, query or credentials, not %q", name, raw)
	}
	if secure && u.Scheme != "https" {
		return nil, fmt.Errorf("%s must be https with ERP_ENV=production, not %q", name, raw)
	}
	return u, nil
}

// loadTLS reads the public listener's certificate: references to mounted
// files or the environment, read now and again while erp-web runs.
// Production requires it.
func (c *Config) loadTLS(getenv func(string) string, production bool) []error {
	src := tlsconf.Source{Cert: getenv("ERP_WEB_PUBLIC_CERT"), Key: getenv("ERP_WEB_PUBLIC_KEY")}
	if src.Enabled() {
		s := &tlsconf.Store{Source: src, Resolver: secrets.Local{Getenv: getenv}}
		if err := s.Refresh(context.Background()); err != nil {
			return []error{fmt.Errorf("ERP_WEB_PUBLIC_CERT, ERP_WEB_PUBLIC_KEY: %w", err)}
		}
		c.Public = s
	}
	if production && c.Public == nil {
		return []error{errors.New("ERP_WEB_PUBLIC_CERT and ERP_WEB_PUBLIC_KEY are required with ERP_ENV=production: the workspace serves TLS only (ADR-0052 §6)")}
	}
	return c.loadIdentity(getenv, production)
}

// webIdentity is the end of erp-web's SPIFFE ID, whatever the trust
// domain: the router believes the person's address from this identity
// alone.
const webIdentity = "/global/erp-web"

// loadIdentity reads erp-web's SPIFFE identity, which it presents to the
// router. Production requires it: without it the router would record
// erp-web's address for every person, a silent loss of the audit's
// addresses.
func (c *Config) loadIdentity(getenv func(string) string, production bool) []error {
	src := tlsconf.Source{Cert: getenv("ERP_TLS_CERT"), Key: getenv("ERP_TLS_KEY")}
	if !src.Enabled() {
		if production {
			return []error{errors.New("ERP_TLS_CERT and ERP_TLS_KEY are required with ERP_ENV=production: erp-web presents its SPIFFE identity to the router, which believes the person's address from it alone (ADR-0033)")}
		}
		return nil
	}
	s := &tlsconf.Store{Source: src, Resolver: secrets.Local{Getenv: getenv}}
	if err := s.Refresh(context.Background()); err != nil {
		return []error{fmt.Errorf("ERP_TLS_CERT, ERP_TLS_KEY: %w", err)}
	}
	cert, _ := s.Certificate() // Refresh read the certificate and key, both set, or failed above
	if id := spiffeID(cert.Leaf); !strings.HasPrefix(id, "spiffe://") || !strings.HasSuffix(id, webIdentity) {
		return []error{fmt.Errorf("ERP_TLS_CERT must name erp-web's SPIFFE ID, spiffe://<domain>%s, not %q", webIdentity, id)}
	}
	c.Identity = s
	return nil
}

// spiffeID is the certificate's first spiffe URI, or "".
func spiffeID(cert *x509.Certificate) string {
	for _, u := range cert.URIs {
		if u.Scheme == "spiffe" {
			return u.String()
		}
	}
	return ""
}

// loadSignIn reads sign-in's settings: all of them, or (outside
// production) none. Secrets are references, read now, so a missing one
// fails startup.
func (c *Config) loadSignIn(getenv func(string) string, production bool) []error {
	c.DirectoryURL = getenv("ERP_WEB_DIRECTORY_URL")
	if c.DirectoryURL == "" {
		if production {
			return []error{errors.New("ERP_WEB_DIRECTORY_URL is required with ERP_ENV=production: sessions are kept in the directory database (ADR-0052 §6)")}
		}
		return nil
	}
	var errs []error
	if u, err := url.Parse(c.DirectoryURL); err != nil || (production && u.Query().Get("sslmode") != "verify-full") {
		errs = append(errs, errors.New("ERP_WEB_DIRECTORY_URL must be a postgres URL with sslmode=verify-full with ERP_ENV=production"))
	}
	if c.PublicURL == nil {
		errs = append(errs, errors.New("ERP_WEB_PUBLIC_URL is required to sign in: the redirect URI is its /callback"))
	}
	ctx, local := context.Background(), secrets.Local{Getenv: getenv}
	// In production erp-web's login presents RDS IAM tokens alone, its
	// IRSA role's (ADR-0035 As built 2026-11-01); in development a
	// password reference, read again for each new connection, or the URL's.
	var err error
	switch ref := getenv("ERP_WEB_DIRECTORY_PASSWORD"); {
	case production:
		c.DirectoryPassword, err = rdsiam.Load(getenv, rdsiam.Settings{URLName: "ERP_WEB_DIRECTORY_URL", URL: c.DirectoryURL,
			RegionName: "ERP_WEB_DIRECTORY_REGION", PasswordName: "ERP_WEB_DIRECTORY_PASSWORD"})
	case ref != "":
		if c.DirectoryPassword, err = secrets.Rotating(ctx, local, ref); err != nil {
			err = fmt.Errorf("ERP_WEB_DIRECTORY_PASSWORD: %w", err)
		}
	}
	if err != nil {
		errs = append(errs, err)
	}
	for _, name := range []string{"ERP_WEB_SESSION_KEY", "ERP_WEB_SESSION_KEY_PREVIOUS"} {
		ref := getenv(name)
		if ref == "" && name != "ERP_WEB_SESSION_KEY" {
			continue
		}
		v, err := secrets.Value(ctx, local, ref)
		if err != nil || !secrets.IsRef(ref) || len(v) < 32 {
			errs = append(errs, fmt.Errorf("%s must reference (env: or file:) a secret of at least 32 bytes", name))
			continue
		}
		c.SessionSecrets = append(c.SessionSecrets, []byte(v))
	}
	if c.CredentialPrefix = getenv("ERP_WEB_CLIENT_CREDENTIALS"); !secrets.IsRef(c.CredentialPrefix) || !strings.HasSuffix(c.CredentialPrefix, "/") {
		errs = append(errs, errors.New("ERP_WEB_CLIENT_CREDENTIALS must be the reference prefix of the web clients' credentials, such as file:/etc/erp/web-clients/"))
	}
	return errs
}
