package sapconfig

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// TestDiscountTermsFromT052: SAP's payment terms keep up to two cash
// discount tiers and the net days in ZTAG1/ZPRZ1, ZTAG2/ZPRZ2 and ZTAG3; a
// position without a percent holds the net days. The tiers become the
// term's discounts; SAP's cash discount received account (T030 SKE)
// becomes the company's cash_discount. A term whose last discount runs to
// its net due date, or whose numbers do not parse, is noted and left to
// fail validation until a reviewer sets it.
func TestDiscountTermsFromT052(t *testing.T) {
	cases := map[string]struct {
		row       sap.T052
		net       int
		discounts string
		note      string // the path a note names; "" means none on the term
	}{
		"two tiers": {sap.T052{ZTAG1: "14", ZPRZ1: "3.000", ZTAG2: "30", ZPRZ2: "2.000", ZTAG3: "45"}, 45, "[{14 3.000} {30 2.000}]", ""},
		"one tier":  {sap.T052{ZTAG1: "14", ZPRZ1: "2.000", ZTAG2: "30", ZPRZ2: "0.000"}, 30, "[{14 2.000}]", ""},
		"net only":  {sap.T052{ZTAG1: "30", ZPRZ1: "0.000"}, 30, "[]", ""},
		"discount until the net due date": {sap.T052{ZTAG1: "14", ZPRZ1: "2.000"}, 14, "[{14 2.000}]",
			"payment_terms[ZB01].net_days"},
		"percent not a number": {sap.T052{ZTAG1: "14", ZPRZ1: "two", ZTAG2: "30"}, unknownDays, "[]", "payment_terms[ZB01].discounts"},
		"tier days not a number": {sap.T052{ZTAG1: "14", ZPRZ1: "3.000", ZTAG2: "x", ZPRZ2: "2.000", ZTAG3: "45"}, unknownDays, "[{14 3.000}]",
			"payment_terms[ZB01].discounts"},
		"second discount until the net due date": {sap.T052{ZTAG1: "14", ZPRZ1: "3.000", ZTAG2: "30", ZPRZ2: "2.000"}, 30, "[{14 3.000} {30 2.000}]",
			"payment_terms[ZB01].net_days"},
		"zero-filled, discount until the net due date": {sap.T052{ZTAG1: "014", ZPRZ1: "2.000", ZTAG2: "000", ZPRZ2: "0.000", ZTAG3: "000"}, 14,
			"[{14 2.000}]", "payment_terms[ZB01].net_days"},
		"zero-filled, second discount until the net due date": {sap.T052{ZTAG1: "014", ZPRZ1: "3.000", ZTAG2: "030", ZPRZ2: "2.000", ZTAG3: "000"},
			30, "[{14 3.000} {30 2.000}]", "payment_terms[ZB01].net_days"},
		"zero-filled, net only": {sap.T052{ZTAG1: "030", ZPRZ1: "0.000", ZTAG2: "000", ZPRZ2: "0.000", ZTAG3: "000"}, 30, "[]", ""},
		"decimal comma":         {sap.T052{ZTAG1: "14", ZPRZ1: "2,000", ZTAG2: "30", ZPRZ2: "0,000"}, 30, "[{14 2.000}]", ""},
		"months and fixed days": {sap.T052{ZTAG1: "0", ZPRZ1: "2.000", ZMONA1: "01", ZFAEL1: "15", ZTAG2: "30"}, unknownDays, "[]",
			"payment_terms[ZB01]"},
		"net days not a number": {sap.T052{ZTAG1: "14", ZPRZ1: "3.000", ZTAG2: "30", ZPRZ2: "2.000", ZTAG3: "x"}, unknownDays, "[{14 3.000} {30 2.000}]",
			"payment_terms[ZB01].net_days"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := sap.GenerateMock(42, "2026")
			c.row.ZTERM, c.row.TEXT1 = "ZB01", "Terms"
			e.T052 = append(e.T052, c.row)
			e.SKB1 = append(e.SKB1, sap.SKB1{BUKRS: "1000", SAKNR: "0000475000", TXT50: "Cash discount received"},
				sap.SKB1{BUKRS: "2000", SAKNR: "0000475000", TXT50: "Cash discount received"})
			e.T030 = append(e.T030, sap.T030{BUKRS: "1000", KTOSL: "SKE", KONTS: "0000475000"}, sap.T030{BUKRS: "2000", KTOSL: "SKE", KONTS: "0000475000"})
			cfg, notes := Generate(e, DefaultOptions())
			term := cfg.PaymentTerm("ZB01")
			if term.NetDays != c.net {
				t.Errorf("net days: got %d, want %d", term.NetDays, c.net)
			}
			if got := fmt.Sprint(term.Discounts); got != c.discounts {
				t.Errorf("discounts: got %s, want %s", got, c.discounts)
			}
			if got := cfg.Company("1000").AccountDetermination.CashDiscount; got != "475000" {
				t.Errorf("cash discount account from T030 SKE: got %q, want 475000", got)
			}
			all := notesText(notes)
			if got := strings.Contains(all, "payment_terms[ZB01]"); got != (c.note != "") {
				t.Errorf("notes on the term: got %v, want %v:\n%s", got, c.note != "", all)
			}
			if c.note != "" && !strings.Contains(all, c.note+": ") {
				t.Errorf("notes: got\n%s\nwant one on %s", all, c.note)
			}
			valid := !config.HasErrors(config.Validate(cfg, currency.Seeded()))
			if valid != (c.note == "") {
				t.Errorf("draft valid: got %v, want %v: %v", valid, c.note == "", config.Validate(cfg, currency.Seeded()))
			}
		})
	}
}
