package integration

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// gateCenters is a valid scope of company 2000 with controlling area A1:
// segment S1, profit center P1 and cost center C1 with it.
func gateCenters() *sap.Extract {
	return &sap.Extract{
		FAGL_SEGM:  []sap.FAGLSegm{{SEGMENT: "S1"}},
		FAGL_SEGMT: []sap.FAGLSegmt{{SEGMENT: "S1", LANGU: "EN", NAME: "One"}},
		CEPC:       []sap.CEPC{{KOKRS: "A1", PRCTR: "P1", DATAB: "20200101", DATBI: "99991231", SEGMENT: "S1", VERAK: "Ana"}},
		CEPCT:      []sap.CEPCT{{KOKRS: "A1", PRCTR: "P1", DATBI: "99991231", SPRAS: "E", KTEXT: "P"}},
		CEPC_BUKRS: []sap.CEPCBukrs{{KOKRS: "A1", PRCTR: "P1", BUKRS: "2000"}},
		CSKS:       []sap.CSKS{{KOKRS: "A1", KOSTL: "C1", DATAB: "20200101", DATBI: "99991231", BUKRS: "2000", KOSAR: "ADMIN", VERAK: "Bo", PRCTR: "P1", WAERS: "EUR"}},
		CSKT:       []sap.CSKT{{KOKRS: "A1", KOSTL: "C1", DATBI: "99991231", SPRAS: "E", KTEXT: "C"}},
	}
}

func gate(x *sap.Extract) []Issue {
	q := &qualityCheck{e: x, t: target{categories: map[string]bool{"ADMIN": true}}, currency: map[string]string{"2000": "EUR"}}
	q.centers()
	return q.out
}

// TestCenterQualityGate: the gate refuses controlling master data the
// platform cannot hold or the load cannot read, and warns of what it
// loads differently (ADR-0050 §10, T56h).
func TestCenterQualityGate(t *testing.T) {
	if got := gate(gateCenters()); len(got) != 0 {
		t.Fatalf("a valid scope: got %v, want no issue", got)
	}
	for name, tc := range map[string]struct {
		change func(x *sap.Extract)
		want   string
	}{
		"a segment code":         {func(x *sap.Extract) { x.FAGL_SEGM[0].SEGMENT = "s 1" }, `error FAGL_SEGM s 1: segment "s 1" is not a code`},
		"a segment without name": {func(x *sap.Extract) { x.FAGL_SEGMT = nil }, "error FAGL_SEGM S1: no text with a name"},
		"a segment in German":    {func(x *sap.Extract) { x.FAGL_SEGMT[0].LANGU = "D" }, `warning FAGL_SEGM S1: no text in English; it loads named "One", its text in language D`},
		"an assignment orphaned": {func(x *sap.Extract) { x.CEPC_BUKRS[0].PRCTR = "P9" }, "error CEPC_BUKRS A1/P9/2000: profit center P9 of controlling area A1 is not in CEPC"},
		"a profit center twice": {func(x *sap.Extract) {
			x.CEPC = append(x.CEPC, sap.CEPC{KOKRS: "A2", PRCTR: "P1", DATAB: "20200101", DATBI: "99991231", VERAK: "V"})
		}, "error CEPC P1: profit center P1 is in controlling areas A1, A2"},
		"a profit center code":    {func(x *sap.Extract) { x.CEPC[0].PRCTR = "p1" }, `error CEPC A1/p1: profit center "p1" is not a code`},
		"a profit center's dates": {func(x *sap.Extract) { x.CEPC[0].DATAB = "2020" }, `error CEPC A1/P1: DATAB: invalid SAP date "2020"`},
		"a lock":                  {func(x *sap.Extract) { x.CEPC[0].LOCK_IND = "Y" }, `error CEPC A1/P1: LOCK_IND "Y" is neither X nor blank`},
		"two segments": {func(x *sap.Extract) {
			x.CEPC[0].DATBI = "20251231"
			x.CEPC = append(x.CEPC, sap.CEPC{KOKRS: "A1", PRCTR: "P1", DATAB: "20260101", DATBI: "99991231", SEGMENT: "S2", VERAK: "Ana"})
		}, `error CEPC A1/P1: its slices name segments "S1" and "S2"`},
		"an unknown segment":           {func(x *sap.Extract) { x.CEPC[0].SEGMENT = "S9" }, "error CEPC A1/P1: segment S9 is not in FAGL_SEGM"},
		"no one responsible for a PC":  {func(x *sap.Extract) { x.CEPC[0].VERAK = " " }, "error CEPC A1/P1: no person responsible (VERAK)"},
		"a profit center without name": {func(x *sap.Extract) { x.CEPCT[0].KTEXT = "" }, "error CEPC A1/P1: no text with a name"},
		"a cost center code":           {func(x *sap.Extract) { x.CSKS[0].KOSTL = "c1"; x.CSKT[0].KOSTL = "c1" }, `error CSKS A1/c1: cost center "c1" is not a code`},
		"two companies": {func(x *sap.Extract) {
			x.CSKS[0].DATBI = "20251231"
			x.CSKS = append(x.CSKS, sap.CSKS{KOKRS: "A1", KOSTL: "C1", DATAB: "20260101", DATBI: "99991231", BUKRS: "1000", KOSAR: "ADMIN", VERAK: "Bo"})
		}, "error CSKS A1/C1: its slices are in company codes 2000 and 1000"},
		"no one responsible for a CC": {func(x *sap.Extract) { x.CSKS[0].VERAK = "" }, "error CSKS A1/C1: no person responsible (VERAK)"},
		"another currency":            {func(x *sap.Extract) { x.CSKS[0].WAERS = "USD" }, "warning CSKS A1/C1: currency USD is not company 2000's EUR"},
		"a cost center's lock":        {func(x *sap.Extract) { x.CSKS[0].BKZKP = "1" }, `error CSKS A1/C1: BKZKP "1" is neither X nor blank`},
		"an end before the start":     {func(x *sap.Extract) { x.CSKS[0].DATBI = "20191231" }, "error CSKS A1/C1: DATBI 20191231 is before DATAB 20200101"},
		"an end unread":               {func(x *sap.Extract) { x.CSKS[0].DATBI = "x" }, `error CSKS A1/C1: DATBI: invalid SAP date "x"`},
		"slices apart": {func(x *sap.Extract) {
			x.CSKS[0].DATBI = "20251231"
			x.CSKS = append(x.CSKS, sap.CSKS{KOKRS: "A1", KOSTL: "C1", DATAB: "20260201", DATBI: "99991231", BUKRS: "2000", KOSAR: "ADMIN", VERAK: "Bo"})
		}, "error CSKS A1/C1: its slices do not adjoin: one starting 2026-02-01 follows one ending 2025-12-31"},
		"a slice after an open one": {func(x *sap.Extract) {
			x.CSKS = append(x.CSKS, sap.CSKS{KOKRS: "A1", KOSTL: "C1", DATAB: "20260201", DATBI: "99991231", BUKRS: "2000", KOSAR: "ADMIN", VERAK: "Bo"})
		}, "error CSKS A1/C1: its slices do not adjoin: one starting 2026-02-01 follows one ending 99991231"},
		"an unknown category":        {func(x *sap.Extract) { x.CSKS[0].KOSAR = "E" }, `error CSKS A1/C1: category "E" (KOSAR) is not a cost center category of the configuration`},
		"an unknown profit center":   {func(x *sap.Extract) { x.CSKS[0].PRCTR = "P9" }, "error CSKS A1/C1: unknown profit center P9: not in CEPC of controlling area A1 assigned to company 2000"},
		"a PC of another company":    {func(x *sap.Extract) { x.CEPC_BUKRS[0].BUKRS = "1000" }, "error CSKS A1/C1: profit center P1 is not assigned to company 2000 (CEPC_BUKRS)"},
		"a PC not valid":             {func(x *sap.Extract) { x.CEPC[0].DATAB = "20210101" }, "error CSKS A1/C1: profit center P1 is not valid from 2020-01-01 to 99991231"},
		"a cost center without name": {func(x *sap.Extract) { x.CSKT = nil }, "error CSKS A1/C1: no text with a name"},
	} {
		t.Run(name, func(t *testing.T) {
			x := gateCenters()
			tc.change(x)
			var got []string
			for _, i := range gate(x) {
				got = append(got, fmt.Sprintf("%s %s %s: %s", i.Severity, i.Table, i.Key, i.Message))
			}
			if !strings.Contains(strings.Join(got, "\n"), tc.want) {
				t.Errorf("issues: got %q, want one starting %q", got, tc.want)
			}
		})
	}
}

// TestPickText: a text in English is taken over others, then the first
// language by key, each of its latest slice; one without a name is none.
func TestPickText(t *testing.T) {
	for name, tc := range map[string]struct {
		texts         []sapText
		name          string
		english, some bool
	}{
		"none":            {nil, "", false, false},
		"no name":         {[]sapText{{lang: "E", name: " "}}, "", false, false},
		"english first":   {[]sapText{{lang: "D", name: "Finanzen"}, {lang: "EN", name: "Finance"}}, "Finance", true, true},
		"latest slice":    {[]sapText{{lang: "E", datbi: "20251231", name: "Old"}, {lang: "E", datbi: "99991231", name: "New"}}, "New", true, true},
		"first by key":    {[]sapText{{lang: "F", name: "Finances"}, {lang: "D", name: "Finanzen"}}, "Finanzen", false, true},
		"english at last": {[]sapText{{lang: "E", name: "Finance"}, {lang: "D", name: "Finanzen"}}, "Finance", true, true},
	} {
		t.Run(name, func(t *testing.T) {
			got, english, ok := pickText(tc.texts)
			if got.name != tc.name || english != tc.english || ok != tc.some {
				t.Errorf("got %q, english %t, found %t; want %q, %t, %t", got.name, english, ok, tc.name, tc.english, tc.some)
			}
		})
	}
}

// TestCenterReconciliationReadsSAPAgain: a reconciliation extracts again
// without the gate, so a center whose slices do not read is refused there
// and compares as unreadable; a profit center id no run loaded is named
// by its id.
func TestCenterReconciliationReadsSAPAgain(t *testing.T) {
	x := gateCenters()
	x.CSKS[0].DATAB, x.CEPC[0].DATBI = "bad", "bad"
	rc := &reconciler{ext: x}
	if got := rc.sapCostCenter(costCenters(x)[0]); got != "unreadable" {
		t.Errorf("cost center: got %q, want unreadable", got)
	}
	if got := rc.sapProfitCenter(profitCenters(x)[0]); got != "unreadable" {
		t.Errorf("profit center: got %q, want unreadable", got)
	}
	if len(rc.rec.Issues) != 2 || !hasErrors(rc.rec.Issues) {
		t.Errorf("issues: got %v, want two errors", rc.rec.Issues)
	}
	id := uuid.New()
	st := &centerStates{profitCodes: map[uuid.UUID]string{}}
	if got := st.code(id); got != id.String() {
		t.Errorf("an unknown profit center: got %q, want its id", got)
	}
	if got := st.code(uuid.Nil); got != "" {
		t.Errorf("no profit center: got %q, want none", got)
	}
}
