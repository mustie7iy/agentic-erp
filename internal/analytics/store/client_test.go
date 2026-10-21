package store_test

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/analytics/store"
	"github.com/mustie7iy/agentic-erp/internal/analytics/wire"
	"github.com/mustie7iy/agentic-erp/internal/platform/secrets"
	"github.com/mustie7iy/agentic-erp/internal/platform/tlsconf"
	"github.com/mustie7iy/agentic-erp/internal/platform/tlsconf/tlstest"
)

var (
	readerID = tlsconf.ID("erp.test", "c1", "erp-analytics")
	serverID = tlsconf.ID("erp.test", "c1", "erp-server")
)

// identity is a store holding ca's bundle and a certificate for id.
func identity(t *testing.T, ca *tlstest.CA, id string) *tlsconf.Store {
	t.Helper()
	dir := t.TempDir()
	leaf := ca.Issue(t, tlstest.Options{URIs: []string{id}})
	s := &tlsconf.Store{Source: tlsconf.Source{CA: "file:" + tlstest.WriteFile(t, dir, "ca.crt", ca.PEM),
		Cert: "file:" + tlstest.WriteFile(t, dir, "tls.crt", leaf.CertPEM), Key: "file:" + tlstest.WriteFile(t, dir, "tls.key", leaf.KeyPEM)},
		Resolver: secrets.Local{}}
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestClient: erp-server calls the reader over mutual TLS, accepts the
// reader's SPIFFE ID alone, and takes a 200 answer from the manifest it
// named as the only answer.
func TestClient(t *testing.T) {
	ca := tlstest.NewCA(t)
	req := wire.Request{TenantID: uuid.New(), ManifestKey: "analytics/v1/tenant=x/manifest-1.json"}
	answer := map[string]struct {
		code int
		body string
	}{
		"ok":       {200, `{"manifest_key":"` + req.ManifestKey + `","rows":[{"by":["2000"],"currency":"EUR","exponent":2,"amount_minor":150}]}`},
		"refused":  {422, `{"code":"analytics.refused"}`},
		"busy":     {503, `{"code":"analytics.busy"}`},
		"garbled":  {200, `{"rows":`},
		"manifest": {200, `{"manifest_key":"another","rows":[]}`},
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tlsconf.PeerID(r.TLS) != serverID || r.URL.Path != wire.QueryPath {
			http.Error(w, "who", http.StatusForbidden)
			return
		}
		b, _ := io.ReadAll(r.Body) // the test's own request
		a := answer[r.Header.Get("X-Case")]
		if !strings.Contains(string(b), req.TenantID.String()) {
			a.code = http.StatusBadRequest
		}
		w.WriteHeader(a.code)
		_, _ = io.WriteString(w, a.body) // a test response
	}))
	srv.TLS = identity(t, ca, readerID).ServerConfig(tlsconf.Internal, tls.RequireAndVerifyClientCert)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	server := identity(t, ca, serverID)
	client := func(c string) *store.Client {
		h := server.InternalClient(5 * time.Second)
		h.Transport = caseHeader{c, h.Transport}
		return &store.Client{URL: srv.URL, Peer: readerID, HTTP: h}
	}
	t.Run("ok", func(t *testing.T) {
		res, err := client("ok").Query(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Rows) != 1 || res.Rows[0].Amount != 150 {
			t.Errorf("rows: got %+v, want 150 EUR for 2000", res.Rows)
		}
	})
	for _, c := range []string{"refused", "busy", "garbled", "manifest"} {
		t.Run(c, func(t *testing.T) {
			if _, err := client(c).Query(context.Background(), req); err == nil {
				t.Error("query: got nil error, want one")
			}
		})
	}
	t.Run("another peer", func(t *testing.T) {
		c := client("ok")
		c.Peer = tlsconf.ID("erp.test", "c2", "erp-analytics")
		if _, err := c.Query(context.Background(), req); err == nil {
			t.Error("query of a server presenting another identity: got nil error, want one")
		}
	})
	t.Run("bad url", func(t *testing.T) {
		c := client("ok")
		c.URL = "://nowhere"
		if _, err := c.Query(context.Background(), req); err == nil {
			t.Error("query: got nil error, want one")
		}
	})
	t.Run("body", func(t *testing.T) {
		c := &store.Client{URL: "https://reader", Peer: readerID, HTTP: &http.Client{Transport: brokenBody{}}}
		if _, err := c.Query(context.Background(), req); err == nil {
			t.Error("query: got nil error, want the body's")
		}
	})
}

// caseHeader names the answer the test server gives.
type caseHeader struct {
	name string
	next http.RoundTripper
}

func (c caseHeader) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("X-Case", c.name)
	return c.next.RoundTrip(r)
}

// brokenBody answers with a body that fails to read.
type brokenBody struct{}

func (brokenBody) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(failingReader{}), Header: http.Header{}}, nil
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("reset") }
