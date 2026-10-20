package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/analytics/extract"
	"github.com/mustie7iy/agentic-erp/internal/analytics/reader"
	"github.com/mustie7iy/agentic-erp/internal/platform/awscreds"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/tlsconf"
	"github.com/mustie7iy/agentic-erp/internal/platform/tlsconf/tlstest"
)

type fixture struct {
	env            map[string]string
	ca             *tlstest.CA
	server, router tls.Certificate
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	ca := tlstest.NewCA(t)
	id := ca.Issue(t, tlstest.Options{URIs: []string{tlsconf.ID("erp.test", "c1", "erp-analytics")}})
	f := &fixture{ca: ca, env: map[string]string{
		"ERP_ANALYTICS_ADDR":  "127.0.0.1:0",
		"ERP_ANALYTICS_CACHE": filepath.Join(dir, "cache"),
		"ERP_TRUST_DOMAIN":    "erp.test",
		"ERP_CELL":            "c1",
		"ERP_TLS_CA":          "file:" + tlstest.WriteFile(t, dir, "ca.crt", ca.PEM),
		"ERP_TLS_CERT":        "file:" + tlstest.WriteFile(t, dir, "tls.crt", id.CertPEM),
		"ERP_TLS_KEY":         "file:" + tlstest.WriteFile(t, dir, "tls.key", id.KeyPEM),
		"ERP_OBJECT_DIR":      filepath.Join(dir, "objects"),
	}}
	f.server = ca.Issue(t, tlstest.Options{URIs: []string{tlsconf.ID("erp.test", "c1", "erp-server")}}).Pair(t)
	f.router = ca.Issue(t, tlstest.Options{URIs: []string{tlsconf.ID("erp.test", "global", "erp-router")}}).Pair(t)
	return f
}

func (f *fixture) getenv(k string) string { return f.env[k] }

func TestConfigurationRefused(t *testing.T) {
	for name, edit := range map[string]func(map[string]string){
		"log level":    func(e map[string]string) { e["ERP_LOG_LEVEL"] = "loud" },
		"trust domain": func(e map[string]string) { delete(e, "ERP_TRUST_DOMAIN") },
		"cell":         func(e map[string]string) { e["ERP_CELL"] = "Not A Cell" },
		"no tls":       func(e map[string]string) { delete(e, "ERP_TLS_KEY") },
		"bad tls":      func(e map[string]string) { e["ERP_TLS_CERT"] = "file:/nonexistent" },
		"no store":     func(e map[string]string) { delete(e, "ERP_OBJECT_DIR") },
		"two stores":   func(e map[string]string) { e["ERP_OBJECT_S3_ENDPOINT"] = "https://s3.example" },
		"threads":      func(e map[string]string) { e["ERP_ANALYTICS_THREADS"] = "many" },
		"queries":      func(e map[string]string) { e["ERP_ANALYTICS_MAX_QUERIES"] = "0" },
		"memory":       func(e map[string]string) { e["ERP_ANALYTICS_MEMORY_LIMIT"] = "all of it" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			edit(f.env)
			var stderr bytes.Buffer
			if code := cli(context.Background(), f.getenv, &stderr, nil); code != 2 {
				t.Errorf("exit: got %d (%s), want 2", code, stderr.String())
			}
		})
	}
}

func TestDefaultsAndS3(t *testing.T) {
	f := newFixture(t)
	delete(f.env, "ERP_ANALYTICS_ADDR")
	delete(f.env, "ERP_ANALYTICS_CACHE")
	delete(f.env, "ERP_OBJECT_DIR")
	f.env["ERP_OBJECT_S3_ENDPOINT"], f.env["ERP_OBJECT_S3_BUCKET"] = "https://s3.example", "cell"
	s, err := load(context.Background(), f.getenv)
	if err != nil {
		t.Fatal(err)
	}
	if s3, ok := s.store.(blobstore.S3); !ok || s3.Region != "us-east-1" || s.addr != ":8443" || !filepath.IsAbs(s.cache) {
		t.Errorf("settings: got %+v, want S3 in us-east-1 on :8443 with an absolute cache", s)
	}
	if want := (reader.Limits{Threads: 2, MemoryLimit: "1GB", MaxQueries: 4}); s.limits != want {
		t.Errorf("limits: got %+v, want %+v", s.limits, want)
	}
	f.env["ERP_ANALYTICS_THREADS"], f.env["ERP_ANALYTICS_MAX_QUERIES"], f.env["ERP_ANALYTICS_MEMORY_LIMIT"] = "1", "8", "512MB"
	if s, err = load(context.Background(), f.getenv); err != nil {
		t.Fatal(err)
	}
	if want := (reader.Limits{Threads: 1, MemoryLimit: "512MB", MaxQueries: 8}); s.limits != want {
		t.Errorf("limits: got %+v, want %+v", s.limits, want)
	}
	f.env["ERP_AWS_STS_ENDPOINT"], f.env["ERP_AWS_IMDS_ENDPOINT"] = "https://sts.eu-central-1.amazonaws.com", "http://169.254.169.254"
	if s, err = load(context.Background(), f.getenv); err != nil {
		t.Fatal(err)
	}
	chain := s.store.(blobstore.S3).Credentials.(*awscreds.Cached).Provider.(awscreds.Chain)
	if got := chain[2].(awscreds.WebIdentity).Endpoint; got != "https://sts.eu-central-1.amazonaws.com" {
		t.Errorf("STS endpoint: got %q, want the cell's regional one (ERP_AWS_STS_ENDPOINT)", got)
	}
	if got := chain[3].(awscreds.IMDS).Endpoint; got != "http://169.254.169.254" {
		t.Errorf("IMDS endpoint: got %q, want ERP_AWS_IMDS_ENDPOINT", got)
	}
}

func TestRunFailures(t *testing.T) {
	t.Run("cache", func(t *testing.T) {
		f := newFixture(t)
		file := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		f.env["ERP_ANALYTICS_CACHE"] = filepath.Join(file, "cache")
		if code := cli(context.Background(), f.getenv, io.Discard, nil); code != 1 {
			t.Errorf("exit: got %d, want 1", code)
		}
	})
	t.Run("relative cache", func(t *testing.T) {
		f := newFixture(t)
		f.env["ERP_ANALYTICS_CACHE"] = "rel-" + uuid.NewString()
		t.Cleanup(func() { _ = os.Remove(f.env["ERP_ANALYTICS_CACHE"]) }) // the directory the run made, if any
		if code := cli(context.Background(), f.getenv, io.Discard, nil); code != 1 {
			t.Errorf("exit: got %d, want 1", code)
		}
	})
	t.Run("listen", func(t *testing.T) {
		f := newFixture(t)
		f.env["ERP_ANALYTICS_ADDR"] = "256.0.0.1:1"
		if code := cli(context.Background(), f.getenv, io.Discard, nil); code != 1 {
			t.Errorf("exit: got %d, want 1", code)
		}
	})
}

// TestServesERPServerAlone: the listener speaks mTLS, admits the cell's
// erp-server and refuses another service's certificate.
func TestServesERPServerAlone(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	addrc, done := make(chan net.Addr, 1), make(chan int, 1)
	go func() { done <- cli(ctx, f.getenv, io.Discard, func(a net.Addr) { addrc <- a }) }()
	var addr net.Addr
	select {
	case addr = <-addrc:
	case code := <-done:
		t.Fatalf("exit %d before serving", code)
	}
	client := func(cert tls.Certificate) *http.Client {
		return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs: f.ca.Pool(), Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}}}
	}
	tenant := uuid.New()
	body := `{"tenant_id":"` + tenant.String() + `","manifest_key":"` + extract.ManifestKey(uuid.New(), 1, "") +
		`","scope":{"all_companies":true,"datasets":["gl_lines"]},"plan":{"dataset":"gl_lines","aggregate":"sum","ledger":"` + uuid.NewString() + `"}}`
	for name, tc := range map[string]struct {
		cert tls.Certificate
		code int
	}{
		"erp-server": {f.server, http.StatusUnprocessableEntity}, // admitted; the forged manifest key refused
		"erp-router": {f.router, http.StatusForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://"+addr.String()+reader.QueryPath, strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			res, err := client(tc.cert).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = res.Body.Close() // the status is what is checked
			if res.StatusCode != tc.code {
				t.Errorf("status: got %d, want %d", res.StatusCode, tc.code)
			}
		})
	}
	t.Run("no client certificate", func(t *testing.T) {
		c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: f.ca.Pool(), MinVersion: tls.VersionTLS13}}}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+addr.String()+"/version", nil)
		if err != nil {
			t.Fatal(err)
		}
		if res, err := c.Do(req); err == nil {
			_ = res.Body.Close() // a response is already the failure
			t.Error("handshake without a certificate: got a response, want it refused")
		}
	})
	cancel()
	if code := <-done; code != 0 {
		t.Errorf("exit: got %d, want 0", code)
	}
}

func TestMain(m *testing.M) {
	exit = func(int) {}
	main() // a signal context that ends with the configuration error: exit 2, swallowed
	os.Exit(m.Run())
}
