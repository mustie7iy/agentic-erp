package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/api"
	"github.com/mustie7iy/agentic-erp/api/gen"
)

// TestAnnounceDeprecations: a deprecated operation answers with Deprecation,
// Sunset and Link, built from its lifecycle; other operations and unknown
// paths answer without them (ADR-0041).
func TestAnnounceDeprecations(t *testing.T) {
	srv, err := gen.NewServer(gen.UnimplementedHandler{}, security{})
	if err != nil {
		t.Fatal(err)
	}
	table := deprecations(map[string]api.Operation{
		"listOperations": {ID: "listOperations", Lifecycle: api.Lifecycle{Deprecated: true,
			DeprecatedAt: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), Sunset: time.Date(2028, 1, 3, 0, 0, 0, 0, time.UTC),
			Link: "https://docs.example.com/v1/operations"}},
		"getOperation": {ID: "getOperation"},
	})
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := announce(srv, table, next)
	for _, c := range []struct {
		name, path, deprecation, sunset, link string
	}{
		{"deprecated", "/v1/operations", "@1798761600", "Mon, 03 Jan 2028 00:00:00 GMT",
			`<https://docs.example.com/v1/operations>; rel="deprecation"; type="text/html"`},
		{"current", "/v1/operations/0192a000-0000-7000-8000-000000000001", "", "", ""},
		{"unknown path", "/v1/nothing", "", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
			if got := rec.Header().Get("Deprecation"); got != c.deprecation {
				t.Errorf("Deprecation: got %q, want %q", got, c.deprecation)
			}
			if got := rec.Header().Get("Sunset"); got != c.sunset {
				t.Errorf("Sunset: got %q, want %q", got, c.sunset)
			}
			if got := rec.Header().Get("Link"); got != c.link {
				t.Errorf("Link: got %q, want %q", got, c.link)
			}
			if rec.Code != http.StatusNoContent {
				t.Errorf("status: got %d, want the next handler's 204", rec.Code)
			}
		})
	}
}

// TestAnnounceWithoutDeprecations: with nothing deprecated the pipeline
// is left as it is, without a route lookup per request.
func TestAnnounceWithoutDeprecations(t *testing.T) {
	next := http.NewServeMux()
	if got := announce(nil, deprecations(nil), next); got != http.Handler(next) {
		t.Errorf("got %T, want the next handler unwrapped", got)
	}
}
