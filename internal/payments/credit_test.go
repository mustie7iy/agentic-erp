package payments_test

import (
	"fmt"
	"testing"

	"github.com/google/uuid"
)

// creditMemo enters and posts a credit memo of the supplier in company
// 2000 through payables (kind credit_memo, on an account) and returns it.
func (e env) creditMemo(t *testing.T, supplier uuid.UUID, ref, gross, date string) map[string]any {
	t.Helper()
	memo, _ := e.Must(201, "POST", "/v1/supplier-invoices", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "supplier_reference": %q,
		"invoice_date": %q, "kind": "credit_memo", "gross_amount": {"amount": %q, "currency": "EUR"}, "tax_amount": {"amount": "0.00", "currency": "EUR"},
		"items": [{"account_id": %q, "description": "Credit", "quantity": {"value": "1", "unit": "EA"}, "amount": {"amount": %q, "currency": "EUR"}}]}`,
		e.DE, supplier, ref, date, gross, e.expense, gross), "")
	posted, _ := e.Must(200, "POST", "/v1/supplier-invoices/"+id(memo)+":post", fmt.Sprintf(`{"posting_date": %q}`, date), `"1"`)
	return posted
}

// TestCreditMemosNetInProposals: a credit memo is due on its document date,
// as SAP has a credit memo without an invoice reference due at once, so it
// nets with the supplier's invoices due in the same run (an invoice of 100
// and a credit memo of 30 pay 70); a supplier whose due items are only
// credits is excluded with its debit balance and is paid nothing, its
// credit memo staying open for a later run.
func TestCreditMemosNetInProposals(t *testing.T) {
	e := setup(t)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	e.bankAccount(t, e.Vendor, "DE02120300000000202051", "Vendor", "2026-01-01", "", "create")
	e.invoice(t, e.Supplier, "INV-1", "100.00", "2026-03-01")  // net 30 days: due 31 March
	e.creditMemo(t, e.Supplier, "CM-1", "30.00", "2026-03-20") // due at once
	e.creditMemo(t, e.Vendor, "CM-2", "30.00", "2026-03-20")
	run := e.run(t, "2026-04-01", "2026-04-01")
	pays := e.payments(t, run)
	if len(pays) != 1 {
		t.Fatalf("payments: got %d, want 1: %v", len(pays), pays)
	}
	pay := pays[0].(map[string]any)
	if got := pay["amount"].(map[string]any)["amount"]; got != "70.00" {
		t.Errorf("payment to the supplier: got %v, want 70.00 (100.00 less the credit memo's 30.00)", got)
	}
	if n := len(pay["items"].([]any)); n != 2 {
		t.Errorf("items paid: got %d, want the invoice and the credit memo", n)
	}
	if got := e.exclusions(t, run)[e.Vendor.String()]; got != "debit_balance -30.00" {
		t.Errorf("vendor with a credit memo only: got %q, want debit_balance -30.00", got)
	}
	open, _ := e.Must(200, "GET", "/v1/open-items?partner_id="+e.Vendor.String(), "", "")
	if n := len(open["items"].([]any)); n != 1 {
		t.Errorf("vendor's open items after the run: got %d, want its credit memo", n)
	}
}
