package migration

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// broken builds a small extract in which every kind of data problem the
// quality gate and transform know about occurs at least once.
func broken() *sap.Extract {
	acdoca := func(belnr, docln, racct, hsl, budat, lifnr, xblnr, augbl string) sap.ACDOCA {
		return sap.ACDOCA{RLDNR: "0L", RBUKRS: "1000", GJAHR: "2026", BELNR: belnr, DOCLN: docln, RACCT: racct, HSL: hsl,
			BUDAT: budat, BLART: "SA", LIFNR: lifnr, XBLNR: xblnr, AUGBL: augbl}
	}
	return &sap.Extract{
		T001: []sap.T001{{BUKRS: "1000", BUTXT: "Test", WAERS: "USD"}, {BUKRS: "3000", BUTXT: "No currency"}},
		SKB1: []sap.SKB1{
			{BUKRS: "1000", SAKNR: "0000100000", TXT50: "Bank", XBILK: "X"},
			{BUKRS: "1000", SAKNR: "0000211000", TXT50: "AP", XBILK: "X", MITKZ: "K"},
			{BUKRS: "1000", SAKNR: "0000510000", TXT50: "Materials"},
			{BUKRS: "1000", SAKNR: "ABC", TXT50: "Not numeric"},
			{BUKRS: "1000", SAKNR: "0001500000", TXT50: "Outside every rule"},
		},
		LFA1: []sap.LFA1{{LIFNR: "0000000001", NAME1: "One", STCD1: "1"}, {LIFNR: "0000000002", NAME1: "Two", STCD1: "2"}},
		LFB1: []sap.LFB1{
			{LIFNR: "0000000001", BUKRS: "1000", AKONT: "0000211000", ZTERM: "0001"},
			{LIFNR: "0000000002", BUKRS: "1000", AKONT: "0000510000", ZTERM: "XX"},
			{LIFNR: "0000000009", BUKRS: "1000", AKONT: "0000999999", ZTERM: "NT30"},
		},
		EKKO: []sap.EKKO{
			{EBELN: "45001", BUKRS: "1000", LIFNR: "0000000001", WAERS: "USD", BEDAT: "20260105"},
			{EBELN: "45002", BUKRS: "1000", LIFNR: "0000000077", WAERS: "USD", BEDAT: "2026"},
		},
		EKPO: []sap.EKPO{
			{EBELN: "45001", EBELP: "00010", MENGE: "2", NETPR: "10.00", PEINH: "1", SAKTO: "0000510000"},
			{EBELN: "45999", EBELP: "00010", MENGE: "1", NETPR: "1.00", SAKTO: "0000510000"},
			{EBELN: "45001", EBELP: "00020", MENGE: "1", NETPR: "1.00", SAKTO: "0000777777"},
			{EBELN: "45001", EBELP: "00030", MENGE: "x", NETPR: "1.00", SAKTO: "0000510000"},
			{EBELN: "45001", EBELP: "00040", MENGE: "1", NETPR: "y", SAKTO: "0000510000"},
			{EBELN: "45001", EBELP: "00050", MENGE: "1", NETPR: "1.00", PEINH: "z", SAKTO: "0000510000"},
		},
		MSEG: []sap.MSEG{
			{MBLNR: "5001", MJAHR: "2026", ZEILE: "1", BWART: "101", EBELN: "45001", EBELP: "00010", MENGE: "2", BUDAT: "20260110"},
			{MBLNR: "5002", MJAHR: "2026", ZEILE: "1", BWART: "102", EBELN: "45001", EBELP: "00010", MENGE: "1", BUDAT: "20260111"},
			{MBLNR: "5003", MJAHR: "2026", ZEILE: "1", BWART: "261", EBELN: "45001", EBELP: "00010", MENGE: "1", BUDAT: "20260112"},
			{MBLNR: "5004", MJAHR: "2026", ZEILE: "1", BWART: "101", EBELN: "45001", EBELP: "00010", MENGE: "q", BUDAT: "20260112"},
		},
		RBKP: []sap.RBKP{
			{BELNR: "51001", GJAHR: "2026", BUKRS: "1000", LIFNR: "0000000001", XBLNR: "A", BLDAT: "20260115", WAERS: "USD", RMWWR: "20.00", WMWST1: "0.00", RBSTAT: "A"},
			{BELNR: "51002", GJAHR: "2026", BUKRS: "1000", LIFNR: "0000000001", XBLNR: "B", BLDAT: "20260115", WAERS: "USD", RMWWR: "1.00", WMWST1: "0.00", RBSTAT: "3"},
			{BELNR: "51003", GJAHR: "2026", BUKRS: "1000", LIFNR: "0000000001", XBLNR: "C", BLDAT: "20260115", WAERS: "USD", RMWWR: "bad", WMWST1: "0.00", RBSTAT: "A"},
			{BELNR: "51004", GJAHR: "2026", BUKRS: "1000", LIFNR: "0000000001", XBLNR: "D", BLDAT: "20260115", WAERS: "USD", RMWWR: "1.00", WMWST1: "0.00", RBSTAT: "A"},
		},
		RSEG: []sap.RSEG{
			{BELNR: "51001", GJAHR: "2026", BUZEI: "1", EBELN: "45001", EBELP: "00010", MENGE: "1", WRBTR: "10.00"},
			{BELNR: "51001", GJAHR: "2026", BUZEI: "2", EBELN: "45002", EBELP: "00010", MENGE: "1", WRBTR: "10.00"},
			{BELNR: "51004", GJAHR: "2026", BUZEI: "1", EBELN: "45001", EBELP: "00010", MENGE: "1", WRBTR: "x"},
		},
		ACDOCA: []sap.ACDOCA{
			acdoca("1", "1", "0000100000", "-5.00", "20260120", "", "", ""),
			acdoca("1", "2", "0000211000", "5.00", "20260120", "0000000001", "", "9"),
			acdoca("2", "1", "0000100000", "x", "20260121", "", "", ""),
			acdoca("3", "1", "0000888888", "1.00", "20260122", "", "", ""),
			acdoca("3", "2", "0000211000", "-1.00", "20260122", "", "", ""),
			acdoca("4", "1", "0000211000", "-2.00", "2099X", "0000000077", "", ""),
			acdoca("4", "2", "0000510000", "2.00", "20260123", "", "", ""),
			acdoca("5", "1", "0000211000", "-3.00", "20260124", "0000000001", "", ""),
			acdoca("5", "2", "0000510000", "3.00", "20260124", "", "", ""),
			acdoca("6", "1", "0000510000", "7.00", "20260125", "", "", ""),
		},
	}
}

func messages(issues []Issue) string {
	var b strings.Builder
	for _, i := range issues {
		b.WriteString(i.String() + "\n")
	}
	return b.String()
}

func TestQualityGateCatchesEveryProblem(t *testing.T) {
	text := messages(CheckQuality(broken(), DefaultMapping()))
	for _, want := range []string{
		"company code has no currency", "not numeric", "no account type rule covers account 1500000",
		"company data without vendor master", "reconciliation account 0000999999 missing", "is not a vendor reconciliation account",
		`payment terms "XX" not mapped`, "vendor 0000000077 not in extract", "item without header", "G/L account 0000777777 missing",
		`invalid decimal "x"`, `invalid decimal "y"`, "missing from chart of accounts", "without vendor", "invalid SAP date",
		"does not balance", "unparseable amounts", `invoice status "3" is not migrated`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing finding %q in:\n%s", want, text)
		}
	}
}

func TestTransformReportsEveryProblem(t *testing.T) {
	ds, issues := Transform(broken(), DefaultMapping(), ModeDocuments)
	text := messages(issues)
	for _, want := range []string{
		"SKB1 1000/ABC", "LFB1 0000000009: no vendor master", `payment terms "XX" not mapped`, "EKKO 45002", "EKPO 45999/00010: item without header",
		"EKPO 45001/00030", "EKPO 45001/00050", "MSEG 5004/2026/1", "RBKP 51003", "RSEG 51004/2026/1", "invoice references several POs",
		"ACDOCA 1000/2", "ACDOCA 1000/4",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing transform issue %q in:\n%s", want, text)
		}
	}
	var reversal bool
	for _, g := range ds.Receipts {
		if g.Quantity < 0 {
			reversal = true
		}
	}
	if !reversal || len(ds.Receipts) != 2 {
		t.Fatalf("101 and 102 receipts migrate, other movements do not: %+v", ds.Receipts)
	}
	if len(ds.Invoices) != 2 {
		t.Fatalf("parked invoices with parseable headers migrate: %d", len(ds.Invoices))
	}
}

func TestBalancesModeProblems(t *testing.T) {
	_, issues := Transform(broken(), DefaultMapping(), ModeBalances)
	text := messages(issues)
	for _, want := range []string{"cleared vendor items net to", "ACDOCA 1000/2/1", "invalid SAP date"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing balances issue %q in:\n%s", want, text)
		}
	}
	// An open item without a vendor reference is labelled with its SAP document number.
	e := broken()
	e.ACDOCA = []sap.ACDOCA{e.ACDOCA[7], e.ACDOCA[8]}
	ds, _ := Transform(e, DefaultMapping(), ModeBalances)
	if len(ds.Entries) != 1 || !strings.Contains(ds.Entries[0].Lines[1].Text, "Open item 5 (SAP 5)") {
		t.Fatalf("open item label: %+v", ds.Entries)
	}
}

func TestMappingHelpers(t *testing.T) {
	m := DefaultMapping()
	if d, _ := m.paymentDays("0001"); d != 0 {
		t.Fatal("mapped terms")
	}
	if m.decimals("JPY") != 0 || m.decimals("USD") != 2 {
		t.Fatal("currency decimals")
	}
	if _, err := isoDate("2026"); err == nil {
		t.Fatal("short date")
	}
	if _, err := m.AccountType("1500000"); err == nil {
		t.Fatal("uncovered account")
	}
}

type failingSource struct{}

func (failingSource) Name() string { return "failing" }
func (failingSource) Extract(context.Context, sap.Scope) (*sap.Extract, error) {
	return nil, errors.New("down")
}

func TestReconcileEdgesAndPrint(t *testing.T) {
	if _, err := Run(context.Background(), failingSource{}, scope1000, Options{}, core.NewStore()); err == nil {
		t.Fatal("extract failure must surface")
	}
	e := sap.GenerateMock(42, "2026")
	s := core.NewStore()
	run(t, e, scope1000, ModeDocuments, s)
	// Feed the unfiltered extract (other companies' rows must be ignored) plus
	// an unparseable amount, and break one vendor's open items in the store.
	full := sap.FilterScope(e, sap.Scope{CompanyCodes: []string{"1000", "2000"}, FiscalYear: "2026"})
	full.ACDOCA = append(full.ACDOCA, sap.ACDOCA{RLDNR: "0L", RBUKRS: "1000", GJAHR: "2026", BELNR: "X", RACCT: "0000100000", HSL: "bad"})
	for _, je := range s.Journal {
		for i, l := range je.Lines {
			if l.VendorID == "100012" && l.ClearedBy == "" {
				je.Lines[i].ClearedBy = "TEST"
			}
		}
	}
	r := Reconcile(full, s, scope1000, DefaultMapping(), ModeDocuments)
	if r.Passed {
		t.Fatal("broken open items must fail reconciliation")
	}
	found := false
	for _, d := range r.Diffs {
		found = found || d.Key == "vendor 100012"
	}
	if !found {
		t.Fatalf("vendor difference not reported: %+v", r.Diffs)
	}
	var buf bytes.Buffer
	r.Print(&buf)
	out := buf.String()
	for _, want := range []string{"FAIL", "Differences:", "vendor 100012", "FAILED - do not cut over"} {
		if !strings.Contains(out, want) {
			t.Errorf("printed report lacks %q", want)
		}
	}
	ok := Reconcile(sap.FilterScope(e, scope1000), core.NewStore(), scope1000, DefaultMapping(), ModeBalances)
	buf.Reset()
	ok.Print(&buf)
	if !strings.Contains(buf.String(), "COMPANY") {
		t.Fatal("print header")
	}
	passing := &Report{Passed: true, Checks: []Check{{Name: "x", Pass: true}}}
	buf.Reset()
	passing.Print(&buf)
	if !strings.Contains(buf.String(), "PASSED") || strings.Contains(buf.String(), "Differences") {
		t.Fatal("passing report")
	}
}

func TestInsightEdges(t *testing.T) {
	e := sap.GenerateMock(42, "2026")
	two := sap.FilterScope(e, sap.Scope{CompanyCodes: []string{"1000", "2000"}, FiscalYear: "2026"})
	props := ProposeAccountTypes(two, DefaultMapping())
	if props[0].CompanyCode != "1000" || props[len(props)-1].CompanyCode != "2000" {
		t.Fatal("proposals sorted by company")
	}
	s := core.NewStore()
	run(t, e, sap.Scope{CompanyCodes: []string{"1000", "2000"}, FiscalYear: "2026"}, ModeDocuments, s)
	if d, err := ExplainAccount(two, s, DefaultMapping(), "1000", "100000"); err != nil || len(d) != 0 {
		t.Fatalf("other company's entries are ignored: %v %v", err, d)
	}
	if companyCurrency(two, "9999") != "" {
		t.Fatal("unknown company")
	}
	// Of two duplicates, the vendor with more open items is kept.
	dup := &sap.Extract{ACDOCA: []sap.ACDOCA{{LIFNR: "B"}, {LIFNR: "B"}, {LIFNR: "A"}}}
	p := ProposeCleansing(dup, []Issue{{Severity: SevWarning, Table: "LFA1", Key: "A,B", Message: "possible duplicate vendors named \"X\""}})
	if !strings.Contains(p[0].Action, "keep B and block A") {
		t.Fatalf("keep the busier vendor: %s", p[0].Action)
	}
	a := &core.JournalEntry{CompanyCode: "1000", PostingDate: "2026-01-01"}
	b := &core.JournalEntry{CompanyCode: "1000", PostingDate: "2026-01-02"}
	if sameEntry(a, b) {
		t.Fatal("different posting dates")
	}
}
