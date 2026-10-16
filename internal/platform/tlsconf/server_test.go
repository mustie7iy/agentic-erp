package tlsconf_test

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/secrets"
	"github.com/mustie7iy/agentic-erp/internal/platform/tlsconf"
	"github.com/mustie7iy/agentic-erp/internal/platform/tlsconf/tlstest"
)

const domain = "erp.example"

// identity is a store holding ca's bundle and a certificate for the
// SPIFFE ID id, valid for localhost: "none" is a certificate without an
// ID, "" no certificate.
func identity(t *testing.T, ca *tlstest.CA, id string) *tlsconf.Store {
	t.Helper()
	dir := t.TempDir()
	src := tlsconf.Source{CA: "file:" + tlstest.WriteFile(t, dir, "ca.crt", ca.PEM)}
	if id != "" {
		var uris []string
		if id != "none" {
			uris = []string{id}
		}
		leaf := ca.Issue(t, tlstest.Options{URIs: uris})
		src.Cert = "file:" + tlstest.WriteFile(t, dir, "tls.crt", leaf.CertPEM)
		src.Key = "file:" + tlstest.WriteFile(t, dir, "tls.key", leaf.KeyPEM)
	}
	s := &tlsconf.Store{Source: src, Resolver: secrets.Local{}}
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

// peerServer serves over the store's server configuration and answers
// with the SPIFFE ID of the client it verified.
func peerServer(t *testing.T, s *tlsconf.Store, auth tls.ClientAuthType) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Peer", tlsconf.PeerID(r.TLS))
	}))
	srv.TLS = s.ServerConfig(tlsconf.Internal, auth)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// TestServiceIdentities: services present SPIFFE identities over mutual
// TLS 1.3 (ADR-0033). A server takes its certificate and the clients'
// authority from the store on each handshake; an internal client checks
// the server's identity and trusts the deployment's authority only.
func TestServiceIdentities(t *testing.T) {
	ca := tlstest.NewCA(t)
	cellID, routerID := tlsconf.ID(domain, "eu1", "erp-server"), tlsconf.ID(domain, "global", "erp-router")
	if cellID != "spiffe://erp.example/eu1/erp-server" {
		t.Errorf("ID: got %q, want spiffe://erp.example/eu1/erp-server", cellID)
	}
	cell, router := identity(t, ca, cellID), identity(t, ca, routerID)
	required := peerServer(t, cell, tls.RequireAndVerifyClientCert)
	for _, c := range []struct {
		name   string
		client *http.Client
		url    string
		expect string // the identity the request names
		peer   string // the identity the server verified
		want   string // the error; empty: served
	}{
		{"router to cell", router.InternalClient(5 * time.Second), required.URL, cellID, routerID, ""},
		{"no client certificate", identity(t, ca, "").InternalClient(5 * time.Second), required.URL, cellID, "", "certificate"},
		{"another cell than named", router.InternalClient(5 * time.Second), required.URL, tlsconf.ID(domain, "eu2", "erp-server"), "",
			`presented "spiffe://erp.example/eu1/erp-server", want "spiffe://erp.example/eu2/erp-server"`},
		{"server without an identity", router.InternalClient(5 * time.Second), peerServer(t, identity(t, ca, "none"), tls.NoClientCert).URL, cellID, "", `presented ""`},
		{"client of another authority", identity(t, tlstest.NewCA(t), routerID).InternalClient(5 * time.Second), required.URL, cellID, "", "certificate"},
		{"no identity named", router.InternalClient(5 * time.Second), required.URL, "", "", "names no identity"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := t.Context()
			if c.expect != "" {
				ctx = tlsconf.ExpectPeer(ctx, c.expect)
			}
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
			resp, err := c.client.Do(req)
			if c.want != "" {
				if err == nil {
					_ = resp.Body.Close() // the failure is what is reported
				}
				if err == nil || !strings.Contains(err.Error(), c.want) {
					t.Errorf("got %v, want an error containing %q", err, c.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close() // headers only
			if got := resp.Header.Get("X-Peer"); got != c.peer {
				t.Errorf("peer: got %q, want %q", got, c.peer)
			}
		})
	}
	// A server that verifies a certificate when one is given: probes
	// connect without one and are served.
	optional := peerServer(t, cell, tls.VerifyClientCertIfGiven)
	probe := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: ca.Pool()}}}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, optional.URL, nil)
	resp, err := probe.Do(req)
	if err != nil {
		t.Fatalf("a probe without a certificate: %v", err)
	}
	_ = resp.Body.Close() // headers only
	if got := resp.Header.Get("X-Peer"); got != "" {
		t.Errorf("peer of a probe: got %q, want none", got)
	}
}

// TestServerWithoutMaterial: a server whose store never read, or holds no
// certificate, fails the handshake rather than serving without one.
func TestServerWithoutMaterial(t *testing.T) {
	ca := tlstest.NewCA(t)
	for _, c := range []struct {
		name  string
		store *tlsconf.Store
	}{
		{"never read", &tlsconf.Store{}},
		{"no certificate", identity(t, ca, "")},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := peerServer(t, c.store, tls.NoClientCert)
			req, _ := http.NewRequestWithContext(tlsconf.ExpectPeer(t.Context(), ""), http.MethodGet, srv.URL, nil)
			resp, err := identity(t, ca, "").InternalClient(time.Second).Do(req)
			if err == nil {
				_ = resp.Body.Close() // the failure is what is reported
				t.Error("got served, want the handshake refused")
			}
		})
	}
}

// TestPeerID: only a verified chain names a peer, and only by a SPIFFE URI.
func TestPeerID(t *testing.T) {
	if got := tlsconf.PeerID(nil); got != "" {
		t.Errorf("no TLS: got %q, want none", got)
	}
	if got := tlsconf.PeerID(&tls.ConnectionState{}); got != "" {
		t.Errorf("no verified chain: got %q, want none", got)
	}
}

// TestInternalClientBeforeRefresh: a client of a store that never read
// refuses to connect rather than connecting without its material.
func TestInternalClientBeforeRefresh(t *testing.T) {
	srv := peerServer(t, identity(t, tlstest.NewCA(t), "none"), tls.NoClientCert)
	req, _ := http.NewRequestWithContext(tlsconf.ExpectPeer(t.Context(), ""), http.MethodGet, srv.URL, nil)
	resp, err := (&tlsconf.Store{}).InternalClient(time.Second).Do(req)
	if err == nil {
		_ = resp.Body.Close() // the failure is what is reported
	}
	if err == nil || !strings.Contains(err.Error(), "not read yet") {
		t.Errorf("got %v, want refused before the material is read", err)
	}
}

// TestServerConfigHasNoTickets: internal servers resume no sessions, so a
// client whose authority left the bundle is verified again on its next
// connection rather than resumed (ADR-0033).
func TestServerConfigHasNoTickets(t *testing.T) {
	ca := tlstest.NewCA(t)
	cfg, err := identity(t, ca, "none").ServerConfig(tlsconf.Internal, tls.RequireAndVerifyClientCert).GetConfigForClient(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.SessionTicketsDisabled {
		t.Error("SessionTicketsDisabled: got false, want true")
	}
}
