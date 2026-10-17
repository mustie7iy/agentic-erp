package httpapi_test

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi"
)

// TestRequirePeer: a handler behind RequirePeer serves only the service
// whose verified SPIFFE ID is the one named (ADR-0033); anything else,
// a plaintext request included, is refused with 403.
func TestRequirePeer(t *testing.T) {
	const router = "spiffe://erp.example/global/erp-router"
	verified := func(id string) *tls.ConnectionState {
		u, _ := url.Parse(id)
		return &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{{URIs: []*url.URL{u}}}}}
	}
	var logged bytes.Buffer
	h := httpapi.RequirePeer(router, slog.New(slog.NewTextHandler(&logged, nil)), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	for _, c := range []struct {
		name string
		tls  *tls.ConnectionState
		want int
	}{
		{"the router", verified(router), http.StatusNoContent},
		{"another service", verified("spiffe://erp.example/eu1/erp-server"), http.StatusForbidden},
		{"no certificate", &tls.ConnectionState{}, http.StatusForbidden},
		{"plaintext", nil, http.StatusForbidden},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
			r.TLS = c.tls
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			if rec.Code != c.want {
				t.Errorf("status: got %d, want %d", rec.Code, c.want)
			}
			if refused := strings.Contains(logged.String(), "peer not allowed"); refused != (c.want == http.StatusForbidden) {
				t.Errorf("logged a refusal: got %v, want %v", refused, c.want == http.StatusForbidden)
			}
			logged.Reset()
		})
	}
}
