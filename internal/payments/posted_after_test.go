package payments_test

import (
	"fmt"
	"testing"
)

// TestRunLeavesOutItemsPostedAfterItsDate (review:T75c, PO 2026-11-12;
// ADR-0006 As built): a payment posts on its run's date and the ledger
// clears no item before it was posted, so a run's proposal leaves out an
// open item posted after the run date, however long it is due, and takes
// the one posted on the run date; a run dated a day later pays the other.
func TestRunLeavesOutItemsPostedAfterItsDate(t *testing.T) {
	e := setup(t)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	e.invoice(t, e.Supplier, "ON-THE-RUN-DATE", "70.00", "2026-04-15")
	late, _ := e.Must(201, "POST", "/v1/supplier-invoices", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "supplier_reference": "POSTED-LATER",
		"invoice_date": "2026-03-01", "gross_amount": {"amount": "100.00", "currency": "EUR"}, "tax_amount": {"amount": "0.00", "currency": "EUR"},
		"items": [{"account_id": %q, "description": "Fees", "quantity": {"value": "1", "unit": "EA"}, "amount": {"amount": "100.00", "currency": "EUR"}}]}`,
		e.DE, e.Supplier, e.expense), "")
	e.Must(200, "POST", "/v1/supplier-invoices/"+id(late)+":post", `{"posting_date": "2026-04-16"}`, `"1"`)
	for _, c := range []struct{ name, runDate, total string }{
		{"dated a day before the later item's posting date", "2026-04-15", "70.00"},
		{"dated on its posting date", "2026-04-16", "100.00"},
	} {
		t.Run(c.name, func(t *testing.T) {
			run := e.in(t).run(t, c.runDate, "2026-12-31")
			if run["status"] != "proposed" {
				t.Fatalf("status: got %v, want proposed", run["status"])
			}
			if got := run["total_amount"].(map[string]any)["amount"]; got != c.total {
				t.Errorf("the run of %s pays: got %v, want %s", c.runDate, got, c.total)
			}
			if n := len(e.in(t).payments(t, run)); n != 1 {
				t.Errorf("payments: got %d, want 1", n)
			}
		})
	}
}
