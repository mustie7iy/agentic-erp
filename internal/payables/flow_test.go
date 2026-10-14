package payables_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// clean is an invoice that matches order po once item 1 (10 EA) and item 2
// (5 KG) are received: 120.00 + 15.00 net, 22.80 tax (V1, 19% of 120.00).
func (e env) clean(t *testing.T, po uuid.UUID, ref string) map[string]any {
	t.Helper()
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, ref, "157.80", "22.80",
		poItem(po, 1, "10", "EA", "120.00"), poItem(po, 2, "5", "KG", "15.00")), "")
	return inv
}

func TestCleanMatchPostAndReverse(t *testing.T) {
	e := setup(t)
	po := e.order(t, e.Supplier, "PO-1")
	e.receive(t, po, 1, "10")
	e.receive(t, po, 2, "5")
	inv := e.clean(t, po, "INV-1")
	e.Problem(409, "payables.match_required", "POST", "/v1/supplier-invoices/"+id(inv)+":post", "", `"1"`)
	m := e.act(t, 200, id(inv), "match", "")
	if m["is_clean"] != true {
		t.Errorf("match is_clean: got %v, want true", m["is_clean"])
	}
	if got := m["issues"].([]any); len(got) != 0 {
		t.Errorf("match issues: got %v, want none", got)
	}
	if m["config_version"] != float64(1) {
		t.Errorf("match config version: got %v, want 1", m["config_version"])
	}
	posted := e.act(t, 200, id(inv), "post", `{"posting_date": "2026-03-15"}`)
	if posted["status"] != "posted" {
		t.Errorf("posted status: got %v, want posted", posted["status"])
	}
	if posted["posting_date"] != "2026-03-15" {
		t.Errorf("posted posting date: got %v, want 2026-03-15", posted["posting_date"])
	}
	if posted["journal_entry_id"] == nil {
		t.Fatalf("posted journal entry: got none, want one: %v", posted)
	}
	je, _ := e.Must(200, "GET", "/v1/journal-entries/"+posted["journal_entry_id"].(string), "", "")
	var lines []string
	for _, l := range je["lines"].([]any) {
		line := l.(map[string]any)
		lines = append(lines, fmt.Sprintf("%s %v %v", line["amount"].(map[string]any)["amount"], line["tax_code"], line["partner_id"] != nil))
	}
	if je["document_type"] != "KR" {
		t.Errorf("entry document type: got %v, want KR", je["document_type"])
	}
	if je["source_context"] != "payables" {
		t.Errorf("entry source context: got %v, want payables", je["source_context"])
	}
	if je["source_document_id"] != id(inv) {
		t.Errorf("entry source document: got %v, want %v", je["source_document_id"], id(inv))
	}
	if je["reference"] != "INV-1" {
		t.Errorf("entry reference: got %v, want INV-1", je["reference"])
	}
	if got, want := strings.Join(lines, "|"), "120.00 V1 false|15.00 <nil> false|22.80 V1 false|-157.80 <nil> true"; got != want {
		t.Errorf("entry lines: got %s, want %s", got, want)
	}
	if ev := events(t, e.Tenant, "supplier_invoice_posted"); len(ev) != 1 || ev[0]["journal_entry_id"] != posted["journal_entry_id"] {
		t.Fatalf("posted events: %v", ev)
	}
	e.Problem(409, "payables.invalid_status", "POST", "/v1/supplier-invoices/"+id(inv)+":match", "", e.tag(t, id(inv)))
	reversed := e.act(t, 200, id(inv), "reverse", `{"reason": "Wrong supplier", "posting_date": "2026-03-20"}`)
	if reversed["status"] != "reversed" || reversed["reversal_entry_id"] == nil {
		t.Fatalf("reversed: %v", reversed)
	}
	if ev := events(t, e.Tenant, "supplier_invoice_reversed"); len(ev) != 1 || ev[0]["reversal_posting_date"] != "2026-03-20" {
		t.Fatalf("reversed events: %v", ev)
	}
	e.Problem(409, "payables.invalid_status", "POST", "/v1/supplier-invoices/"+id(inv)+":reverse", `{"reason": "again"}`, e.tag(t, id(inv)))
	// The reversal clears the invoice's open item against its own.
	open, _ := e.Must(200, "GET", "/v1/open-items?partner_id="+e.Supplier.String(), "", "")
	if got := open["items"].([]any); len(got) != 0 {
		t.Errorf("open items of the reversed invoice: got %v, want none", got)
	}
	// The reference is free again for the corrected invoice.
	again := e.clean(t, po, "INV-1")
	e.act(t, 200, id(again), "match", "")
	e.act(t, 200, id(again), "post", "")

	// Account-based invoices post without a match; amounts must add up.
	wrong, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Vendor, "W-1", "120.00", "19.00", accountItem(e.expense, "100.00")), "")
	e.Problem(422, "payables.amounts_mismatch", "POST", "/v1/supplier-invoices/"+id(wrong)+":post", "", e.tag(t, id(wrong)))
	e.Problem(422, "payables.no_purchase_order", "POST", "/v1/supplier-invoices/"+id(wrong)+":match", "", e.tag(t, id(wrong)))
	paid, _ := e.Must(201, "POST", "/v1/supplier-invoices", strings.Replace(e.invoice(e.Vendor, "W-2", "119.00", "19.00", accountItem(e.expense, "100.00")),
		`"items"`, `"posting_date": "2026-03-11", "items"`, 1), "")
	paid = e.act(t, 200, id(paid), "post", "")
	if paid["posting_date"] != "2026-03-11" {
		t.Fatalf("posting date of the invoice: %v", paid)
	}
	// Paying and clearing the open item stops the reversal.
	bank := e.Account(t, "2000", "100000")
	pay, _ := e.Must(201, "POST", "/v1/journal-entries", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-03-25", "posting_date": "2026-03-25",
		"currency": "EUR", "lines": [{"account_id": %q, "amount": {"amount": "119.00", "currency": "EUR"}, "partner_id": %q},
		{"account_id": %q, "amount": {"amount": "-119.00", "currency": "EUR"}}]}`, e.DE, e.ap, e.Vendor, bank), "")
	e.Must(201, "POST", "/v1/clearings", fmt.Sprintf(`{"legal_entity_id": %q, "clearing_date": "2026-03-31", "items": [
		{"fiscal_year": 2026, "journal_entry_id": %q, "line_no": 3}, {"fiscal_year": 2026, "journal_entry_id": %q, "line_no": 1}]}`,
		e.DE, paid["journal_entry_id"], id(pay)), "")
	e.Problem(409, "payables.invoice_cleared", "POST", "/v1/supplier-invoices/"+id(paid)+":reverse", `{"reason": "r"}`, e.tag(t, id(paid)))

	// A supplier blocked after entry cannot be posted; a reversal without a date posts today.
	todo, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Vendor, "W-3", "119.00", "19.00", accountItem(e.expense, "100.00")), "")
	pgtest.Exec(t, `UPDATE partner.supplier_company SET is_posting_blocked = true, version = version + 1 WHERE partner_id = $1`, e.Vendor)
	e.Problem(422, "payables.supplier_not_usable", "POST", "/v1/supplier-invoices/"+id(todo)+":post", "", e.tag(t, id(todo)))
	pgtest.Exec(t, `UPDATE partner.supplier_company SET is_posting_blocked = false, version = version + 1 WHERE partner_id = $1`, e.Vendor)
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'open', version = version + 1 WHERE tenant_id = $1`, e.Tenant.ID)
	posted3 := e.act(t, 200, id(todo), "post", `{"posting_date": "2026-03-12"}`)
	if _, err := uuid.Parse(posted3["journal_entry_id"].(string)); err != nil {
		t.Fatal(err)
	}
	if r := e.act(t, 200, id(todo), "reverse", `{"reason": "Duplicate"}`); r["status"] != "reversed" {
		t.Fatalf("reversed today: %v", r)
	}
}

func TestExceptionApproved(t *testing.T) {
	e := setup(t)
	po := e.order(t, e.Supplier, "PO-1")
	e.receive(t, po, 1, "10")
	// 12 EA at 13.00: over the price tolerance and over the received quantity; gross off by 1.00.
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "INV-X", "186.64", "29.64", poItem(po, 1, "12", "EA", "156.00")), "")
	m := e.act(t, 200, id(inv), "match", "")
	codes := []string{}
	for _, is := range m["issues"].([]any) {
		codes = append(codes, is.(map[string]any)["code"].(string))
	}
	if m["is_clean"] != false {
		t.Errorf("match is_clean: got %v, want false", m["is_clean"])
	}
	if got, want := strings.Join(codes, ","), "tax_or_total_mismatch,price_variance,quantity_exceeds_received"; got != want {
		t.Errorf("match issues: got %s, want %s", got, want)
	}
	held, _ := e.Must(200, "GET", "/v1/supplier-invoices/"+id(inv), "", "")
	if held["status"] != "on_hold" {
		t.Errorf("held status: got %v, want on_hold", held["status"])
	}
	if got := held["hold_reason"].(string); !strings.HasPrefix(got, "Three-way match: 3 issue") {
		t.Errorf("hold reason: got %q, want prefix %q", got, "Three-way match: 3 issue")
	}
	if held["workflow_request_id"] == nil {
		t.Fatalf("held workflow request: got none, want one: %v", held)
	}
	if ev := events(t, e.Tenant, "invoice_held"); len(ev) != 1 || ev[0]["workflow_request_id"] != held["workflow_request_id"] {
		t.Fatalf("held events: %v", ev)
	}
	r := e.Pending(t, id(inv))
	if r["object_type"] != "invoice_exception" {
		t.Errorf("request object type: got %v, want invoice_exception", r["object_type"])
	}
	if got := r["amount"].(map[string]any)["amount"]; got != "186.64" {
		t.Errorf("request amount: got %v, want 186.64", got)
	}
	// Matching again while the exception is pending keeps the request.
	e.act(t, 200, id(inv), "match", "")
	if again, _ := e.Must(200, "GET", "/v1/supplier-invoices/"+id(inv), "", ""); again["workflow_request_id"] != held["workflow_request_id"] {
		t.Fatalf("re-matched: %v", again)
	}
	e.Approve(t, id(inv))
	parked, _ := e.Must(200, "GET", "/v1/supplier-invoices/"+id(inv), "", "")
	if parked["status"] != "parked" {
		t.Errorf("approved status: got %v, want parked", parked["status"])
	}
	if parked["hold_reason"] != nil {
		t.Errorf("approved hold reason: got %v, want none", parked["hold_reason"])
	}
	if parked["workflow_request_id"] != held["workflow_request_id"] {
		t.Errorf("approved workflow request: got %v, want %v", parked["workflow_request_id"], held["workflow_request_id"])
	}
	if ev := events(t, e.Tenant, "invoice_hold_released"); len(ev) != 1 || ev[0]["workflow_request_id"] != held["workflow_request_id"] {
		t.Fatalf("released events: %v", ev)
	}
	// The approved exception covers the issues, but not a gross that does not add up.
	e.Problem(422, "payables.amounts_mismatch", "POST", "/v1/supplier-invoices/"+id(inv)+":post", "", e.tag(t, id(inv)))
	tag := e.tag(t, id(inv))
	e.Must(200, "PATCH", "/v1/supplier-invoices/"+id(inv), `{"gross_amount": {"amount": "185.64", "currency": "EUR"}}`, tag)
	if p := e.act(t, 200, id(inv), "post", ""); p["status"] != "posted" {
		t.Fatalf("posted: %v", p)
	}

	results, _ := e.Must(200, "GET", "/v1/supplier-invoices/"+id(inv)+"/match-results?page_size=1", "", "")
	first := results["items"].([]any)[0].(map[string]any)
	if results["next_page_token"] == nil {
		t.Fatalf("results: %v", results)
	}
	rest, _ := e.Must(200, "GET", "/v1/supplier-invoices/"+id(inv)+"/match-results?page_size=1&page_token="+results["next_page_token"].(string), "", "")
	if len(rest["items"].([]any)) != 1 || rest["items"].([]any)[0].(map[string]any)["id"] == first["id"] {
		t.Fatalf("results page 2: %v", rest)
	}
	got, tagM := e.Must(200, "GET", "/v1/match-results/"+first["id"].(string), "", "")
	if got["supplier_invoice_id"] != id(inv) {
		t.Errorf("result invoice: got %v, want %v", got["supplier_invoice_id"], id(inv))
	}
	if len(tagM) < 10 {
		t.Errorf("result etag: got %q, want at least 10 characters", tagM)
	}
	e.Problem(404, "request.not_found", "GET", "/v1/match-results/"+uuid.NewString(), "", "")
	e.Problem(404, "request.not_found", "GET", "/v1/supplier-invoices/"+uuid.NewString()+"/match-results", "", "")
	svc := e.As(e.Tenant.Service)
	svc.Problem(404, "request.not_found", "GET", "/v1/match-results/"+first["id"].(string), "", "")
	if ev := events(t, e.Tenant, "supplier_invoice_matched"); len(ev) != 2 {
		t.Fatalf("matched events: %v", ev)
	}
}

func TestExceptionRejectedWithdrawnReleased(t *testing.T) {
	e := setup(t)
	po := e.order(t, e.Supplier, "PO-1")
	e.receive(t, po, 1, "8")
	over := func(ref string) map[string]any {
		inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, ref, "142.80", "22.80", poItem(po, 1, "10", "EA", "120.00")), "")
		e.act(t, 200, id(inv), "match", "")
		return inv
	}
	// Rejected: the invoice is rejected.
	rejected := over("R-1")
	r := e.Pending(t, id(rejected))
	e.As(e.Approver).Must(200, "POST", "/v1/approval-requests/"+r["id"].(string)+":reject", `{"comment": "Not received"}`, `"1"`)
	if got, _ := e.Must(200, "GET", "/v1/supplier-invoices/"+id(rejected), "", ""); got["status"] != "rejected" {
		t.Fatalf("rejected: %v", got)
	}
	if ev := events(t, e.Tenant, "supplier_invoice_rejected"); len(ev) != 1 {
		t.Fatalf("rejected events: %v", ev)
	}

	// Withdrawn by the requester: the invoice stays on hold; matching again opens a new request.
	held := over("R-2")
	w := e.Pending(t, id(held))
	e.Must(200, "POST", "/v1/approval-requests/"+w["id"].(string)+":cancel", `{"comment": "Asking the supplier"}`, `"1"`)
	got, _ := e.Must(200, "GET", "/v1/supplier-invoices/"+id(held), "", "")
	if got["status"] != "on_hold" || got["workflow_request_id"] != nil {
		t.Fatalf("withdrawn: %v", got)
	}
	e.act(t, 200, id(held), "match", "")
	second := e.Pending(t, id(held))
	if second["id"] == w["id"] {
		t.Fatal("no new request")
	}
	// Receiving the rest makes the match clean: the match's hold ends and its request is withdrawn.
	e.receive(t, po, 1, "2")
	e.act(t, 200, id(held), "match", "")
	if got, _ := e.Must(200, "GET", "/v1/supplier-invoices/"+id(held), "", ""); got["status"] != "parked" || got["workflow_request_id"] != nil {
		t.Fatalf("clean again: %v", got)
	}
	if req, _ := e.As(e.Approver).Must(200, "GET", "/v1/approval-requests/"+second["id"].(string), "", ""); req["status"] != "cancelled" {
		t.Fatalf("request: %v", req)
	}

	// A manual hold survives a clean match and is released by hand.
	e.act(t, 200, id(held), "hold", `{"reason": "Waiting for the credit note"}`)
	e.act(t, 200, id(held), "match", "")
	got, _ = e.Must(200, "GET", "/v1/supplier-invoices/"+id(held), "", "")
	if got["status"] != "on_hold" || got["hold_reason"] != "Waiting for the credit note" {
		t.Fatalf("manual hold: %v", got)
	}
	e.Problem(409, "payables.invalid_status", "POST", "/v1/supplier-invoices/"+id(held)+":hold", `{"reason": "again"}`, e.tag(t, id(held)))
	released := e.act(t, 200, id(held), "release", `{"comment": "Credit note arrived"}`)
	if released["status"] != "parked" || released["hold_reason"] != nil {
		t.Fatalf("released: %v", released)
	}
	e.Problem(409, "payables.invalid_status", "POST", "/v1/supplier-invoices/"+id(held)+":release", "", e.tag(t, id(held)))

	// Cancelled while an exception is pending: the request is withdrawn.
	pgtest.Exec(t, `UPDATE procurement.purchase_order SET status = 'cancelled', version = version + 1 WHERE id = $1`, po)
	m := e.act(t, 200, id(held), "match", "")
	if m["issues"].([]any)[0].(map[string]any)["code"] != "order_not_open" {
		t.Fatalf("cancelled order: %v", m)
	}
	pending := e.Pending(t, id(held))
	cancelled := e.act(t, 200, id(held), "cancel", `{"reason": "Order cancelled"}`)
	if cancelled["status"] != "cancelled" {
		t.Fatalf("cancelled: %v", cancelled)
	}
	if req, _ := e.As(e.Approver).Must(200, "GET", "/v1/approval-requests/"+pending["id"].(string), "", ""); req["status"] != "cancelled" {
		t.Fatalf("request after cancel: %v", req)
	}
	e.Problem(409, "payables.invalid_status", "POST", "/v1/supplier-invoices/"+id(held)+":cancel", `{"reason": "again"}`, e.tag(t, id(held)))
	if ev := events(t, e.Tenant, "supplier_invoice_cancelled"); len(ev) != 1 || ev[0]["previous_status"] != "on_hold" {
		t.Fatalf("cancelled events: %v", ev)
	}

	// Decisions for an invoice that is no longer on hold are refused.
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		return payables.Approvals{}.Approved(ctx, uuid.MustParse(id(held)), uuid.New())
	})
	pgtest.WantErr(t, err, "payables.invalid_status")

	// Purchase orders with live invoices.
	err = apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		has, err := payables.Invoices{}.HasInvoices(ctx, po)
		if err == nil && has {
			t.Error("cancelled and rejected invoices count")
		}
		other := e.order(t, e.Vendor, "PO-2")
		if has, _ := (payables.Invoices{}).HasInvoices(ctx, other); has {
			t.Error("an order without invoices has some")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
