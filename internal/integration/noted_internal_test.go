package integration

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// TestNotedItemsReported: the quality gate warns of the noted items the
// scope set aside (review:batch63), per company how many, and each open
// one by what its line says (its vendor by number and name, its amount in
// its document currency too where that is not the company's), in document
// order whatever the extract's; one whose amount does not parse is still only warned of, its amount
// shown as extracted, since it is never posted.
func TestNotedItemsReported(t *testing.T) {
	e0 := dpExtract()
	line := func(belnr, docln string, change func(l *sap.ACDOCA)) sap.ACDOCA {
		l := sap.ACDOCA{RLDNR: "0L", RBUKRS: "2000", GJAHR: "2026", BELNR: belnr, DOCLN: docln, HSL: "-30.00", BUDAT: "20260310", BSTAT: sap.StatusNoted}
		change(&l)
		return l
	}
	const advice = " is not migrated; if it is still wanted, enter it on the platform (a down payment request is a payables document, not a journal entry)"
	summary := func(lines, documents, open int) string {
		return fmt.Sprintf("2000: noted items (BSTAT S, such as down payment requests) of company 2000 are not migrated: lines %d, documents %d, "+
			"open in SAP (AUGBL blank; each listed) %d. SAP posts nothing to the G/L for a noted item, so it loads as no journal entry and counts as "+
			"no open item", lines, documents, open)
	}
	for _, c := range []struct {
		name  string
		noted []sap.ACDOCA
		want  []string
	}{
		{"none", nil, nil},
		{"cleared ones alone", []sap.ACDOCA{line("0100000031", "000001", func(l *sap.ACDOCA) { l.AUGBL = "0100000040" })}, []string{summary(1, 1, 0)}},
		{"open ones in document order", []sap.ACDOCA{
			line("0100000032", "000001", func(l *sap.ACDOCA) { l.HSL = "x" }),
			line("0100000031", "000003", func(l *sap.ACDOCA) { l.LIFNR, l.RWCUR, l.WSL = "0000000009", "USD", "-33.00" }),
			line("0100000031", "000002", func(l *sap.ACDOCA) { l.LIFNR, l.UMSKZ, l.RWCUR, l.WSL = e0.LFA1[0].LIFNR, "F", "EUR", "-30.00" }),
			line("0100000031", "000001", func(l *sap.ACDOCA) { l.AUGBL = "0100000040" })},
			[]string{summary(4, 2, 3),
				"2000/2026/0100000031/000002: open noted item (BSTAT S) of vendor " + e0.LFA1[0].LIFNR + " " + fmt.Sprintf("%q", e0.LFA1[0].NAME1) +
					` with special G/L indicator "F" posted on 20260310 over -30.00 EUR` + advice,
				`2000/2026/0100000031/000003: open noted item (BSTAT S) of vendor 0000000009 posted on 20260310 over -30.00 EUR ` +
					"(-33.00 USD in its document currency)" + advice,
				`2000/2026/0100000032/000001: open noted item (BSTAT S) posted on 20260310 over "x" EUR (as extracted)` + advice}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, tg := dpExtract(), dpTarget()
			tg.exponents["USD"] = 2
			e.Noted = c.noted
			var got []string
			for _, i := range checkQuality(e, tg) {
				if strings.Contains(i.Message, "noted item") {
					if i.Severity != SevWarning || i.Table != "ACDOCA" {
						t.Errorf("finding %s: got %s on %s, want a warning on ACDOCA", i.Key, i.Severity, i.Table)
					}
					got = append(got, i.Key+": "+i.Message)
				}
			}
			if strings.Join(got, "\n") != strings.Join(c.want, "\n") {
				t.Errorf("noted items reported:\ngot  %s\nwant %s", strings.Join(got, "\n"), strings.Join(c.want, "\n"))
			}
		})
	}
}
