package integration

import (
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// TestReconciliationAccounts: the open vendor items of each vendor
// reconciliation account are checked against its balance at the key date,
// the company's latest posting date (po:2026-11-09, A15): open by AUGDT,
// noted items and the special G/L accounts left out, and a difference
// warned of with its parts, never refused.
func TestReconciliationAccounts(t *testing.T) {
	item := func(hsl string, change func(l *sap.ACDOCA)) func(e *sap.Extract) {
		return func(e *sap.Extract) {
			l := sap.ACDOCA{RACCT: "0000211000", HSL: hsl, LIFNR: "0000000001"}
			change(&l)
			withDocument(e, "0100000011", "EUR", l)
		}
	}
	same := func(*sap.ACDOCA) {}
	for _, c := range []struct {
		name   string
		change func(e *sap.Extract)
		want   string // the warning's start after the account, "" for none
	}{
		{"no line on the vendor account", func(*sap.Extract) {}, ""},
		{"an open item", item("-50.00", same), ""},
		{"an item cleared after the key date", item("-50.00", func(l *sap.ACDOCA) { l.AUGBL, l.AUGDT = "0100000020", "20261231" }), ""},
		{"a noted item", func(e *sap.Extract) {
			item("-50.00", same)(e)
			e.ACDOCA = append(e.ACDOCA, sap.ACDOCA{RBUKRS: "2000", RACCT: "0000211000", HSL: "-30.00", LIFNR: "0000000001", BSTAT: "S", BUDAT: "20260310"})
		}, ""},
		{"an amount that does not parse", item("x", same), ""},
		{"an item cleared by the key date", item("-50.00", func(l *sap.ACDOCA) { l.AUGBL, l.AUGDT = "0100000020", "20260310" }),
			"at the key date 20260310 add up to 0.00 EUR, its balance to -50.00 EUR; of the difference: lines without a vendor (LIFNR blank: " +
				"a balance without vendor items, a period 000 carry-forward, a valuation posted to the account) 0.00 EUR; special G/L lines (UMSKZ, " +
				"which belong on an alternative reconciliation account, T074) 0.00 EUR; vendor items cleared by the key date that do not net to zero " +
				"in the extract (a clearing in another year or on another account) -50.00 EUR. It is reported, not refused"},
		{"an item cleared without a clearing date", item("-50.00", func(l *sap.ACDOCA) { l.AUGBL, l.AUGDT = "0100000020", "00000000" }),
			"at the key date 20260310 add up to 0.00 EUR, its balance to -50.00 EUR"},
		{"a special G/L line", item("-50.00", func(l *sap.ACDOCA) { l.UMSKZ = "W" }), "add up to 0.00 EUR, its balance to -50.00 EUR; of the difference: " +
			"lines without a vendor (LIFNR blank: a balance without vendor items, a period 000 carry-forward, a valuation posted to the account) " +
			"0.00 EUR; special G/L lines (UMSKZ, which belong on an alternative reconciliation account, T074) -50.00 EUR"},
		{"a line without a vendor", item("-50.00", func(l *sap.ACDOCA) { l.LIFNR = "" }), "add up to 0.00 EUR, its balance to -50.00 EUR; of the " +
			"difference: lines without a vendor (LIFNR blank: a balance without vendor items, a period 000 carry-forward, a valuation posted to the account) -50.00 EUR"},
		{"an item of a vendor whose account changed", func(e *sap.Extract) {
			item("-50.00", func(l *sap.ACDOCA) { l.AUGBL = "0100000020" })(e)
			e.LFB1[0].AKONT = "0000159000"
		}, "add up to 0.00 EUR, its balance to -50.00 EUR"},
		{"an open item of a vendor without company data", func(e *sap.Extract) {
			item("-50.00", same)(e)
			item("-20.00", func(l *sap.ACDOCA) { l.AUGBL = "0100000020" })(e)
			e.LFB1 = nil
		}, "(a clearing in another year or on another account) -20.00 EUR. It is reported"},
		{"lines adding up to more than an amount holds", func(e *sap.Extract) {
			item("92233720368547758.07", same)(e)
			item("92233720368547758.07", same)(e)
		}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, tg := dpExtract(), dpTarget()
			c.change(e)
			var got []string
			for _, i := range checkQuality(e, tg) {
				if i.Table == "ACDOCA" && i.Key == "2000/211000" {
					if i.Severity != SevWarning {
						t.Errorf("severity: got %s, want %s", i.Severity, SevWarning)
					}
					got = append(got, i.Message)
				}
			}
			switch {
			case c.want == "" && len(got) > 0:
				t.Errorf("warnings: got %q, want none", got)
			case c.want != "" && len(got) != 1:
				t.Errorf("warnings: got %q, want one containing %q", got, c.want)
			case c.want != "" && !strings.Contains(got[0], c.want):
				t.Errorf("warning: got %q, want it to contain %q", got[0], c.want)
			}
		})
	}
}

// TestReconciliationAccountOfAChangedVendor: an open item on an account a
// vendor's master no longer names (LFB1-AKONT changed) is said so.
func TestReconciliationAccountOfAChangedVendor(t *testing.T) {
	e, tg := dpExtract(), dpTarget()
	withDocument(e, "0100000011", "EUR", sap.ACDOCA{RACCT: "0000211000", HSL: "-50.00", LIFNR: "0000000001"})
	withDocument(e, "0100000012", "EUR", sap.ACDOCA{RACCT: "0000211000", HSL: "-20.00", LIFNR: "0000000001", AUGBL: "0100000020"})
	e.LFB1[0].AKONT = "0000159000"
	want := "; 1 open items belong to vendors whose reconciliation account (LFB1-AKONT) is now another. It is reported"
	for _, i := range checkQuality(e, tg) {
		if i.Key == "2000/211000" && strings.Contains(i.Message, want) {
			return
		}
	}
	t.Errorf("warnings: got none containing %q", want)
}
