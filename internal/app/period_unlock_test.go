package app_test

import (
	"fmt"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
)

// TestPeriodUnlock is T32c's exit criterion: a locked period returns to
// closed only when its unlock request is approved by the controller; a
// rejected or withdrawn request leaves it locked, and only one request is
// pending at a time.
func TestPeriodUnlock(t *testing.T) {
	f := fixture.New(t, "ledger.period.read", "ledger.period.manage", "ledger.period.lock")
	page, _ := f.Must(200, "GET", fmt.Sprintf("/v1/fiscal-periods?legal_entity_id=%s&fiscal_year=2026&order_by=period&page_size=1", f.DE), "", "")
	path := "/v1/fiscal-periods/" + page["items"].([]any)[0].(map[string]any)["id"].(string)
	// lock locks the period from whatever status it is in.
	lock := func() {
		t.Helper()
		p, tag := f.Must(200, "GET", path, "", "")
		if p["status"] == "open" {
			_, tag = f.Must(200, "POST", path+":close", "", tag)
		}
		f.Must(200, "POST", path+":lock", "", tag)
	}
	// request asks for the unlock and returns its pending approval request.
	request := func() map[string]any {
		t.Helper()
		_, tag := f.Must(200, "GET", path, "", "")
		p, tag := f.Must(200, "POST", path+":unlock", `{"reason": "Audit adjustment"}`, tag)
		f.Problem(409, "ledger.unlock_pending", "POST", path+":unlock", `{"reason": "Again"}`, tag)
		r := f.Pending(t, p["id"].(string))
		if p["unlock_request_id"] != r["id"] {
			t.Errorf("unlock_request_id: got %v, want %v", p["unlock_request_id"], r["id"])
		}
		if r["object_type"] != "period_unlock" {
			t.Errorf("object_type: got %v, want period_unlock", r["object_type"])
		}
		return r
	}
	// still asserts the period's status and that it names no request.
	still := func(when, want string) {
		t.Helper()
		p, _ := f.Must(200, "GET", path, "", "")
		if p["status"] != want {
			t.Errorf("%s: status got %v, want %s", when, p["status"], want)
		}
		if _, ok := p["unlock_request_id"]; ok {
			t.Errorf("%s: unlock_request_id got %v, want absent", when, p["unlock_request_id"])
		}
	}
	decide := func(r map[string]any, action string) {
		t.Helper()
		f.As(f.Approver).Must(200, "POST", "/v1/approval-requests/"+r["id"].(string)+":"+action, `{"comment": "No"}`, fmt.Sprintf(`"%v"`, r["version"]))
	}

	lock()
	decide(request(), "reject")
	still("rejected", "locked")
	r := request()
	f.Must(200, "POST", "/v1/approval-requests/"+r["id"].(string)+":cancel", `{"comment": "Not needed"}`, fmt.Sprintf(`"%v"`, r["version"]))
	still("withdrawn", "locked")
	if got := apitest.Published(t, f.Tenant, outbox.Event{Context: "ledger", Name: "period_unlocked"}); len(got) != 0 {
		t.Fatalf("period_unlocked before an approval: got %v, want none", got)
	}
	f.Approve(t, request()["object_id"].(string))
	still("approved", "closed")
	got := apitest.Published(t, f.Tenant, outbox.Event{Context: "ledger", Name: "period_unlocked"})
	if len(got) != 1 {
		t.Fatalf("period_unlocked events: got %v, want 1", got)
	}
	if got[0]["status"] != "closed" || got[0]["previous_status"] != "locked" {
		t.Errorf("period_unlocked: got %v from %v, want closed from locked", got[0]["status"], got[0]["previous_status"])
	}
	// Unlocked means closed: it opens again only by :open.
	_, tag := f.Must(200, "GET", path, "", "")
	f.Problem(409, "ledger.invalid_period_status", "POST", path+":unlock", `{"reason": "Closed"}`, tag)
	f.Must(200, "POST", path+":open", "", tag)
}
