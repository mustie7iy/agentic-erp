package web

import (
	"maps"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/tlsconf/tlstest"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLoadConfigDefaults(t *testing.T) {
	c, err := LoadConfig(env(map[string]string{"ERP_ENV": "development", "ERP_WEB_API_URL": "http://localhost:8081"}))
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ got, want any }{
		"addr":           {c.Addr, ":8082"},
		"admin addr":     {c.AdminAddr, ":9092"},
		"API":            {c.APIURL.String(), "http://localhost:8081"},
		"public URL":     {c.PublicURL == nil, true},
		"public TLS":     {c.Public == nil, true},
		"shutdown":       {c.ShutdownTimeout, 20 * time.Second},
		"pseudo-locales": {c.PseudoLocales, true},
		"proxy sources":  {len(c.ProxyFrom), 0},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("got %v, want %v", tc.got, tc.want)
			}
		})
	}
}

// TestLoadConfigProduction: production takes https addresses and the
// public certificate, and turns the pseudo-locales off.
func TestLoadConfigProduction(t *testing.T) {
	ca, dir := tlstest.NewCA(t), t.TempDir()
	public := ca.Issue(t, tlstest.Options{DNS: []string{"app.example.com"}})
	c, err := LoadConfig(env(map[string]string{
		"ERP_ENV": "production", "ERP_WEB_PUBLIC_URL": "https://app.example.com/", "ERP_WEB_API_URL": "https://api.example.com",
		"ERP_WEB_PUBLIC_CERT": "file:" + tlstest.WriteFile(t, dir, "tls.crt", public.CertPEM),
		"ERP_WEB_PUBLIC_KEY":  "file:" + tlstest.WriteFile(t, dir, "tls.key", public.KeyPEM),
		"ERP_WEB_PROXY_FROM":  "10.16.0.0/24,10.16.1.0/24", "ERP_WEB_API_DIAL": "erp-router.erp-global.svc:8081", "ERP_WEB_ADDR": ":8443", "ERP_LOG_LEVEL": "warn", "ERP_SHUTDOWN_TIMEOUT": "5s",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.PseudoLocales {
		t.Error("pseudo-locales: on in production")
	}
	if c.Public == nil || !c.Public.HasCertificate() {
		t.Error("public certificate: not loaded")
	}
	if want := []netip.Prefix{netip.MustParsePrefix("10.16.0.0/24"), netip.MustParsePrefix("10.16.1.0/24")}; !slices.Equal(c.ProxyFrom, want) {
		t.Errorf("proxy sources: got %v, want %v", c.ProxyFrom, want)
	}
	if c.Addr != ":8443" || c.APIDial != "erp-router.erp-global.svc:8081" || c.ShutdownTimeout != 5*time.Second || c.LogLevel.String() != "WARN" || c.PublicURL.Host != "app.example.com" {
		t.Errorf("settings: got %+v", c)
	}
}

func TestLoadConfigRefuses(t *testing.T) {
	ca, dir := tlstest.NewCA(t), t.TempDir()
	public := ca.Issue(t, tlstest.Options{DNS: []string{"app.example.com"}})
	prod := map[string]string{
		"ERP_ENV": "production", "ERP_WEB_PUBLIC_URL": "https://app.example.com", "ERP_WEB_API_URL": "https://api.example.com",
		"ERP_WEB_PUBLIC_CERT": "file:" + tlstest.WriteFile(t, dir, "tls.crt", public.CertPEM),
		"ERP_WEB_PUBLIC_KEY":  "file:" + tlstest.WriteFile(t, dir, "tls.key", public.KeyPEM),
	}
	with := func(set map[string]string) map[string]string {
		m := maps.Clone(prod)
		maps.Copy(m, set)
		return m
	}
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"no environment":         {with(map[string]string{"ERP_ENV": ""}), "ERP_ENV is required"},
		"no public URL":          {with(map[string]string{"ERP_WEB_PUBLIC_URL": ""}), "ERP_WEB_PUBLIC_URL is required"},
		"plain public URL":       {with(map[string]string{"ERP_WEB_PUBLIC_URL": "http://app.example.com"}), "ERP_WEB_PUBLIC_URL must be https with ERP_ENV=production"},
		"public URL with a path": {with(map[string]string{"ERP_WEB_PUBLIC_URL": "https://app.example.com/x"}), "ERP_WEB_PUBLIC_URL must be an absolute http(s) URL"},
		"public URL with a user": {with(map[string]string{"ERP_WEB_PUBLIC_URL": "https://u:p@app.example.com"}), "ERP_WEB_PUBLIC_URL must be an absolute http(s) URL"},
		"no API":                 {with(map[string]string{"ERP_WEB_API_URL": ""}), "ERP_WEB_API_URL is required"},
		"plain API":              {with(map[string]string{"ERP_WEB_API_URL": "http://api.example.com"}), "ERP_WEB_API_URL must be https"},
		"API not a URL":          {with(map[string]string{"ERP_WEB_API_URL": "api.example.com"}), "ERP_WEB_API_URL must be an absolute http(s) URL"},
		"API unparsable":         {with(map[string]string{"ERP_WEB_API_URL": "https://[::1"}), "ERP_WEB_API_URL must be an absolute http(s) URL"},
		"no certificate":         {with(map[string]string{"ERP_WEB_PUBLIC_CERT": "", "ERP_WEB_PUBLIC_KEY": ""}), "ERP_WEB_PUBLIC_CERT and ERP_WEB_PUBLIC_KEY are required with ERP_ENV=production"},
		"certificate alone":      {with(map[string]string{"ERP_WEB_PUBLIC_KEY": ""}), "ERP_WEB_PUBLIC_CERT, ERP_WEB_PUBLIC_KEY: "},
		"bad shutdown":           {with(map[string]string{"ERP_SHUTDOWN_TIMEOUT": "-1s"}), "ERP_SHUTDOWN_TIMEOUT must be a positive duration"},
		"bad log level":          {with(map[string]string{"ERP_LOG_LEVEL": "loud"}), "ERP_LOG_LEVEL must be"},
		"bad API dial":           {with(map[string]string{"ERP_WEB_API_DIAL": "erp-router"}), "ERP_WEB_API_DIAL must be host:port"},
		"bad proxy sources":      {with(map[string]string{"ERP_WEB_PROXY_FROM": "10.0.0.0/33"}), "ERP_WEB_PROXY_FROM must be a comma-separated list of CIDRs"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadConfig(env(tc.env)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want an error with %q", err, tc.want)
			}
		})
	}
}
