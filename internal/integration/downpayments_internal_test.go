package integration

import (
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/sap"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// dpLine is a line of company 2000's down payment document 0100000009 of
// 2026, a vendor payment (KZ).
func dpLine(docln, account, hsl, lifnr, umskz, mwskz string) sap.ACDOCA {
	return sap.ACDOCA{RLDNR: "0L", RBUKRS: "2000", GJAHR: "2026", BELNR: "0100000009", DOCLN: docln, RACCT: account, HSL: hsl, BUDAT: "20260310",
		BLART: "KZ", LIFNR: lifnr, UMSKZ: umskz, MWSKZ: mwskz}
}

// dpExtract is company 2000 with an open down payment of 1,000.00 and its
// input tax of 190.00 on V1 (19%) to vendor 0000000001, in SAP's net
// display: the down payment line, the tax line and the bank line.
func dpExtract() *sap.Extract {
	return &sap.Extract{
		T001: []sap.T001{{BUKRS: "2000", BUTXT: "Acme GmbH", WAERS: "EUR"}},
		SKB1: []sap.SKB1{{BUKRS: "2000", SAKNR: "0000100000"}, {BUKRS: "2000", SAKNR: "0000140000"}, {BUKRS: "2000", SAKNR: "0000170000"},
			{BUKRS: "2000", SAKNR: "0000211000", MITKZ: "K"}, {BUKRS: "2000", SAKNR: "0000159000", MITKZ: "K"}},
		LFA1: []sap.LFA1{{LIFNR: "0000000001", NAME1: "Tiny GmbH", STCD1: "DE1"}},
		LFB1: []sap.LFB1{{LIFNR: "0000000001", BUKRS: "2000", AKONT: "0000211000", ZTERM: "NT30"}},
		ACDOCA: []sap.ACDOCA{
			dpLine("000001", "0000159000", "1000.00", "0000000001", "A", "V1"),
			dpLine("000002", "0000140000", "190.00", "", "", "V1"),
			dpLine("000003", "0000100000", "-1190.00", "", "", ""),
		},
	}
}

// dpTarget is the platform for dpExtract: legal entity 2000 in EUR with
// 159000 as its down_payment account and the tax codes V1 (19% input tax
// on 140000), A1 (output tax), V2 (two components) and V3 (from 2027).
func dpTarget() target {
	return target{
		entities: map[string]tenancy.Entity{"2000": {Code: "2000", LocalCurrency: "EUR"}},
		charts: map[string]map[string]ledger.ChartAccount{"2000": {"100000": {}, "140000": {}, "170000": {},
			"211000": {ReconciliationKind: "supplier"}, "159000": {ReconciliationKind: "supplier", DownPayment: true}}},
		terms:     map[string]bool{"NT30": true},
		ranges:    map[string]bool{"2000/journal_kz/2026": true, "2000/journal_ob/2026": true},
		exponents: map[string]int{"EUR": 2},
		taxes: map[string]map[string]config.TaxCode{"2000": {
			"V1": {Code: "V1", RatePercent: "19", Account: "140000"},
			"A1": {Code: "A1", Direction: config.TaxOutput, RatePercent: "19", Account: "140000"},
			"V2": {Code: "V2", Components: []config.TaxComponent{
				{Key: "gst", AccountKey: config.KeyInput, Account: "140000", Rates: []config.TaxRate{{RatePercent: "5"}}},
				{Key: "pst", AccountKey: config.KeyNonDeductible, Rates: []config.TaxRate{{RatePercent: "7"}}}}},
			"V3": {Code: "V3", Components: []config.TaxComponent{
				{Key: "tax", AccountKey: config.KeyInput, Account: "140000", Rates: []config.TaxRate{{RatePercent: "19", ValidFrom: "2027-01-01"}}}}},
		}},
		year: 2026,
	}
}

// TestDownPaymentQuality: the quality gate takes an open SAP down payment
// that it can load as the platform posts one, and refuses, rather than
// guesses, one it cannot read (T72c): another special G/L indicator, A off
// the down payment account or a line on it without A, a down payment SAP
// cleared in documents mode, two in one document, a credit, a tax code
// that is not one deductible input tax with a rate, a tax not on the
// code's account or not the code's rate of the down payment (SAP's gross
// display), and in balances mode a cleared vendor line beside it or no
// number range for its document.
func TestDownPaymentQuality(t *testing.T) {
	line := func(e *sap.Extract, i int) *sap.ACDOCA { return &e.ACDOCA[i] }
	cases := []struct {
		name   string
		change func(e *sap.Extract, t *target)
		want   string // "" for no error
	}{
		{"net display", func(*sap.Extract, *target) {}, ""},
		{"without tax", func(e *sap.Extract, _ *target) { line(e, 0).MWSKZ, line(e, 1).MWSKZ = "", "" }, ""},
		{"balances mode", func(_ *sap.Extract, t *target) { t.balances = true }, ""},
		{"cleared in balances mode", func(e *sap.Extract, t *target) { t.balances, line(e, 0).AUGBL = true, "0100000010" }, ""},
		{"another indicator", func(e *sap.Extract, _ *target) { line(e, 0).UMSKZ = "F" }, `special G/L indicator "F" is not migrated`},
		{"off the down payment account", func(e *sap.Extract, _ *target) { line(e, 0).RACCT = "0000211000" },
			"which is not the legal entity's down_payment account"},
		{"no indicator", func(e *sap.Extract, _ *target) { line(e, 0).UMSKZ = "" }, "without special G/L indicator A"},
		{"cleared in documents mode", func(e *sap.Extract, _ *target) { line(e, 0).AUGBL = "0100000010" }, "its clearing is not migrated in documents mode"},
		{"two in a document", func(e *sap.Extract, _ *target) {
			e.ACDOCA = append(e.ACDOCA, dpLine("000004", "0000159000", "10.00", "0000000001", "A", ""), dpLine("000005", "0000100000", "-10.00", "", "", ""))
		}, "document holds 2 open down payments"},
		{"a credit", func(e *sap.Extract, _ *target) {
			line(e, 0).HSL, line(e, 1).HSL, line(e, 2).HSL = "-1000.00", "-190.00", "1190.00"
		}, "the open down payment is not a debit"},
		{"unknown tax code", func(e *sap.Extract, _ *target) { line(e, 0).MWSKZ, line(e, 1).MWSKZ = "ZZ", "ZZ" }, "tax code ZZ is not a tax code of company 2000"},
		{"output tax code", func(e *sap.Extract, _ *target) { line(e, 0).MWSKZ, line(e, 1).MWSKZ = "A1", "A1" }, "is not an input tax code with one deductible component"},
		{"two components", func(e *sap.Extract, _ *target) { line(e, 0).MWSKZ, line(e, 1).MWSKZ = "V2", "V2" }, "is not an input tax code with one deductible component"},
		{"no rate on the date", func(e *sap.Extract, _ *target) { line(e, 0).MWSKZ, line(e, 1).MWSKZ = "V3", "V3" }, "tax code V3 has no rate on 2026-03-10"},
		{"gross display, the clearing on the code", func(e *sap.Extract, _ *target) {
			line(e, 0).HSL, line(e, 2).HSL = "1190.00", "-1190.00"
			e.ACDOCA = append(e.ACDOCA, dpLine("000004", "0000170000", "-190.00", "", "", "V1"), dpLine("000005", "0000100000", "190.00", "", "", ""))
		}, "line on account 170000 carries the down payment's tax code V1, whose tax account is 140000"},
		{"gross display", func(e *sap.Extract, _ *target) {
			line(e, 0).HSL, line(e, 2).HSL = "1190.00", "-1190.00"
			e.ACDOCA = append(e.ACDOCA, dpLine("000004", "0000170000", "-190.00", "", "", ""), dpLine("000005", "0000100000", "190.00", "", "", ""))
		}, "the down payment's tax on V1 is 190.00, not 19% of the down payment, 226.10"},
		{"a tax out of range", func(e *sap.Extract, _ *target) {
			line(e, 0).HSL, line(e, 2).HSL = "92233720368547758.07", "-92233720368547758.07"
			line(e, 1).HSL = "0.00"
		}, "not 19% of the down payment"},
		{"taxes out of range", func(e *sap.Extract, _ *target) {
			line(e, 1).HSL = "92233720368547758.07"
			e.ACDOCA = append(e.ACDOCA, dpLine("000004", "0000140000", "92233720368547758.07", "", "", "V1"))
		}, "not 19% of the down payment"},
		{"a cleared vendor line beside it in balances mode", func(e *sap.Extract, t *target) {
			t.balances = true
			e.ACDOCA = append(e.ACDOCA, dpLine("000004", "0000211000", "10.00", "0000000001", "", ""), dpLine("000005", "0000100000", "-10.00", "", "", ""))
			e.ACDOCA[3].AUGBL = "0100000010"
		}, "has a vendor line cleared by 0100000010; load the year in documents mode"},
		{"no number range in balances mode", func(_ *sap.Extract, t *target) {
			t.balances = true
			delete(t.ranges, "2000/journal_kz/2026")
		}, "no journal_kz number range for the down payment's document"},
		{"an amount that does not parse", func(e *sap.Extract, _ *target) { line(e, 0).HSL = "x" }, "document does not balance"},
		{"a tax that does not parse", func(e *sap.Extract, _ *target) { line(e, 1).HSL = "x" }, "document does not balance"},
		{"a date that does not parse", func(e *sap.Extract, _ *target) { line(e, 0).BUDAT = "2026" }, `invalid SAP date "2026"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, tg := dpExtract(), dpTarget()
			c.change(e, &tg)
			var errors []string
			for _, i := range checkQuality(e, tg) {
				if i.Severity == SevError {
					errors = append(errors, i.Message)
				}
			}
			got := strings.Join(errors, "; ")
			switch {
			case c.want == "" && got != "":
				t.Errorf("errors: got %s, want none", got)
			case c.want != "" && !strings.Contains(got, c.want):
				t.Errorf("errors: got %q, want one containing %q", got, c.want)
			}
		})
	}
}
