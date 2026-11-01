package sapconfig

import (
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// reviewed is the draft after its reviewers did what the note on company
// 3000's USD house bank MUSD asks: its ledger account 102000 flagged
// revalue_foreign_currency, with the mock's exchange gain and loss
// accounts as its unrealised gain and loss (ADR-0042).
func reviewed(c *config.Config) *config.Config {
	co := c.Company("3000")
	if co == nil {
		return c
	}
	if a := co.Account("102000"); a != nil {
		a.RevalueForeignCurrency = true
	}
	co.FXValuation.Accounts = append(co.FXValuation.Accounts,
		config.FXValuationAccount{Account: "102000", UnrealisedGain: "480000", UnrealisedLoss: "680000"})
	return c
}

// errorPaths are the paths of the draft's validation errors.
func errorPaths(c *config.Config) []string {
	var paths []string
	for _, f := range config.Validate(c, currency.Seeded()) {
		if f.Severity == config.SevError {
			paths = append(paths, f.Path)
		}
	}
	return paths
}

// TestHouseBankCurrencyFromT012K: a house bank is generated in its
// account's currency, T012K WAERS (ADR-0038, T53a): the company's own
// leaves the bank's currency empty; another is taken, noted, and the
// draft validates once the reviewers revalue the bank's account. The
// mock's company 3000 has a CAD account MAIN and a USD account MUSD.
func TestHouseBankCurrencyFromT012K(t *testing.T) {
	c, notes := Generate(sap.GenerateMock(42, "2026"), zoned())
	co := c.Company("3000")
	if got := co.HouseBank.Currency; got != "" {
		t.Errorf("currency of MAIN, an account in the company's CAD: got %q, want none", got)
	}
	if len(co.HouseBanks) != 1 {
		t.Fatalf("further house banks of 3000: got %+v, want MUSD", co.HouseBanks)
	}
	musd := co.HouseBanks[0]
	for name, f := range map[string]struct{ got, want string }{
		"name":           {musd.Name, "MUSD"},
		"currency":       {musd.Currency, "USD"},
		"ledger account": {musd.GLAccount, "102000"},
		"account number": {musd.AccountNumber, "5012346"},
		"institution":    {musd.InstitutionNumber, "003"},
	} {
		if f.got != f.want {
			t.Errorf("MUSD's %s: got %q, want %q", name, f.got, f.want)
		}
	}
	want := "companies[3000].house_banks[MUSD].currency: the account is in USD (T012K WAERS), not the company's CAD: flag the bank's ledger accounts revalue_foreign_currency"
	if all := notesText(notes); !strings.Contains(all, want) {
		t.Errorf("notes: got\n%s\nwant one beginning %q", all, want)
	}
	if all := notesText(notes); strings.Contains(all, "house_banks[MAIN].currency") {
		t.Errorf("notes: got one on MAIN's currency, want none for an account in the company's currency:\n%s", all)
	}
	if got := strings.Join(errorPaths(c), " "); got != "companies[3000].house_banks[MUSD].gl_account" {
		t.Errorf("errors of the draft before review: got %q, want the USD bank's unrevalued account", got)
	}
	if got := errorPaths(reviewed(c)); len(got) != 0 {
		t.Errorf("errors of the reviewed draft: got %v, want none", got)
	}
}

// TestHouseBankCurrencyRefused: an account without a currency is not
// guessed to be in the company's, which could never be corrected under
// the bank's code; it is drafted with a value validation refuses, noted,
// as an account in a currency the reference data does not know is.
func TestHouseBankCurrencyRefused(t *testing.T) {
	for name, c := range map[string]struct{ waers, currency, note string }{
		"missing":          {"", unknownCurrency, "companies[3000].house_banks[MUSD].currency: T012K gives the account no currency (WAERS); set it"},
		"blank":            {"   ", unknownCurrency, "companies[3000].house_banks[MUSD].currency: T012K gives the account no currency (WAERS); set it"},
		"unknown currency": {"XYZ", "XYZ", "companies[3000].house_banks[MUSD].currency: the account is in XYZ (T012K WAERS)"},
	} {
		t.Run(name, func(t *testing.T) {
			e := sap.GenerateMock(42, "2026")
			for i := range e.T012K {
				if e.T012K[i].HBKID == "MUSD" {
					e.T012K[i].WAERS = c.waers
				}
			}
			cfg, notes := Generate(e, zoned())
			if got := cfg.Company("3000").HouseBanks[0].Currency; got != c.currency {
				t.Errorf("MUSD's currency: got %q, want %q", got, c.currency)
			}
			if all := notesText(notes); !strings.Contains(all, c.note) {
				t.Errorf("notes: got\n%s\nwant one beginning %q", all, c.note)
			}
			if got := strings.Join(errorPaths(reviewed(cfg)), " "); got != "companies[3000].house_banks[MUSD].currency" {
				t.Errorf("errors of the reviewed draft: got %q, want MUSD's currency", got)
			}
		})
	}
}
