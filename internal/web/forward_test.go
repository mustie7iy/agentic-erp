package web

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/proxyproto"
	"github.com/mustie7iy/agentic-erp/internal/platform/secrets"
	"github.com/mustie7iy/agentic-erp/internal/platform/tlsconf"
	"github.com/mustie7iy/agentic-erp/internal/platform/tlsconf/tlstest"
)

// TestAPIClientForwards: every connection to the API presents erp-web's
// SPIFFE identity, and every call the person's address alone, never one
// a caller set itself (ADR-0011 as amended by T73c, ADR-0033).
func TestAPIClientForwards(t *testing.T) {
	ca, dir := tlstest.NewCA(t), t.TempDir()
	webID := "spiffe://erp.example/global/erp-web"
	leaf := ca.Issue(t, tlstest.Options{URIs: []string{webID}})
	identity := &tlsconf.Store{Source: tlsconf.Source{Cert: "file:" + tlstest.WriteFile(t, dir, "tls.crt", leaf.CertPEM),
		Key: "file:" + tlstest.WriteFile(t, dir, "tls.key", leaf.KeyPEM)}, Resolver: secrets.Local{}}
	if err := identity.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	var peer, address string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		peer, address = tlsconf.PeerID(r.TLS), strings.Join(r.Header.Values(proxyproto.ClientAddressHeader), ",")
	}))
	srv.TLS = &tls.Config{MinVersion: tls.VersionTLS12, ClientCAs: ca.Pool(), ClientAuth: tls.RequireAndVerifyClientCert,
		Certificates: []tls.Certificate{ca.Issue(t, tlstest.Options{}).Pair(t)}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	c := apiClient("", &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: ca.Pool()}, identity)
	call := func(ctx context.Context, header string) error {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil) // a test server's URL always parses
		if header != "" {
			req.Header.Set(proxyproto.ClientAddressHeader, header)
		}
		resp, err := c.Do(req)
		if err == nil {
			_ = resp.Body.Close() // only what the server saw is asserted
		}
		return err
	}
	person := WithClientAddress(t.Context(), netip.MustParseAddr("198.51.100.23"))
	for _, tc := range []struct {
		name   string
		ctx    context.Context
		header string
		want   string
	}{
		{"a person's call", person, "", "198.51.100.23"},
		{"a person's call, with another address set", person, "203.0.113.1", "198.51.100.23"},
		{"a call without a person's address", t.Context(), "203.0.113.1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := call(tc.ctx, tc.header); err != nil {
				t.Fatal(err)
			}
			if peer != webID {
				t.Errorf("the API's peer: got %q, want erp-web's %s", peer, webID)
			}
			if address != tc.want {
				t.Errorf("forwarded address: got %q, want %q", address, tc.want)
			}
		})
	}
	// An identity not read yet fails the handshake rather than calling
	// without it; without a configuration of its own the client still
	// verifies at TLS 1.2 or later.
	c = apiClient("", &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: ca.Pool()}, &tlsconf.Store{})
	if err := call(person, ""); err == nil {
		t.Error("an identity not read yet: got a call, want the handshake refused")
	}
	if got := apiClient("", nil, identity).Transport.(forwardAddress).next.(*http.Transport).TLSClientConfig.MinVersion; got != tlsconf.External {
		t.Errorf("default TLS floor: got %x, want %x", got, tlsconf.External)
	}
}
