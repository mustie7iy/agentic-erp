package simulate

import (
	"errors"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/testkit"
)

func TestCloneIsIndependent(t *testing.T) {
	s := testkit.Store(t)
	c := Clone(s)
	for id := range c.Vendors {
		c.Vendors[id].Name = "changed"
		break
	}
	for _, v := range s.Vendors {
		if v.Name == "changed" {
			t.Fatal("clone shares data with the live store")
		}
	}
}

func TestCandidates(t *testing.T) {
	s := testkit.Store(t)
	all := Candidates(s, "", "")
	if len(all) != 16 { // 8 parked invoices in each company; SAP-posted ones are history
		t.Fatalf("want 16 candidates, got %d", len(all))
	}
	for _, id := range all {
		if s.Invoices[id].Status != core.InvoiceParked {
			t.Fatalf("%s is %s", id, s.Invoices[id].Status)
		}
	}
	if n := len(Candidates(s, "2026-04-01", "2026-04-30")); n == 0 || n >= 16 {
		t.Fatalf("date window not applied: %d", n)
	}
	s.Invoices[all[0]].Status = core.InvoiceRejected
	if len(Candidates(s, "", "")) != 15 {
		t.Fatal("rejected invoices are not replayed")
	}
}

func TestReplaySameConfigChangesNothing(t *testing.T) {
	s := testkit.Store(t)
	cfg := testkit.Config(t)
	r, err := Replay(s, cfg, cfg, "", "", "2026-10-09")
	if err != nil {
		t.Fatal(err)
	}
	if r.Cases != 16 || len(r.Changed) != 0 || len(r.LedgerImpact) != 0 {
		t.Fatalf("unexpected report: %+v", r)
	}
	if r.Before[AutoPost] == 0 || r.Before[Exception] == 0 || r.Before[Review] == 0 {
		t.Fatalf("mock data should exercise every decision: %v", r.Before)
	}
	if !strings.Contains(r.Summary(), "no decision or posting changes") || !r.Evidence().Passed {
		t.Fatal("summary/evidence for an unchanged replay")
	}
	// The live store is untouched.
	for _, id := range Candidates(s, "", "") {
		if s.Invoices[id].Status != core.InvoiceParked {
			t.Fatal("replay modified the live store")
		}
	}
}

func TestReplayShowsEffectOfRaisingTheLimit(t *testing.T) {
	s := testkit.Store(t)
	before := testkit.Config(t)
	after := testkit.Config(t)
	after.Company("1000").AutoPostLimit = "100000.00"
	r, err := Replay(s, before, after, "", "", "2026-10-09")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Changed) == 0 {
		t.Fatal("raising the limit should auto-post an invoice that was above it")
	}
	d := r.Changed[0]
	if d.Before.Decision != Review || d.After.Decision != AutoPost || len(d.After.Lines) == 0 {
		t.Fatalf("change: %+v", d)
	}
	if r.LedgerImpact["1000"]["211000"] >= 0 {
		t.Fatalf("more posted invoices raise payables (credit): %v", r.LedgerImpact)
	}
	ev := r.Evidence()
	if ev.Passed || ev.Kind != "simulation" || !strings.Contains(ev.Summary, d.InvoiceID) || !strings.Contains(ev.Summary, "211000") {
		t.Fatalf("evidence: %+v", ev)
	}
}

func TestEvaluateResetsNativePostingsAndErrors(t *testing.T) {
	s := testkit.Store(t)
	cfg := testkit.Config(t)
	box := Clone(s)
	ids := Candidates(box, "", "")
	first, err := Evaluate(box, cfg, ids, "2026-10-09")
	if err != nil {
		t.Fatal(err)
	}
	// Evaluating again resets the auto-posted invoices first: same result, no duplicate postings.
	journal := len(box.Journal)
	second, err := Evaluate(box, cfg, ids, "2026-10-09")
	if err != nil || len(box.Journal) != journal {
		t.Fatalf("re-evaluation must not duplicate postings: %v %d vs %d", err, len(box.Journal), journal)
	}
	for _, id := range ids {
		if first[id].Decision != second[id].Decision {
			t.Fatalf("%s: %s then %s", id, first[id].Decision, second[id].Decision)
		}
	}
	if _, err := Evaluate(box, cfg, []string{"NOPE"}, "2026-10-09"); err == nil {
		t.Fatal("unknown invoice must error")
	}
	bad := testkit.Config(t)
	bad.Companies = bad.Companies[:1] // company 2000 no longer configured
	if _, err := Evaluate(box, bad, ids, "2026-10-09"); err == nil {
		t.Fatal("unconfigured company must error")
	}
	if _, err := Replay(s, bad, cfg, "", "", "2026-10-09"); err == nil {
		t.Fatal("replay must surface configuration errors")
	}
	if _, err := Replay(s, cfg, bad, "", "", "2026-10-09"); err == nil {
		t.Fatal("replay must surface configuration errors")
	}
	limitBroken := testkit.Config(t)
	limitBroken.Company("1000").AutoPostLimit = "x"
	if _, err := Evaluate(box, limitBroken, ids, "2026-10-09"); err == nil {
		t.Fatal("unusable policy must error")
	}
}

func TestPostingFailureIsAnException(t *testing.T) {
	s := testkit.Store(t)
	cfg := testkit.Config(t)
	box := Clone(s)
	ids := Candidates(box, "", "")
	cfg.Company("1000").AccountDetermination.InputTax = "999999" // posting will fail validation
	out, err := Evaluate(box, cfg, ids, "2026-10-09")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, o := range out {
		if o.Decision == Exception && strings.Contains(o.Reason, "unknown account 999999") {
			found = true
		}
	}
	if !found {
		t.Fatal("a posting failure should be reported as an exception")
	}
}

func TestOutcomeHelpersAndSummary(t *testing.T) {
	a := Outcome{Decision: AutoPost, Lines: []core.JournalLine{{Account: "1", Amount: 5}}}
	b := a
	b.Lines = []core.JournalLine{{Account: "1", Amount: 6}}
	if a.Equal(b) || !a.Equal(a) || a.Equal(Outcome{Decision: Review}) {
		t.Fatal("Equal")
	}
	ex := Outcome{Decision: Exception, Issues: []core.IssueKind{core.IssuePriceVariance, core.IssueDuplicate}}
	if ex.String() != "exception (price_variance, possible_duplicate)" || a.String() != AutoPost {
		t.Fatalf("String: %s", ex.String())
	}
	r := &Report{Cases: 9}
	for i := 0; i < 7; i++ {
		r.Changed = append(r.Changed, Diff{InvoiceID: string(rune('A' + i)), Before: a, After: ex})
	}
	if !strings.Contains(r.Summary(), "and 2 more") {
		t.Fatalf("long summaries are truncated: %s", r.Summary())
	}
}

func TestMustPanics(t *testing.T) {
	must(nil)
	defer func() {
		if recover() == nil {
			t.Fatal("must should panic on an error")
		}
	}()
	must(errors.New("boom"))
}
