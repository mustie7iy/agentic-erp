package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

// reconciling sets company 1000 up to reconcile its main bank's
// statements (ADR-0047): an open-item managed clearing account, a main
// bank account and an incoming payments clearing account, a charges rule,
// every matching rule and approval tiers.
func reconciling(c *Config) {
	co := &c.Companies[0]
	co.ChartOfAccounts[0].OpenItems = true
	co.ChartOfAccounts = append(co.ChartOfAccounts,
		Account{Number: "102000", Name: "Bank - Main", Type: Asset},
		Account{Number: "103000", Name: "Bank - Incoming clearing", Type: Asset, OpenItems: true},
		Account{Number: "690000", Name: "Bank charges", Type: Expense})
	co.HouseBank.MainGLAccount, co.HouseBank.IncomingGLAccount = "102000", "103000"
	co.BankStatements = BankStatements{
		PostingRules: []StatementPostingRule{
			{TransactionCode: "PMNT-CCRD-CHRG", Description: "Charges", Account: "690000"},
			{TransactionCode: "NCHG", Description: "Charges (MT940)", Account: "690000"},
			{TransactionCode: "398", Description: "Charges (BAI)", Account: "690000"},
		},
		MatchingRules: []MatchingRule{{Kind: MatchEndToEndID}, {Kind: MatchBatchID}, {Kind: MatchTransactionCode},
			{Kind: MatchAmount, AmountTolerance: "0.50", DateWindowDays: 5, Counterparty: true}},
		ApprovalTiers: []ApprovalTier{{UpTo: "1000.00"}, {Steps: []ApprovalStep{{Role: "treasurer", Approvals: 1}}}},
	}
}

func TestBankStatementsValid(t *testing.T) {
	for name, change := range map[string]func(c *Config){
		"reconciling":                 func(*Config) {},
		"without approval tiers":      func(c *Config) { c.Companies[0].BankStatements.ApprovalTiers = nil },
		"without an incoming account": func(c *Config) { c.Companies[0].HouseBank.IncomingGLAccount = "" },
	} {
		t.Run(name, func(t *testing.T) {
			c := fixture(t)
			reconciling(c)
			change(c)
			if fs := Validate(c, currency.Seeded()); HasErrors(fs) {
				t.Fatalf("got findings %v, want none", fs)
			}
		})
	}
	t.Run("a company without statement settings renders none", func(t *testing.T) {
		b, err := fixture(t).Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "bank_statements") {
			t.Errorf("got bank_statements in\n%s, want none", b)
		}
	})
}

// Each mutation of a reconciling company must produce an error finding at
// the expected path.
func TestBankStatementRules(t *testing.T) {
	const p = "companies[1000]"
	bs := func(change func(b *BankStatements)) func(c *Config) {
		return func(c *Config) { change(&c.Companies[0].BankStatements) }
	}
	cases := []struct {
		name   string
		mutate func(c *Config)
		path   string
	}{
		{"incoming without main", func(c *Config) { c.Companies[0].HouseBank.MainGLAccount = "" }, p + ".house_bank.incoming_gl_account"},
		{"main not in the chart", func(c *Config) { c.Companies[0].HouseBank.MainGLAccount = "109999" }, p + ".house_bank.main_gl_account"},
		{"main not an asset", func(c *Config) { c.Companies[0].HouseBank.MainGLAccount = "690000" }, p + ".house_bank.main_gl_account"},
		{"main is the clearing account", func(c *Config) { c.Companies[0].HouseBank.MainGLAccount = "100000" }, p + ".house_bank.main_gl_account"},
		{"main takes tax", func(c *Config) { c.Companies[0].HouseBank.MainGLAccount = "140000" }, p + ".house_bank.main_gl_account"},
		{"incoming is the main account", func(c *Config) { c.Companies[0].HouseBank.IncomingGLAccount = "102000" }, p + ".house_bank.incoming_gl_account"},
		{"clearing not open-item managed", func(c *Config) { c.Companies[0].ChartOfAccounts[0].OpenItems = false }, p + ".house_bank.gl_account"},
		{"incoming not open-item managed", func(c *Config) { c.Companies[0].Account("103000").OpenItems = false }, p + ".house_bank.incoming_gl_account"},
		{"transaction code", bs(func(b *BankStatements) { b.PostingRules[0].TransactionCode = "pmnt-ccrd" }), p + ".bank_statements.posting_rules[pmnt-ccrd].transaction_code"},
		{"duplicate transaction code", bs(func(b *BankStatements) { b.PostingRules[1].TransactionCode = "398" }), p + ".bank_statements.posting_rules[398].transaction_code"},
		{"rule account not in the chart", bs(func(b *BankStatements) { b.PostingRules[0].Account = "999999" }), p + ".bank_statements.posting_rules[PMNT-CCRD-CHRG].account"},
		{"rule on a reconciliation account", bs(func(b *BankStatements) { b.PostingRules[0].Account = "211000" }), p + ".bank_statements.posting_rules[PMNT-CCRD-CHRG].account"},
		{"rule on a tax account", bs(func(b *BankStatements) { b.PostingRules[0].Account = "140000" }), p + ".bank_statements.posting_rules[PMNT-CCRD-CHRG].account"},
		{"rule on a bank's account", bs(func(b *BankStatements) { b.PostingRules[0].Account = "102000" }), p + ".bank_statements.posting_rules[PMNT-CCRD-CHRG].account"},
		{"matching kind", bs(func(b *BankStatements) { b.MatchingRules[0].Kind = "fuzzy" }), p + ".bank_statements.matching_rules[fuzzy].kind"},
		{"duplicate matching rule", bs(func(b *BankStatements) { b.MatchingRules[1].Kind = MatchEndToEndID }), p + ".bank_statements.matching_rules[end_to_end_id]"},
		{"tolerance on another rule", bs(func(b *BankStatements) { b.MatchingRules[0].DateWindowDays = 3 }), p + ".bank_statements.matching_rules[end_to_end_id]"},
		{"date window", bs(func(b *BankStatements) { b.MatchingRules[3].DateWindowDays = 32 }), p + ".bank_statements.matching_rules[amount].date_window_days"},
		{"negative window", bs(func(b *BankStatements) { b.MatchingRules[3].DateWindowDays = -1 }), p + ".bank_statements.matching_rules[amount].date_window_days"},
		{"tolerance", bs(func(b *BankStatements) { b.MatchingRules[3].AmountTolerance = "0.001" }), p + ".bank_statements.matching_rules[amount].amount_tolerance"},
		{"code rule without posting rules", bs(func(b *BankStatements) { b.PostingRules = nil }), p + ".bank_statements.matching_rules[transaction_code]"},
		{"tier without up_to", bs(func(b *BankStatements) { b.ApprovalTiers[0].UpTo = "" }), p + ".bank_statements.approval_tiers[0].up_to"},
		{"last tier with up_to", bs(func(b *BankStatements) { b.ApprovalTiers[1].UpTo = "5000.00" }), p + ".bank_statements.approval_tiers[1].up_to"},
		{"tier amount", bs(func(b *BankStatements) { b.ApprovalTiers[0].UpTo = "1000.001" }), p + ".bank_statements.approval_tiers[0].up_to"},
		{"tiers out of order", bs(func(b *BankStatements) {
			b.ApprovalTiers = append([]ApprovalTier{{UpTo: "2000.00"}}, b.ApprovalTiers...)
		}), p + ".bank_statements.approval_tiers[1].up_to"},
		{"tier step", bs(func(b *BankStatements) { b.ApprovalTiers[1].Steps[0].Approvals = 0 }), p + ".bank_statements.approval_tiers[1].steps[0].approvals"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fixture(t)
			reconciling(c)
			tc.mutate(c)
			fs := Validate(c, currency.Seeded())
			if !slices.ContainsFunc(fs, func(f Finding) bool { return f.Severity == SevError && f.Path == tc.path }) {
				t.Fatalf("no error at %s; got %v", tc.path, fs)
			}
		})
	}
}

// An unknown currency reads no amounts, so its tiers and tolerance add no
// findings of their own beside the currency's.
func TestBankStatementsUnknownCurrency(t *testing.T) {
	c := fixture(t)
	reconciling(c)
	c.Companies[0].Currency = "XXX"
	c.Companies[0].BankStatements.ApprovalTiers[0].UpTo = "lots"
	c.Companies[0].BankStatements.MatchingRules[3].AmountTolerance = "lots"
	for _, f := range Validate(c, currency.Seeded()) {
		if strings.Contains(f.Path, "bank_statements") {
			t.Errorf("finding %v with an unknown currency, want none under bank_statements", f)
		}
	}
}

func TestValidTransactionCode(t *testing.T) {
	for code, want := range map[string]bool{
		"PMNT-ICDT-ESCT": true, "NCHG": true, "S202": true, "FMSC": true, "398": true, "MyBank01": true,
		"": false, "PMNT-ICDT": false, "PMNT/ICDT/ESCT": false, "N CHG": false, strings.Repeat("A", 36): false,
	} {
		t.Run(code, func(t *testing.T) {
			if got := ValidTransactionCode(code); got != want {
				t.Errorf("ValidTransactionCode(%q) = %v, want %v", code, got, want)
			}
		})
	}
}

func TestBankStatementDiffTiers(t *testing.T) {
	old := fixture(t)
	reconciling(old)
	changed := fixture(t)
	reconciling(changed)
	changed.Companies[0].BankStatements.PostingRules[0].Description = "Bank charges"
	changed.Companies[0].BankStatements.MatchingRules[3].DateWindowDays = 7
	changes, err := Diff(old, changed)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Tier{
		"companies[1000].bank_statements.posting_rules[PMNT-CCRD-CHRG].description": TierCosmetic,
		"companies[1000].bank_statements.matching_rules[amount].date_window_days":   TierPostingRelated,
	}
	if len(changes) != len(want) {
		t.Fatalf("changes %v, want %d", changes, len(want))
	}
	for _, ch := range changes {
		if ch.Tier != want[ch.Path] {
			t.Errorf("%s: tier %v, want %v", ch.Path, ch.Tier, want[ch.Path])
		}
	}
}
