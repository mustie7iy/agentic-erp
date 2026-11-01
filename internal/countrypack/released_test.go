package countrypack

import (
	"os"
	"reflect"
	"strings"
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

// The Canada pack brings the federal holidays and is not yet signed off;
// its tax codes wait for ADR-0049's schema (T61a).
func TestCanadaPack(t *testing.T) {
	lib, err := LoadAll(packs.FS)
	if err != nil {
		t.Fatalf("LoadAll(packs.FS): %v", err)
	}
	ca := lib.Get("CA", "2026.1")
	if ca == nil {
		t.Fatal("Get(CA, 2026.1): got nil, want the Canada pack")
	}
	if n := len(ca.Tax.TaxCodes); n != 0 {
		t.Errorf("tax codes: got %d, want none until T61a", n)
	}
	if ca.SignedOff() {
		t.Error("SignedOff: got true, want false until the tax specialist signs it off")
	}
	draft, _, err := Apply(companyIn(t, "CA", "CAD"), ca, lib, Options{Company: "1000", On: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), FiscalYear: 2026})
	if err != nil {
		t.Fatalf("Apply: %v", err)
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
		d, err := time.Parse(time.DateOnly, day)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := draft.Holiday("1000", d); !ok {
			t.Errorf("holiday on %s: got none, want one", day)
		}
	}
}

// Canada 2026.2 suggests the activities of a cash flow statement as the
// top of the tenant's liquidity items (ADR-0051 §4, T53b), and is 2026.1
// otherwise.
func TestCanadaPackCash(t *testing.T) {
	lib, err := LoadAll(packs.FS)
	if err != nil {
		t.Fatalf("LoadAll(packs.FS): %v", err)
	}
	prev, ca := lib.Get("CA", "2026.1"), lib.Get("CA", "2026.2")
	if ca == nil {
		t.Fatal("Get(CA, 2026.2): got nil, want the Canada pack")
	}
	var codes []string
	for _, it := range ca.Cash.LiquidityItems {
		codes = append(codes, it.Code+"/"+it.Direction)
	}
	if got := strings.Join(codes, " "); got != "OPERATING/both INVESTING/both FINANCING/both" {
		t.Errorf("liquidity items: got %s, want OPERATING, INVESTING and FINANCING for both directions", got)
	}
	if len(prev.Cash.LiquidityItems) != 0 {
		t.Errorf("2026.1's liquidity items: got %d, want none", len(prev.Cash.LiquidityItems))
	}
	if !reflect.DeepEqual(prev.Accounts, ca.Accounts) || !reflect.DeepEqual(prev.Calendar, ca.Calendar) || !reflect.DeepEqual(prev.Payments, ca.Payments) {
		t.Error("2026.2: got accounts, holidays or payment terms other than 2026.1's, want them unchanged")
	}
}

// TestCanadaPackNextYear: the shipped pack applied for a second fiscal
// year creates that year's ranges afresh, from the same prefixes and
// first numbers, so the change is refused: the year's first payment run
// would be numbered PY1 again, which last year's holds (review:T73g2). The
// refusal names every such range; journal ranges, numbered per year, are
// not among them, and the ranges given a prefix with the year pass.
func TestCanadaPackNextYear(t *testing.T) {
	lib, err := LoadAll(packs.FS)
	if err != nil {
		t.Fatalf("LoadAll(packs.FS): %v", err)
	}
	ca := lib.Latest("CA")
	on := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	first, _, err := Apply(companyIn(t, "CA", "CAD"), ca, lib, Options{Company: "1000", On: on, FiscalYear: 2026})
	if err != nil {
		t.Fatalf("Apply for 2026: %v", err)
	}
	next, _, err := Apply(first, ca, lib, Options{Company: "1000", On: on, FiscalYear: 2027})
	if err != nil {
		t.Fatalf("Apply for 2027: %v", err)
	}
	found := map[string]string{}
	for _, f := range config.RangeRepeats(first, next) {
		found[f.Path] = f.Message
	}
	if got := found["number_ranges[1000/payment_run/2027].prefix"]; !strings.Contains(got, "would issue PY1,") {
		t.Errorf("payment run range of 2027: got %q, want it refused for issuing PY1 again", got)
	}
	var want int
	for _, r := range ca.Numbering.NumberRanges {
		if !strings.HasPrefix(r.DocumentType, "journal_") {
			want++
		}
	}
	if len(found) != want {
		t.Errorf("ranges refused: got %d %v, want the %d ranges not of journal entries", len(found), found, want)
	}
	for i, n := range next.NumberRanges {
		if n.FiscalYear == 2027 && !strings.HasPrefix(n.DocumentType, "journal_") {
			next.NumberRanges[i].Prefix += "2027-"
		}
	}
	if fs := config.RangeRepeats(first, next); len(fs) != 0 {
		t.Errorf("ranges with the year in the prefix: got %v, want none refused", fs)
	}
}
