package procurement_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var perms = []string{"procurement.requisition.read", "procurement.requisition.create", "procurement.order.read", "procurement.order.create",
	"procurement.order.release", "procurement.contract.read", "procurement.contract.manage", "procurement.receipt.read", "procurement.receipt.post"}

type env struct {
	fixture.Env
	expense, office string // accounts 510000 and 520000 of company 2000
}

func setup(t *testing.T) env {
	t.Helper()
	f := fixture.New(t, perms...)
	return env{Env: f, expense: f.Account(t, "2000", "510000").String(), office: f.Account(t, "2000", "520000").String()}
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
	e := setup(t)
	r := e.requisition(t, items(item(e.office, "10", "EA", "50.00", "EUR", ""), item(e.expense, "2.5", "KG", "4.00", "EUR", `, "price_unit": 2`)),
		fmt.Sprintf(`, "supplier_id": %q, "needed_by": "2026-11-01", "extensions": {"cost_center": "MKT"}`, e.Supplier))
	path := "/v1/requisitions/" + id(r)
	if r["document_number"] != "PR100000" || r["status"] != "draft" || r["total_amount"].(map[string]any)["amount"] != "505.00" ||
		r["requester_id"] != e.Tenant.Human.ID.String() || len(r["items"].([]any)) != 2 {
		t.Fatalf("created: %v", r)
	}
	got, tag := e.Must(200, "GET", path+"?expand=supplier", "", "")
	if got["expanded"].(map[string]any)["supplier"].(map[string]any)["number"] != "BP90000001" || tag != `"1"` {
		t.Fatalf("expanded: %v", got)
	}
	// Items are replaced as a whole; a new currency needs new prices.
	e.Problem(422, "procurement.currency_mismatch", "PATCH", path, `{"currency": "USD"}`, tag)
	upd, tag := e.Must(200, "PATCH", path, `{"items": `+items(item(e.office, "3", "EA", "10.00", "EUR", `, "item_no": 1`))+`, "needed_by": null,
		"extensions": {"cost_center": null, "project": "P1"}}`, tag)
	if len(upd["items"].([]any)) != 1 || upd["total_amount"].(map[string]any)["amount"] != "30.00" || upd["needed_by"] != nil ||
		upd["extensions"].(map[string]any)["project"] != "P1" {
		t.Fatalf("updated: %v", upd)
	}
	upd, tag = e.Must(200, "PATCH", path, `{"items": `+items(item(e.office, "1", "EA", "10.00", "EUR", ""), item(e.office, "1", "EA", "5.00", "EUR", ""))+`}`, tag)
	if len(upd["items"].([]any)) != 2 {
		t.Fatalf("grown again: %v", upd)
	}
	// Only the requester changes a requisition.
	e.As(e.Approver).Problem(403, "procurement.not_requester", "POST", path+":submit", "", tag)
	sub, _ := e.Must(200, "POST", path+":submit", "", tag)
	if sub["status"] != "pending_approval" || sub["workflow_request_id"] == nil {
		t.Fatalf("submitted: %v", sub)
	}
	pending := e.Pending(t, id(r))
	if len(pending["steps"].([]any)) != 1 || pending["amount"].(map[string]any)["amount"] != "15.00" {
		t.Fatalf("approval request: %v", pending)
	}
	e.Problem(403, "workflow.own_request", "POST", "/v1/approval-requests/"+id(pending)+":approve", "", `"1"`)
	e.Problem(409, "procurement.invalid_status", "PATCH", path, `{"needed_by": null}`, "*")
	e.Approve(t, id(r))
	if got, _ := e.Must(200, "GET", path, "", ""); got["status"] != "approved" {
		t.Fatalf("approved: %v", got)
	}
	if len(events(t, e, "requisition_submitted")) != 1 || len(events(t, e, "requisition_approved")) != 1 {
		t.Fatal("events")
	}
}
