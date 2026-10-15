package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// TestSettleOnTheMatchedVersion: the rule posts or holds an invoice with
// the ETag it sent to :match, so an invoice edited between the match and
// the post is refused (412) and left parked, never posted on the strength
// of a match of another version (F13). Before, it read the invoice again
// and posted with the fresh ETag.
func TestSettleOnTheMatchedVersion(t *testing.T) {
	for _, c := range []struct {
		outcome, action string
	}{{"auto_post", ":post"}, {"review", ":hold"}} {
		t.Run(c.outcome, func(t *testing.T) {
			var mu sync.Mutex
			version, sent := `"1"`, map[string]string{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				switch r.Method + " " + r.URL.Path {
				case "GET /v1/supplier-invoices":
					w.Write([]byte(`{"items": [{"id": "i1", "document_number": "5101", "purchase_order_id": "po"}]}`))
				case "GET " + invoicePath:
					w.Header().Set("ETag", version)
					w.Write([]byte(`{}`))
				case "POST " + invoicePath + ":match":
					sent[":match"] = r.Header.Get("If-Match")
					version = `"2"` // a person edits the invoice right after the match
					w.Header().Set("ETag", `"m1"`)
					w.Write([]byte(`{"is_clean": true, "decision": {"outcome": "` + c.outcome + `", "reason": "r"}}`))
				case "POST " + invoicePath + c.action:
					sent[c.action] = r.Header.Get("If-Match")
					if sent[c.action] != version {
						w.WriteHeader(http.StatusPreconditionFailed)
						w.Write([]byte(`{"code": "request.version_mismatch", "detail": "The resource has changed."}`))
						return
					}
					w.Write([]byte(`{}`))
				default:
					t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(srv.Close)
			env := map[string]string{"ERP_API_URL": srv.URL, "ERP_TOKEN": "t"}
			getenv = func(k string) string { return env[k] }
			t.Cleanup(func() { getenv = os.Getenv })
			got := must(t, "invoices")
			mu.Lock()
			defer mu.Unlock()
			if sent[c.action] != sent[":match"] {
				t.Errorf("If-Match of %s: got %s, want %s, the one sent to :match", c.action, sent[c.action], sent[":match"])
			}
			if want := "Left parked 1: 5101 (changed since its match)"; !strings.Contains(got, want) {
				t.Errorf("output: got\n%s\nwant it to contain %q", got, want)
			}
		})
	}
}
