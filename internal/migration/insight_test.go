package migration

import (
	"context"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

func TestProposeAccountTypes(t *testing.T) {
	e := sap.FilterScope(sap.GenerateMock(42, "2026"), scope1000)
	e.SKB1 = append(e.SKB1,
		sap.SKB1{BUKRS: "1000", SAKNR: "0000150000", TXT50: "Trade Payables Clearing", XBILK: "X"},       // rule asset, name liability, both balance sheet
		sap.SKB1{BUKRS: "1000", SAKNR: "0000560000", TXT50: "Accrued Bonus", XBILK: "X"},                 // rule expense, SAP says balance sheet, name liability
		sap.SKB1{BUKRS: "1000", SAKNR: "0000570000", TXT50: "Misc", XBILK: "X"},                          // rule expense vs SAP balance sheet, no name signal
		sap.SKB1{BUKRS: "1000", SAKNR: "ZZ01", TXT50: "Sales Germany"},                                   // no rule covers it
		sap.SKB1{BUKRS: "1000", SAKNR: "0000580000", TXT50: "General"},                                   // rule and indicator agree, no name signal
		sap.SKB1{BUKRS: "1000", SAKNR: "0000590000", TXT50: "Vendor sub-ledger", XBILK: "X", MITKZ: "K"}, // recon, but the rule says expense
	)
	got := map[string]AccountProposal{}
	for _, p := range ProposeAccountTypes(e, DefaultMapping()) {
		got[p.Account] = p
	}
	check := func(acct string, typ core.AccountType, conf, reason string) {
		t.Helper()
		p := got[acct]
		if p.Proposed != typ || p.Confidence != conf || !strings.Contains(p.Reason, reason) {
			t.Errorf("%s: got %s/%s (%s), want %s/%s containing %q", acct, p.Proposed, p.Confidence, p.Reason, typ, conf, reason)
		}
	}
	check("400000", core.Revenue, "high", "agree")
	check("211000", core.Liability, "high", "vendor reconciliation")
	check("150000", core.Asset, "medium", "name suggests liability")
	check("560000", core.Liability, "low", "agrees with SAP")
	check("570000", core.Expense, "low", "balance-sheet indicator")
	check("ZZ01", core.Revenue, "low", "no number-range rule")
	check("580000", core.Expense, "medium", "no signal")
	check("590000", core.Liability, "low", "number-range rule says expense")
}

func TestExplainAccountAndVendor(t *testing.T) {
	e := sap.GenerateMock(42, "2026")
	s := core.NewStore()
	run(t, e, scope1000, ModeDocuments, s)
	ext := sap.FilterScope(e, scope1000)
	if d, err := ExplainAccount(ext, s, DefaultMapping(), "1000", "400000"); err != nil || len(d) != 0 {
		t.Fatalf("a reconciled account has no differences: %v %v", err, d)
	}
	// Remove one migrated document and alter another.
	delete(s.Journal, "SAP-1000-2026-0100000002")
	s.Journal["SAP-1000-2026-0100000004"].Lines[1].Amount -= 100
	s.Journal["SAP-1000-2026-0100000004"].Lines[0].Amount += 100
	s.Journal["NATIVE-1"] = &core.JournalEntry{ID: "NATIVE-1", CompanyCode: "1000", Lines: []core.JournalLine{{Account: "400000", Amount: -5}}}
	d, err := ExplainAccount(ext, s, DefaultMapping(), "1000", "400000")
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[string]string{}
	for _, x := range d {
		statuses[x.Document] = x.Status
	}
	if statuses["SAP-1000-2026-0100000002"] != "missing_in_core" || statuses["NATIVE-1"] != "only_in_core" {
		t.Fatalf("differences: %+v", d)
	}
	vd, err := ExplainVendor(ext, s, DefaultMapping(), "1000", "100012")
	if err != nil || len(vd) != 0 {
		t.Fatalf("vendor open items agree: %v %v", err, vd)
	}
	bad := sap.FilterScope(e, scope1000)
	bad.ACDOCA[0].HSL = "x"
	if _, err := ExplainAccount(bad, s, DefaultMapping(), "1000", alpha(bad.ACDOCA[0].RACCT)); err == nil {
		t.Fatal("unparseable amount")
	}
	for i, l := range bad.ACDOCA {
		if l.LIFNR != "" && l.AUGBL == "" {
			bad.ACDOCA[i].HSL = "x"
			if _, err := ExplainVendor(bad, s, DefaultMapping(), "1000", alpha(l.LIFNR)); err == nil {
				t.Fatal("unparseable vendor amount")
			}
			break
		}
	}
}

func TestProposeCleansing(t *testing.T) {
	e := sap.FilterScope(sap.GenerateMock(42, "2026"), scope1000)
	issues := CheckQuality(e, DefaultMapping())
	issues = append(issues, Issue{Severity: SevError, Table: "ACDOCA", Key: "x", Message: "document does not balance"},
		Issue{Severity: SevWarning, Table: "X", Key: "y", Message: "something else"})
	props := ProposeCleansing(e, issues)
	text := ""
	for _, p := range props {
		text += p.Action + " | " + p.Owner + "\n"
	}
	for _, want := range []string{"Request the tax number", "Confirm the duplicates; keep", "Decide before cutover", "corrected invoice", "load is blocked", "Review and record"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing proposal %q in\n%s", want, text)
		}
	}
}

func TestRerunMigrationIsIdempotent(t *testing.T) {
	e := sap.GenerateMock(42, "2026")
	s := core.NewStore()
	run(t, e, scope1000, ModeDocuments, s)
	entries := len(s.Journal)
	// The platform processes a parked invoice before the rerun.
	for _, inv := range s.Invoices {
		if inv.Status == core.InvoiceParked {
			inv.Status = core.InvoiceOnHold
			break
		}
	}
	res, err := Run(context.Background(), memSource{e}, scope1000, Options{}, s)
	if err != nil || len(res.Load) != 0 || !res.Report.Passed {
		t.Fatalf("rerun must be clean: %v %v", err, res.Load)
	}
	if len(s.Journal) != entries {
		t.Fatal("rerun must not duplicate entries")
	}
	held := 0
	for _, inv := range s.Invoices {
		if inv.Status == core.InvoiceOnHold {
			held++
		}
	}
	if held != 1 {
		t.Fatal("platform-processed invoices keep their state")
	}
	// If SAP changed a document after the last load, the rerun reports it.
	e.ACDOCA[2].HSL, e.ACDOCA[3].HSL = "1.00", "-1.00"
	res, _ = Run(context.Background(), memSource{e}, scope1000, Options{}, s)
	if len(res.Load) != 1 || !strings.Contains(res.Load[0].Message, "different content") {
		t.Fatalf("changed document: %+v", res.Load)
	}
}
