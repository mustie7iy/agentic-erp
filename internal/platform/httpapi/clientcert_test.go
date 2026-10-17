package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// TestClientCertificate: the forwarded thumbprint reaches the request only
// from a trusted peer; the header itself never goes further (ADR-0033).
func TestClientCertificate(t *testing.T) {
	for _, c := range []struct {
		name    string
		trusted bool
		header  string
		want    string
	}{
		{"from the router", true, "t1", "t1"},
		{"from anyone else", false, "t1", ""},
		{"none forwarded", true, "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got, header string
			h := httpapi.ClientCertificate(c.trusted, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				got, header = reqctx.ClientCertFrom(r.Context()), r.Header.Get(httpapi.ClientCertHeader)
			}))
			r := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
			r.Header.Set(httpapi.ClientCertHeader, c.header)
			h.ServeHTTP(httptest.NewRecorder(), r)
			if got != c.want {
				t.Errorf("thumbprint: got %q, want %q", got, c.want)
			}
			if header != "" {
				t.Errorf("header past the middleware: got %q, want removed", header)
			}
		})
	}
}
