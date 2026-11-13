package procurement_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var perms = []string{"procurement.requisition.read", "procurement.requisition.create", "procurement.order.read", "procurement.order.create",
	"procurement.order.release", "procurement.contract.read", "procurement.contract.manage", "procurement.receipt.read", "procurement.receipt.post",
	"procurement.gr_ir.clear", "procurement.rfq.read", "procurement.rfq.manage", "procurement.rfq.award", "procurement.catalog.read", "procurement.catalog.manage", "ledger.entry.read", "partner.supplier.read"}

type env struct {
	fixture.Env
	expense, office string // accounts 510000 and 520000 of company 2000
}

func setup(t *testing.T) env {
	t.Helper()
	return setupWith(t, nil)
}

// setupWith is setup with the configuration document changed by extend
// before it is bootstrapped (fixture.NewWith).
func setupWith(t *testing.T, extend func(*config.Config)) env {
	t.Helper()
	f := fixture.NewWith(t, nil, extend, perms...)
	return env{Env: f, expense: f.Account(t, "2000", "510000").String(), office: f.Account(t, "2000", "520000").String()}
}

// setupIn2026 is setup with the API's clock on a day of 2026, for a test
// that reads the number of a document numbered on its company's date (a
// requisition, a contract): that number is then drawn from 2026's range,
// which starts at 100000, whatever the year the test runs in. From 2027
// the fixture also has the year the test runs in, whose ranges number
// above 2026's (fixture.Years, config.RangeRepeats).
func setupIn2026(t *testing.T) env {
	t.Helper()
	e := setup(t)
	e.API = apitest.NewWith(t, func(cfg *app.Config) {
		cfg.Now = func() time.Time { return time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC) }
	})
	return e
}

// in returns e reporting to t, so a subtest's failure stops that subtest
// rather than its parent.
func (e env) in(t *testing.T) env {
	t.Helper()
	e.T = t
	return e
}

func id(m map[string]any) string { return m["id"].(string) }

func events(t *testing.T, e env, name string) []map[string]any {
	t.Helper()
	return apitest.Published(t, e.Tenant, outbox.Event{Context: "procurement", Name: name})
}

// item is a requisition or order item body.
func item(account, qty, unit, price, currency string, extra string) string {
	return fmt.Sprintf(`{"description": "Paper", "quantity": {"value": %q, "unit": %q}, "unit_price": {"amount": %q, "currency": %q}, "account_id": %q%s}`,
		qty, unit, price, currency, account, extra)
}

func items(list ...string) string { return "[" + strings.Join(list, ", ") + "]" }

func (e env) requisition(t *testing.T, its string, extra string) map[string]any {
	t.Helper()
	r, _ := e.Must(201, "POST", "/v1/requisitions", fmt.Sprintf(`{"legal_entity_id": %q, "currency": "EUR", "items": %s%s}`, e.DE, its, extra), "")
	return r
}

func (e env) act(t *testing.T, path, action, body string) map[string]any {
	t.Helper()
	_, tag := e.Must(200, "GET", path, "", "")
	got, _ := e.Must(200, "POST", path+":"+action, body, tag)
	return got
}

func TestRequisitionLifecycle(t *testing.T) {
	e := setupIn2026(t)
	r := e.requisition(t, items(item(e.office, "10", "EA", "50.00", "EUR", ""), item(e.expense, "2.5", "KG", "4.00", "EUR", `, "price_unit": 2`)),
		fmt.Sprintf(`, "supplier_id": %q, "needed_by": "2026-11-01", "extensions": {"region": "MKT"}`, e.Supplier))
	path := "/v1/requisitions/" + id(r)
	if r["document_number"] != "PR100000" {
		t.Errorf("created document number: got %v, want PR100000", r["document_number"])
	}
	if r["status"] != "draft" {
		t.Errorf("created status: got %v, want draft", r["status"])
	}
	if got := r["total_amount"].(map[string]any)["amount"]; got != "505.00" {
		t.Errorf("created total: got %v, want 505.00", got)
	}
	if r["requester_id"] != e.Tenant.Human.ID.String() {
		t.Errorf("created requester: got %v, want %v", r["requester_id"], e.Tenant.Human.ID)
	}
	if n := len(r["items"].([]any)); n != 2 {
		t.Errorf("created items: got %d, want 2", n)
	}
	got, tag := e.Must(200, "GET", path+"?expand=supplier", "", "")
	if number := got["expanded"].(map[string]any)["supplier"].(map[string]any)["number"]; number != "BP90000001" {
		t.Errorf("expanded supplier: got %v, want BP90000001", number)
	}
	if tag != `"1"` {
		t.Fatalf("etag: got %s, want \"1\"", tag)
	}
	// Items are replaced as a whole; a new currency needs new prices.
	e.Problem(422, "procurement.currency_mismatch", "PATCH", path, `{"currency": "USD"}`, tag)
	upd, tag := e.Must(200, "PATCH", path, `{"items": `+items(item(e.office, "3", "EA", "10.00", "EUR", `, "item_no": 1`))+`, "needed_by": null,
		"extensions": {"region": null, "project": "P1"}}`, tag)
	if n := len(upd["items"].([]any)); n != 1 {
		t.Errorf("updated items: got %d, want 1", n)
	}
	if got := upd["total_amount"].(map[string]any)["amount"]; got != "30.00" {
		t.Errorf("updated total: got %v, want 30.00", got)
	}
	if upd["needed_by"] != nil {
		t.Errorf("updated needed by: got %v, want none", upd["needed_by"])
	}
	if got := upd["extensions"].(map[string]any)["project"]; got != "P1" {
		t.Errorf("updated project: got %v, want P1", got)
	}
	upd, tag = e.Must(200, "PATCH", path, `{"items": `+items(item(e.office, "1", "EA", "10.00", "EUR", ""), item(e.office, "1", "EA", "5.00", "EUR", ""))+`}`, tag)
	if len(upd["items"].([]any)) != 2 {
		t.Fatalf("grown again: %v", upd)
	}
	// Only the requester changes a requisition.
	e.As(e.Approver).Problem(403, "procurement.not_requester", "POST", path+":submit", "", tag)
	sub, _ := e.Must(200, "POST", path+":submit", "", tag)
	if sub["status"] != "pending_approval" {
		t.Errorf("submitted status: got %v, want pending_approval", sub["status"])
	}
	if sub["workflow_request_id"] == nil {
		t.Fatalf("submitted workflow request: got none, want one: %v", sub)
	}
	pending := e.Pending(t, id(r))
	if n := len(pending["steps"].([]any)); n != 1 {
		t.Errorf("approval request steps: got %d, want 1", n)
	}
	if got := pending["amount"].(map[string]any)["amount"]; got != "15.00" {
		t.Errorf("approval request amount: got %v, want 15.00", got)
	}
	e.Problem(403, "workflow.own_request", "POST", "/v1/approval-requests/"+id(pending)+":approve", "", `"1"`)
	e.Problem(409, "procurement.invalid_status", "PATCH", path, `{"needed_by": null}`, "*")
	e.Approve(t, id(r))
	if got, _ := e.Must(200, "GET", path, "", ""); got["status"] != "approved" {
		t.Fatalf("approved: %v", got)
	}
	for _, name := range []string{"requisition_submitted", "requisition_approved"} {
		if got := len(events(t, e, name)); got != 1 {
			t.Errorf("%s events: got %d, want 1", name, got)
		}
	}
}
