package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mustie7iy/agentic-erp/api"
	"github.com/mustie7iy/agentic-erp/api/gen"
)

// TestSnapshot: a request to an operation marked x-snapshot reads as of
// one database state; other operations and unknown paths do not.
func TestSnapshot(t *testing.T) {
	srv, err := gen.NewServer(gen.UnimplementedHandler{}, security{})
	if err != nil {
		t.Fatal(err)
	}
	marked := snapshot(srv, map[string]api.Operation{"listOperations": {ID: "listOperations", Snapshot: true}, "getOperation": {ID: "getOperation"}})
	for _, c := range []struct {
		name, path string
		want       bool
	}{
		{"marked", "/v1/operations", true},
		{"not marked", "/v1/operations/0192a000-0000-7000-8000-000000000001", false},
		{"unknown path", "/v1/nothing", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := marked(httptest.NewRequest(http.MethodGet, c.path, nil)); got != c.want {
				t.Errorf("snapshot: got %v, want %v", got, c.want)
			}
		})
	}
}
