package sapconfig

import (
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// TestDefaultAssignmentsFromMockSAP: of the mock's OKB9 rows for company
// 3000 (T56h3) only salaries' general assignment to cost center 3100 is
// proposed, in a note giving the entry for a change after the migration,
// and none is in the draft, which is provisioned before the cost centers
// are loaded; each other row is noted with why, the detail rows a REGEL
// names, and never TKA3A's own cost center as a fallback.
func TestDefaultAssignmentsFromMockSAP(t *testing.T) {
	c, notes := Generate(sap.GenerateMock(42, "2026"), zoned())
	for _, co := range c.Companies {
		if co.DefaultAssignments != nil {
			t.Errorf("company %s's default assignments in the draft: got %v, want none", co.Code, co.DefaultAssignments)
		}
	}
	text := notesText(notes)
	p := "companies[3000].default_assignments"
	for _, want := range []string{
		p + `[610000]: propose after the migration: {account: "610000", cost_center: "3100"}, the default cost center from TKA3A (OKB9, REGEL blank); ` +
			"the ledger activates a default only for a cost center it holds, so it is left out of the first configuration until the run has loaded 3100",
		p + "[520000]: TKA3A 1000/520000: cost center 1000/3999 is not one of the company's cost centers in the extract (CSKS), so the migration does not load it; not proposed",
		p + "[599000]: TKA3A 1000/599000: account 599000 is not in the company's chart of accounts; not proposed",
		p + "[510000]: TKA3A 1000/510000: REGEL 1, valuation area mandatory: SAP takes the object per valuation area from TKA3C " +
			"(TKA3C valuation area 3000, business area (blank): cost center 3120); the platform keeps one cost center per company and account; not proposed",
		p + "[530000]: TKA3A 1000/530000: REGEL 2, business area mandatory: SAP takes the object per business area from TKA3C " +
			"(TKA3C valuation area (blank), business area CA01: cost center 3100; TKA3C valuation area (blank), business area CA02: cost center 3200)",
		p + "[540000]: TKA3A 1000/540000: REGEL 3, profit center mandatory: SAP takes the object per profit center from TKA3P " +
			"(TKA3P profit center P3100: cost center 3120; TKA3P profit center P3200: cost center 3200); the platform keeps one cost center " +
			"per company and account, and TKA3A's own cost center 3120 is not taken as a fallback; not proposed",
		p + "[591000]: TKA3A 1000/591000: order 000000400001 without a cost center: the platform's default assignments hold cost centers only; not proposed",
		p + "[400000]: TKA3A 1000/400000: profit center P3100 without a cost center",
		p + "[680000]: TKA3A 1000/680000: the profitability segment is found by substitution (BSSUBST X), which the platform does not have; not proposed",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("notes: got\n%s\nwant %q", text, want)
		}
	}
	if n := strings.Count(text, "; not proposed"); n != 8 {
		t.Errorf("rows not proposed: got %d, want 8", n)
	}
}

// TestDefaultAssignmentRows: the rules for one TKA3A row of company 1000,
// whose chart has an expense 510000 and an exchange loss 680000 that is no
// cost element in SAP, and whose cost centers C1 (open), C2 (in two
// company codes), C3 (ended) and C4 (locked) are in area A1. No row is
// ever drafted into the configuration itself.
func TestDefaultAssignmentRows(t *testing.T) {
	base := func() *sap.Extract {
		return &sap.Extract{
			T001: []sap.T001{{BUKRS: "1000", BUTXT: "A", WAERS: "USD"}},
			SKB1: []sap.SKB1{{BUKRS: "1000", SAKNR: "0000510000", TXT50: "Costs"}, {BUKRS: "1000", SAKNR: "0000680000", TXT50: "FX losses"}},
			CSKS: []sap.CSKS{{KOKRS: "A1", KOSTL: "C1", BUKRS: "1000", DATBI: "20241231"}, {KOKRS: "A1", KOSTL: "C1", BUKRS: "1000", DATBI: "99991231"},
				{KOKRS: "A1", KOSTL: "C2", BUKRS: "1000", DATBI: "99991231"}, {KOKRS: "A1", KOSTL: "C2", BUKRS: "2000", DATBI: "99991231"},
				{KOKRS: "A1", KOSTL: "C3", BUKRS: "1000", DATBI: "20261231"}, {KOKRS: "A1", KOSTL: "C4", BUKRS: "1000", DATBI: "99991231", BKZKP: "X"}},
			CSKB: []sap.CSKB{{KOKRS: "A1", KSTAR: "0000510000", DATBI: "99991231", KATYP: "01"}},
		}
	}
	row := sap.TKA3A{MANDT: "100", BUKRS: "1000", KSTAR: "0000510000", KOKRS: "A1", KOSTL: "C1"}
	with := func(f func(*sap.TKA3A)) sap.TKA3A { r := row; f(&r); return r }
	for name, tc := range map[string]struct {
		row   sap.TKA3A
		tka3c []sap.TKA3C
		tka3p []sap.TKA3P
		note  string
	}{
		"general assignment": {row: row, note: `propose after the migration: {account: "510000", cost_center: "C1"}`},
		"with an order and profit center beside it": {row: with(func(r *sap.TKA3A) { r.AUFNR, r.PRCTR = "4711", "P1" }),
			note: "TKA3A's order 4711, profit center P1 beside it is not proposed, as a default holds a cost center alone"},
		"another company's": {row: with(func(r *sap.TKA3A) { r.BUKRS = "2000" })},
		"another area's cost center": {row: with(func(r *sap.TKA3A) { r.KOKRS = "A2" }),
			note: "cost center A2/C1 is not one of the company's cost centers in the extract"},
		"a cost center in two company codes": {row: with(func(r *sap.TKA3A) { r.KOSTL = "C2" }),
			note: "cost center A1/C2 is not one of the company's cost centers in the extract"},
		"an ended cost center": {row: with(func(r *sap.TKA3A) { r.KOSTL = "C3" }),
			note: "cost center A1/C3 ends on 20261231 (its latest CSKS slice), so the ledger takes no default on it"},
		"a locked cost center": {row: with(func(r *sap.TKA3A) { r.KOSTL = "C4" }),
			note: "cost center A1/C4 is locked for actual costs (BKZKP X in its latest CSKS slice)"},
		"an account that needs no cost center": {row: with(func(r *sap.TKA3A) { r.KSTAR = "0000680000" }),
			note: "account 680000 has cost element none; a default cost center is only for accounts that need one (primary_cost)"},
		"an account not in the chart": {row: with(func(r *sap.TKA3A) { r.KSTAR = "0000599000" }),
			note: "account 599000 is not in the company's chart of accounts; not proposed"},
		"no object at all": {row: with(func(r *sap.TKA3A) { r.KOSTL = "" }),
			note: "no cost center, order or profit center without a cost center"},
		"an unknown detail indicator": {row: with(func(r *sap.TKA3A) { r.REGEL = "4" }),
			note: "REGEL 4 is not a detail indicator the specialist described (blank, 1, 2 or 3); not proposed"},
		"a detail indicator and a substitution": {row: with(func(r *sap.TKA3A) { r.REGEL, r.BSSUBST = "1", "X" }),
			note: "is not taken as a fallback; the profitability segment is found by substitution (BSSUBST X), which the platform does not have; not proposed"},
		"REGEL 1 without detail rows": {row: with(func(r *sap.TKA3A) { r.REGEL = "1" }),
			note: "(the extract has no TKA3C rows for it); the platform keeps one cost center per company and account, and TKA3A's own cost center C1 is not taken as a fallback"},
		"REGEL 2 with a detail row naming nothing": {row: with(func(r *sap.TKA3A) { r.REGEL, r.KOSTL = "2", "" }),
			tka3c: []sap.TKA3C{{BUKRS: "1000", KSTAR: "0000510000", GSBER: "B1", KOKRS: "A1"}, {BUKRS: "1000", KSTAR: "0000520000", GSBER: "B2", KOKRS: "A1", KOSTL: "C1"},
				{BUKRS: "1000", KSTAR: "0000510000", GSBER: "B3", KOKRS: "A1", AUFNR: "4711", PRCTR: "P1"}},
			note: "(TKA3C valuation area (blank), business area B1: no object; TKA3C valuation area (blank), business area B3: order 4711, profit center P1); the platform keeps one cost center per company and account; not proposed"},
		"REGEL 3 without detail rows": {row: with(func(r *sap.TKA3A) { r.REGEL = "3" }),
			tka3p: []sap.TKA3P{{BUKRS: "2000", KSTAR: "0000510000", PRCTR: "P1", KOSTL: "C1"}}, note: "(the extract has no TKA3P rows for it)"},
		"REGEL 3 with an order": {row: with(func(r *sap.TKA3A) { r.REGEL = "3" }),
			tka3p: []sap.TKA3P{{BUKRS: "1000", KSTAR: "0000510000", PRCTR: "P1", AUFNR: "4711"}, {BUKRS: "1000", KSTAR: "0000510000", PRCTR: "P2"}},
			note:  "(TKA3P profit center P1: order 4711; TKA3P profit center P2: no object)"},
	} {
		t.Run(name, func(t *testing.T) {
			x := base()
			x.TKA3A, x.TKA3C, x.TKA3P = []sap.TKA3A{tc.row}, tc.tka3c, tc.tka3p
			c, notes := Generate(x, zoned())
			if got := c.Company("1000").DefaultAssignments; got != nil {
				t.Errorf("default assignments in the draft: got %v, want none", got)
			}
			text := notesText(notes)
			if tc.note != "" && !strings.Contains(text, tc.note) {
				t.Errorf("notes: got\n%s\nwant %q", text, tc.note)
			}
			if tc.note == "" && strings.Contains(text, "default_assignments") {
				t.Errorf("notes: got\n%s\nwant none on default assignments", text)
			}
		})
	}
}
