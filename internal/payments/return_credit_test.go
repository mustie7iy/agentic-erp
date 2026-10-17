package payments_test

import (
	"fmt"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/app"
)

// TestReturnClearedByCreditMemo (T47h, ADR-0034): goods returned to the
// supplier after their invoice (122, T47b) leave the order item's GR/IR
// position invoiced but not received, a debit at the valuation price; the
// supplier's credit memo for them (T34) clears it.
func TestReturnClearedByCreditMemo(t *testing.T) {
	e := setupWith(t, []string{"procurement.order.read", "procurement.order.create", "procurement.order.release", "procurement.receipt.post",
		"payables.invoice.review"}, func(*app.Config) {})
	e.GRIR(t)
	grir, expense := e.Account(t, "2000", "219000").String(), e.expense.String()
	po, _ := e.receivedOrder(t) // 10 EA at 12.00, received
	invoice, _ := e.Must(201, "POST", "/v1/supplier-invoices", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "supplier_reference": "INV-1",
		"invoice_date": "2026-03-10", "gross_amount": {"amount": "142.80", "currency": "EUR"}, "tax_amount": {"amount": "22.80", "currency": "EUR"},
		"items": [%s]}`, e.DE, e.Supplier, orderItem(po, "10", "120.00")), "")
	e.matchedAndPosted(t, invoice, "2026-03-10")
	e.position(t, "invoiced as received", po, "10", "10", "0.00")

	ret, _ := e.Must(201, "POST", "/v1/goods-receipts", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-03-11", "posting_date": "2026-03-11",
		"movement_type": "122", "reason": "Damaged", "items": [{"purchase_order_id": %q, "po_item_no": 1, "quantity": {"value": "2", "unit": "EA"}}]}`,
		e.DE, po), "")
	e.entry(t, "return", ret["journal_entry_id"], "WE", map[string]string{grir: "24.00", expense: "-24.00"})
	e.position(t, "after the return", po, "8", "10", "24.00")

	memo, _ := e.Must(201, "POST", "/v1/supplier-invoices", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "supplier_reference": "CM-1",
		"invoice_date": "2026-03-12", "kind": "credit_memo", "reference_invoice_id": %q, "gross_amount": {"amount": "28.56", "currency": "EUR"},
		"tax_amount": {"amount": "4.56", "currency": "EUR"}, "items": [%s]}`, e.DE, e.Supplier, id(invoice), orderItem(po, "2", "24.00")), "")
	e.matchedAndPosted(t, memo, "2026-03-12")
	e.position(t, "after the credit memo", po, "8", "8", "0.00")
}

// position checks the GR/IR position of item 1 of the order.
func (e env) position(t *testing.T, step, po, received, invoiced, balance string) {
	t.Helper()
	page, _ := e.Must(200, "GET", fmt.Sprintf("/v1/gr-ir-balances?legal_entity_id=%s&purchase_order_id=%s&open_only=false", e.DE, po), "", "")
	items := page["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("%s: GR/IR positions got %d, want 1: %v", step, len(items), items)
	}
	p := items[0].(map[string]any)
	if got := p["received_quantity"].(map[string]any)["value"]; got != received {
		t.Errorf("%s: received got %v, want %s", step, got, received)
	}
	if got := p["invoiced_quantity"].(map[string]any)["value"]; got != invoiced {
		t.Errorf("%s: invoiced got %v, want %s", step, got, invoiced)
	}
	if got := p["balance"].(map[string]any)["amount"]; got != balance {
		t.Errorf("%s: balance got %v, want %s", step, got, balance)
	}
}
