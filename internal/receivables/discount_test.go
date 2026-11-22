package receivables_test

import (
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestDiscountOnLeviedJurisdiction (batch 85, the emirate on a sales item;
// ADR-0049 §7, ADR-0063 T75f and T75o): an invoice under a code that
// levies its lines' subdivision (AJ, as the UAE's S5 levies each emirate),
// its item naming DE-BY, with a 2% cash discount term, paid within the
// discount period less the discount. Its company, of country DE, grants
// no cash discount (only a UAE company does, TestUAECashDiscount; ADR-0063
// As built, cash discount granted): what the customer took off is a
// payment difference, written off gross without a tax code, within the
// company's tolerance to 680000 or by a reason that writes off to its own
// account, 690000. So the clearing builds no tax line that would need the
// item's jurisdiction (no ledger.jurisdiction_required) and posts no tax
// item: the invoice's tax stays where it was levied, DE-BY.
func TestDiscountOnLeviedJurisdiction(t *testing.T) {
	e := setupReasons(t)
	pgtest.Exec(t, `INSERT INTO partner.payment_term (tenant_id, code, description, net_days, discount_days, discount_pct, created_by, updated_by)
		VALUES ($1, 'SK14', '14 days 2%, 30 net', 30, 14, 2, $2, $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	customer := e.partner(t, "BP70000002", "Desert Rose Retail", "active", "customer")
	e.companyData(t, customer, "SK14")
	for _, c := range []struct {
		name, reason, difference, lines string
	}{
		{"within the tolerance", "", "2.10 EUR", "103000 10290 - -; 120000 -10500 - -; 680000 210 - -"},
		{"by a reason that writes off", "bank_charges", "0.00 EUR", "103000 10290 - -; 120000 -10500 - -; 690000 210 - -"},
	} {
		t.Run(c.name, func(t *testing.T) {
			invoice := e.post(t, e.entered(t, e.invoice(customer, "2026-03-10", in(e.item("100.00", "AJ"), "DE-BY"))), "2026-03-10")
			if a := amount(t, invoice, "gross_amount"); a != "105.00 EUR" {
				t.Fatalf("the invoice's gross: got %s, want 105.00 EUR", a)
			}
			// 2% of 105.00 is 2.10: 102.90 paid on 20 March, within the 14 days.
			item := itemRef(invoice["journal_entry_id"], 1)
			if c.reason != "" {
				item = withResidual(item, "2.10", c.reason, "")
			}
			got, _ := e.Must(201, "POST", "/v1/incoming-payments", e.payment(customer, e.receipt(t, 10290, "2026-03-20").ID, "2026-03-21", item), "")
			if d := amount(t, got, "difference"); d != c.difference {
				t.Errorf("difference: got %s, want %s", d, c.difference)
			}
			if lines := e.jurisdictionLines(t, got["journal_entry_id"]); lines != c.lines {
				t.Errorf("the clearing entry's lines: got %s, want %s", lines, c.lines)
			}
			if its := e.taxItems(t, got["journal_entry_id"]); its != "" {
				t.Errorf("the clearing entry's tax items: got %s, want none", its)
			}
			if its, want := e.taxItems(t, invoice["journal_entry_id"]), "AJ DE-BY -10000 -500"; its != want {
				t.Errorf("the invoice's tax items: got %s, want %s", its, want)
			}
		})
	}
}
