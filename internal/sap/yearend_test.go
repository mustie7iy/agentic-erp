package sap

import (
	"fmt"
	"testing"
)

// crossYear is company 2000's invoice of 2025 that SAP cleared in 2026
// with a payment (both cleared by the payment, AUGGJ 2026), an invoice of
// 2025 it cleared in 2025, one of 2026 it cleared in 2027, one cleared
// without a clearing year in the extract and one with a clearing year
// that is no year, beside company 1000's invoice of 2025 cleared in 2026
// and the payment's line in another ledger and a noted line.
func crossYear() *Extract {
	line := func(cc, gjahr, belnr, docln, hsl, augbl, augdt, auggj string) ACDOCA {
		return ACDOCA{RLDNR: LeadingLedger, RBUKRS: cc, GJAHR: gjahr, BELNR: belnr, DOCLN: docln, RACCT: "0000211000", HSL: hsl,
			LIFNR: "0000000001", AUGBL: augbl, AUGDT: augdt, AUGGJ: auggj}
	}
	other := line("2000", "2025", "0100000001", "000002", "80.00", "", "", "")
	other.LIFNR = ""
	otherLedger := line("2000", "2026", "0100000002", "000001", "80.00", "0100000002", "20260115", "2026")
	otherLedger.RLDNR = "2L"
	noted := line("2000", "2025", "0100000005", "000001", "-5.00", "0100000002", "20260115", "2026")
	noted.BSTAT = StatusNoted
	return &Extract{ACDOCA: []ACDOCA{
		line("2000", "2025", "0100000001", "000001", "-80.00", "0100000002", "20260115", "2026"), other,
		line("2000", "2025", "0100000003", "000001", "-10.00", "0100000004", "20251201", "2025"),
		line("2000", "2026", "0100000002", "000001", "80.00", "0100000002", "20260115", "2026"), otherLedger,
		line("2000", "2026", "0100000006", "000001", "-30.00", "0100000099", "20270110", "2027"),
		line("2000", "2026", "0100000007", "000001", "-40.00", "0100000098", "", ""),
		line("2000", "2026", "0100000008", "000001", "-50.00", "0100000097", "20270110", "x027"),
		line("1000", "2025", "0100000001", "000001", "-90.00", "0100000002", "20260115", "2026"), noted,
	}}
}

// TestFilterScopeReadsTheYearEnd: a scope reads its journal at the fiscal
// year's end, a line SAP cleared in a later fiscal year open and one
// cleared without a clearing year, or with one that is no year, as AUGBL
// says; and it sets apart, whole, the earlier years' documents of its
// company codes a line of which SAP cleared in its year, in its ledger and
// without noted items (ADR-0034 As built, items cleared in a later fiscal
// year).
func TestFilterScopeReadsTheYearEnd(t *testing.T) {
	cleared := func(lines []ACDOCA) []string {
		var out []string
		for _, l := range lines {
			out = append(out, fmt.Sprintf("%s/%s/%s/%s %q %q %q", l.RBUKRS, l.GJAHR, l.BELNR, l.DOCLN, l.AUGBL, l.AUGDT, l.AUGGJ))
		}
		return out
	}
	y2026 := FilterScope(crossYear(), Scope{CompanyCodes: []string{"2000"}, FiscalYear: "2026"})
	for name, c := range map[string]struct{ got, want []string }{
		"2026's journal": {cleared(y2026.ACDOCA), []string{
			`2000/2026/0100000002/000001 "0100000002" "20260115" "2026"`,
			`2000/2026/0100000006/000001 "" "" ""`,
			`2000/2026/0100000007/000001 "0100000098" "" ""`,
			`2000/2026/0100000008/000001 "0100000097" "20270110" "x027"`}},
		"2026's earlier documents": {cleared(y2026.ClearedEarlier), []string{
			`2000/2025/0100000001/000001 "0100000002" "20260115" "2026"`,
			`2000/2025/0100000001/000002 "" "" ""`}},
	} {
		t.Run(name, func(t *testing.T) {
			if fmt.Sprint(c.got) != fmt.Sprint(c.want) {
				t.Errorf("got %q, want %q", c.got, c.want)
			}
		})
	}
	y2025 := FilterScope(crossYear(), Scope{CompanyCodes: []string{"2000"}, FiscalYear: "2025"})
	t.Run("2025's journal", func(t *testing.T) {
		want := []string{`2000/2025/0100000001/000001 "" "" ""`, `2000/2025/0100000001/000002 "" "" ""`,
			`2000/2025/0100000003/000001 "0100000004" "20251201" "2025"`}
		if got := cleared(y2025.ACDOCA); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("got %q, want %q", got, want)
		}
		if got := cleared(y2025.Noted); len(got) != 1 || got[0] != `2000/2025/0100000005/000001 "" "" ""` {
			t.Errorf("noted: got %q, want the noted line open at the year's end", got)
		}
		if y2025.ClearedEarlier != nil {
			t.Errorf("earlier documents: got %q, want none", cleared(y2025.ClearedEarlier))
		}
	})
}

func TestIsYear(t *testing.T) {
	for s, want := range map[string]bool{"2026": true, "0000": false, "": false, "202": false, "20260": false, "x026": false} {
		t.Run(s, func(t *testing.T) {
			if got := IsYear(s); got != want {
				t.Errorf("IsYear(%q): got %t, want %t", s, got, want)
			}
		})
	}
}
