package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// lineGate runs the gate's check of the journal lines' centers on
// gateCenters with the lines, in documents or balances mode, with the
// documents posted in a foreign currency.
func lineGate(balances bool, foreign map[string]string, change func(x *sap.Extract), lines ...sap.ACDOCA) []string {
	x := gateCenters()
	x.ACDOCA = lines
	if change != nil {
		change(x)
	}
	q := &qualityCheck{e: x, t: target{balances: balances}, foreign: foreign}
	q.lineCenters()
	out := make([]string, len(q.out))
	for i, is := range q.out {
		out[i] = is.Severity + " " + is.Table + " " + is.Key + ": " + is.Message
	}
	return out
}

func centerLine(rcntr, prctr, segment string) sap.ACDOCA {
	return sap.ACDOCA{RBUKRS: "2000", GJAHR: "2026", BELNR: "0100000001", DOCLN: "000001", RCNTR: rcntr, PRCTR: prctr, SEGMENT: segment}
}

// TestLineCenterQualityGate: in documents mode the gate refuses a journal
// line naming a center the run does not migrate, in the line's company
// code, or a segment that is not the line's profit center's (T56h4); in
// balances mode only the lines of the documents posted on their own (a
// down payment's, a foreign item's) are, the others going into the
// opening entry without centers.
func TestLineCenterQualityGate(t *testing.T) {
	const key = "error ACDOCA 2000/2026/0100000001/000001: "
	for name, tc := range map[string]struct {
		change func(x *sap.Extract)
		line   sap.ACDOCA
		want   []string
	}{
		"centers migrated": {line: centerLine("C1", "P1", "S1")},
		"no centers":       {line: centerLine("", "", "")},
		"a cost center not migrated": {line: centerLine("C9", "", ""),
			want: []string{key + "cost center C9 is not one of company 2000's in the extract (CSKS), so the run does not migrate it"}},
		"a cost center of another company code": {line: centerLine("C1", "", ""), change: func(x *sap.Extract) { x.CSKS[0].BUKRS = "1000" },
			want: []string{key + "cost center C1 is not one of company 2000's in the extract (CSKS), so the run does not migrate it"}},
		"a cost center in two areas": {line: centerLine("C1", "", ""), change: func(x *sap.Extract) {
			x.CSKS = append(x.CSKS, sap.CSKS{KOKRS: "A2", KOSTL: "C1", BUKRS: "2000"}, sap.CSKS{KOKRS: "A2", KOSTL: "C1", BUKRS: "2000"})
		}, want: []string{key + "cost center C1 of company 2000 is in controlling areas A1, A2; a company code is taken to belong to one"}},
		"a profit center not assigned": {line: centerLine("", "P1", ""), change: func(x *sap.Extract) { x.CEPC_BUKRS[0].BUKRS = "1000" },
			want: []string{key + "profit center P1 is not one of company 2000's in the extract (CEPC_BUKRS), so the run does not migrate it"}},
		"another segment": {line: centerLine("C1", "P1", "S2"), want: []string{key + "segment S2 is not profit center P1's (S1)"}},
		"a segment of a profit center without one": {line: centerLine("", "P1", "S1"), change: func(x *sap.Extract) { x.CEPC[0].SEGMENT = "" },
			want: []string{key + "segment S1 is not profit center P1's (none)"}},
		"a segment without a profit center": {line: centerLine("C1", "", "S1"),
			want: []string{key + "segment S1 without a profit center; the platform takes a line's segment from its profit center"}},
		"every refusal": {line: centerLine("C9", "P9", "S1"), want: []string{
			key + "cost center C9 is not one of company 2000's in the extract (CSKS), so the run does not migrate it",
			key + "profit center P9 is not one of company 2000's in the extract (CEPC_BUKRS), so the run does not migrate it"}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := lineGate(false, nil, tc.change, tc.line); fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("issues:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
	foreign := centerLine("C9", "", "")
	foreign.BELNR = "0100000002"
	downPayment := centerLine("C9", "", "")
	downPayment.BELNR, downPayment.UMSKZ = "0100000003", specialDownPayment
	for name, tc := range map[string]struct {
		line sap.ACDOCA
		want int
	}{"in the opening entry": {centerLine("C9", "P9", "S9"), 0}, "a foreign item's": {foreign, 1}, "a down payment's": {downPayment, 1}} {
		t.Run("balances mode, "+name, func(t *testing.T) {
			if got := lineGate(true, map[string]string{"2000/2026/0100000002": "USD"}, nil, tc.line); len(got) != tc.want {
				t.Errorf("issues: got %q, want %d", got, tc.want)
			}
		})
	}
}

// TestLineCentersNotLoaded: a posted line takes the centers runs loaded,
// and one naming a center no run loaded is not posted without it: the
// loader names the center, which refuses the document.
func TestLineCentersNotLoaded(t *testing.T) {
	x := gateCenters()
	cc, pc, seg := uuid.New(), uuid.New(), uuid.New()
	l := &loader{lines: lineCentersOf(x), refs: refs{"CSKS/A1/C1": cc, "CEPC/A1/P1": pc, "FAGL_SEGM/S1": seg}}
	var pl ledger.PostingLine
	if why := l.assignCenters(centerLine("C1", "P1", "S1"), &pl); why != "" {
		t.Fatalf("loaded centers: got %q, want none missing", why)
	}
	for name, c := range map[string]struct {
		got  *uuid.UUID
		want uuid.UUID
	}{"cost center": {pl.CostCenterID, cc}, "profit center": {pl.ProfitCenterID, pc}, "segment": {pl.SegmentID, seg}} {
		t.Run(name, func(t *testing.T) {
			if c.got == nil || *c.got != c.want {
				t.Errorf("%s: got %v, want %v", name, c.got, c.want)
			}
		})
	}
	delete(l.refs, "CEPC/A1/P1")
	if why := l.assignCenters(centerLine("C1", "P1", "S1"), &ledger.PostingLine{}); why != "profit center P1 was not loaded" {
		t.Errorf("an unloaded profit center: got %q, want %q", why, "profit center P1 was not loaded")
	}
}

// TestReadActuals: the actuals are read page by page, each page after the
// last row of the one before, and a failed read is returned.
func TestReadActuals(t *testing.T) {
	defer func(n int) { actualsPage = n }(actualsPage)
	actualsPage = 2
	a, b := uuid.New(), uuid.New()
	rows := []ledger.AssignmentActual{{CenterAccount: ledger.CenterAccount{CenterID: a}, Amount: 1}, {CenterAccount: ledger.CenterAccount{CenterID: a}, Amount: 2},
		{CenterAccount: ledger.CenterAccount{CenterID: b}, Amount: 3}}
	var afters []string
	read := func(_ context.Context, q ledger.ActualsQuery) ([]ledger.AssignmentActual, error) {
		start := 0
		if q.After != nil {
			afters = append(afters, q.After.CenterID.String())
			start = 2
		}
		return rows[start:min(start+q.Limit, len(rows))], nil
	}
	got, err := readActuals(context.Background(), read, uuid.New(), 2026, map[uuid.UUID]string{a: "A1/C1", b: "A1/C2"})
	if err != nil {
		t.Fatalf("actuals: got %v, want no error", err)
	}
	if want := fmt.Sprint(map[string][]money.Minor{"A1/C1": {1, 2}, "A1/C2": {3}}); fmt.Sprint(got) != want {
		t.Errorf("actuals: got %v, want %s", got, want)
	}
	if strings.Join(afters, " ") != a.String() {
		t.Errorf("pages after: got %v, want the first page's last row", afters)
	}
	failed := errors.New("failed")
	if _, err := readActuals(context.Background(), func(context.Context, ledger.ActualsQuery) ([]ledger.AssignmentActual, error) { return nil, failed },
		uuid.New(), 2026, map[uuid.UUID]string{a: "A1/C1"}); !errors.Is(err, failed) {
		t.Errorf("a failed read: got %v, want it returned", err)
	}
}

// TestCenterTotalsReadSAPAgain: a reconciliation extracts again without
// the gate, so a line naming a center the run does not migrate is refused
// there and counts for none; lines on balance sheet accounts are left out.
func TestCenterTotalsReadSAPAgain(t *testing.T) {
	x := gateCenters()
	pnl, bs := centerLine("C1", "P1", "S1"), centerLine("C1", "P1", "S1")
	pnl.RACCT, pnl.HSL, bs.RACCT, bs.HSL = "0000520000", "10.00", "0000100000", "5.00"
	unknown := centerLine("C9", "", "")
	unknown.RACCT, unknown.HSL = "0000520000", "1.00"
	x.ACDOCA = []sap.ACDOCA{pnl, bs, unknown}
	rc := &reconciler{ext: x, t: target{exponents: map[string]int{"EUR": 2}}, pnl: map[string]bool{"2000/0000520000": true}}
	got := rc.sapCenterTotals(companyScope{cc: "2000", cur: "EUR"})
	if fmt.Sprint(got) != "[map[A1/C1:1000 minor] map[A1/P1:1000 minor] map[S1:1000 minor]]" {
		t.Errorf("totals: got %v, want the profit and loss line's on C1, P1 and S1", got)
	}
	if len(rc.rec.Issues) != 1 || !strings.Contains(rc.rec.Issues[0].Message, "cost center C9 is not one of company 2000's") {
		t.Errorf("issues: got %v, want the line on C9 refused", rc.rec.Issues)
	}
}
