package integration_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// costCenterID is the id of the company's cost center with the code.
func (e env) costCenterID(t *testing.T, code string) string {
	t.Helper()
	page, _ := e.Must(200, "GET", "/v1/cost-centers?legal_entity_id="+e.DE.String(), "", "")
	for _, it := range page["items"].([]any) {
		if m := it.(map[string]any); m["code"] == code {
			return m["id"].(string)
		}
	}
	t.Fatalf("cost center %s: not in %v", code, page["items"])
	return ""
}

// checkText is a check's SAP and platform figures and whether it passed.
func checkText(c map[string]any) string {
	return fmt.Sprintf("%v %v %v", c["sap"], c["platform"], c["pass"])
}

// diffsOf are a report's differences as "key: sap | platform".
func diffsOf(got map[string]any) []string {
	var out []string
	for _, d := range report(got)["diffs"].([]any) {
		m := d.(map[string]any)
		out = append(out, fmt.Sprintf("%v: %v | %v", m["key"], m["sap"], m["platform"]))
	}
	return out
}

var centerTotalNames = []string{"Cost center totals: centers with differences", "Profit center totals: centers with differences",
	"Segment totals: segments with differences"}

// TestMigratedLineCenters: in documents mode a journal line's cost center,
// profit center and segment (ACDOCA RCNTR, PRCTR, SEGMENT) load onto its
// journal line, and the reconciliation compares the totals per center and
// segment with the center reports (T56h4). A later run of an extract
// whose line SAP moved to another center finds the difference by center.
func TestMigratedLineCenters(t *testing.T) {
	e := centerSetup(t)
	conn := e.connector(t, "centers.json")
	_, run := e.act(t, e.run(t, conn, "documents", false, e.DE.String()), "start")
	if run["status"] != "loaded" {
		t.Fatalf("status: got %v (%v), want loaded", run["status"], report(run))
	}
	c1 := e.costCenterID(t, "C1")
	page, _ := e.Must(200, "GET", "/v1/journal-entries?legal_entity_id="+e.DE.String()+"&fiscal_year=2026&cost_center_id="+c1, "", "")
	var lines []string
	for _, it := range page["items"].([]any) {
		for _, l := range it.(map[string]any)["lines"].([]any) {
			if m := l.(map[string]any); m["cost_center_code"] != nil {
				lines = append(lines, fmt.Sprintf("%v %v %v %v", m["cost_center_code"], m["profit_center_code"], m["segment_code"], m["amount"].(map[string]any)["amount"]))
			}
		}
	}
	if fmt.Sprint(lines) != "[C1 P1 SEG1 80.00]" {
		t.Errorf("lines on C1: got %v, want %s", lines, "[C1 P1 SEG1 80.00]")
	}
	_, got := e.act(t, run, "reconcile")
	if got["status"] != "reconciled" {
		t.Fatalf("reconciled: got %v (%v)", got["status"], diffsOf(got))
	}
	for _, name := range centerTotalNames {
		if c := checkText(checkOf(t, got, "2000", name)); c != "0 0 true" {
			t.Errorf("check %q: got %s, want %s", name, c, "0 0 true")
		}
	}
	x := centersTiny()
	x.ACDOCA[0].RCNTR, x.ACDOCA[0].PRCTR, x.ACDOCA[0].SEGMENT = "C2", "", ""
	write(t, e.dir, "centers.json", x)
	_, later := e.act(t, e.run(t, conn, "documents", false, e.DE.String()), "start")
	_, got = e.act(t, later, "reconcile")
	if got["status"] != "failed" {
		t.Fatalf("status after SAP's change: got %v, want failed", got["status"])
	}
	want := []string{"cost center total A1/C1: 0.00 EUR | 80.00 EUR", "cost center total A1/C2: 80.00 EUR | 0.00 EUR",
		"profit center total A1/P1: 0.00 EUR | 80.00 EUR", "segment total SEG1: 0.00 EUR | 80.00 EUR"}
	if d := diffsOf(got); fmt.Sprint(d) != fmt.Sprint(want) {
		t.Errorf("diffs:\n got %q\nwant %q", d, want)
	}
	if c := checkText(checkOf(t, got, "2000", centerTotalNames[0])); c != "0 2 false" {
		t.Errorf("cost center totals: got %s, want %s", c, "0 2 false")
	}
}

// TestMigratedLineCentersValidated: the ledger checks a migrated line's
// centers as any posting's (ADR-0050 §5): a line on profit center P1 after
// its lock is refused, and the run fails with nothing loaded; the quality
// gate refuses a segment that is not the profit center's before anything
// loads.
func TestMigratedLineCentersValidated(t *testing.T) {
	e := centerSetup(t)
	x := centersTiny()
	x.ACDOCA = append(x.ACDOCA,
		sap.ACDOCA{RLDNR: "0L", RBUKRS: "2000", RWCUR: "EUR", GJAHR: "2026", BELNR: "0100000003", DOCLN: "000001", RACCT: "0000520000", HSL: "10.00", BUDAT: "20261005",
			BLART: "SA", RCNTR: "C2", PRCTR: "P1", SEGMENT: "SEG1"},
		sap.ACDOCA{RLDNR: "0L", RBUKRS: "2000", RWCUR: "EUR", GJAHR: "2026", BELNR: "0100000003", DOCLN: "000002", RACCT: "0000100000", HSL: "-10.00", BUDAT: "20261005",
			BLART: "SA"})
	write(t, e.dir, "locked.json", x)
	_, got := e.act(t, e.run(t, e.connector(t, "locked.json"), "documents", false, e.DE.String()), "start")
	if got["status"] != "failed" {
		t.Fatalf("status: got %v (%v), want failed", got["status"], report(got))
	}
	if load := fmt.Sprint(report(got)["load"]); !strings.Contains(load, "2000/2026/0100000003") || !strings.Contains(load, "ledger.profit_center_invalid") {
		t.Errorf("load refusals: got %s, want the document on locked P1 refused", load)
	}
	if page, _ := e.Must(200, "GET", "/v1/cost-centers?legal_entity_id="+e.DE.String(), "", ""); len(page["items"].([]any)) != 0 {
		t.Errorf("cost centers after a failed run: got %v, want none", page["items"])
	}
	x = centersTiny()
	x.ACDOCA[0].SEGMENT = "SEG2"
	write(t, e.dir, "segment.json", x)
	_, got = e.act(t, e.run(t, e.connector(t, "segment.json"), "documents", false, e.DE.String()), "start")
	if got["status"] != "failed" {
		t.Errorf("status with a segment not the profit center's: got %v, want failed", got["status"])
	}
	if q := fmt.Sprint(report(got)["quality"]); !strings.Contains(q, "segment SEG2 is not profit center P1's (SEG1)") {
		t.Errorf("quality: got %s, want %q", q, "segment SEG2 is not profit center P1's (SEG1)")
	}
}

// TestMigratedLineCentersBalances: a balances-mode run's opening entry
// carries SAP's balances, not its lines, so the line on C1 of a document
// that is no entry of its own (no open down payment or foreign item)
// carries no center, and the reconciliation has no center totals
// (ADR-0050 §10).
func TestMigratedLineCentersBalances(t *testing.T) {
	e := centerSetup(t)
	conn := e.connector(t, "centers.json")
	_, run := e.act(t, e.run(t, conn, "balances", false, e.DE.String()), "start")
	if run["status"] != "loaded" {
		t.Fatalf("status: got %v (%v), want loaded", run["status"], report(run))
	}
	page, _ := e.Must(200, "GET", "/v1/journal-entries?legal_entity_id="+e.DE.String()+"&fiscal_year=2026&cost_center_id="+e.costCenterID(t, "C1"), "", "")
	if items := page["items"].([]any); len(items) != 0 {
		t.Errorf("entries on C1: got %v, want none", items)
	}
	_, got := e.act(t, run, "reconcile")
	if got["status"] != "reconciled" {
		t.Fatalf("reconciled: got %v (%v)", got["status"], diffsOf(got))
	}
	for _, c := range report(got)["checks"].([]any) {
		if name := c.(map[string]any)["name"].(string); slices.Contains(centerTotalNames, name) {
			t.Errorf("check %q in balances mode: want none", name)
		}
	}
}
