package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/proxyproto"
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
	if err := config.CheckEnvironment(c.Environment); err != nil {
		errs = append(errs, err)
	}
	production := c.Environment == config.Production
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
	return nil
}
