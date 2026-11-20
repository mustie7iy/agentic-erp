package receivables_test

import (
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestDiscountOnLeviedJurisdiction (batch 85, the emirate on a sales item;
// ADR-0049 §7, ADR-0063 T75f): an invoice under a code that levies its
// lines' subdivision (AJ, as the UAE's S5 levies each emirate), its item
// naming DE-BY, with a 2% cash discount term, paid within the discount
// period less the discount. Receivables grants no cash discount of its
// own (ADR-0063 T75f, "Not built"): what the customer took off is a
// payment difference within the company's tolerance, written off gross
// to 680000 without a tax code, so the clearing builds no tax line that
// would need the item's jurisdiction (no ledger.jurisdiction_required)
// and posts no tax item: the invoice's tax stays where it was levied,
// DE-BY.
func TestDiscountOnLeviedJurisdiction(t *testing.T) {
	e := setupReasons(t)
	pgtest.Exec(t, `INSERT INTO partner.payment_term (tenant_id, code, description, net_days, discount_days, discount_pct, created_by, updated_by)
		VALUES ($1, 'SK14', '14 days 2%, 30 net', 30, 14, 2, $2, $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	customer := e.partner(t, "BP70000002", "Desert Rose Retail", "active", "customer")
	e.companyData(t, customer, "SK14")
	invoice := e.post(t, e.entered(t, e.invoice(customer, "2026-03-10", in(e.item("100.00", "AJ"), "DE-BY"))), "2026-03-10")
	if a := amount(t, invoice, "gross_amount"); a != "105.00 EUR" {
		t.Fatalf("the invoice's gross: got %s, want 105.00 EUR", a)
	}
	// 2% of 105.00 is 2.10: 102.90 paid on 20 March, within the 14 days.
	got, _ := e.Must(201, "POST", "/v1/incoming-payments",
		e.payment(customer, e.receipt(t, 10290, "2026-03-20").ID, "2026-03-21", itemRef(invoice["journal_entry_id"], 1)), "")
	if d := amount(t, got, "difference"); d != "2.10 EUR" {
		t.Errorf("difference: got %s, want 2.10 EUR", d)
	}
	if lines, want := e.jurisdictionLines(t, got["journal_entry_id"]), "103000 10290 - -; 120000 -10500 - -; 680000 210 - -"; lines != want {
		t.Errorf("the clearing entry's lines: got %s, want %s", lines, want)
	}
	if its := e.taxItems(t, got["journal_entry_id"]); its != "" {
		t.Errorf("the clearing entry's tax items: got %s, want none", its)
	}
	if its, want := e.taxItems(t, invoice["journal_entry_id"]), "AJ DE-BY -10000 -500"; its != want {
		t.Errorf("the invoice's tax items: got %s, want %s", its, want)
	}
}
