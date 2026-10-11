package config

import (
	"os"
	"slices"
	"strings"
	"testing"
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
	if fs := Validate(fixture(t)); HasErrors(fs) {
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
		{"over delivery", func(c *Config) { c.Companies[0].OverDeliveryPercent = 101 }, "companies[1000].gr_over_delivery_percent"},
		{"iban", func(c *Config) { c.Companies[0].HouseBank.IBAN = "GB33BUKB20201555555556" }, "companies[1000].house_bank.iban"},
		{"bic", func(c *Config) { c.Companies[0].HouseBank.BIC = "BUK" }, "companies[1000].house_bank.bic"},
		{"terms key", func(c *Config) { c.PaymentTerms[0].Key = "net thirty days!" }, "payment_terms[net thirty days!]"},
		{"terms days", func(c *Config) { c.PaymentTerms[0].NetDays = 400 }, "payment_terms[NT14].net_days"},
		{"no terms", func(c *Config) { c.PaymentTerms = nil }, "payment_terms"},
		{"tiers empty", func(c *Config) { c.Purchasing.RequisitionApproval = nil }, "purchasing.requisition_approval"},
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
			fs := Validate(c)
			if !slices.ContainsFunc(fs, func(f Finding) bool { return f.Severity == SevError && f.Path == tc.path }) {
				t.Fatalf("no error at %s; got %v", tc.path, fs)
			}
		})
	}
}

func TestValidationWarnings(t *testing.T) {
	c := fixture(t)
	c.Companies[0].TaxCodes = nil
	fs := Validate(c)
	if HasErrors(fs) {
		t.Fatalf("warnings only expected: %v", fs)
	}
	if len(fs) != 1 || fs[0].String() == "" {
		t.Fatalf("want 1 warning, got %v", fs)
	}
}

func TestValidIBAN(t *testing.T) {
	for iban, want := range map[string]bool{
		"GB33BUKB20201555555555": true, "DE89 3704 0044 0532 0130 00": true,
		"GB33BUKB20201555555556": false, "XX": false, "1234567890123456": false, "GB33BUKB2020155555555!": false,
	} {
		if ValidIBAN(iban) != want {
			t.Errorf("ValidIBAN(%q) != %v", iban, want)
		}
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
		{"hold days", func(c *Config) { c.Payments.BankChangeHoldDays = 10 }, "payments.bank_change_hold_days", TierOperational},
		{"output", func(c *Config) { c.Output.PurchaseOrderChannel = "edi" }, "output.purchase_order_channel", TierOperational},
		{"account determination", func(c *Config) { c.Companies[0].AccountDetermination.InputTax = "100000" }, "companies[1000].account_determination.input_tax", TierPostingRelated},
		{"auto limit", func(c *Config) { c.Companies[0].AutoPostLimit = "75000.00" }, "companies[1000].auto_post_limit", TierPostingRelated},
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
			if len(changes) != 1 || changes[0].Path != tc.path || changes[0].Tier != tc.tier {
				t.Fatalf("want one change %s at %v, got %v", tc.path, tc.tier, changes)
			}
			if changes[0].String() == "" || MaxTier(changes) != tc.tier {
				t.Fatal("String/MaxTier")
			}
		})
	}
	// Removal is reported at the item.
	c := fixture(t)
	c.PaymentTerms = c.PaymentTerms[1:]
	changes, _ := Diff(base, c)
	if len(changes) != 1 || changes[0].Path != "payment_terms[NT14]" || changes[0].Old != "(removed)" || !strings.HasPrefix(changes[0].String(), "-") {
		t.Fatalf("removal: %v", changes)
	}
	if changes, _ := Diff(base, fixture(t)); len(changes) != 0 || MaxTier(changes) != 0 {
		t.Fatal("identical configs must not differ")
	}
	if Tier(0).String() != "no change" || TierStructural.String() == "" {
		t.Fatal("tier names")
	}
	if classify("unknown.path") != TierPostingRelated {
		t.Fatal("unmatched paths default to posting-relevant")
	}
}

func TestRuntime(t *testing.T) {
	c := fixture(t)
	if ApprovalSteps(TierCosmetic) != nil || len(ApprovalSteps(TierOperational)) != 1 || len(ApprovalSteps(TierPostingRelated)) != 2 ||
		ApprovalSteps(TierStructural)[0].Role != "solution_architect" {
		t.Fatal("approval steps by tier")
	}
	if RequiredEvidence(TierOperational) != nil || len(RequiredEvidence(TierPostingRelated)) != 1 || RequiredEvidence(TierStructural)[0] != "simulation" {
		t.Fatal("evidence by tier")
	}
	if got := strings.Join(c.RoleNames(), ","); got != "Z_PURCH_MGR,process_owner,Z_TREASURY_MGR,vendor_master_approver,controller,solution_architect" {
		t.Fatalf("role names: %s", got)
	}
	if c.PaymentTerm("NT30") == nil || c.PaymentTerm("X") != nil {
		t.Fatal("lookups")
	}
}
