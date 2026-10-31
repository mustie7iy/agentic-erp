package sapconfig

import (
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
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
	c, notes := Generate(sap.GenerateMock(42, "2026"), zoned())
	us := c.Company("1000")
	if us == nil {
		t.Fatal("company 1000: got none, want it from T001")
	}
	a := us.Account("211000")
	if a == nil {
		t.Fatal("account 211000: got none, want the reconciliation account from SKB1-MITKZ")
	}
	if len(us.TaxCodes) != 2 {
		t.Fatalf("tax codes of 1000 from T007A: got %d, want 2: %+v", len(us.TaxCodes), us.TaxCodes)
	}
	for name, f := range map[string]struct{ got, want any }{
		"company 1000 currency":                       {us.Currency, "USD"},
		"company 1000 country":                        {us.Country, "US"},
		"company 1000 chart of accounts":              {len(us.ChartOfAccounts), 10},
		"AP reconciliation account from T030/LFB1":    {us.AccountDetermination.APReconciliation, "211000"},
		"input tax account from T030":                 {us.AccountDetermination.InputTax, "140000"},
		"bank account from T030":                      {us.AccountDetermination.Bank, "100000"},
		"211000 is a vendor reconciliation account":   {a.VendorReconciliation, true},
		"211000 type":                                 {a.Type, config.Liability},
		"tax code rate of 1000 from T007A":            {us.TaxCodes[0].RatePercent, "8"},
		"tax code rate of 2000 from T007A":            {c.Company("2000").TaxCodes[0].RatePercent, "19"},
		"price tolerance percent from T169G":          {us.InvoiceTolerance.PricePercent, 2},
		"price tolerance amount from T169G":           {us.InvoiceTolerance.PriceAbsolute, "5.00"},
		"house bank IBAN from T012K":                  {us.HouseBank.IBAN, "GB33BUKB20201555555555"},
		"payment terms from T052":                     {len(c.PaymentTerms), 4},
		"NT45 net days from T052":                     {c.PaymentTerm("NT45").NetDays, 45},
		"payment release role":                        {c.Payments.ReleaseRole, "Z_TREASURY_MGR"},
		"vendor approver role":                        {c.Payments.VendorApproverRole, "vendor_master_approver"},
		"requisition approval role of the first step": {c.Purchasing.RequisitionApproval[0].Steps[0].Role, "Z_PURCH_MGR"},
	} {
		if f.got != f.want {
			t.Errorf("%s: got %v, want %v", name, f.got, f.want)
		}
	}
	ranges := map[string]bool{}
	for _, n := range c.NumberRanges {
		ranges[n.Key()] = true
	}
	for _, key := range []string{"1000/journal_re/2026", "2000/journal_kz/2026", "1000/payment_run/2026", "2000/journal_ob/2026"} {
		if !ranges[key] {
			t.Errorf("number range %s: got none, want one", key)
		}
	}
	all := notesText(notes)
	for _, want := range []string{"auto_post_limit", "manual_entry_approval_limit", "fiscal year variant", "release strategies", "bank_change_hold_days",
		"provision these identity roles", "roles[Z_AP_CLERK]: SAP role"} {
		if !strings.Contains(all, want) {
			t.Errorf("got notes without %s, want a note about it:\n%s", want, all)
		}
	}
	if fs := config.Validate(c, currency.Seeded()); config.HasErrors(fs) {
		t.Fatalf("draft does not validate: %v", fs)
	}
}

func TestGenerateGapsAndDefaults(t *testing.T) {
	e := sap.GenerateMock(42, "2026")
	e.T052, e.T169G, e.T030, e.ACDOCA = nil, nil, nil, nil
	e.SKB1 = append(e.SKB1, sap.SKB1{BUKRS: "1000", SAKNR: "ABC", TXT50: "Odd account"}, sap.SKB1{BUKRS: "1000", SAKNR: "0000000050", TXT50: "Low"})
	e.LFB1 = append(e.LFB1, sap.LFB1{LIFNR: "0000100001", BUKRS: "1000", AKONT: "0000212000", ZTERM: "NT30"}, sap.LFB1{LIFNR: "0000100002", BUKRS: "1000", AKONT: "0000212000", ZTERM: "NT30"})
	opts := zoned()
	opts.AccountTypes = append(opts.AccountTypes, RangeRule{From: "x", To: "y", Type: config.Asset})
	c, notes := Generate(e, opts)
	all := notesText(notes)
	for _, want := range []string{"T052 not in extract", "T169G key PP not found", "not numeric", "no account type rule covers account 50",
		"reconciliation accounts", "has no journal"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing note %q in:\n%s", want, all)
		}
	}
	if c.PaymentTerm("NT30") == nil {
		t.Error("payment term NT30 without T052: got none, want a placeholder")
	}
	if got := c.Company("1000").InvoiceTolerance.PricePercent; got != 2 {
		t.Errorf("price tolerance without T169G: got %d, want the default 2", got)
	}
	if len(c.NumberRanges) != 0 {
		t.Errorf("number ranges without ACDOCA: got %d, want none", len(c.NumberRanges))
	}
	if c.Company("1000").AccountDetermination.Bank != "100000" {
		t.Fatal("bank account falls back to the house bank G/L account")
	}
	if c.Company("1000").AccountDetermination.InputTax != "" {
		t.Fatal("no T030 means no input tax account; validation will flag it")
	}
}

// Account numbers drop SAP's ALPHA zeros with sap.Alpha, as the migration
// does: it finds journal accounts in the configured chart by that number,
// so an all-zero account is "0" on both sides.
func TestAlphaMatchesTheMigration(t *testing.T) {
	e := sap.GenerateMock(42, "2026")
	e.SKB1 = append(e.SKB1, sap.SKB1{BUKRS: "1000", SAKNR: "0000000000", TXT50: "Zero"})
	opts := zoned()
	opts.AccountTypes = append(opts.AccountTypes, RangeRule{From: "0", To: "0", Type: config.Asset})
	c, _ := Generate(e, opts)
	if a := c.Company("1000").Account("0"); a == nil || a.Name != "Zero" {
		t.Errorf("got account 0 %+v, want the all-zero SAP account", a)
	}
}

// A tolerance percentage without a leading zero reads like one with it,
// and one too large to hold is reported as such, not as "not a number".
func TestTolerancePercentageForms(t *testing.T) {
	cases := map[string]struct {
		proz string
		pct  int
		note string
	}{
		"leading zero":    {"0.5", 0, `"0.5" is not a whole percent; used 0%`},
		"no leading zero": {".5", 0, `".5" is not a whole percent; used 0%`},
		"only a point":    {".", 2, `"." is not a number; used 2%`},
		"decimal comma":   {"0,50", 0, `"0,50" is not a whole percent; used 0%`},
		"two commas":      {"1,2,3", 2, `"1,2,3" is not a number; used 2%`},
		"out of range":    {"99999999999999999999", 2, `"99999999999999999999" is out of range; used 2%`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := sap.GenerateMock(42, "2026")
			for i := range e.T169G {
				e.T169G[i].PROZ = tc.proz
			}
			c, notes := Generate(e, zoned())
			if got := c.Company("1000").InvoiceTolerance.PricePercent; got != tc.pct {
				t.Errorf("got price percent %d, want %d", got, tc.pct)
			}
			if all := notesText(notes); !strings.Contains(all, "companies[1000].invoice_tolerance.price_percent: T169G key PP percentage "+tc.note) {
				t.Errorf("got notes\n%s\nwant one ending %s", all, tc.note)
			}
		})
	}
}

// A payment term whose days do not parse must not validate as "due at
// once": payments would propose every invoice under it on its document
// date. The draft fails validation on that term until someone sets it.
func TestUnparseableDaysFailValidation(t *testing.T) {
	for _, ztag1 := range []string{"abc", "", "2.5"} {
		t.Run(ztag1, func(t *testing.T) {
			e := sap.GenerateMock(42, "2026")
			e.T052 = append(e.T052, sap.T052{ZTERM: "NT99", ZTAG1: ztag1, TEXT1: "Odd"})
			c, _ := Generate(e, zoned())
			var found bool
			for _, f := range config.Validate(c, currency.Seeded()) {
				if f.Path == "payment_terms[NT99].net_days" && f.Severity == config.SevError {
					found = true
				}
			}
			if !found {
				t.Errorf("got no error on payment_terms[NT99].net_days, want one: %v", config.Validate(c, currency.Seeded()))
			}
		})
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
		"decimal fraction": {"2.5", "2.5", 2, -1, `"2.5"`},
		"not a number":     {"abc", "abc", 2, -1, `"abc"`},
		"empty":            {"", "", 2, -1, `""`},
	}
	paths := []string{"companies[1000].invoice_tolerance.price_percent", "payment_terms[NT30].net_days"}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := sap.GenerateMock(42, "2026")
			for i := range e.T169G {
				e.T169G[i].PROZ = tc.proz
			}
			e.T052 = []sap.T052{{ZTERM: "NT30", ZTAG1: tc.ztag1, TEXT1: "Net 30 days"}}
			c, notes := Generate(e, zoned())
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

// TestGRIRFromT030: SAP's GR/IR clearing account (T030 WRX) and price
// difference account (PRD) become the company's gr_ir and price_difference,
// and the draft numbers the WE entries its receipts will post.
func TestGRIRFromT030(t *testing.T) {
	e := sap.GenerateMock(42, "2026")
	e.SKB1 = append(e.SKB1, sap.SKB1{BUKRS: "1000", SAKNR: "0000219000", TXT50: "GR/IR clearing"},
		sap.SKB1{BUKRS: "1000", SAKNR: "0000590000", TXT50: "Price differences"})
	e.T030 = append(e.T030, sap.T030{BUKRS: "1000", KTOSL: "WRX", KONTS: "0000219000"}, sap.T030{BUKRS: "1000", KTOSL: "PRD", KONTS: "0000590000"})
	c, _ := Generate(e, zoned())
	ad := c.Company("1000").AccountDetermination
	if ad.GRIR != "219000" {
		t.Errorf("gr_ir from T030 WRX: got %q, want 219000", ad.GRIR)
	}
	if ad.PriceDifference != "590000" {
		t.Errorf("price_difference from T030 PRD: got %q, want 590000", ad.PriceDifference)
	}
	if fs := config.Validate(c, currency.Seeded()); config.HasErrors(fs) {
		t.Fatalf("draft with GR/IR does not validate: %v", fs)
	}
}

// TestFurtherHouseBanks: a company's first T012K row is its main house
// bank; each further one is a house bank on its own G/L account.
func TestFurtherHouseBanks(t *testing.T) {
	e := sap.GenerateMock(42, "2026")
	e.SKB1 = append(e.SKB1, sap.SKB1{BUKRS: "1000", SAKNR: "0000101000", TXT50: "Bank clearing - DB"})
	e.T012K = append(e.T012K, sap.T012K{BUKRS: "1000", HBKID: "DB", HKONT: "0000101000", IBAN: "DE02120300000000202051", SWIFT: "DEUTDEFFXXX"},
		sap.T012K{BUKRS: "1000", HBKID: "DB", HKONT: "0000102000", IBAN: "DE89370400440532013000", SWIFT: "DEUTDEFFXXX"})
	c, notes := Generate(e, zoned())
	if !strings.Contains(notesText(notes), "house bank DB has several accounts") {
		t.Errorf("a second account of DB: got notes %s, want one on it", notesText(notes))
	}
	co := c.Company("1000")
	if co.HouseBank.Name != "MAIN" {
		t.Errorf("main house bank: got %q, want MAIN", co.HouseBank.Name)
	}
	want := []config.HouseBank{{Name: "DB", IBAN: "DE02120300000000202051", BIC: "DEUTDEFFXXX", GLAccount: "101000"}}
	if !slices.Equal(co.HouseBanks, want) {
		t.Errorf("further house banks: got %+v, want %+v", co.HouseBanks, want)
	}
	if fs := config.Validate(c, currency.Seeded()); config.HasErrors(fs) {
		t.Fatalf("draft with a further house bank does not validate: %v", fs)
	}
}

// TestMainHouseBankOnItsOwnAccount: where T030 names another bank
// account than the first T012K row's, the main house bank keeps its own.
func TestMainHouseBankOnItsOwnAccount(t *testing.T) {
	e := sap.GenerateMock(42, "2026")
	e.SKB1 = append(e.SKB1, sap.SKB1{BUKRS: "1000", SAKNR: "0000101000", TXT50: "Bank clearing - MAIN"})
	for i := range e.T012K {
		if e.T012K[i].BUKRS == "1000" {
			e.T012K[i].HKONT = "0000101000"
		}
	}
	c, _ := Generate(e, zoned())
	co := c.Company("1000")
	if co.HouseBank.GLAccount != "101000" {
		t.Errorf("main house bank account: got %q, want 101000", co.HouseBank.GLAccount)
	}
	if co.AccountDetermination.Bank != "100000" {
		t.Errorf("bank account from T030: got %q, want 100000", co.AccountDetermination.Bank)
	}
	if fs := config.Validate(c, currency.Seeded()); config.HasErrors(fs) {
		t.Fatalf("draft does not validate: %v", fs)
	}
}

// TestGRIRRowsDisagree: SAP keeps WRX and PRD per valuation class; rows of
// one company naming different accounts are noted, as only one is taken.
func TestGRIRRowsDisagree(t *testing.T) {
	e := sap.GenerateMock(42, "2026")
	e.T030 = append(e.T030, sap.T030{BUKRS: "1000", KTOSL: "WRX", KONTS: "0000219000"}, sap.T030{BUKRS: "1000", KTOSL: "WRX", KONTS: "0000219100"},
		sap.T030{BUKRS: "1000", KTOSL: "PRD", KONTS: "0000590000"}, sap.T030{BUKRS: "1000", KTOSL: "PRD", KONTS: "0000590000"})
	_, notes := Generate(e, zoned())
	all := notesText(notes)
	if want := "companies[1000].account_determination.gr_ir: T030 WRX names accounts 219000 and 219100; 219000 taken"; !strings.Contains(all, want) {
		t.Errorf("got notes without %q:\n%s", want, all)
	}
	if strings.Contains(all, "price_difference") {
		t.Errorf("got a note on PRD rows that agree:\n%s", all)
	}
}

// TestDownPaymentFromT074: a company's down payment account is the
// special G/L account of T074 for vendors and indicator A (ADR-0037,
// T72c); a company without such a row has none, rows of other account
// types or indicators are ignored, and a second account is noted, as only
// one is taken. The mock's company 3000 validates with it.
func TestDownPaymentFromT074(t *testing.T) {
	e := sap.GenerateMock(42, "2026")
	e.T074 = append(e.T074, sap.T074{BUKRS: "3000", KOART: "K", UMSKZ: "A", HKONT: "0000212000", SKONT: "0000159100"},
		sap.T074{BUKRS: "3000", KOART: "D", UMSKZ: "A", HKONT: "0000120000", SKONT: "0000159200"},
		sap.T074{BUKRS: "3000", KOART: "K", UMSKZ: "I", HKONT: "0000211000", SKONT: "0000159300"},
		sap.T074{BUKRS: "3000", KOART: "K", UMSKZ: "A", HKONT: "0000213000", SKONT: "0000159000"})
	c, notes := Generate(e, zoned())
	for code, want := range map[string]string{"1000": "", "3000": "159000"} {
		if got := c.Company(code).AccountDetermination.DownPayment; got != want {
			t.Errorf("company %s down payment account: got %q, want %q", code, got, want)
		}
	}
	all := notesText(notes)
	if want := "companies[3000].account_determination.down_payment: T074 names down payment accounts 159000 and 159100; 159000 taken"; !strings.Contains(all, want) {
		t.Errorf("got notes without %q:\n%s", want, all)
	}
	if n := strings.Count(all, "T074"); n != 1 {
		t.Errorf("T074 notes: got %d, want 1:\n%s", n, all)
	}
	plain, _ := Generate(sap.GenerateMock(42, "2026"), zoned())
	if fs := config.Validate(plain, currency.Seeded()); config.HasErrors(fs) {
		t.Errorf("draft with company 3000's down payment account does not validate: %v", fs)
	}
}

// TestCanadianHouseBank: company 3000's house bank has no IBAN; its
// account is taken by the routing in its bank key and its account number
// (T72d), and a bank key in another form is noted and left for a reviewer,
// whom validation then stops.
func TestCanadianHouseBank(t *testing.T) {
	c, _ := Generate(sap.GenerateMock(42, "2026"), zoned())
	want := config.HouseBank{Name: "MAIN", BIC: "ROYCCAT2", InstitutionNumber: "003", TransitNumber: "00001", AccountNumber: "5012345"}
	if got := c.Company("3000").HouseBank; got != want {
		t.Errorf("house bank of 3000: got %+v, want %+v", got, want)
	}
	if fs := config.Validate(c, currency.Seeded()); config.HasErrors(fs) {
		t.Fatalf("draft with a Canadian house bank does not validate: %v", fs)
	}
	for name, bankKey := range map[string]string{"transit first": "00001-003", "without its zero": "00300001", "empty": ""} {
		t.Run(name, func(t *testing.T) {
			e := sap.GenerateMock(42, "2026")
			for i := range e.T012K {
				if e.T012K[i].BUKRS == "3000" {
					e.T012K[i].BANKL = bankKey
				}
			}
			c, notes := Generate(e, zoned())
			if want := "companies[3000].house_banks[MAIN]: bank key"; !strings.Contains(notesText(notes), want) {
				t.Errorf("got notes without %q:\n%s", want, notesText(notes))
			}
			if hb := c.Company("3000").HouseBank; hb.InstitutionNumber != "" || hb.TransitNumber != "" {
				t.Errorf("routing of a bank key %q: got %s/%s, want none", bankKey, hb.InstitutionNumber, hb.TransitNumber)
			}
			if fs := config.Validate(c, currency.Seeded()); !config.HasErrors(fs) {
				t.Errorf("draft without the routing: got no errors, want validation to refuse it")
			}
		})
	}
}
