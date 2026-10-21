package countrypack

import (
	"os"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/packs"
)

// companyIn is the prototype's valid document with company 1000 moved to
// country (in currency) and stripped of what a pack brings: its chart of
// accounts, account determination, tax codes and payment terms.
func companyIn(t *testing.T, country, currencyCode string) *config.Config {
	t.Helper()
	b, err := os.ReadFile("../config/testdata/valid.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	co := c.Company("1000")
	co.Country, co.Currency = country, currencyCode
	co.ChartOfAccounts, co.AccountDetermination, co.TaxCodes = nil, config.AccountDetermination{}, nil
	for i := range co.PaymentMethods {
		for j := range co.PaymentMethods[i].Ranking {
			co.PaymentMethods[i].Ranking[j].Currency = currencyCode
		}
	}
	return c
}

// Every released pack applied to a company of its country, on the first
// adoption, gives a document that validates: the pack is complete enough
// to post with (ADR-0048).
func TestReleasedPacksApply(t *testing.T) {
	lib, err := LoadAll(packs.FS)
	if err != nil {
		t.Fatalf("LoadAll(packs.FS): %v", err)
	}
	currencies := map[string]string{"CA": "CAD"}
	for _, p := range lib.All() {
		t.Run(p.Dir(), func(t *testing.T) {
			cur, ok := currencies[p.Manifest.Country]
			if !ok {
				t.Fatalf("no currency for %s: add the country's to the test", p.Manifest.Country)
			}
			doc := companyIn(t, p.Manifest.Country, cur)
			draft, _, err := Apply(doc, p, lib, Options{Company: "1000", On: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), FiscalYear: 2026})
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			for _, f := range config.Validate(draft, currency.Seeded()) {
				if f.Severity == config.SevError {
					t.Errorf("finding: %s: %s", f.Path, f.Message)
				}
			}
		})
	}
}

// The Canada pack writes the rates in force on the date of application,
// Nova Scotia's lowered HST among them, and is not yet signed off.
func TestCanadaPack(t *testing.T) {
	lib, err := LoadAll(packs.FS)
	if err != nil {
		t.Fatalf("LoadAll(packs.FS): %v", err)
	}
	ca := lib.Get("CA", "2026.1")
	if ca == nil {
		t.Fatal("Get(CA, 2026.1): got nil, want the Canada pack")
	}
	if ca.SignedOff() {
		t.Error("SignedOff: got true, want false until the tax specialist signs it off")
	}
	draft, _, err := Apply(companyIn(t, "CA", "CAD"), ca, lib, Options{Company: "1000", On: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), FiscalYear: 2026})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	rates := map[string]string{}
	for _, tc := range draft.Company("1000").TaxCodes {
		rates[tc.Code] = tc.RatePercent
	}
	for code, want := range map[string]string{"G5": "5", "HON": "13", "HNB": "15", "HNL": "15", "HNS": "14", "HPE": "15", "QST": "9.975", "PBC": "7", "PMB": "7", "PSK": "6"} {
		if rates[code] != want {
			t.Errorf("tax code %s: got rate %q, want %q", code, rates[code], want)
		}
	}
	cal := draft.Calendar("1000")
	if cal == nil {
		t.Fatal("calendar: got none, want the federal holidays")
	}
	if len(cal.Holidays) != 20 {
		t.Errorf("holidays: got %d, want the 20 federal holidays of 2026 and 2027", len(cal.Holidays))
	}
	// The movable ones, which are computed: Easter, the Monday before May
	// 25, the first Monday of September, the second Monday of October.
	for _, day := range []string{"2026-04-03", "2027-03-26", "2026-05-18", "2027-05-24", "2026-09-07", "2027-09-06", "2026-10-12", "2027-10-11"} {
		d, _ := time.Parse(time.DateOnly, day) // the list's dates are valid
		if _, ok := draft.Holiday("1000", d); !ok {
			t.Errorf("holiday on %s: got none, want one", day)
		}
	}
}
