package integration_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// TestJournalLedgers: SAP holds a journal line once per ledger (ACDOCA
// RLDNR), and the scope migrates the leading ledger's, 0L (the SAP
// specialist, PO 2026-11-09; review:T56h4). An extract also holding lines
// of another ledger, or of none named, is refused by the quality gate,
// with nothing posted, and by the reconciliation, which computes no
// control; the leading ledger's lines alone load.
func TestJournalLedgers(t *testing.T) {
	e := setup(t)
	two := tiny()
	for _, l := range tiny().ACDOCA {
		l.RLDNR = "2L"
		two.ACDOCA = append(two.ACDOCA, l)
	}
	unnamed := tiny()
	unnamed.ACDOCA[0].RLDNR = ""
	conn := e.connector(t, "ledgers.json")
	for name, tc := range map[string]struct {
		x    *sap.Extract
		want string
	}{
		"another ledger's lines": {two, `the extract holds journal lines of other ledgers than the leading ledger "0L" (RLDNR "2L": 5 lines)`},
		"a line of no ledger":    {unnamed, `(RLDNR "": 1 line)`},
	} {
		t.Run(name, func(t *testing.T) {
			write(t, e.dir, "ledgers.json", tc.x)
			_, refused := e.act(t, e.run(t, conn, "documents", false, e.DE.String()), "start")
			if refused["status"] != "failed" {
				t.Errorf("status: got %v, want failed", refused["status"])
			}
			if q := fmt.Sprint(report(refused)["quality"]); !strings.Contains(q, tc.want) {
				t.Errorf("quality: got %s, want %q", q, tc.want)
			}
			if got := e.entries(t); got != 0 {
				t.Errorf("journal entries after the refused run: got %d, want 0", got)
			}
		})
	}

	write(t, e.dir, "ledgers.json", tiny())
	_, loaded := e.act(t, e.run(t, conn, "documents", false, e.DE.String()), "start")
	if loaded["status"] != "loaded" {
		t.Fatalf("the leading ledger's lines: got %v (%v), want loaded", loaded["status"], report(loaded))
	}
	if got := e.entries(t); got != 2 {
		t.Errorf("journal entries of the leading ledger: got %d, want 2", got)
	}

	write(t, e.dir, "ledgers.json", two)
	_, rec := e.act(t, loaded, "reconcile")
	if rec["status"] != "failed" {
		t.Errorf("reconciling two ledgers: got %v, want failed", rec["status"])
	}
	if issues := fmt.Sprint(report(rec)["issues"]); !strings.Contains(issues, `"2L": 5 lines`) {
		t.Errorf("reconciliation issues: got %s, want %q", issues, `"2L": 5 lines`)
	}
	if c := fmt.Sprint(report(rec)["checks"]); c != "[]" {
		t.Errorf("reconciliation checks: got %s, want none computed", c)
	}
}
