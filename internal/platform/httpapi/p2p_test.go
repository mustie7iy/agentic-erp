package httpapi_test

import (
	"fmt"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
)

// TestProcureToPay is T4's exit criterion: requisition → purchase order →
// goods receipt → invoice → match → post through the API, with the
// requisition and an invoice exception decided on approval requests.
func TestProcureToPay(t *testing.T) {
	f := fixture.New(t, "procurement.requisition.read", "procurement.requisition.create", "procurement.order.read", "procurement.order.create",
		"procurement.order.release", "procurement.receipt.read", "procurement.receipt.post", "payables.invoice.read", "payables.invoice.capture",
		"payables.invoice.post", "ledger.entry.read")
	expense := f.Account(t, "2000", "520000")
	act := func(path, action, body string) map[string]any {
		t.Helper()
		_, tag := f.Must(200, "GET", path, "", "")
		got, _ := f.Must(200, "POST", path+":"+action, body, tag)
		return got
	}
	item := fmt.Sprintf(`{"description": "Laptop", "quantity": {"value": "10", "unit": "EA"}, "unit_price": {"amount": "1200.00", "currency": "EUR"},
		"account_id": %q, "tax_code": "V1"}`, expense)

	// 12,000 EUR is above the first tier: purchasing manager, then process owner.
	req, _ := f.Must(201, "POST", "/v1/requisitions", fmt.Sprintf(`{"legal_entity_id": %q, "currency": "EUR",
		"items": [{"description": "Laptop", "quantity": {"value": "10", "unit": "EA"}, "unit_price": {"amount": "1200.00", "currency": "EUR"}, "account_id": %q}]}`,
		f.DE, expense), "")
	reqPath := "/v1/requisitions/" + req["id"].(string)
	if got := act(reqPath, "submit", ""); got["status"] != "pending_approval" {
		t.Fatalf("submitted: %v", got)
	}
	if steps := f.Pending(t, req["id"].(string))["steps"].([]any); len(steps) != 2 {
		t.Fatalf("steps: %v", steps)
	}
	f.Approve(t, req["id"].(string))
	if got, _ := f.Must(200, "GET", reqPath, "", ""); got["status"] != "approved" {
		t.Fatalf("requisition: %v", got)
	}

	po, _ := f.Must(201, "POST", "/v1/purchase-orders", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "currency": "EUR", "order_date": "2026-03-10",
		"requisition_id": %q, "items": [%s]}`, f.DE, f.Supplier, req["id"], item), "")
	poPath := "/v1/purchase-orders/" + po["id"].(string)
	if got := act(poPath, "release", ""); got["status"] != "released" {
		t.Fatalf("released: %v", got)
	}
	if got, _ := f.Must(200, "GET", reqPath, "", ""); got["status"] != "ordered" {
		t.Fatalf("requisition after ordering: %v", got)
	}
	f.Must(201, "POST", "/v1/goods-receipts", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-03-15", "posting_date": "2026-03-15",
		"items": [{"purchase_order_id": %q, "po_item_no": 1, "quantity": {"value": "10", "unit": "EA"}}]}`, f.DE, po["id"]), "")

	invoice := func(ref, qty, net, tax, gross string) string {
		inv, _ := f.Must(201, "POST", "/v1/supplier-invoices", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "supplier_reference": %q,
			"invoice_date": "2026-03-20", "gross_amount": {"amount": %q, "currency": "EUR"}, "tax_amount": {"amount": %q, "currency": "EUR"},
			"purchase_order_id": %q, "items": [{"purchase_order_id": %q, "po_item_no": 1, "quantity": {"value": %q, "unit": "EA"},
			"amount": {"amount": %q, "currency": "EUR"}}]}`, f.DE, f.Supplier, ref, gross, tax, po["id"], po["id"], qty, net), "")
		return "/v1/supplier-invoices/" + inv["id"].(string)
	}
	// A clean match posts: expense and input tax against the supplier.
	first := invoice("INV-1", "10", "12000.00", "2280.00", "14280.00")
	if m := act(first, "match", ""); m["is_clean"] != true {
		t.Fatalf("match: %v", m)
	}
	posted := act(first, "post", `{"posting_date": "2026-03-20"}`)
	if posted["status"] != "posted" || posted["journal_entry_id"] == nil {
		t.Fatalf("posted: %v", posted)
	}
	je, _ := f.Must(200, "GET", "/v1/journal-entries/"+posted["journal_entry_id"].(string), "", "")
	if je["source_context"] != "payables" || len(je["lines"].([]any)) != 3 {
		t.Fatalf("journal entry: %v", je)
	}
	open, _ := f.Must(200, "GET", "/v1/open-items?partner_id="+f.Supplier.String(), "", "")
	if items := open["items"].([]any); len(items) != 1 || items[0].(map[string]any)["amount"].(map[string]any)["amount"] != "-14280.00" {
		t.Fatalf("open items: %v", open)
	}

	// Invoicing more than was received holds the invoice; an approved exception lets it post.
	second := invoice("INV-2", "1", "1200.00", "228.00", "1428.00")
	if m := act(second, "match", ""); m["is_clean"] != false {
		t.Fatalf("second match: %v", m)
	}
	held, _ := f.Must(200, "GET", second, "", "")
	if held["status"] != "on_hold" || held["workflow_request_id"] == nil {
		t.Fatalf("held: %v", held)
	}
	f.Problem(409, "payables.invalid_status", "POST", second+":post", "", `"`+fmt.Sprint(held["version"])+`"`)
	f.Approve(t, held["id"].(string))
	if got := act(second, "post", `{"posting_date": "2026-03-21"}`); got["status"] != "posted" {
		t.Fatalf("posted after the exception: %v", got)
	}
}
