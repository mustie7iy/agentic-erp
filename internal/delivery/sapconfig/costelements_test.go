package sapconfig

import (
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// TestCostElementsFromMockSAP: the mock's company 3000 takes its accounts'
// cost elements from CSKB-KATYP (T56h2): revenue 11, expenses 01, and its
// exchange rate accounts, which are no cost elements, none; 1000, which
// has no centers and whose area (TKA02) has no cost elements, keeps its
// types' defaults.
func TestCostElementsFromMockSAP(t *testing.T) {
	c, notes := Generate(sap.GenerateMock(42, "2026"), zoned())
	ca := c.Company("3000")
	for number, want := range map[string]config.CostElement{"400000": config.CostElementRevenue, "510000": config.CostElementPrimaryCost,
		"591000": config.CostElementPrimaryCost, "480000": config.CostElementNone, "680000": config.CostElementNone, "100000": ""} {
		t.Run(number, func(t *testing.T) {
			if got := ca.Account(number).CostElement; got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
	if got := c.Company("1000").Account("510000").CostElement; got != "" {
		t.Errorf("company 1000's 510000: got %q, want its type's default", got)
	}
	text := notesText(notes)
	for _, want := range []string{"companies[3000].chart_of_accounts[480000].cost_element: not a cost element in SAP (no CSKB row): none",
		"companies[1000].chart_of_accounts: the extract has no cost elements (CSKB) of the company's controlling areas (2000)"} {
		if !strings.Contains(text, want) {
			t.Errorf("notes: got\n%s\nwant %q", text, want)
		}
	}
}

// TestCostElementCategories: a category the account's type cannot take,
// a secondary one, or areas that disagree leave the default with a note;
// the slice valid at the cutover counts, else the latest with a note
// (A12(d)); a company whose areas have no cost elements
// keeps its defaults, without a note when the extract has none at all.
func TestCostElementCategories(t *testing.T) {
	base := func() *sap.Extract {
		return &sap.Extract{
			T001: []sap.T001{{BUKRS: "1000", BUTXT: "A", WAERS: "USD"}},
			SKB1: []sap.SKB1{{BUKRS: "1000", SAKNR: "0000400000", TXT50: "Revenue"}, {BUKRS: "1000", SAKNR: "0000510000", TXT50: "Costs"}},
			CSKS: []sap.CSKS{{KOKRS: "A1", KOSTL: "C1", BUKRS: "1000"}},
		}
	}
	for name, tc := range map[string]struct {
		cskb    []sap.CSKB
		account string
		want    config.CostElement
		note    string
	}{
		"the slice at the cutover": {[]sap.CSKB{{KOKRS: "A1", KSTAR: "0000510000", DATAB: "20200101", DATBI: "20251231", KATYP: "01"},
			{KOKRS: "A1", KSTAR: "0000510000", DATAB: "20260101", DATBI: "99991231", KATYP: "42"}}, "510000", config.CostElementPrimaryCost, ""},
		"none at the cutover": {[]sap.CSKB{{KOKRS: "A1", KSTAR: "0000510000", DATAB: "20200101", DATBI: "20241231", KATYP: "01"},
			{KOKRS: "A1", KSTAR: "0000510000", DATAB: "20250101", DATBI: "20250630", KATYP: "42"}}, "510000", "",
			"no CSKB slice of controlling area A1 is valid at the cutover date 20250701; its latest, to 20250630, is taken: an expired cost element"},
		"01 on revenue":    {[]sap.CSKB{{KOKRS: "A1", KSTAR: "0000400000", DATBI: "99991231", KATYP: "01"}}, "400000", "", "category 01 (CSKB-KATYP) does not fit the account's type, revenue"},
		"11 on an expense": {[]sap.CSKB{{KOKRS: "A1", KSTAR: "0000510000", DATBI: "99991231", KATYP: "11"}}, "510000", "", "category 11 (CSKB-KATYP) does not fit the account's type, expense"},
		"12 on revenue":    {[]sap.CSKB{{KOKRS: "A1", KSTAR: "0000400000", DATBI: "99991231", KATYP: "12"}}, "400000", config.CostElementRevenue, ""},
		"secondary": {[]sap.CSKB{{KOKRS: "A1", KSTAR: "0000510000", DATBI: "99991231", KATYP: "42"}}, "510000", "",
			"category 42 (CSKB-KATYP) is a secondary or statistical cost element"},
		"latest slice without a cutover": {[]sap.CSKB{{KOKRS: "A1", KSTAR: "0000510000", DATBI: "99991231", KATYP: "01"},
			{KOKRS: "A1", KSTAR: "0000510000", DATBI: "20251231", KATYP: "42"}}, "510000", config.CostElementPrimaryCost,
			"companies[1000].chart_of_accounts: no cutover date given (-cutover): each account's cost element is its latest CSKB slice's"},
		"areas disagree": {[]sap.CSKB{{KOKRS: "A1", KSTAR: "0000510000", DATBI: "99991231", KATYP: "01"}, {KOKRS: "A2", KSTAR: "0000510000", DATBI: "99991231", KATYP: "42"}},
			"510000", "", "the controlling areas give categories 01, 42"},
		"another area's only": {[]sap.CSKB{{KOKRS: "A9", KSTAR: "0000510000", DATBI: "99991231", KATYP: "01"}}, "510000", "",
			"companies[1000].chart_of_accounts: the extract has no cost elements (CSKB) of the company's controlling areas (A1)"},
		"none at all": {nil, "510000", "", ""},
		"no area known": {[]sap.CSKB{{KOKRS: "A1", KSTAR: "0000510000", DATBI: "99991231", KATYP: "01"}}, "510000", "",
			"controlling areas (none known: the company has no centers, and the extract no TKA02 row of it)"},
		"area from TKA02": {[]sap.CSKB{{KOKRS: "A7", KSTAR: "0000510000", DATBI: "99991231", KATYP: "01"}}, "510000", config.CostElementPrimaryCost, ""},
	} {
		t.Run(name, func(t *testing.T) {
			x := base()
			x.CSKB = tc.cskb
			switch name {
			case "no area known": // a company code without centers, of an extract without TKA02
				x.CSKS = nil
			case "area from TKA02": // TKA02 names the area of a company code without centers (A8)
				x.CSKS, x.TKA02 = nil, []sap.TKA02{{BUKRS: "1000", KOKRS: "A7"}}
			}
			if slices.ContainsFunc(tc.cskb, func(r sap.CSKB) bool { return r.KOKRS == "A2" }) { // a second area of the company, by its profit centers
				x.CEPC_BUKRS = []sap.CEPCBukrs{{KOKRS: "A2", PRCTR: "P1", BUKRS: "1000"}, {KOKRS: "A2", PRCTR: "P2", BUKRS: "1000"}}
			}
			opts := zoned()
			opts.Cutover = "20260101"
			if cutover, ok := map[string]string{"the slice at the cutover": "20250701", "none at the cutover": "20250701",
				"latest slice without a cutover": ""}[name]; ok {
				opts.Cutover = cutover
			}
			c, notes := Generate(x, opts)
			if got := c.Company("1000").Account(tc.account).CostElement; got != tc.want {
				t.Errorf("cost element: got %q, want %q", got, tc.want)
			}
			text := notesText(notes)
			if tc.note != "" && !strings.Contains(text, tc.note) {
				t.Errorf("notes: got\n%s\nwant %q", text, tc.note)
			}
			if path := "chart_of_accounts[" + tc.account + "].cost_element"; tc.note == "" && strings.Contains(text, path) {
				t.Errorf("notes: got\n%s\nwant none on %s", text, path)
			}
		})
	}
}

// TestCostElementAccountTypes: an account without a CSKB row is none when
// its G/L account type (SKA1-GLACCOUNT_TYPE) is N or not given, and keeps
// its type's default with a note when the type says it is a cost element
// or not a profit and loss account (A12(c)).
func TestCostElementAccountTypes(t *testing.T) {
	for glType, tc := range map[string]struct {
		want config.CostElement
		note string
	}{
		"":  {config.CostElementNone, "not a cost element in SAP (no CSKB row): none"},
		"N": {config.CostElementNone, "not a cost element in SAP (no CSKB row): none"},
		"P": {"", "no CSKB row, but its G/L account type (SKA1-GLACCOUNT_TYPE) P says it is a cost element; left at its type's default"},
		"S": {"", "no CSKB row, but its G/L account type (SKA1-GLACCOUNT_TYPE) S says it is a cost element"},
		"X": {"", "no CSKB row, and its G/L account type (SKA1-GLACCOUNT_TYPE) X is not a profit and loss one"},
	} {
		t.Run("type "+glType, func(t *testing.T) {
			x := &sap.Extract{
				T001: []sap.T001{{BUKRS: "1000", BUTXT: "A", WAERS: "USD"}},
				SKB1: []sap.SKB1{{BUKRS: "1000", SAKNR: "0000400000", TXT50: "Revenue"},
					{BUKRS: "1000", SAKNR: "0000510000", TXT50: "Costs", GLACCOUNT_TYPE: glType}},
				TKA02: []sap.TKA02{{BUKRS: "1000", KOKRS: "A1"}},
				CSKB:  []sap.CSKB{{KOKRS: "A1", KSTAR: "0000400000", DATBI: "99991231", KATYP: "11"}},
			}
			opts := zoned()
			opts.Cutover = "20260101"
			c, notes := Generate(x, opts)
			if got := c.Company("1000").Account("510000").CostElement; got != tc.want {
				t.Errorf("cost element: got %q, want %q", got, tc.want)
			}
			if text := notesText(notes); !strings.Contains(text, "chart_of_accounts[510000].cost_element: "+tc.note) {
				t.Errorf("notes: got\n%s\nwant %q", text, tc.note)
			}
		})
	}
}
