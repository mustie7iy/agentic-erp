package integration_test

import (
	"fmt"
	"strings"
	"testing"
)

// TestJournalLedgers: SAP holds a journal line once per ledger (ACDOCA
// RLDNR), and the leading ledger's identifier is not recorded
// (review:T56h4). One ledger's lines load whatever it is called; an
// extract with the lines of two is refused by the quality gate, with
// nothing posted, and by the reconciliation, which computes no control.
func TestJournalLedgers(t *testing.T) {
	e := setup(t)
	two := tiny()
	for _, l := range tiny().ACDOCA {
		l.RLDNR = "2L"
		two.ACDOCA = append(two.ACDOCA, l)
	}
	write(t, e.dir, "ledgers.json", two)
	conn := e.connector(t, "ledgers.json")
	const want = `the journal lines are in 2 ledgers (RLDNR "0L", "2L")`
	_, refused := e.act(t, e.run(t, conn, "documents", false, e.DE.String()), "start")
	if refused["status"] != "failed" {
		t.Errorf("two ledgers: got %v, want failed", refused["status"])
	}
	if q := fmt.Sprint(report(refused)["quality"]); !strings.Contains(q, want) {
		t.Errorf("quality: got %s, want %q", q, want)
	}
	if got := e.entries(t); got != 0 {
		t.Errorf("journal entries after the refused run: got %d, want 0", got)
	}

	one := tiny()
	for i := range one.ACDOCA {
		one.ACDOCA[i].RLDNR = "" // no ledger guessed: a ledger without a name loads too
	}
	write(t, e.dir, "ledgers.json", one)
	_, loaded := e.act(t, e.run(t, conn, "documents", false, e.DE.String()), "start")
	if loaded["status"] != "loaded" {
		t.Fatalf("one ledger: got %v (%v), want loaded", loaded["status"], report(loaded))
	}
	if got := e.entries(t); got != 2 {
		t.Errorf("journal entries of one ledger: got %d, want 2", got)
	}

	write(t, e.dir, "ledgers.json", two)
	_, rec := e.act(t, loaded, "reconcile")
	if rec["status"] != "failed" {
		t.Errorf("reconciling two ledgers: got %v, want failed", rec["status"])
	}
	if issues := fmt.Sprint(report(rec)["issues"]); !strings.Contains(issues, want) {
		t.Errorf("reconciliation issues: got %s, want %q", issues, want)
	}
	if c := fmt.Sprint(report(rec)["checks"]); c != "[]" {
		t.Errorf("reconciliation checks: got %s, want none computed", c)
	}
}
