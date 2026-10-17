package payments_test

import (
	"testing"
)

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
	e.invoice(t, e.Supplier, "INV-1", "100.00", "2026-03-01")               // net 30 days: due 31 March
	e.document(t, "credit_memo", e.Supplier, "CM-1", "30.00", "2026-03-20") // due at once
	vendorMemo := e.document(t, "credit_memo", e.Vendor, "CM-2", "30.00", "2026-03-20")
	run := e.run(t, "2026-04-01", "2026-04-01")
	pays := e.payments(t, run)
	if len(pays) != 1 {
		t.Fatalf("payments: got %d, want 1: %v", len(pays), pays)
	}
	pay := pays[0].(map[string]any)
	if got := pay["amount"].(map[string]any)["amount"]; got != "70.00" {
		t.Errorf("payment to the supplier: got %v, want 70.00 (100.00 less the credit memo's 30.00)", got)
	}
	items := pay["items"].([]any)
	if n := len(items); n != 2 {
		t.Fatalf("items paid: got %d, want the invoice and the credit memo", n)
	}
	var memoItem map[string]any
	for _, it := range items {
		if m := it.(map[string]any); m["amount"].(map[string]any)["amount"] == "-30.00" {
			memoItem = m
		}
	}
	if memoItem == nil {
		t.Fatalf("items paid: got %v, want one of -30.00, the credit memo", items)
	}
	if memoItem["due_date"] != "2026-03-20" {
		t.Errorf("credit memo item due date: got %v, want its document date 2026-03-20", memoItem["due_date"])
	}
	if got := e.exclusions(t, run)[e.Vendor.String()]; got != "debit_balance -30.00" {
		t.Errorf("vendor with a credit memo only: got %q, want debit_balance -30.00", got)
	}
	open, _ := e.Must(200, "GET", "/v1/open-items?partner_id="+e.Vendor.String(), "", "")
	openItems := open["items"].([]any)
	if len(openItems) != 1 {
		t.Fatalf("vendor's open items after the run: got %d, want its credit memo", len(openItems))
	}
	if got := openItems[0].(map[string]any)["journal_entry_id"]; got != vendorMemo["journal_entry_id"] {
		t.Errorf("vendor's open item: got entry %v, want the credit memo's %v", got, vendorMemo["journal_entry_id"])
	}
}
