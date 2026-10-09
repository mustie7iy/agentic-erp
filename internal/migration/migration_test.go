package migration

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// memSource serves an in-memory extract.
type memSource struct{ e *sap.Extract }

func (m memSource) Name() string { return "memory" }
func (m memSource) Extract(_ context.Context, scope sap.Scope) (*sap.Extract, error) {
	return sap.FilterScope(m.e, scope), nil
}

var scope1000 = sap.Scope{CompanyCodes: []string{"1000"}, FiscalYear: "2026"}

func run(t *testing.T, e *sap.Extract, scope sap.Scope, mode Mode, s *core.Store) *Result {
	t.Helper()
	res, err := Run(context.Background(), memSource{e}, scope, Options{Mode: mode}, s)
	if err != nil {
		t.Fatalf("run: %v (quality: %v)", err, res.Quality)
	}
	return res
}

func TestMockMigrationReconcilesInBothModes(t *testing.T) {
	for _, mode := range []Mode{ModeDocuments, ModeBalances} {
		t.Run(string(mode), func(t *testing.T) {
			s := core.NewStore()
			res := run(t, sap.GenerateMock(42, "2026"), scope1000, mode, s)
			if len(res.Load) > 0 || len(res.Transform) > 0 {
				t.Fatalf("load/transform issues: %v %v", res.Load, res.Transform)
			}
			if !res.Report.Passed {
				t.Fatalf("reconciliation failed: %+v", res.Report)
			}
			if tb := s.TrialBalance("1000", ""); sumTB(tb) != 0 {
				t.Fatalf("trial balance does not net to zero: %v", tb)
			}
		})
	}
}

func TestBalancesModeKeepsOpenItemsPayable(t *testing.T) {
	s := core.NewStore()
	run(t, sap.GenerateMock(42, "2026"), scope1000, ModeBalances, s)
	if len(s.Journal) != 1 {
		t.Fatalf("balances mode should create one opening entry, got %d", len(s.Journal))
	}
	open := s.VendorOpenItems("1000")
	if len(open) == 0 {
		t.Fatal("open vendor items must survive as individual lines")
	}
}

func TestQualityGateFindsInjectedProblems(t *testing.T) {
	e := sap.GenerateMock(42, "2026")
	issues := CheckQuality(sap.FilterScope(e, scope1000), DefaultMapping())
	for _, want := range []string{"no tax number", "possible duplicate vendors", "posting-blocked but has open payables"} {
		if !containsMsg(issues, want) {
			t.Errorf("missing finding %q", want)
		}
	}
	if HasErrors(issues) {
		t.Fatalf("mock data should have warnings only: %v", issues)
	}
}

func TestQualityGateBlocksUnbalancedDocument(t *testing.T) {
	e := sap.GenerateMock(42, "2026")
	e.ACDOCA[0].HSL = "1.00" // corrupt one line
	s := core.NewStore()
	res, err := Run(context.Background(), memSource{e}, scope1000, Options{}, s)
	if !errors.Is(err, ErrQualityGate) {
		t.Fatalf("want quality gate error, got %v", err)
	}
	if !containsMsg(res.Quality, "does not balance") {
		t.Fatalf("unbalanced document not reported: %v", res.Quality)
	}
	if len(s.Journal) != 0 || res.Loaded {
		t.Fatal("nothing may be loaded when the gate blocks")
	}
}

func TestForcedLoadStillValidatesAndReconcileFails(t *testing.T) {
	e := sap.GenerateMock(42, "2026")
	e.ACDOCA[0].HSL = "1.00"
	s := core.NewStore()
	res, err := Run(context.Background(), memSource{e}, scope1000, Options{Force: true}, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Load) != 1 || !strings.Contains(res.Load[0].Message, "unbalanced") {
		t.Fatalf("ledger should reject the unbalanced entry: %v", res.Load)
	}
	if res.Report.Passed {
		t.Fatal("reconciliation must fail when an entry was rejected")
	}
}

func TestReconcileDetectsDivergence(t *testing.T) {
	e := sap.GenerateMock(42, "2026")
	s := core.NewStore()
	run(t, e, scope1000, ModeDocuments, s)
	// Simulate a transform bug: one migrated line is booked to the wrong account.
	je := s.Journal["SAP-1000-2026-0100000002"]
	je.Lines[0].Account = "520000"
	r := Reconcile(sap.FilterScope(e, scope1000), s, scope1000, DefaultMapping(), ModeDocuments)
	if r.Passed || len(r.Diffs) != 2 {
		t.Fatalf("expected failure with 2 account diffs, got passed=%v diffs=%v", r.Passed, r.Diffs)
	}
}

func TestIncrementalCompanyByCompany(t *testing.T) {
	e := sap.GenerateMock(42, "2026")
	s := core.NewStore()
	run(t, e, scope1000, ModeDocuments, s)
	res := run(t, e, sap.Scope{CompanyCodes: []string{"2000"}, FiscalYear: "2026"}, ModeDocuments, s)
	if !res.Report.Passed {
		t.Fatalf("second company failed: %+v", res.Report.Checks)
	}
	// Vendors shared by both company codes keep both AP assignments.
	v := s.Vendors["100015"]
	if v == nil || v.APAccount["1000"] == "" || v.APAccount["2000"] == "" {
		t.Fatalf("shared vendor not merged: %+v", v)
	}
	// Re-check company 1000 after loading 2000: still intact.
	if r := Reconcile(sap.FilterScope(e, scope1000), s, scope1000, DefaultMapping(), ModeDocuments); !r.Passed {
		t.Fatalf("company 1000 broke after loading 2000: %+v", r.Checks)
	}
}

func TestMappingDefaults(t *testing.T) {
	m := DefaultMapping()
	if d, _ := m.paymentDays("NT45"); d != 45 {
		t.Errorf("NT45 -> %d", d)
	}
	if typ, _ := m.accountType("211000"); typ != core.Liability {
		t.Errorf("211000 -> %s", typ)
	}
	if alpha("0000100001") != "100001" || alpha("0000") != "0" {
		t.Error("alpha conversion")
	}
}

func containsMsg(issues []Issue, sub string) bool {
	for _, i := range issues {
		if strings.Contains(i.Message, sub) {
			return true
		}
	}
	return false
}

func sumTB(tb map[string]money.Minor) money.Minor {
	var t money.Minor
	for _, v := range tb {
		t += v
	}
	return t
}
