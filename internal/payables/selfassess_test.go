package payables_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestSelfAssessment: worked cases of reverse charge codes on supplier
// invoices (ADR-0049 §9, T61e): the supplier is owed the base; the tax is
// owed on 240000 and the part recovered is on 140000, two items; the part
// not recovered is a cost on the item's line.
func TestSelfAssessment(t *testing.T) {
	e := setup(t)
	doc := inputCodes(t, e)
	pair := func(code, pct, recoverable string) config.TaxCode {
		r := []config.TaxRate{rate(pct, "2026-01-01", "")}
		return config.TaxCode{Code: code, Description: code, Kind: config.TaxReverseCharge, Components: []config.TaxComponent{
			{Key: "due", AccountKey: config.KeySelfOutput, Account: "240000", Rates: r},
			{Key: "claim", AccountKey: config.KeySelfInput, Account: "140000", RecoverablePercent: recoverable, Rates: r}}}
	}
	fixture.TaxCodes(t, e.Tenant, doc,
		pair("CA4", "13", "60"), // Canada line 405: imported services used 60% in commercial activities (HST 13%, ITC on 106)
		pair("PS7", "7", "0"),   // BC PST self-assessed where the vendor charged none: a cost
		pair("AR5", "5", "100"), // UAE imported services: box 3 owed, box 10 recovered
		pair("AD5", "5", "100")) // UAE domestic reverse charge (electronics), the buyer's declaration kept with the invoice
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'AED', 'EUR', '2026-01-01', 0.25, 'ECB', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	output := e.Account(t, "2000", "240000").String()
	for name, c := range map[string]struct {
		code, currency, amount string
		lines                  []line
		items                  string
	}{
		"Canada line 405": {"CA4", "EUR", "1000.00", []line{{e.expense.String(), "1052.00", nil}, {output, "-130.00", nil}, {e.tax.String(), "78.00", nil},
			{e.ap.String(), "-1000.00", nil}}, "CA4/claim 100000 13000 7800 at 13 on 2026-03-10; CA4/due 100000 -13000 0 at 13 on 2026-03-10"},
		"BC PST self-assessed": {"PS7", "EUR", "1000.00", []line{{e.expense.String(), "1070.00", nil}, {output, "-70.00", nil}, {e.ap.String(), "-1000.00", nil}},
			"PS7/claim 100000 7000 0 at 7 on 2026-03-10; PS7/due 100000 -7000 0 at 7 on 2026-03-10"},
		"UAE imported services": {"AR5", "AED", "4000.00", []line{{e.expense.String(), "4000.00", nil}, {output, "-200.00", nil}, {e.tax.String(), "200.00", nil},
			{e.ap.String(), "-4000.00", nil}}, "AR5/claim 400000 20000 20000 at 5 on 2026-03-10; AR5/due 400000 -20000 0 at 5 on 2026-03-10"},
		"UAE domestic reverse charge": {"AD5", "AED", "4000.00", []line{{e.expense.String(), "4000.00", nil}, {output, "-200.00", nil}, {e.tax.String(), "200.00", nil},
			{e.ap.String(), "-4000.00", nil}}, "AD5/claim 400000 20000 20000 at 5 on 2026-03-10; AD5/due 400000 -20000 0 at 5 on 2026-03-10"},
	} {
		t.Run(name, func(t *testing.T) {
			body := e.invoice(e.Supplier, "SA-"+c.code, c.amount, "0.00", coded(e, c.amount, c.code))
			if c.currency != "EUR" {
				body = strings.ReplaceAll(body, `"EUR"`, fmt.Sprintf("%q", c.currency))
			}
			inv, _ := e.in(t).Must(201, "POST", "/v1/supplier-invoices", body, "")
			entry := e.in(t).postedEntry(t, inv)
			if got := e.entryLines(t, entry); fmt.Sprint(sorted(got)) != fmt.Sprint(sorted(c.lines)) {
				t.Errorf("lines: got %v, want %v", sorted(got), sorted(c.lines))
			}
			if got := taxItems(t, entry); got != c.items {
				t.Errorf("items:\ngot  %s\nwant %s", got, c.items)
			}
		})
	}
}
