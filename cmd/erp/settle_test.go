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

// TestSettleWhenTheInvoiceIsGoneOrChanged: an invoice the rule cannot read
// (404) or that changed between its read and its match (412) is left
// parked with the reason, and the other invoices go on (F16). Before, the
// rule matched with the empty ETag of the failed read, and a 412 of the
// match stopped it.
func TestSettleWhenTheInvoiceIsGoneOrChanged(t *testing.T) {
	model(t)
	notFound := reply{404, `{"code": "request.not_found", "detail": "No supplier invoice with this id."}`}
	changed := reply{412, `{"code": "request.version_mismatch", "detail": "The resource has changed."}`}
	for name, c := range map[string]struct {
		r    routes
		want string
	}{
		"read not found": {routes{"GET " + invoicePath: {notFound}, "POST " + invoicePath + ":match": {{500, `{"code": "unexpected"}`}}},
			"Left parked 1: 5101 (not found: request.not_found)"},
		"match not found": {routes{"POST " + invoicePath + ":match": {notFound}}, "Left parked 1: 5101 (not found: request.not_found)"},
		"changed before its match": {routes{"POST " + invoicePath + ":match": {changed}},
			"Left parked 1: 5101 (changed before its match: request.version_mismatch)"},
	} {
		t.Run(name, func(t *testing.T) {
			fakeAPI(t, parked(c.r))
			got := must(t, "invoices")
			for _, want := range []string{"Posted 0:", c.want} {
				if !strings.Contains(got, want) {
					t.Errorf("output: got\n%s\nwant it to contain %q", got, want)
				}
			}
		})
	}
	// A failed read that is neither stops the rule.
	fakeAPI(t, parked(routes{"GET " + invoicePath: {{500, `{"code": "internal"}`}}}))
	fails(t, "invoice 5101: API 500", "invoices")
}

// TestApprovedExceptionLeftParked: an invoice parked on an approved
// exception (workflow_request_id set) is neither matched nor posted by the
// rule, but listed for a person to post (F18). Before, the rule matched
// it, and the match held it again and asked the approvers again on every
// run.
func TestApprovedExceptionLeftParked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method+" "+r.URL.Path == "GET /v1/supplier-invoices" {
			w.Write([]byte(`{"items": [{"id": "i1", "document_number": "5101", "purchase_order_id": "po", "workflow_request_id": "wr"}]}`))
			return
		}
		t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	env := map[string]string{"ERP_API_URL": srv.URL, "ERP_TOKEN": "t"}
	getenv = func(k string) string { return env[k] }
	t.Cleanup(func() { getenv = os.Getenv })
	got := must(t, "invoices")
	for _, want := range []string{"Posted 0:", "Held 0:", "Left parked 1: 5101 (approved exception: a person posts it)"} {
		if !strings.Contains(got, want) {
			t.Errorf("output: got\n%s\nwant it to contain %q", got, want)
		}
	}
}

// TestInvoicesFollowEveryPage: the rule settles the parked invoices of
// every page, not the first 500 (F16).
func TestInvoicesFollowEveryPage(t *testing.T) {
	var mu sync.Mutex
	var tokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"1"`)
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/supplier-invoices":
			mu.Lock()
			tokens = append(tokens, r.URL.Query().Get("page_token"))
			mu.Unlock()
			if r.URL.Query().Get("page_token") == "" {
				w.Write([]byte(`{"items": [{"id": "i0", "document_number": "5100"}], "next_page_token": "p2"}`))
				return
			}
			w.Write([]byte(`{"items": [{"id": "i1", "document_number": "5101", "purchase_order_id": "po"}]}`))
		case "GET " + invoicePath:
			w.Write([]byte(`{}`))
		case "POST " + invoicePath + ":match":
			w.Write([]byte(`{"is_clean": true, "decision": {"outcome": "auto_post", "reason": "r"}}`))
		case "POST " + invoicePath + ":post":
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
	if want := "Posted 1: 5101"; !strings.Contains(got, want) {
		t.Errorf("output: got\n%s\nwant it to contain %q", got, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if got, want := strings.Join(tokens, ","), ",p2"; got != want {
		t.Errorf("page tokens sent: got %q, want %q", got, want)
	}
}
