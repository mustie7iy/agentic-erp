package config

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

func fixtureYAML(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/valid.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fixture(t *testing.T) *Config {
	t.Helper()
	c, err := Parse(fixtureYAML(t))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestFixtureIsValid(t *testing.T) {
	if fs := Validate(fixture(t), currency.Seeded()); HasErrors(fs) {
		t.Fatalf("fixture invalid: %v", fs)
	}
}

func TestParseIsStrict(t *testing.T) {
	if _, err := Parse([]byte("version: 1\ncompnies: []\n")); err == nil {
		t.Fatal("misspelled field must be rejected")
	}
	if _, err := Parse([]byte("version: [")); err == nil {
		t.Fatal("broken YAML must be rejected")
	}
	if _, err := Load("testdata/missing.yaml"); err == nil {
		t.Fatal("missing file must error")
	}
	if c, err := Load("testdata/valid.yaml"); err != nil || c.Company("1000") == nil {
		t.Fatalf("load: %v", err)
	}
}

// Each mutation must produce an error finding at the expected path.
func TestValidationRules(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(c *Config)
		path   string
	}{
		{"version", func(c *Config) { c.Version = 2 }, "version"},
		{"no companies", func(c *Config) { c.Companies = nil }, "companies"},
		{"duplicate company", func(c *Config) { c.Companies = append(c.Companies, c.Companies[0]) }, "companies[1000]"},
		{"empty code", func(c *Config) { c.Companies[0].Code = "" }, "companies[0].code"},
		{"name", func(c *Config) { c.Companies[0].Name = " " }, "companies[1000].name"},
		{"currency", func(c *Config) { c.Companies[0].Currency = "usd" }, "companies[1000].currency"},
		{"country", func(c *Config) { c.Companies[0].Country = "USA" }, "companies[1000].country"},
		{"fiscal", func(c *Config) { c.Companies[0].FiscalYearStartMonth = 13 }, "companies[1000].fiscal_year_start_month"},
		{"account number", func(c *Config) { c.Companies[0].ChartOfAccounts[0].Number = "12-34" }, "companies[1000].chart_of_accounts[12-34]"},
		{"duplicate account", func(c *Config) {
			co := &c.Companies[0]
			co.ChartOfAccounts = append(co.ChartOfAccounts, co.ChartOfAccounts[0])
		}, "companies[1000].chart_of_accounts[100000]"},
		{"account type", func(c *Config) { c.Companies[0].ChartOfAccounts[0].Type = "cash" }, "companies[1000].chart_of_accounts[100000].type"},
		{"recon must be liability", func(c *Config) { c.Companies[0].ChartOfAccounts[0].VendorReconciliation = true }, "companies[1000].chart_of_accounts[100000]"},
		{"empty chart", func(c *Config) { c.Companies[0].ChartOfAccounts = nil }, "companies[1000].chart_of_accounts"},
		{"no recon", func(c *Config) {
			for i := range c.Companies[0].ChartOfAccounts {
				c.Companies[0].ChartOfAccounts[i].VendorReconciliation = false
			}
		}, "companies[1000].chart_of_accounts"},
		{"ap missing", func(c *Config) { c.Companies[0].AccountDetermination.APReconciliation = "" }, "companies[1000].account_determination.ap_reconciliation"},
		{"ap unknown", func(c *Config) { c.Companies[0].AccountDetermination.APReconciliation = "999999" }, "companies[1000].account_determination.ap_reconciliation"},
		{"input tax wrong type", func(c *Config) { c.Companies[0].AccountDetermination.InputTax = "400000" }, "companies[1000].account_determination.input_tax"},
		{"fx gain unknown", func(c *Config) { c.Companies[0].AccountDetermination.FXGain = "999999" }, "companies[1000].account_determination.fx_gain"},
		{"fx loss wrong type", func(c *Config) { c.Companies[0].AccountDetermination.FXLoss = "400000" }, "companies[1000].account_determination.fx_loss"},
		{"ap not recon", func(c *Config) {
			c.Companies[0].ChartOfAccounts = append(c.Companies[0].ChartOfAccounts, Account{Number: "212000", Name: "Other", Type: Liability})
			c.Companies[0].AccountDetermination.APReconciliation = "212000"
		}, "companies[1000].account_determination.ap_reconciliation"},
		{"tax duplicate", func(c *Config) {
			co := &c.Companies[0]
			co.TaxCodes = append(co.TaxCodes, co.TaxCodes[0])
		}, "companies[1000].tax_codes[V1]"},
		{"tax rate", func(c *Config) { c.Companies[0].TaxCodes[0].RatePercent = "101" }, "companies[1000].tax_codes[V1].rate_percent"},
		{"tax rate parse", func(c *Config) { c.Companies[0].TaxCodes[0].RatePercent = "eight" }, "companies[1000].tax_codes[V1].rate_percent"},
		{"tax account", func(c *Config) { c.Companies[0].TaxCodes[0].Account = "1" }, "companies[1000].tax_codes[V1].account"},
		{"tolerance pct", func(c *Config) { c.Companies[0].InvoiceTolerance.PricePercent = -1 }, "companies[1000].invoice_tolerance.price_percent"},
		{"tolerance abs", func(c *Config) { c.Companies[0].InvoiceTolerance.PriceAbsolute = "-5" }, "companies[1000].invoice_tolerance.price_absolute"},
		{"limit", func(c *Config) { c.Companies[0].AutoPostLimit = "lots" }, "companies[1000].auto_post_limit"},
		{"manual entry limit", func(c *Config) { c.Companies[0].ManualEntryApprovalLimit = "lots" }, "companies[1000].manual_entry_approval_limit"},
		{"manual entry limit missing", func(c *Config) { c.Companies[0].ManualEntryApprovalLimit = "" }, "companies[1000].manual_entry_approval_limit"},
		{"manual entry limit negative", func(c *Config) { c.Companies[0].ManualEntryApprovalLimit = "-1.00" }, "companies[1000].manual_entry_approval_limit"},
		{"over delivery", func(c *Config) { c.Companies[0].OverDeliveryPercent = 101 }, "companies[1000].gr_over_delivery_percent"},
		{"iban", func(c *Config) { c.Companies[0].HouseBank.IBAN = "GB33BUKB20201555555556" }, "companies[1000].house_bank.iban"},
		{"bic", func(c *Config) { c.Companies[0].HouseBank.BIC = "BUK" }, "companies[1000].house_bank.bic"},
		{"terms key", func(c *Config) { c.PaymentTerms[0].Key = "net thirty days!" }, "payment_terms[net thirty days!]"},
		{"terms days", func(c *Config) { c.PaymentTerms[0].NetDays = 400 }, "payment_terms[NT14].net_days"},
		{"no terms", func(c *Config) { c.PaymentTerms = nil }, "payment_terms"},
		{"tiers empty", func(c *Config) { c.Purchasing.RequisitionApproval = nil }, "purchasing.requisition_approval"},
		{"order tiers empty", func(c *Config) { c.Purchasing.OrderReleaseApproval = nil }, "purchasing.order_release_approval"},
		{"order last tier capped", func(c *Config) { c.Purchasing.OrderReleaseApproval[2].UpTo = "1000000.00" }, "purchasing.order_release_approval[2].up_to"},
		{"order tier role", func(c *Config) { c.Purchasing.OrderReleaseApproval[1].Steps[0].Role = " " }, "purchasing.order_release_approval[1].steps[0].role"},
		{"no expiry notice", func(c *Config) { c.Purchasing.ContractExpiryNoticeDays = 0 }, "purchasing.contract_expiry_notice_days"},
		{"expiry notice too long", func(c *Config) { c.Purchasing.ContractExpiryNoticeDays = 366 }, "purchasing.contract_expiry_notice_days"},
		{"last tier capped", func(c *Config) { c.Purchasing.RequisitionApproval[1].UpTo = "1000000.00" }, "purchasing.requisition_approval[1].up_to"},
		{"middle tier open", func(c *Config) { c.Purchasing.RequisitionApproval[0].UpTo = "" }, "purchasing.requisition_approval[0].up_to"},
		{"tier order", func(c *Config) {
			r := c.Purchasing.RequisitionApproval
			c.Purchasing.RequisitionApproval = []ApprovalTier{{UpTo: "100.00", Steps: r[0].Steps}, {UpTo: "50.00", Steps: r[0].Steps}, r[1]}
		}, "purchasing.requisition_approval[1].up_to"},
		{"tier amount", func(c *Config) { c.Purchasing.RequisitionApproval[0].UpTo = "x" }, "purchasing.requisition_approval[0].up_to"},
		{"tier role", func(c *Config) { c.Purchasing.RequisitionApproval[0].Steps[0].Role = " " }, "purchasing.requisition_approval[0].steps[0].role"},
		{"tier approvals", func(c *Config) { c.Purchasing.RequisitionApproval[0].Steps[0].Approvals = 0 }, "purchasing.requisition_approval[0].steps[0].approvals"},
		{"release role", func(c *Config) { c.Payments.ReleaseRole = "" }, "payments.release_role"},
		{"release approvals", func(c *Config) { c.Payments.ReleaseApprovals = 0 }, "payments.release_approvals"},
		{"hold days", func(c *Config) { c.Payments.BankChangeHoldDays = -1 }, "payments.bank_change_hold_days"},
		{"output", func(c *Config) { c.Output.PurchaseOrderChannel = "fax" }, "output.purchase_order_channel"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fixture(t)
			tc.mutate(c)
			fs := Validate(c, currency.Seeded())
			if !slices.ContainsFunc(fs, func(f Finding) bool { return f.Severity == SevError && f.Path == tc.path }) {
				t.Fatalf("no error at %s; got %v", tc.path, fs)
			}
		})
	}
}

func TestValidationWarnings(t *testing.T) {
	c := fixture(t)
	c.Companies[0].TaxCodes = nil
	fs := Validate(c, currency.Seeded())
	if HasErrors(fs) {
		t.Fatalf("warnings only expected: %v", fs)
	}
	if len(fs) != 1 {
		t.Fatalf("warnings: got %v, want 1", fs)
	}
	if fs[0].String() == "" {
		t.Fatalf("warning text: got empty, want a description of %#v", fs[0])
	}
}

func TestValidIBAN(t *testing.T) {
	for iban, want := range map[string]bool{
		"GB33BUKB20201555555555": true, "DE89 3704 0044 0532 0130 00": true,
		"GB33BUKB20201555555556": false, "XX": false, "1234567890123456": false, "GB33BUKB2020155555555!": false,
	} {
		t.Run(iban, func(t *testing.T) {
			if got := ValidIBAN(iban); got != want {
				t.Errorf("ValidIBAN(%q): got %v, want %v", iban, got, want)
			}
		})
	}
}

func TestDiffTiers(t *testing.T) {
	base := fixture(t)
	cases := []struct {
		name   string
		mutate func(c *Config)
		path   string
		tier   Tier
	}{
		{"rename company", func(c *Config) { c.Companies[0].Name = "Acme Inc" }, "companies[1000].name", TierCosmetic},
		{"account name", func(c *Config) { c.Companies[0].ChartOfAccounts[0].Name = "Bank" }, "companies[1000].chart_of_accounts[100000].name", TierCosmetic},
		{"tolerance", func(c *Config) { c.Companies[0].InvoiceTolerance.PricePercent = 3 }, "companies[1000].invoice_tolerance.price_percent", TierOperational},
		{"terms", func(c *Config) { c.PaymentTerms[0].NetDays = 21 }, "payment_terms[NT14].net_days", TierOperational},
		{"terms text", func(c *Config) { c.PaymentTerms[0].Description = "Two weeks" }, "payment_terms[NT14].description", TierCosmetic},
		{"routing", func(c *Config) { c.Purchasing.RequisitionApproval[0].UpTo = "5000.00" }, "purchasing.requisition_approval[0].up_to", TierOperational},
		{"order routing", func(c *Config) { c.Purchasing.OrderReleaseApproval[0].UpTo = "7500.00" }, "purchasing.order_release_approval[0].up_to", TierOperational},
		{"expiry notice", func(c *Config) { c.Purchasing.ContractExpiryNoticeDays = 45 }, "purchasing.contract_expiry_notice_days", TierOperational},
		{"hold days", func(c *Config) { c.Payments.BankChangeHoldDays = 10 }, "payments.bank_change_hold_days", TierOperational},
		{"output", func(c *Config) { c.Output.PurchaseOrderChannel = "edi" }, "output.purchase_order_channel", TierOperational},
		{"account determination", func(c *Config) { c.Companies[0].AccountDetermination.InputTax = "100000" }, "companies[1000].account_determination.input_tax", TierPostingRelated},
		{"auto limit", func(c *Config) { c.Companies[0].AutoPostLimit = "75000.00" }, "companies[1000].auto_post_limit", TierPostingRelated},
		{"manual entry limit", func(c *Config) { c.Companies[0].ManualEntryApprovalLimit = "25000.00" }, "companies[1000].manual_entry_approval_limit", TierPostingRelated},
		{"release role", func(c *Config) { c.Payments.ReleaseRole = "controller" }, "payments.release_role", TierPostingRelated},
		{"currency", func(c *Config) { c.Companies[0].Currency = "CAD" }, "companies[1000].currency", TierStructural},
		{"new account", func(c *Config) {
			c.Companies[0].ChartOfAccounts = append(c.Companies[0].ChartOfAccounts, Account{Number: "620000", Name: "Rent", Type: Expense})
		}, "companies[1000].chart_of_accounts[620000]", TierStructural},
		{"new company", func(c *Config) {
			n := c.Companies[0]
			n.Code = "3000"
			c.Companies = append(c.Companies, n)
		}, "companies[3000]", TierStructural},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fixture(t)
			tc.mutate(c)
			changes, err := Diff(base, c)
			if err != nil {
				t.Fatal(err)
			}
			if len(changes) != 1 {
				t.Fatalf("changes: got %v, want one change at %s", changes, tc.path)
			}
			if changes[0].Path != tc.path {
				t.Errorf("path: got %s, want %s", changes[0].Path, tc.path)
			}
			if changes[0].Tier != tc.tier {
				t.Errorf("tier: got %v, want %v", changes[0].Tier, tc.tier)
			}
			if changes[0].String() == "" {
				t.Errorf("String: got empty, want a description of %#v", changes[0])
			}
			if got := MaxTier(changes); got != tc.tier {
				t.Errorf("MaxTier: got %v, want %v", got, tc.tier)
			}
		})
	}
	// Removal is reported at the item.
	c := fixture(t)
	c.PaymentTerms = c.PaymentTerms[1:]
	changes, _ := Diff(base, c)
	if len(changes) != 1 {
		t.Fatalf("removal changes: got %v, want 1", changes)
	}
	if got := changes[0].Path; got != "payment_terms[NT14]" {
		t.Errorf("removal path: got %s, want payment_terms[NT14]", got)
	}
	if got := changes[0].Old; got != "(removed)" {
		t.Errorf("removal old: got %s, want (removed)", got)
	}
	if got := changes[0].String(); !strings.HasPrefix(got, "-") {
		t.Errorf("removal String: got %q, want a - prefix", got)
	}
	same, _ := Diff(base, fixture(t))
	if len(same) != 0 {
		t.Errorf("identical configs: got %v, want no changes", same)
	}
	if got := MaxTier(same); got != 0 {
		t.Errorf("identical configs MaxTier: got %v, want 0", got)
	}
	if got := Tier(0).String(); got != "no change" {
		t.Errorf("Tier(0): got %q, want no change", got)
	}
	if TierStructural.String() == "" {
		t.Error("TierStructural: got empty name, want one")
	}
	if classify("unknown.path") != TierPostingRelated {
		t.Fatal("unmatched paths default to posting-relevant")
	}
}

func TestRuntime(t *testing.T) {
	c := fixture(t)
	if got := ApprovalSteps(TierCosmetic); got != nil {
		t.Errorf("cosmetic approval steps: got %v, want none", got)
	}
	if got := len(ApprovalSteps(TierOperational)); got != 1 {
		t.Errorf("operational approval steps: got %d, want 1", got)
	}
	if got := len(ApprovalSteps(TierPostingRelated)); got != 2 {
		t.Errorf("posting-relevant approval steps: got %d, want 2", got)
	}
	if got := ApprovalSteps(TierStructural)[0].Role; got != "solution_architect" {
		t.Errorf("structural first approver: got %s, want solution_architect", got)
	}
	if got := RequiredEvidence(TierOperational); got != nil {
		t.Errorf("operational evidence: got %v, want none", got)
	}
	if got := len(RequiredEvidence(TierPostingRelated)); got != 2 {
		t.Errorf("posting-relevant evidence: got %d, want 2", got)
	}
	if got := RequiredEvidence(TierStructural)[1]; got != "regression" {
		t.Errorf("structural second evidence: got %s, want regression", got)
	}
	if got := strings.Join(c.RoleNames(), ","); got != "Z_PURCH_MGR,process_owner,Z_PURCH_HEAD,Z_TREASURY_MGR,vendor_master_approver,controller,solution_architect" {
		t.Fatalf("role names: %s", got)
	}
	if c.PaymentTerm("NT30") == nil {
		t.Error("PaymentTerm(NT30): got nil, want the term")
	}
	if got := c.PaymentTerm("X"); got != nil {
		t.Errorf("PaymentTerm(X): got %v, want nil", got)
	}
}

// withGRIR gives company 1000 a GR/IR account and a price difference
// account, with the goods receipt and WE journal ranges of 2026.
func withGRIR(t *testing.T) *Config {
	t.Helper()
	c := fixture(t)
	co := &c.Companies[0]
	co.ChartOfAccounts = append(co.ChartOfAccounts, Account{Number: "219000", Name: "GR/IR clearing", Type: Liability},
		Account{Number: "590000", Name: "Price differences", Type: Expense})
	co.AccountDetermination.GRIR, co.AccountDetermination.PriceDifference = "219000", "590000"
	c.NumberRanges = append(c.NumberRanges, NumberRange{Company: "1000", DocumentType: "goods_receipt", FiscalYear: 2026, First: 1, Last: 9},
		NumberRange{Company: "1000", DocumentType: "journal_we", FiscalYear: 2026, First: 1, Last: 9})
	return c
}

// TestGRIRDetermination: the GR/IR account is a liability that is not a
// reconciliation account, it comes with an expense account for price
// differences, and a company that values receipts through it has a WE
// journal range in every fiscal year it numbers goods receipts in.
func TestGRIRDetermination(t *testing.T) {
	if fs := Validate(withGRIR(t), currency.Seeded()); HasErrors(fs) {
		t.Fatalf("GR/IR configured: got %v, want no errors", fs)
	}
	cases := []struct {
		name   string
		mutate func(c *Config)
		path   string
	}{
		{"gr_ir unknown", func(c *Config) { c.Companies[0].AccountDetermination.GRIR = "999999" }, "companies[1000].account_determination.gr_ir"},
		{"gr_ir an expense", func(c *Config) { c.Companies[0].AccountDetermination.GRIR = "520000" }, "companies[1000].account_determination.gr_ir"},
		{"gr_ir a reconciliation account", func(c *Config) { c.Companies[0].AccountDetermination.GRIR = "211000" }, "companies[1000].account_determination.gr_ir"},
		{"gr_ir without price difference", func(c *Config) { c.Companies[0].AccountDetermination.PriceDifference = "" }, "companies[1000].account_determination.price_difference"},
		{"price difference a revenue", func(c *Config) { c.Companies[0].AccountDetermination.PriceDifference = "400000" }, "companies[1000].account_determination.price_difference"},
		{"tax code on GR/IR", func(c *Config) { c.Companies[0].TaxCodes[0].Account = "219000" }, "companies[1000].tax_codes[V1].account"},
		{"no WE journal range", func(c *Config) { c.NumberRanges = c.NumberRanges[:len(c.NumberRanges)-1] }, "number_ranges[1000/journal_we/2026]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := withGRIR(t)
			tc.mutate(c)
			fs := Validate(c, currency.Seeded())
			if !slices.ContainsFunc(fs, func(f Finding) bool { return f.Severity == SevError && f.Path == tc.path }) {
				t.Fatalf("no error at %s; got %v", tc.path, fs)
			}
		})
	}
	t.Run("price difference alone", func(t *testing.T) {
		c := fixture(t)
		c.Companies[0].AccountDetermination.PriceDifference = "520000"
		if fs := Validate(c, currency.Seeded()); HasErrors(fs) {
			t.Fatalf("price difference without GR/IR: got %v, want no errors", fs)
		}
	})
}

// TestCreditMemoJournalRanges: a company that numbers supplier invoices in
// a fiscal year numbers their KG journal entries too (journal_kg): credit
// memos and subsequent credits post as KG (T34c): a missing range is an
// error.
func TestCreditMemoJournalRanges(t *testing.T) {
	c := fixture(t)
	c.NumberRanges = []NumberRange{{Company: "1000", DocumentType: "supplier_invoice", FiscalYear: 2026, First: 1, Last: 9},
		{Company: "1000", DocumentType: "journal_kg", FiscalYear: 2026, First: 1, Last: 9}}
	missing := func(f Finding) bool { return f.Path == "number_ranges[1000/journal_kg/2026]" }
	if fs := Validate(c, currency.Seeded()); slices.ContainsFunc(fs, missing) {
		t.Errorf("with journal_kg: got %v, want no finding on it", fs)
	}
	c.NumberRanges = c.NumberRanges[:1]
	fs := Validate(c, currency.Seeded())
	if !slices.ContainsFunc(fs, func(f Finding) bool { return missing(f) && f.Severity == SevError }) {
		t.Errorf("without journal_kg: got %v, want an error at number_ranges[1000/journal_kg/2026]", fs)
	}
}
