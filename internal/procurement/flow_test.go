package procurement_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func TestRequisitionDecisions(t *testing.T) {
	e := setup(t)
	big := items(item(e.office, "300", "EA", "50.00", "EUR", "")) // 15,000.00: two approval steps
	// Rejected at the second step.
	r := e.requisition(t, big, "")
	path := "/v1/requisitions/" + id(r)
	e.act(t, path, "submit", "")
	if p := e.Pending(t, id(r)); len(p["steps"].([]any)) != 2 {
		t.Fatalf("steps: %v", p)
	}
	p := e.Pending(t, id(r))
	e.As(e.Approver).Must(200, "POST", "/v1/approval-requests/"+id(p)+":approve", "", `"1"`)
	p = e.Pending(t, id(r))
	e.As(e.Approver2).Must(200, "POST", "/v1/approval-requests/"+id(p)+":reject", `{"comment": "Too expensive"}`, fmt.Sprintf(`"%v"`, p["version"]))
	if got, _ := e.Must(200, "GET", path, "", ""); got["status"] != "rejected" {
		t.Fatalf("rejected: %v", got)
	}
	if len(events(t, e, "requisition_rejected")) != 1 {
		t.Fatal("rejected event")
	}
	e.Problem(409, "procurement.invalid_status", "POST", path+":cancel", "", "*")

	// Withdrawn by the requester: back to draft, then cancelled.
	r = e.requisition(t, big, "")
	path = "/v1/requisitions/" + id(r)
	e.act(t, path, "submit", "")
	p = e.Pending(t, id(r))
	e.Must(200, "POST", "/v1/approval-requests/"+id(p)+":cancel", `{"comment": "Changing it"}`, `"1"`)
	got, _ := e.Must(200, "GET", path, "", "")
	if got["status"] != "draft" {
		t.Errorf("withdrawn status: got %v, want draft", got["status"])
	}
	if got["workflow_request_id"] != nil {
		t.Errorf("withdrawn workflow request: got %v, want none", got["workflow_request_id"])
	}
	if c := e.act(t, path, "cancel", ""); c["status"] != "cancelled" {
		t.Fatalf("cancelled: %v", c)
	}

	// Cancelling a pending requisition withdraws its approval request.
	r = e.requisition(t, big, "")
	path = "/v1/requisitions/" + id(r)
	e.act(t, path, "submit", "")
	p = e.Pending(t, id(r))
	e.act(t, path, "cancel", "")
	if req, _ := e.As(e.Approver).Must(200, "GET", "/v1/approval-requests/"+id(p), "", ""); req["status"] != "cancelled" {
		t.Fatalf("approval request: %v", req)
	}
	if ev := events(t, e, "requisition_cancelled"); len(ev) != 2 || ev[1]["previous_status"] != "pending_approval" {
		t.Fatalf("cancelled events: %v", ev)
	}
}

func TestRequisitionRefusals(t *testing.T) {
	e := setup(t)
	ok := items(item(e.office, "1", "EA", "1.00", "EUR", ""))
	usExpense := e.Account(t, "1000", "520000").String()
	blocked := e.NewSupplier(t, "BP90000003")
	pgtest.Exec(t, `UPDATE partner.business_partner SET status = 'blocked', version = version + 1 WHERE id = $1`, blocked)
	for name, c := range map[string]struct {
		body   string
		status int
		code   string
	}{
		"legal entity":   {fmt.Sprintf(`{"legal_entity_id": %q, "currency": "EUR", "items": %s}`, uuid.New(), ok), 422, "request.reference_not_found"},
		"currency":       {fmt.Sprintf(`{"legal_entity_id": %q, "currency": "XXQ", "items": %s}`, e.DE, ok), 422, "procurement.unknown_currency"},
		"price currency": {fmt.Sprintf(`{"legal_entity_id": %q, "currency": "EUR", "items": %s}`, e.DE, items(item(e.office, "1", "EA", "1.00", "USD", ""))), 422, "procurement.currency_mismatch"},
		"decimals":       {fmt.Sprintf(`{"legal_entity_id": %q, "currency": "EUR", "items": %s}`, e.DE, items(item(e.office, "1", "EA", "1.0", "EUR", ""))), 400, "request.invalid_amount"},
		"negative":       {fmt.Sprintf(`{"legal_entity_id": %q, "currency": "EUR", "items": %s}`, e.DE, items(item(e.office, "1", "EA", "-1.00", "EUR", ""))), 422, "procurement.invalid_amount"},
		"quantity":       {fmt.Sprintf(`{"legal_entity_id": %q, "currency": "EUR", "items": %s}`, e.DE, items(item(e.office, "-1", "EA", "1.00", "EUR", ""))), 422, "procurement.invalid_quantity"},
		"unit":           {fmt.Sprintf(`{"legal_entity_id": %q, "currency": "EUR", "items": %s}`, e.DE, items(item(e.office, "1", "XX", "1.00", "EUR", ""))), 422, "procurement.unknown_unit"},
		"account":        {fmt.Sprintf(`{"legal_entity_id": %q, "currency": "EUR", "items": %s}`, e.DE, items(item(usExpense, "1", "EA", "1.00", "EUR", ""))), 422, "procurement.account_not_postable"},
		"item number":    {fmt.Sprintf(`{"legal_entity_id": %q, "currency": "EUR", "items": %s}`, e.DE, items(item(e.office, "1", "EA", "1.00", "EUR", `, "item_no": 2`))), 422, "procurement.invalid_item_no"},
		"supplier":       {fmt.Sprintf(`{"legal_entity_id": %q, "currency": "EUR", "supplier_id": %q, "items": %s}`, e.DE, blocked, ok), 422, "procurement.supplier_not_usable"},
		"no supplier":    {fmt.Sprintf(`{"legal_entity_id": %q, "currency": "EUR", "supplier_id": %q, "items": %s}`, e.DE, uuid.New(), ok), 422, "procurement.supplier_not_usable"},
		"contract":       {fmt.Sprintf(`{"legal_entity_id": %q, "currency": "EUR", "contract_id": %q, "items": %s}`, e.DE, uuid.New(), ok), 422, "procurement.invalid_contract"},
		"extensions":     {fmt.Sprintf(`{"legal_entity_id": %q, "currency": "EUR", "items": %s, "extensions": {"Bad": 1}}`, e.DE, ok), 422, "request.invalid_extension"},
	} {
		t.Run(name, func(t *testing.T) {
			api := e.Client
			api.T = t
			code, m, _ := api.Do("POST", "/v1/requisitions", c.body, "")
			if code != c.status {
				t.Errorf("status: got %d, want %d: %v", code, c.status, m)
			}
			if m["code"] != c.code {
				t.Errorf("problem code: got %v, want %s", m["code"], c.code)
			}
		})
	}
	// Contract references: status, company and supplier.
	draft := e.contract(t, e.Supplier.String(), "")
	active := e.contract(t, e.Supplier.String(), "")
	e.act(t, "/v1/contracts/"+id(active), "release", "")
	for name, c := range map[string]string{
		"draft contract": fmt.Sprintf(`, "contract_id": %q`, id(draft)),
		"other supplier": fmt.Sprintf(`, "contract_id": %q, "supplier_id": %q`, id(active), e.Vendor),
	} {
		t.Run(name, func(t *testing.T) {
			api := e.Client
			api.T = t
			api.Problem(422, "procurement.invalid_contract", "POST", "/v1/requisitions", fmt.Sprintf(`{"legal_entity_id": %q, "currency": "EUR", "items": %s%s}`, e.DE, ok, c), "")
		})
	}
	if code, m, _ := e.Do("POST", "/v1/requisitions", fmt.Sprintf(`{"legal_entity_id": %q, "currency": "USD", "contract_id": %q, "items": %s}`, e.DE, id(active),
		items(item(e.office, "1", "EA", "1.00", "USD", ""))), ""); code != 422 || m["code"] != "procurement.invalid_contract" {
		t.Errorf("other currency: %d %v", code, m)
	}
	// The contract names the supplier.
	r := e.requisition(t, ok, fmt.Sprintf(`, "contract_id": %q`, id(active)))
	if r["supplier_id"] != e.Supplier.String() {
		t.Fatalf("supplier from contract: %v", r)
	}
	path := "/v1/requisitions/" + id(r)
	// Updates re-check what they change.
	e.Problem(422, "procurement.supplier_not_usable", "PATCH", path, fmt.Sprintf(`{"contract_id": null, "supplier_id": %q}`, blocked), `"1"`)
	e.Problem(422, "procurement.invalid_contract", "PATCH", path, fmt.Sprintf(`{"supplier_id": %q}`, e.Vendor), `"1"`)
	e.Problem(422, "procurement.unknown_currency", "PATCH", path, `{"currency": "XXQ", "items": `+items(item(e.office, "1", "EA", "1.00", "XXQ", ""))+`}`, `"1"`)
	e.Problem(422, "request.invalid_extension", "PATCH", path, `{"extensions": {"Bad": 1}}`, `"1"`)
	e.Problem(422, "procurement.account_not_postable", "PATCH", path, `{"items": `+items(item(usExpense, "1", "EA", "1.00", "EUR", ""))+`}`, `"1"`)
	e.Problem(412, "request.version_mismatch", "PATCH", path, `{}`, `"9"`)
	e.Problem(404, "request.not_found", "GET", "/v1/requisitions/"+uuid.NewString(), "", "")
	moved, _ := e.Must(200, "PATCH", path, fmt.Sprintf(`{"contract_id": null, "supplier_id": null, "currency": "USD", "items": %s}`,
		items(item(e.office, "1", "EA", "2.00", "USD", ""))), `"1"`)
	if moved["currency"] != "USD" {
		t.Errorf("moved currency: got %v, want USD", moved["currency"])
	}
	if moved["supplier_id"] != nil {
		t.Errorf("moved supplier: got %v, want none", moved["supplier_id"])
	}
	if moved["contract_id"] != nil {
		t.Errorf("moved contract: got %v, want none", moved["contract_id"])
	}
	moved, _ = e.Must(200, "PATCH", path, fmt.Sprintf(`{"supplier_id": %q, "needed_by": "2026-12-01"}`, e.Vendor), `"2"`)
	if moved["supplier_id"] != e.Vendor.String() {
		t.Errorf("new supplier: got %v, want %v", moved["supplier_id"], e.Vendor)
	}
	if moved["needed_by"] != "2026-12-01" {
		t.Errorf("new needed by: got %v, want 2026-12-01", moved["needed_by"])
	}
	agent := e.As(e.Tenant.Agent)
	agent.Problem(404, "request.not_found", "GET", path, "", "")
}

// contract creates a draft contract of company 2000 with items A and B.
func (e env) contract(t *testing.T, supplier, extra string) map[string]any {
	t.Helper()
	c, _ := e.Must(201, "POST", "/v1/contracts", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "currency": "EUR", "valid_from": "2026-01-01",
		"valid_to": "2026-12-31", "target_value": {"amount": "1000.00", "currency": "EUR"}, "items": [
		{"material_key": "A", "description": "Paper", "unit_price": {"amount": "5.00", "currency": "EUR"}, "uom_code": "EA", "account_id": %q},
		{"material_key": "B", "description": "Toner", "unit_price": {"amount": "30.00", "currency": "EUR"}, "price_unit": 2, "uom_code": "BOX", "account_id": %q}]%s}`,
		e.DE, supplier, e.office, e.office, extra), "")
	return c
}

func TestContracts(t *testing.T) {
	e := setupIn2026(t)
	c := e.contract(t, e.Supplier.String(), "")
	path := "/v1/contracts/" + id(c)
	if c["document_number"] != "CT100000" {
		t.Errorf("created document number: got %v, want CT100000", c["document_number"])
	}
	if c["status"] != "draft" {
		t.Errorf("created status: got %v, want draft", c["status"])
	}
	if got := c["consumed_value"].(map[string]any)["amount"]; got != "0.00" {
		t.Errorf("created consumed value: got %v, want 0.00", got)
	}
	if n := len(c["items"].([]any)); n != 2 {
		t.Errorf("created items: got %d, want 2", n)
	}
	item := func(key string) string {
		return fmt.Sprintf(`{"material_key": %q, "description": "X", "unit_price": {"amount": "1.00", "currency": "EUR"}, "uom_code": "EA", "account_id": %q}`, key, e.office)
	}
	e.Problem(422, "procurement.invalid_validity", "PATCH", path, `{"valid_to": "2025-01-01"}`, `"1"`)
	e.Problem(422, "procurement.duplicate_material_key", "PATCH", path, `{"items": [`+item("A")+`, `+item("A")+`]}`, `"1"`)
	e.Problem(422, "procurement.invalid_amount", "PATCH", path, `{"target_value": {"amount": "0.00", "currency": "EUR"}}`, `"1"`)
	e.Problem(422, "procurement.account_not_postable", "PATCH", path, `{"items": [`+strings.Replace(item("C"), e.office, e.Account(t, "1000", "520000").String(), 1)+`]}`, `"1"`)
	upd, tag := e.Must(200, "PATCH", path, `{"valid_from": "2026-02-01", "valid_to": "2026-11-30", "target_value": {"amount": "2000.00", "currency": "EUR"},
		"items": [`+item("B")+`, `+item("A")+`]}`, `"1"`)
	if got := upd["target_value"].(map[string]any)["amount"]; got != "2000.00" {
		t.Errorf("updated target value: got %v, want 2000.00", got)
	}
	if upd["valid_from"] != "2026-02-01" {
		t.Errorf("updated valid from: got %v, want 2026-02-01", upd["valid_from"])
	}
	if got := upd["items"].([]any)[0].(map[string]any)["material_key"]; got != "B" {
		t.Errorf("updated first item: got %v, want B", got)
	}
	upd, _ = e.Must(200, "PATCH", path, `{}`, tag)
	e.Problem(409, "procurement.invalid_status", "POST", path+":terminate", `{"reason": "r"}`, "*")
	act, _ := e.Must(200, "POST", path+":release", "", fmt.Sprintf(`"%v"`, upd["version"]))
	if act["status"] != "active" {
		t.Fatalf("released: %v", act)
	}
	e.Problem(409, "procurement.invalid_status", "PATCH", path, `{}`, "*")
	e.Problem(409, "procurement.invalid_status", "POST", path+":cancel", "", "*")
	term := e.act(t, path, "terminate", `{"reason": "Supplier went bankrupt"}`)
	if term["status"] != "terminated" {
		t.Fatalf("terminated: %v", term)
	}
	other := e.contract(t, e.Vendor.String(), "")
	if got := e.act(t, "/v1/contracts/"+id(other), "cancel", ""); got["status"] != "cancelled" {
		t.Fatalf("cancelled: %v", got)
	}
	for _, name := range []string{"contract_activated", "contract_terminated", "contract_cancelled"} {
		if got := len(events(t, e, name)); got != 1 {
			t.Errorf("%s events: got %d, want 1", name, got)
		}
	}
	for name, body := range map[string]string{
		"validity": fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "currency": "EUR", "valid_from": "2026-02-01", "valid_to": "2026-01-01",
			"target_value": {"amount": "1.00", "currency": "EUR"}, "items": [%s]}`, e.DE, e.Supplier, item("A")),
		"currency": fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "currency": "XXQ", "valid_from": "2026-01-01", "valid_to": "2026-02-01",
			"target_value": {"amount": "1.00", "currency": "XXQ"}, "items": [%s]}`, e.DE, e.Supplier, item("A")),
		"target": fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "currency": "EUR", "valid_from": "2026-01-01", "valid_to": "2026-02-01",
			"target_value": {"amount": "1.00", "currency": "USD"}, "items": [%s]}`, e.DE, e.Supplier, item("A")),
		"supplier": fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "currency": "EUR", "valid_from": "2026-01-01", "valid_to": "2026-02-01",
			"target_value": {"amount": "1.00", "currency": "EUR"}, "items": [%s]}`, e.DE, uuid.New(), item("A")),
		"items": fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "currency": "EUR", "valid_from": "2026-01-01", "valid_to": "2026-02-01",
			"target_value": {"amount": "1.00", "currency": "EUR"}, "items": [%s, %s]}`, e.DE, e.Supplier, item("A"), item("A")),
		"legal entity": fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "currency": "EUR", "valid_from": "2026-01-01", "valid_to": "2026-02-01",
			"target_value": {"amount": "1.00", "currency": "EUR"}, "items": [%s]}`, uuid.New(), e.Supplier, item("A")),
	} {
		t.Run(name, func(t *testing.T) {
			api := e.Client
			api.T = t
			if code, m, _ := api.Do("POST", "/v1/contracts", body, ""); code/100 != 4 {
				t.Errorf("status: got %d, want 4xx: %v", code, m)
			}
		})
	}
	list := func(q string) (int, string) {
		page, _ := e.Must(200, "GET", "/v1/contracts?"+q, "", "")
		tok, _ := page["next_page_token"].(string)
		return len(page["items"].([]any)), tok
	}
	if n, _ := list(fmt.Sprintf("legal_entity_id=%s&supplier_id=%s&status=terminated", e.DE, e.Supplier)); n != 1 {
		t.Fatalf("filtered: %d", n)
	}
	n, tok := list("page_size=1&order_by=document_number")
	if n != 1 {
		t.Errorf("page 1: got %d contracts, want 1", n)
	}
	if tok == "" {
		t.Fatal("page 1: got no next page token, want one")
	}
	n, tok = list("page_size=1&order_by=document_number&page_token=" + tok)
	if n != 1 {
		t.Errorf("page 2: got %d contracts, want 1", n)
	}
	if tok != "" {
		t.Errorf("page 2: got next page token %q, want none", tok)
	}
	if n, _ := list("page_size=5"); n != 2 {
		t.Fatalf("by validity: %d", n)
	}
	e.Problem(404, "request.not_found", "GET", "/v1/contracts/"+uuid.NewString(), "", "")
	got, tag := e.Must(200, "GET", path, "", "")
	if got["status"] != "terminated" {
		t.Errorf("get status: got %v, want terminated", got["status"])
	}
	if tag == "" {
		t.Error("get etag: got none, want one")
	}
}
