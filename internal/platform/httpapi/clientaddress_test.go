package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi"
	"github.com/mustie7iy/agentic-erp/internal/platform/proxyproto"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// TestClientAddress: from the router the forwarded address is the
// client's; from anyone else a forwarded one is ignored and the
// connection's is used; the header never goes further (ADR-0033).
func TestClientAddress(t *testing.T) {
	for _, c := range []struct {
		name    string
		trusted bool
		header  string
		want    string
	}{
		{"from the router", true, "198.51.100.9", "198.51.100.9"},
		{"from the router, none forwarded", true, "", ""},
		{"from the router, malformed", true, "198.51.100.9, 10.1.2.3", ""},
		{"spoofed by a direct caller", false, "198.51.100.9", "203.0.113.5"},
		{"a direct caller", false, "", "203.0.113.5"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got, header string
			h := httpapi.ClientAddress(c.trusted, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				if a, ok := reqctx.ClientAddressFrom(r.Context()); ok {
					got = a.String()
				}
				header = r.Header.Get(proxyproto.ClientAddressHeader)
			}))
			r := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
			r.RemoteAddr = "203.0.113.5:50123"
			r.Header.Set(proxyproto.ClientAddressHeader, c.header)
			h.ServeHTTP(httptest.NewRecorder(), r)
			if got != c.want {
				t.Errorf("address: got %q, want %q", got, c.want)
			}
			if header != "" {
				t.Errorf("header past the middleware: got %q, want removed", header)
			}
		})
	}
}
