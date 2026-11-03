package config

import (
	"bytes"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

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
	c := fixture(t)
	furtherBank("101000")(c)
	if fs := Validate(c, currency.Seeded()); HasErrors(fs) {
		t.Fatalf("with a further house bank: %v", fs)
	}
	c = fixture(t)
	canadianBank(func(*HouseBank) {})(c)
	if fs := Validate(c, currency.Seeded()); HasErrors(fs) {
		t.Fatalf("with a Canadian house bank: %v", fs)
	}
}

// canadianBank adds a further house bank at a Canadian bank (T45d) to
// company 2000, changed by mutate.
func canadianBank(mutate func(hb *HouseBank)) func(c *Config) {
	return func(c *Config) {
		furtherBank("101000")(c)
		hb := &c.Companies[1].HouseBanks[0]
		hb.IBAN, hb.BIC, hb.InstitutionNumber, hb.TransitNumber, hb.AccountNumber = "", "ROYCCAT2", "003", "00002", "100-0001"
		mutate(hb)
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
		{"pack of no company", adopt(PackAdoption{Company: "9000"}), "packs[9000].company"},
		{"pack of another country", adopt(PackAdoption{Country: "CA"}), "packs[1000].country"},
		{"pack version", adopt(PackAdoption{Version: "2026"}), "packs[1000].version"},
		{"pack applied on", adopt(PackAdoption{AppliedOn: "2026-1-5"}), "packs[1000].applied_on"},
		{"pack adopted twice", func(c *Config) { adopt(PackAdoption{})(c); c.Packs = append(c.Packs, c.Packs[0]) }, "packs[1000]"},
		{"calendar of no company", holidays("9000", Holiday{Date: "2026-01-01", Name: "New Year"}), "calendars[9000].company"},
		{"calendar twice", func(c *Config) { holidays("1000")(c); c.Calendars = append(c.Calendars, c.Calendars[0]) }, "calendars[1000]"},
		{"holiday date", holidays("1000", Holiday{Date: "2026-02-30", Name: "None"}), "calendars[1000].holidays[2026-02-30].date"},
		{"holiday date unpadded", holidays("1000", Holiday{Date: "2026-1-1", Name: "New Year"}), "calendars[1000].holidays[2026-1-1].date"},
		{"holiday twice", holidays("1000", Holiday{Date: "2026-01-01", Name: "New Year"}, Holiday{Date: "2026-01-01", Name: "Again"}), "calendars[1000].holidays[2026-01-01]"},
		{"holiday name", holidays("1000", Holiday{Date: "2026-01-01", Name: " "}), "calendars[1000].holidays[2026-01-01].name"},
		{"no companies", func(c *Config) { c.Companies = nil }, "companies"},
		{"duplicate company", func(c *Config) { c.Companies = append(c.Companies, c.Companies[0]) }, "companies[1000]"},
		{"empty code", func(c *Config) { c.Companies[0].Code = "" }, "companies[0].code"},
		{"name", func(c *Config) { c.Companies[0].Name = " " }, "companies[1000].name"},
		{"currency", func(c *Config) { c.Companies[0].Currency = "usd" }, "companies[1000].currency"},
		{"country", func(c *Config) { c.Companies[0].Country = "USA" }, "companies[1000].country"},
		{"fiscal", func(c *Config) { c.Companies[0].FiscalYearStartMonth = 13 }, "companies[1000].fiscal_year_start_month"},
		{"time zone unknown", func(c *Config) { c.Companies[0].TimeZone = "America/Montreal_Nowhere" }, "companies[1000].time_zone"},
		{"time zone of the host", func(c *Config) { c.Companies[0].TimeZone = "Local" }, "companies[1000].time_zone"},
		{"time zone of the host's file", func(c *Config) { c.Companies[0].TimeZone = "localtime" }, "companies[1000].time_zone"},
		{"time zone posixrules", func(c *Config) { c.Companies[0].TimeZone = "posixrules" }, "companies[1000].time_zone"},
		{"time zone of the posix tree", func(c *Config) { c.Companies[0].TimeZone = "posix/America/Toronto" }, "companies[1000].time_zone"},
		{"time zone of the right tree", func(c *Config) { c.Companies[0].TimeZone = "right/America/Toronto" }, "companies[1000].time_zone"},
		{"time zone in lower case", func(c *Config) { c.Companies[0].TimeZone = "america/toronto" }, "companies[1000].time_zone"},
		{"time zone an offset", func(c *Config) { c.Companies[0].TimeZone = "-05:00" }, "companies[1000].time_zone"},
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
		{"tax tolerance", func(c *Config) { u := -1; c.Companies[0].InvoiceTolerance.TaxUnitsPerCode = &u }, "companies[1000].invoice_tolerance.tax_units_per_code"},
		{"tolerance abs", func(c *Config) { c.Companies[0].InvoiceTolerance.PriceAbsolute = "-5" }, "companies[1000].invoice_tolerance.price_absolute"},
		{"limit", func(c *Config) { c.Companies[0].AutoPostLimit = "lots" }, "companies[1000].auto_post_limit"},
		{"manual entry limit", func(c *Config) { c.Companies[0].ManualEntryApprovalLimit = "lots" }, "companies[1000].manual_entry_approval_limit"},
		{"manual entry limit missing", func(c *Config) { c.Companies[0].ManualEntryApprovalLimit = "" }, "companies[1000].manual_entry_approval_limit"},
		{"manual entry limit negative", func(c *Config) { c.Companies[0].ManualEntryApprovalLimit = "-1.00" }, "companies[1000].manual_entry_approval_limit"},
		{"release dual limit", func(c *Config) { c.Companies[0].ReleaseDualLimit = "lots" }, "companies[1000].release_dual_limit"},
		{"release dual limit negative", func(c *Config) { c.Companies[0].ReleaseDualLimit = "-1.00" }, "companies[1000].release_dual_limit"},
		{"release dual limit digits", func(c *Config) { c.Companies[0].ReleaseDualLimit = "1000.001" }, "companies[1000].release_dual_limit"},
		{"service posting limit", func(c *Config) { c.Companies[0].ServicePostingLimit = "lots" }, "companies[1000].service_posting_limit"},
		{"service posting limit negative", func(c *Config) { c.Companies[0].ServicePostingLimit = "-1.00" }, "companies[1000].service_posting_limit"},
		{"service posting limit digits", func(c *Config) { c.Companies[0].ServicePostingLimit = "1000.001" }, "companies[1000].service_posting_limit"},
		{"over delivery", func(c *Config) { c.Companies[0].OverDeliveryPercent = 101 }, "companies[1000].gr_over_delivery_percent"},
		{"iban", func(c *Config) { c.Companies[0].HouseBank.IBAN = "GB33BUKB20201555555556" }, "companies[1000].house_bank.iban"},
		{"bic", func(c *Config) { c.Companies[0].HouseBank.BIC = "BUK" }, "companies[1000].house_bank.bic"},
		{"main bank account not an asset", func(c *Config) { c.Companies[0].HouseBank.GLAccount = "211000" }, "companies[1000].house_bank.gl_account"},
		{"further bank without an account", furtherBank(""), "companies[2000].house_banks[DB].gl_account"},
		{"further bank account not in the chart", furtherBank("109999"), "companies[2000].house_banks[DB].gl_account"},
		{"further bank on the main bank's account", furtherBank("100000"), "companies[2000].house_banks[DB].gl_account"},
		{"main bank on the input tax account", func(c *Config) { c.Companies[0].AccountDetermination.Bank = "140000" }, "companies[1000].house_bank.gl_account"},
		{"further bank on the input tax account", furtherBank("140000"), "companies[2000].house_banks[DB].gl_account"},
		{"further bank on a tax code's account", func(c *Config) {
			furtherBank("101000")(c)
			c.Companies[1].TaxCodes[0].Account = "101000"
		}, "companies[2000].house_banks[DB].gl_account"},
		{"further bank code taken", func(c *Config) {
			furtherBank("101000")(c)
			c.Companies[1].HouseBanks[0].Name = c.Companies[1].HouseBank.Name
		}, "companies[2000].house_banks[MAIN].name"},
		{"further bank iban", func(c *Config) {
			furtherBank("101000")(c)
			c.Companies[1].HouseBanks[0].IBAN = "DE02120300000000202052"
		}, "companies[2000].house_banks[DB].iban"},
		{"further bank bic", func(c *Config) {
			furtherBank("101000")(c)
			c.Companies[1].HouseBanks[0].BIC = "DEUT"
		}, "companies[2000].house_banks[DB].bic"},
		{"Canadian bank with an IBAN", canadianBank(func(hb *HouseBank) { hb.IBAN = "DE02120300000000202051" }), "companies[2000].house_banks[DB].iban"},
		{"Canadian institution", canadianBank(func(hb *HouseBank) { hb.InstitutionNumber = "3" }), "companies[2000].house_banks[DB].institution_number"},
		{"Canadian transit", canadianBank(func(hb *HouseBank) { hb.TransitNumber = "" }), "companies[2000].house_banks[DB].transit_number"},
		{"Canadian account", canadianBank(func(hb *HouseBank) { hb.AccountNumber = "12345" }), "companies[2000].house_banks[DB].account_number"},
		{"payment method code", method(func(m *PaymentMethod) { m.Code = "TT" }), "companies[1000].payment_methods[TT]"},
		{"payment method code taken", func(c *Config) {
			c.Companies[0].PaymentMethods = append(c.Companies[0].PaymentMethods, c.Companies[0].PaymentMethods[0])
		}, "companies[1000].payment_methods[T]"},
		{"payment method description", method(func(m *PaymentMethod) { m.Description = " " }), "companies[1000].payment_methods[T].description"},
		{"payment method without a ranking", method(func(m *PaymentMethod) { m.Ranking = nil }), "companies[1000].payment_methods[T].ranking"},
		{"ranking currency unknown", method(func(m *PaymentMethod) { m.Ranking[0].Currency = "XXY" }), "companies[1000].payment_methods[T].ranking[XXY]"},
		{"ranking currency twice", method(func(m *PaymentMethod) { m.Ranking = append(m.Ranking, m.Ranking[0]) }), "companies[1000].payment_methods[T].ranking[USD]"},
		{"ranking without banks", method(func(m *PaymentMethod) { m.Ranking[0].HouseBanks = nil }), "companies[1000].payment_methods[T].ranking[USD].house_banks"},
		{"ranking an unknown bank", method(func(m *PaymentMethod) { m.Ranking[0].HouseBanks = []string{"DB"} }), "companies[1000].payment_methods[T].ranking[USD].house_banks"},
		{"ranking a bank twice", method(func(m *PaymentMethod) { m.Ranking[0].HouseBanks = []string{"MAIN", "MAIN"} }),
			"companies[1000].payment_methods[T].ranking[USD].house_banks"},
		{"terms key", func(c *Config) { c.PaymentTerms[0].Key = "net thirty days!" }, "payment_terms[net thirty days!]"},
		{"terms days", func(c *Config) { c.PaymentTerms[0].NetDays = 400 }, "payment_terms[NT14].net_days"},
		{"no terms", func(c *Config) { c.PaymentTerms = nil }, "payment_terms"},
		{"discount days negative", discounts(Discount{Days: -1, Percent: "2"}), "payment_terms[NT30].discounts[0].days"},
		{"discount days at the net days", discounts(Discount{Days: 30, Percent: "2"}), "payment_terms[NT30].discounts[0].days"},
		{"discount percent zero", discounts(Discount{Days: 10, Percent: "0"}), "payment_terms[NT30].discounts[0].percent"},
		{"discount percent 100", discounts(Discount{Days: 10, Percent: "100"}), "payment_terms[NT30].discounts[0].percent"},
		{"discount percent decimals", discounts(Discount{Days: 10, Percent: "2.0001"}), "payment_terms[NT30].discounts[0].percent"},
		{"discount percent text", discounts(Discount{Days: 10, Percent: "two"}), "payment_terms[NT30].discounts[0].percent"},
		{"discount tiers not later", discounts(Discount{Days: 14, Percent: "3"}, Discount{Days: 14, Percent: "2"}), "payment_terms[NT30].discounts[1].days"},
		{"discount tiers not less", discounts(Discount{Days: 10, Percent: "2"}, Discount{Days: 20, Percent: "2.5"}), "payment_terms[NT30].discounts[1].percent"},
		{"three discount tiers", discounts(Discount{Days: 5, Percent: "3"}, Discount{Days: 10, Percent: "2"}, Discount{Days: 20, Percent: "1"}),
			"payment_terms[NT30].discounts"},
		{"discount without a cash discount account", discounts(Discount{Days: 10, Percent: "2"}), "companies[1000].account_determination.cash_discount"},
		{"cash discount unknown", func(c *Config) { c.Companies[0].AccountDetermination.CashDiscount = "999999" }, "companies[1000].account_determination.cash_discount"},
		{"cash discount wrong type", func(c *Config) { c.Companies[0].AccountDetermination.CashDiscount = "140000" }, "companies[1000].account_determination.cash_discount"},
		{"down payment account unknown", func(c *Config) { c.Companies[0].AccountDetermination.DownPayment = "159000" }, "companies[1000].account_determination.down_payment"},
		{"down payment account not reconciliation", func(c *Config) { c.Companies[0].AccountDetermination.DownPayment = "100000" }, "companies[1000].account_determination.down_payment"},
		{"down payment account a liability", func(c *Config) { c.Companies[0].AccountDetermination.DownPayment = "211000" }, "companies[1000].account_determination.down_payment"},
		{"asset reconciliation account not for down payments", func(c *Config) {
			c.Companies[0].ChartOfAccounts = append(c.Companies[0].ChartOfAccounts, Account{Number: "159000", Name: "Advances", Type: Asset, VendorReconciliation: true})
		}, "companies[1000].chart_of_accounts[159000]"},
		{"withholding code", wht(func(w *WithholdingTaxCode) { w.Code = "w-1" }), "companies[1000].withholding_tax_codes[w-1]"},
		{"withholding code twice", func(c *Config) {
			wht(func(*WithholdingTaxCode) {})(c)
			c.Companies[0].WithholdingTaxCodes = append(c.Companies[0].WithholdingTaxCodes, c.Companies[0].WithholdingTaxCodes[0])
		}, "companies[1000].withholding_tax_codes[W1]"},
		{"withholding rate zero", wht(func(w *WithholdingTaxCode) { w.RatePercent = "0" }), "companies[1000].withholding_tax_codes[W1].rate_percent"},
		{"withholding rate 100", wht(func(w *WithholdingTaxCode) { w.RatePercent = "100" }), "companies[1000].withholding_tax_codes[W1].rate_percent"},
		{"withholding rate decimals", wht(func(w *WithholdingTaxCode) { w.RatePercent = "1.2345" }), "companies[1000].withholding_tax_codes[W1].rate_percent"},
		{"withholding base", wht(func(w *WithholdingTaxCode) { w.Base = "tax" }), "companies[1000].withholding_tax_codes[W1].base"},
		{"withholding posting", wht(func(w *WithholdingTaxCode) { w.Posting = "" }), "companies[1000].withholding_tax_codes[W1].posting"},
		{"withholding account unknown", wht(func(w *WithholdingTaxCode) { w.Account = "999999" }), "companies[1000].withholding_tax_codes[W1].account"},
		{"withholding account reconciles", wht(func(w *WithholdingTaxCode) { w.Account = "211000" }), "companies[1000].withholding_tax_codes[W1].account"},
		{"withholding account an expense", wht(func(w *WithholdingTaxCode) { w.Account = "520000" }), "companies[1000].withholding_tax_codes[W1].account"},
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
		{"release approvals above one", func(c *Config) { c.Payments.ReleaseApprovals = 2 }, "payments.release_approvals"},
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

// furtherBank gives company 2000 a second house bank on the account.
func furtherBank(account string) func(c *Config) {
	return func(c *Config) {
		c.Companies[1].HouseBanks = []HouseBank{{Name: "DB", IBAN: "DE02120300000000202051", BIC: "DEUTDEFFXXX", GLAccount: account}}
	}
}

// adopt records company 1000's adoption of the US pack 2026.1, with the
// fields of a that are set instead.
func adopt(a PackAdoption) func(c *Config) {
	return func(c *Config) {
		p := PackAdoption{Company: "1000", Country: "US", Version: "2026.1", AppliedOn: "2026-10-20"}
		for _, f := range []struct {
			to   *string
			from string
		}{{&p.Company, a.Company}, {&p.Country, a.Country}, {&p.Version, a.Version}, {&p.AppliedOn, a.AppliedOn}} {
			if f.from != "" {
				*f.to = f.from
			}
		}
		c.Packs = []PackAdoption{p}
	}
}

// TestReleaseDualLimit: a company's release_dual_limit is optional (no run
// then needs a second person) and, when given, an amount in the company's
// currency (ADR-0038).
func TestReleaseDualLimit(t *testing.T) {
	for name, limit := range map[string]string{"none": "", "an amount": "50000.00", "zero": "0.00"} {
		t.Run(name, func(t *testing.T) {
			c := fixture(t)
			c.Companies[0].ReleaseDualLimit = limit
			if fs := Validate(c, currency.Seeded()); HasErrors(fs) {
				t.Errorf("Validate: got %v, want no errors", fs)
			}
		})
	}
}

// TestServicePostingLimit: a company's service_posting_limit is optional
// and, when given, an amount in the company's currency; without it service
// principals are held to the manual entry approval limit, as before T61n,
// and with neither to 0 (ADR-0049 §7).
func TestServicePostingLimit(t *testing.T) {
	for name, limit := range map[string]string{"none": "", "an amount": "50000.00", "zero": "0.00"} {
		t.Run(name, func(t *testing.T) {
			c := fixture(t)
			c.Companies[0].ServicePostingLimit = limit
			if fs := Validate(c, currency.Seeded()); HasErrors(fs) {
				t.Errorf("Validate: got %v, want no errors", fs)
			}
		})
	}
	for name, c := range map[string]struct{ service, manual, want string }{
		"its own":        {"500.00", "10000.00", "500.00"},
		"its own zero":   {"0.00", "10000.00", "0.00"},
		"the manual one": {"", " 10000.00 ", "10000.00"},
		"neither":        {"", "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			co := Company{ServicePostingLimit: c.service, ManualEntryApprovalLimit: c.manual}
			if got := co.ServiceLimit(); got != c.want {
				t.Errorf("ServiceLimit: got %q, want %q", got, c.want)
			}
		})
	}
}

// A company's pack adoption is valid provenance, found by company, and
// changing it is operational (ADR-0048).
func TestPackAdoption(t *testing.T) {
	c := fixture(t)
	adopt(PackAdoption{})(c)
	if fs := Validate(c, currency.Seeded()); HasErrors(fs) {
		t.Errorf("Validate: got %v, want no errors", fs)
	}
	if a := c.Pack("1000"); a == nil || a.Version != "2026.1" {
		t.Errorf("Pack(1000): got %+v, want version 2026.1", a)
	}
	if a := c.Pack("2000"); a != nil {
		t.Errorf("Pack(2000): got %+v, want nil", a)
	}
	changes, _ := Diff(fixture(t), c) // Diff never fails
	if len(changes) != 1 || changes[0].Path != "packs[1000]" || changes[0].Tier != TierOperational {
		t.Errorf("Diff: got %v, want packs[1000] added at tier 2", changes)
	}
	if n := (Note{Path: "packs[1000]", Message: "adopted"}); n.String() != "packs[1000]: adopted" {
		t.Errorf("Note.String: got %q", n.String())
	}
}

// holidays gives the company a calendar of these holidays.
func holidays(company string, hs ...Holiday) func(c *Config) {
	return func(c *Config) { c.Calendars = append(c.Calendars, Calendar{Company: company, Holidays: hs}) }
}

// A company's calendar is valid, read by company and date, and its changes
// are operational but for a holiday's name, a text (ADR-0048).
func TestCalendars(t *testing.T) {
	c := fixture(t)
	holidays("1000", Holiday{Date: "2026-01-01", Name: "New Year's Day"}, Holiday{Date: "2026-12-25", Name: "Christmas Day"})(c)
	holidays("2000")(c)
	if fs := Validate(c, currency.Seeded()); HasErrors(fs) {
		t.Errorf("Validate: got %v, want no errors", fs)
	}
	newYear := time.Date(2026, 1, 1, 23, 0, 0, 0, time.UTC)
	reads := []struct {
		name    string
		company string
		on      time.Time
		want    string // the holiday's name, or "" for none
	}{
		{"holiday", "1000", newYear, "New Year's Day"},
		{"business day", "1000", newYear.AddDate(0, 0, 1), ""},
		{"empty calendar", "2000", newYear, ""},
		{"no calendar", "3000", newYear, ""},
	}
	for _, r := range reads {
		t.Run(r.name, func(t *testing.T) {
			h, ok := c.Holiday(r.company, r.on)
			got := h.Name
			if ok != (r.want != "") {
				t.Errorf("Holiday(%s, %s) found: got %v, want %v", r.company, r.on.Format(time.DateOnly), ok, r.want != "")
			}
			if got != r.want {
				t.Errorf("Holiday(%s, %s): got %q, want %q", r.company, r.on.Format(time.DateOnly), got, r.want)
			}
		})
	}
	changed := fixture(t)
	holidays("1000", Holiday{Date: "2026-01-01", Name: "New Year"}, Holiday{Date: "2026-05-01", Name: "Labour Day"})(changed)
	holidays("2000", Holiday{Date: "2026-01-01", Name: "New Year"})(changed)
	diffs, _ := Diff(c, changed) // Diff never fails
	want := map[string]Tier{
		"calendars[1000].holidays[2026-01-01].name": TierCosmetic,
		"calendars[1000].holidays[2026-05-01]":      TierOperational,
		"calendars[1000].holidays[2026-12-25]":      TierOperational,
		"calendars[2000].holidays[2026-01-01]":      TierOperational,
	}
	if len(diffs) != len(want) {
		t.Errorf("Diff: got %v, want %d changes", diffs, len(want))
	}
	for _, d := range diffs {
		if tier, ok := want[d.Path]; !ok || d.Tier != tier {
			t.Errorf("Diff %s: got %v, want %v", d.Path, d.Tier, tier)
		}
	}
}

// method changes company 1000's payment method.
func method(change func(m *PaymentMethod)) func(c *Config) {
	return func(c *Config) { change(&c.Companies[0].PaymentMethods[0]) }
}

// discounts gives NT30 the cash discount tiers.
func discounts(tiers ...Discount) func(c *Config) {
	return func(c *Config) { c.PaymentTerm("NT30").Discounts = tiers }
}

// TestDiscountTiersValid: a term of 3% within 14 days and 2% within 21,
// net 30, is valid in companies that name their cash discount account
// (SAP's SKE), and a change to it is posting-related, since it changes
// what payments post.
func TestDiscountTiersValid(t *testing.T) {
	c := fixture(t)
	discounts(Discount{Days: 14, Percent: "3"}, Discount{Days: 21, Percent: "2.5"})(c)
	for i := range c.Companies {
		c.Companies[i].AccountDetermination.CashDiscount = "400000"
	}
	if fs := Validate(c, currency.Seeded()); HasErrors(fs) {
		t.Fatalf("discount tiers: got %v, want no errors", fs)
	}
	changed := fixture(t)
	discounts(Discount{Days: 14, Percent: "3"}, Discount{Days: 21, Percent: "2"})(changed)
	for i := range changed.Companies {
		changed.Companies[i].AccountDetermination.CashDiscount = "400000"
	}
	changes, err := Diff(c, changed)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("changes: got %v, want the second tier's percent", changes)
	}
	if got := changes[0].Path; got != "payment_terms[NT30].discounts[1].percent" {
		t.Errorf("change path: got %s, want payment_terms[NT30].discounts[1].percent", got)
	}
	if changes[0].Tier != TierPostingRelated {
		t.Errorf("change tier: got %v, want %v", changes[0].Tier, TierPostingRelated)
	}
}

// wht gives company 1000 withholding tax code W1, 10% of net at invoice
// on a new liability account 236000, changed by mod.
func wht(mod func(*WithholdingTaxCode)) func(c *Config) {
	return func(c *Config) {
		co := &c.Companies[0]
		co.ChartOfAccounts = append(co.ChartOfAccounts, Account{Number: "236000", Name: "Withholding tax payable", Type: Liability})
		w := WithholdingTaxCode{Code: "W1", Description: "Withholding 10%", RatePercent: "10", Account: "236000", Posting: WithholdingAtInvoice}
		mod(&w)
		co.WithholdingTaxCodes = append(co.WithholdingTaxCodes, w)
	}
}

// TestWithholdingTaxCodesValid: a withholding tax code of net (the
// default) or gross, at invoice or at payment, on a liability that
// reconciles no supplier, is valid, and a change to one is posting-related
// (T46e); its description is cosmetic.
func TestWithholdingTaxCodesValid(t *testing.T) {
	c := fixture(t)
	wht(func(*WithholdingTaxCode) {})(c)
	c.Companies[0].WithholdingTaxCodes = append(c.Companies[0].WithholdingTaxCodes, WithholdingTaxCode{Code: "W2", Description: "15% of gross at payment",
		RatePercent: "15.125", Base: WithholdingGross, Account: "236000", Posting: WithholdingAtPayment})
	if fs := Validate(c, currency.Seeded()); HasErrors(fs) {
		t.Fatalf("withholding tax codes: got %v, want no errors", fs)
	}
	if got := c.Companies[0].WithholdingTaxCodes[0].BaseOrNet(); got != WithholdingNet {
		t.Errorf("default base: got %s, want net", got)
	}
	changed := fixture(t)
	wht(func(w *WithholdingTaxCode) { w.RatePercent = "12"; w.Description = "Withholding 12%" })(changed)
	base := fixture(t)
	wht(func(*WithholdingTaxCode) {})(base)
	changes, err := Diff(base, changed)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	tiers := map[string]Tier{}
	for _, ch := range changes {
		tiers[ch.Path] = ch.Tier
	}
	for path, want := range map[string]Tier{"companies[1000].withholding_tax_codes[W1].rate_percent": TierPostingRelated,
		"companies[1000].withholding_tax_codes[W1].description": TierCosmetic} {
		if got, ok := tiers[path]; !ok || got != want {
			t.Errorf("change %s: got %v (%v), want %v", path, got, ok, want)
		}
	}
}

// TestDownPaymentAccountValid: a company's down payment account (SAP's
// alternative reconciliation account for special G/L A, ADR-0037) is an
// asset that is a vendor reconciliation account, the one asset that may
// be.
func TestDownPaymentAccountValid(t *testing.T) {
	c := fixture(t)
	co := &c.Companies[0]
	co.ChartOfAccounts = append(co.ChartOfAccounts, Account{Number: "159000", Name: "Down payments made", Type: Asset, VendorReconciliation: true})
	co.AccountDetermination.DownPayment = "159000"
	if fs := Validate(c, currency.Seeded()); HasErrors(fs) {
		t.Fatalf("down payment account: got %v, want no errors", fs)
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
		{"tax tolerance", func(c *Config) { u := 2; c.Companies[0].InvoiceTolerance.TaxUnitsPerCode = &u }, "companies[1000].invoice_tolerance.tax_units_per_code", TierOperational},
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
		{"release dual limit", func(c *Config) { c.Companies[0].ReleaseDualLimit = "50000.00" }, "companies[1000].release_dual_limit", TierPostingRelated},
		{"service posting limit", func(c *Config) { c.Companies[0].ServicePostingLimit = "5000.00" }, "companies[1000].service_posting_limit", TierPostingRelated},
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
	// A further house bank is identified by its name, so dropping the first
	// of two is its removal, not new bank details for the first.
	two := fixture(t)
	furtherBank("101000")(two)
	two.Companies[1].HouseBanks = append(two.Companies[1].HouseBanks, HouseBank{Name: "HSBC", IBAN: "GB33BUKB20201555555555", BIC: "BUKBGB22", GLAccount: "102000"})
	one := fixture(t)
	one.Companies[1].HouseBanks = two.Companies[1].HouseBanks[1:]
	banks, _ := Diff(two, one)
	if len(banks) != 1 {
		t.Fatalf("house bank removal changes: got %v, want 1", banks)
	}
	if got := banks[0].Path; got != "companies[2000].house_banks[DB]" {
		t.Errorf("house bank removal path: got %s, want companies[2000].house_banks[DB]", got)
	}
	// A payment method and its rankings are identified by code and
	// currency; a description is cosmetic.
	reranked := fixture(t)
	reranked.Companies[0].PaymentMethods[0].Description = "Wire"
	reranked.Companies[0].PaymentMethods[0].Ranking = append(reranked.Companies[0].PaymentMethods[0].Ranking,
		BankRanking{Currency: "EUR", HouseBanks: []string{"MAIN"}})
	methods, _ := Diff(base, reranked)
	if len(methods) != 2 {
		t.Fatalf("payment method changes: got %v, want 2", methods)
	}
	if got := methods[0]; got.Path != "companies[1000].payment_methods[T].description" || got.Tier != TierCosmetic {
		t.Errorf("description change: got %s %v, want companies[1000].payment_methods[T].description cosmetic", got.Path, got.Tier)
	}
	if got := methods[1]; got.Path != "companies[1000].payment_methods[T].ranking[EUR]" || got.Tier != TierPostingRelated {
		t.Errorf("ranking change: got %s %v, want companies[1000].payment_methods[T].ranking[EUR] posting-related", got.Path, got.Tier)
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
	if got := strings.Join(c.RoleNames(), ","); got != "Z_PURCH_MGR,process_owner,Z_PURCH_HEAD,Z_TREASURY_MGR,vendor_master_approver,controller,solution_architect,tax_manager" {
		t.Fatalf("role names: %s", got)
	}
	c.Companies[1].BankStatements.ApprovalTiers = []ApprovalTier{{UpTo: "1000.00"}, {Steps: []ApprovalStep{{Role: "treasurer", Approvals: 1}}}}
	if got := c.RoleNames(); !slices.Contains(got, "treasurer") {
		t.Errorf("role names with a bank clearing tier: got %v, want treasurer among them", got)
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

// TestParseActive: an activated document is read without the keys this
// binary does not know, which are reported; one that does not read is an
// error. Parse refuses both.
func TestParseActive(t *testing.T) {
	b, err := os.ReadFile("testdata/valid.yaml")
	if err != nil {
		t.Fatal(err)
	}
	newer := append(bytes.Clone(b), []byte("future_section:\n  enabled: true\n")...)
	c, unknown, err := ParseActive(newer)
	if err != nil {
		t.Fatalf("a newer document: %v", err)
	}
	if len(c.Companies) == 0 {
		t.Errorf("a newer document's companies: got none, want them read")
	}
	if len(unknown) != 1 || !strings.Contains(unknown[0], "future_section") {
		t.Errorf("unknown keys: got %v, want one naming future_section", unknown)
	}
	if _, err := Parse(newer); err == nil {
		t.Error("Parse of a newer document: got nil error, want the unknown key refused")
	}
	if _, unknown, err := ParseActive(b); err != nil || len(unknown) != 0 {
		t.Errorf("a current document: got %v unknown, error %v; want none", unknown, err)
	}
	if c, _, err := ParseActive([]byte("payment_terms: none\n")); err == nil || c != nil {
		t.Errorf("an unreadable document: got %v, %v; want an error and no document", c, err)
	}
}

// TestCashDerivationChanges: a change of the liquidity item derivation is
// operational and needs a simulation, which lists the open flows whose
// items it moves (ADR-0051 §4, T53e3); an item's name needs none.
func TestCashDerivationChanges(t *testing.T) {
	base := func() *Config {
		return &Config{Cash: Cash{LiquidityItems: []LiquidityItem{{Code: "OPS", Name: "Operating", Direction: DirectionOutflow}},
			Derivation: Derivation{Defaults: DerivationDefaults{Outflow: "OPS"}}}}
	}
	cases := map[string]struct {
		change   func(*Config)
		tier     Tier
		evidence string
	}{
		"an item's name": {func(c *Config) { c.Cash.LiquidityItems[0].Name = "Ops" }, TierCosmetic, ""},
		"an account rule": {func(c *Config) {
			c.Cash.Derivation.Accounts = []AccountDerivation{{From: "6100", To: "6100", Item: "OPS"}}
		}, TierOperational, "simulation"},
		"a default":        {func(c *Config) { c.Cash.Derivation.Defaults.Outflow = "" }, TierOperational, "simulation"},
		"the rules all":    {func(c *Config) { c.Cash.Derivation = Derivation{} }, TierOperational, "simulation"},
		"a planning group": {func(c *Config) { c.Cash.PlanningGroups = []PlanningGroup{{Code: "TAX", Name: "Tax"}} }, TierOperational, ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			proposed := base()
			c.change(proposed)
			changes, _ := Diff(base(), proposed) // never fails
			if got := MaxTier(changes); got != c.tier {
				t.Errorf("tier: got %v, want %v (%v)", got, c.tier, changes)
			}
			if got := strings.Join(Evidence(changes), " "); got != c.evidence {
				t.Errorf("evidence: got %q, want %q (%v)", got, c.evidence, changes)
			}
		})
	}
}

// TestAnalyticsChanges: a metric's or dimension's words are cosmetic and
// need no evidence; its definition is operational and needs a simulation,
// which a tier needing one already has.
func TestAnalyticsChanges(t *testing.T) {
	base := func() *Config {
		return &Config{Analytics: &Analytics{Dimensions: []Dimension{{Name: "company", Label: "Company"}},
			Metrics: []Metric{{Name: "revenue", Label: "Revenue", Type: "money", Source: "gl_lines", Aggregate: "sum", Filter: map[string][]string{"account_type": {"revenue"}}}}}}
	}
	cases := map[string]struct {
		change   func(*Config)
		tier     Tier
		evidence string
	}{
		"a label":       {func(c *Config) { c.Analytics.Metrics[0].Label = "Sales" }, TierCosmetic, ""},
		"a synonym":     {func(c *Config) { c.Analytics.Metrics[0].Synonyms = []string{"sales"} }, TierCosmetic, ""},
		"a dimension":   {func(c *Config) { c.Analytics.Dimensions[0].Description = "Code" }, TierCosmetic, ""},
		"a filter":      {func(c *Config) { c.Analytics.Metrics[0].Filter["account"] = []string{"400000"} }, TierOperational, "simulation"},
		"a new metric":  {func(c *Config) { c.Analytics.Metrics = append(c.Analytics.Metrics, Metric{Name: "x", Label: "X"}) }, TierOperational, "simulation"},
		"the calendar":  {func(c *Config) { c.Analytics.Calendar.FiscalYearStartMonth = 7 }, TierOperational, "simulation"},
		"with a tier 3": {func(c *Config) { c.Analytics.Metrics[0].Sign = -1; c.Payments.ReleaseApprovals = 2 }, TierPostingRelated, "simulation regression"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			proposed := base()
			c.change(proposed)
			changes, _ := Diff(base(), proposed) // never fails
			if got := MaxTier(changes); got != c.tier {
				t.Errorf("tier: got %v, want %v (%v)", got, c.tier, changes)
			}
			if got := strings.Join(Evidence(changes), " "); got != c.evidence {
				t.Errorf("evidence: got %q, want %q", got, c.evidence)
			}
		})
	}
	changes, _ := Diff(base(), &Config{}) // never fails
	var paths []string
	for _, c := range changes {
		paths = append(paths, c.Path)
	}
	if got, want := strings.Join(paths, " "), "analytics.calendar.fiscal_year_start_month analytics.dimensions[company] analytics.metrics[revenue]"; got != want {
		t.Errorf("section removed: got %s, want %s", got, want)
	}
}

// TestHouseBankAccount: the account sealed is the IBAN, or a Canadian
// account's number in digits, beside its routing in digits (T45d).
func TestHouseBankAccount(t *testing.T) {
	iban := HouseBank{IBAN: "GB33BUKB20201555555555"}
	ca := HouseBank{InstitutionNumber: "0 03", TransitNumber: "00-002", AccountNumber: "100-0001"}
	institution, transit := ca.Routing()
	for _, c := range []struct{ name, got, want string }{
		{"IBAN account", iban.Account(), "GB33BUKB20201555555555"},
		{"Canadian account", ca.Account(), "1000001"},
		{"Canadian institution", institution, "003"},
		{"Canadian transit", transit, "00002"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.got != c.want {
				t.Errorf("got %q, want %q", c.got, c.want)
			}
		})
	}
}
