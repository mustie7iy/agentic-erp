package sapconfig

import (
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

func notesText(ns []Note) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = n.String()
	}
	return strings.Join(parts, "\n")
}

func TestGenerateFromMockSAP(t *testing.T) {
	c, notes := Generate(sap.GenerateMock(42, "2026"), DefaultOptions())
	us := c.Company("1000")
	if us == nil || us.Currency != "USD" || us.Country != "US" || len(us.ChartOfAccounts) != 10 {
		t.Fatalf("company 1000: %+v", us)
	}
	if ad := us.AccountDetermination; ad.APReconciliation != "211000" || ad.InputTax != "140000" || ad.Bank != "100000" {
		t.Fatalf("account determination from T030/LFB1: %+v", ad)
	}
	if a := us.Account("211000"); a == nil || !a.VendorReconciliation || a.Type != config.Liability {
		t.Fatal("reconciliation account from SKB1-MITKZ")
	}
	if len(us.TaxCodes) != 2 || us.TaxCodes[0].RatePercent != "8" || c.Company("2000").TaxCodes[0].RatePercent != "19" {
		t.Fatalf("tax codes from T007A: %+v", us.TaxCodes)
	}
	if us.InvoiceTolerance.PricePercent != 2 || us.InvoiceTolerance.PriceAbsolute != "5.00" || us.HouseBank.IBAN != "GB33BUKB20201555555555" {
		t.Fatal("tolerance from T169G and house bank from T012K")
	}
	if len(c.PaymentTerms) != 4 || c.PaymentTerm("NT45").NetDays != 45 {
		t.Fatal("payment terms from T052")
	}
	if c.Payments.ReleaseRole != "Z_TREASURY_MGR" || c.Payments.VendorApproverRole != "vendor_master_approver" ||
		c.Purchasing.RequisitionApproval[0].Steps[0].Role != "Z_PURCH_MGR" {
		t.Fatalf("approval roles: %+v %+v", c.Payments, c.Purchasing)
	}
	ranges := map[string]bool{}
	for _, n := range c.NumberRanges {
		ranges[n.Key()] = true
	}
	for _, key := range []string{"1000/journal_re/2026", "2000/journal_kz/2026", "1000/payment_run/2026", "2000/journal_ob/2026"} {
		if !ranges[key] {
			t.Errorf("no number range %s", key)
		}
	}
	all := notesText(notes)
	for _, want := range []string{"auto_post_limit", "fiscal year variant", "release strategies", "bank_change_hold_days",
		"provision these identity roles", "roles[Z_AP_CLERK]: SAP role"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing note about %s", want)
		}
	}
	if fs := config.Validate(c); config.HasErrors(fs) {
		t.Fatalf("draft does not validate: %v", fs)
	}
}

func TestGenerateGapsAndDefaults(t *testing.T) {
	e := sap.GenerateMock(42, "2026")
	e.T052, e.T169G, e.T030, e.ACDOCA = nil, nil, nil, nil
	e.SKB1 = append(e.SKB1, sap.SKB1{BUKRS: "1000", SAKNR: "ABC", TXT50: "Odd account"}, sap.SKB1{BUKRS: "1000", SAKNR: "0000000050", TXT50: "Low"})
	e.LFB1 = append(e.LFB1, sap.LFB1{LIFNR: "0000100001", BUKRS: "1000", AKONT: "0000212000", ZTERM: "NT30"}, sap.LFB1{LIFNR: "0000100002", BUKRS: "1000", AKONT: "0000212000", ZTERM: "NT30"})
	opts := DefaultOptions()
	opts.AccountTypes = append(opts.AccountTypes, RangeRule{From: "x", To: "y", Type: config.Asset})
	c, notes := Generate(e, opts)
	all := notesText(notes)
	for _, want := range []string{"T052 not in extract", "T169G key PP not found", "not numeric", "no account type rule covers account 50",
		"reconciliation accounts", "has no journal"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing note %q in:\n%s", want, all)
		}
	}
	if c.PaymentTerm("NT30") == nil || c.Company("1000").InvoiceTolerance.PricePercent != 2 || len(c.NumberRanges) != 0 {
		t.Fatal("placeholders for missing tables")
	}
	if c.Company("1000").AccountDetermination.Bank != "100000" {
		t.Fatal("bank account falls back to the house bank G/L account")
	}
	if c.Company("1000").AccountDetermination.InputTax != "" {
		t.Fatal("no T030 means no input tax account; validation will flag it")
	}
}

// Account numbers drop SAP's ALPHA zeros as the migration's alpha does
// (internal/integration alpha_test.go has the same cases): the migration
// finds journal accounts in the configured chart by that number, so an
// all-zero account is "0" on both sides.
func TestAlphaMatchesTheMigration(t *testing.T) {
	for in, want := range map[string]string{"0000100001": "100001", "0000000000": "0", "": "", "ABC": "ABC"} {
		t.Run(in, func(t *testing.T) {
			if got := alpha(in); got != want {
				t.Errorf("got alpha(%q) = %q, want %q", in, got, want)
			}
		})
	}
	e := sap.GenerateMock(42, "2026")
	e.SKB1 = append(e.SKB1, sap.SKB1{BUKRS: "1000", SAKNR: "0000000000", TXT50: "Zero"})
	opts := DefaultOptions()
	opts.AccountTypes = append(opts.AccountTypes, RangeRule{From: "0", To: "0", Type: config.Asset})
	c, _ := Generate(e, opts)
	if a := c.Company("1000").Account("0"); a == nil || a.Name != "Zero" {
		t.Errorf("got account 0 %+v, want the all-zero SAP account", a)
	}
}

// SAP numbers that do not parse, or that the configuration cannot hold
// exactly, are reported for the reviewer instead of silently defaulted.
func TestUnparseableNumbersAreNoted(t *testing.T) {
	cases := map[string]struct {
		proz, ztag1 string
		pct, days   int
		value       string // how the notes name the value; "" means no notes
	}{
		"whole":            {"3", "30", 3, 30, ""},
		"decimal zero":     {"3.0", "030", 3, 30, ""},
		"decimal fraction": {"2.5", "2.5", 2, 0, `"2.5"`},
		"not a number":     {"abc", "abc", 2, 0, `"abc"`},
		"empty":            {"", "", 2, 0, `""`},
	}
	paths := []string{"companies[1000].invoice_tolerance.price_percent", "payment_terms[NT30].net_days"}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := sap.GenerateMock(42, "2026")
			for i := range e.T169G {
				e.T169G[i].PROZ = tc.proz
			}
			e.T052 = []sap.T052{{ZTERM: "NT30", ZTAG1: tc.ztag1, TEXT1: "Net 30 days"}}
			c, notes := Generate(e, DefaultOptions())
			if got := c.Company("1000").InvoiceTolerance.PricePercent; got != tc.pct {
				t.Errorf("got price percent %d, want %d", got, tc.pct)
			}
			if got := c.PaymentTerm("NT30").NetDays; got != tc.days {
				t.Errorf("got net days %d, want %d", got, tc.days)
			}
			all := notesText(notes)
			for _, path := range paths {
				if got, want := strings.Contains(all, path+": "), tc.value != ""; got != want {
					t.Errorf("note on %s: got %v, want %v:\n%s", path, got, want, all)
				}
			}
			if tc.value != "" && !strings.Contains(all, tc.value) {
				t.Errorf("got notes without %s, want the value named:\n%s", tc.value, all)
			}
		})
	}
}
