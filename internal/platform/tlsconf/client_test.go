package tlsconf_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/secrets"
	"github.com/mustie7iy/agentic-erp/internal/platform/tlsconf"
	"github.com/mustie7iy/agentic-erp/internal/platform/tlsconf/tlstest"
)

// server is an HTTPS server with a certificate ca issued, speaking TLS up
// to maxVersion; with clientCAs it requires a client certificate.
func server(t *testing.T, ca *tlstest.CA, clientCAs *x509.CertPool, maxVersion uint16) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.TLS.PeerCertificates) > 0 {
			w.Header().Set("X-Client", r.TLS.PeerCertificates[0].Subject.CommonName)
		}
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{ca.Issue(t, tlstest.Options{}).Pair(t)}, MinVersion: tls.VersionTLS10, MaxVersion: maxVersion} //nolint:gosec // a server stuck on TLS 1.1, which the client must refuse
	if clientCAs != nil {
		srv.TLS.ClientCAs, srv.TLS.ClientAuth = clientCAs, tls.RequireAndVerifyClientCert
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// TestHTTPClient: the store's client verifies servers against its roots,
// speaks TLS 1.2 at least, offers providers no certificate, reaches them
// directly rather than through a proxy (which would handshake without the
// material), and fails without its material rather than connecting
// without it.
func TestHTTPClient(t *testing.T) {
	ca := tlstest.NewCA(t)
	withCert, _ := files(t, ca, ca.Issue(t, tlstest.Options{}))
	if err := withCert.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	rootsOnly := &tlsconf.Store{Source: tlsconf.Source{CA: "file:" + tlstest.WriteFile(t, dir, "ca.crt", ca.PEM)}, Resolver: secrets.Local{}}
	if err := rootsOnly.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	other := &tlsconf.Store{Resolver: secrets.Local{}}
	if err := other.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	open, mutual, closed := server(t, ca, nil, tls.VersionTLS13), server(t, ca, ca.Pool(), tls.VersionTLS13), server(t, ca, nil, tls.VersionTLS13)
	old := server(t, ca, nil, tls.VersionTLS11)
	closed.Close()
	if tr, ok := rootsOnly.HTTPClient(time.Second).Transport.(*http.Transport); !ok || tr.Proxy != nil {
		t.Error("transport: got one that may use a proxy, want direct connections")
	}
	for _, c := range []struct {
		name   string
		store  *tlsconf.Store
		url    string
		client string // the certificate the server saw; empty: none
		want   string // the error; empty: served
	}{
		{"verified server", rootsOnly, open.URL, "", ""},
		// Providers are outside the deployment: the cell's certificate, and
		// the identity it carries, is never offered to them.
		{"no certificate offered to providers", withCert, mutual.URL, "", "certificate"},
		{"server from an untrusted authority", other, open.URL, "", "certificate"},
		{"material never read", &tlsconf.Store{}, open.URL, "", "not read yet"},
		{"server gone", rootsOnly, closed.URL, "", "connection refused"},
		{"server below TLS 1.2", rootsOnly, old.URL, "", "protocol version"},
	} {
		t.Run(c.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, c.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := c.store.HTTPClient(5 * time.Second).Do(req)
			if c.want != "" {
				if err == nil || !strings.Contains(err.Error(), c.want) {
					t.Errorf("got %v, want an error containing %q", err, c.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close() // headers only
			if got := resp.Header.Get("X-Client"); got != c.client {
				t.Errorf("client certificate: got %q, want %q", got, c.client)
			}
		})
	}
}

// TestClientConfig: a configuration takes the material read last and the
// version floor and server name it is asked for.
func TestClientConfig(t *testing.T) {
	ca := tlstest.NewCA(t)
	s, _ := files(t, ca, ca.Issue(t, tlstest.Options{}))
	if _, err := s.ClientConfig(tlsconf.Internal, "temporal.internal"); !errors.Is(err, tlsconf.ErrNotLoaded) {
		t.Errorf("before reading: got %v, want ErrNotLoaded", err)
	}
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg, err := s.ClientConfig(tlsconf.Internal, "temporal.internal")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion: got %x, want TLS 1.3", cfg.MinVersion)
	}
	if cfg.ServerName != "temporal.internal" {
		t.Errorf("ServerName: got %q, want temporal.internal", cfg.ServerName)
	}
	if cfg.RootCAs == nil {
		t.Error("RootCAs: got nil, want the store's roots")
	}
	cert, err := cfg.GetClientCertificate(&tls.CertificateRequestInfo{})
	if err != nil || len(cert.Certificate) == 0 {
		t.Errorf("client certificate: got %v, %v, want the store's", cert, err)
	}
}

// TestClientConfigWithoutCertificate: without a certificate the
// configuration presents none, leaving the server to decide whether it
// needs one.
func TestClientConfigWithoutCertificate(t *testing.T) {
	s := &tlsconf.Store{Resolver: secrets.Local{}}
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg, err := s.ClientConfig(tlsconf.Internal, "")
	if err != nil {
		t.Fatal(err)
	}
	if cert, err := cfg.GetClientCertificate(&tls.CertificateRequestInfo{}); err != nil || len(cert.Certificate) != 0 {
		t.Errorf("client certificate: got %d certificates, %v, want none and no error", len(cert.Certificate), err)
	}
}
