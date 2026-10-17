package payments_test

import (
	"fmt"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestDueByTheTermsPostedWith: an invoice is due by the payment terms it
// was posted with (T36b): its supplier's NT30 from its invoice date, still
// so after the supplier moves to NT14, so a run due by a date between the
// two leaves it for later; a credit posted without terms (a manual entry)
// is due by the supplier's NT14 now, and is paid.
func TestDueByTheTermsPostedWith(t *testing.T) {
	e := setup(t)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	invoice := e.invoice(t, e.Supplier, "INV-1", "100.00", "2026-03-01") // NT30: due 31 March
	pgtest.Exec(t, `UPDATE partner.supplier_company SET payment_term_code = 'NT14', version = version + 1 WHERE tenant_id = $1 AND partner_id = $2`,
		e.Tenant.ID, e.Supplier)
	e.Must(201, "POST", "/v1/journal-entries", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-03-01", "posting_date": "2026-03-01",
		"currency": "EUR", "reference": "MAN-1", "lines": [{"account_id": %q, "amount": {"amount": "-40.00", "currency": "EUR"}, "partner_id": %q},
		{"account_id": %q, "amount": {"amount": "40.00", "currency": "EUR"}}]}`, e.DE, e.Account(t, "2000", "211000"), e.Supplier, e.expense), "")
	run := e.run(t, "2026-03-20", "2026-03-20")
	pays := e.payments(t, run)
	if len(pays) != 1 {
		t.Fatalf("payments due by 20 March: got %d, want the manual credit only", len(pays))
	}
	if got := pays[0].(map[string]any)["amount"].(map[string]any)["amount"]; got != "40.00" {
		t.Errorf("payment due by 20 March: got %v, want 40.00, the manual credit due 15 March by NT14 (the invoice is due 31 March by the terms it posted with)", got)
	}
	open, _ := e.Must(200, "GET", "/v1/open-items?partner_id="+e.Supplier.String(), "", "")
	var terms any
	for _, it := range open["items"].([]any) { // the invoice's and the manual credit's, of the same posting date
		if item := it.(map[string]any); item["journal_entry_id"] == invoice["journal_entry_id"] {
			terms = item["payment_terms"]
		}
	}
	shown, ok := terms.(map[string]any)
	if !ok {
		t.Fatalf("the invoice's open item terms: got %v, want them shown", terms)
	}
	if got := shown["net_due_date"]; got != "2026-03-31" {
		t.Errorf("open item net due date: got %v, want 2026-03-31", got)
	}
}
