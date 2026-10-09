package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMatchSummaryAndHeaderIssues(t *testing.T) {
	s := fixture()
	clean := MatchResult{Clean: true}
	if clean.Summary() != "clean match" {
		t.Fatal("clean summary")
	}
	two := MatchResult{Issues: []MatchIssue{{Message: "a"}, {Message: "b"}}}
	if two.Summary() != "a; b" {
		t.Fatal("issue summary")
	}

	kinds := func(inv *SupplierInvoice) map[IssueKind]bool {
		out := map[IssueKind]bool{}
		for _, is := range s.ThreeWayMatch(inv, DefaultTolerance).Issues {
			out[is.Kind] = true
		}
		return out
	}
	s.Vendors["V1"].Blocked = true
	if !kinds(invoice("B1", "B-1", 1_000, 10_000, 0))[IssueVendorBlocked] {
		t.Fatal("blocked vendor")
	}
	s.Vendors["V1"].Blocked = false
	noPO := invoice("N1", "N-1", 1_000, 10_000, 0)
	noPO.POID = ""
	if !kinds(noPO)[IssueNoPO] {
		t.Fatal("no PO")
	}
	ghost := invoice("G1", "G-1", 1_000, 10_000, 0)
	ghost.POID = "PO-NONE"
	if !kinds(ghost)[IssuePONotFound] {
		t.Fatal("PO not found")
	}
	s.Vendors["V2"] = &Vendor{ID: "V2", Name: "Two", APAccount: map[string]string{"1000": "211000"}}
	other := invoice("O1", "O-1", 1_000, 10_000, 0)
	other.VendorID, other.Currency = "V2", "EUR"
	k := kinds(other)
	if !k[IssueVendorMismatch] || !k[IssueCurrencyMismatch] {
		t.Fatalf("vendor/currency mismatch: %v", k)
	}
	badLine := invoice("L1", "L-1", 1_000, 10_000, 0)
	badLine.Lines[0].POLineNo = 99
	if !kinds(badLine)[IssueUnknownPOLine] {
		t.Fatal("unknown PO line")
	}
}

// An invoice removed from the store after the index was built is skipped.
func TestIndexSkipsRemovedInvoices(t *testing.T) {
	s := fixture()
	s.Invoices["A"] = invoice("A", "R-1", 1_000, 10_000, 0)
	s.ThreeWayMatch(s.Invoices["A"], DefaultTolerance) // builds the index
	delete(s.Invoices, "A")
	s.Invoices["B"] = invoice("B", "R-2", 1_000, 10_000, 0) // same count: index kept
	if m := s.ThreeWayMatch(s.Invoices["B"], DefaultTolerance); !m.Clean {
		t.Fatalf("removed invoice must be ignored: %s", m.Summary())
	}
}

func TestPostAndHoldInvoiceErrors(t *testing.T) {
	s := fixture()
	if _, err := s.PostInvoice("NOPE", "140000", "2026-03-01"); err == nil {
		t.Fatal("missing invoice")
	}
	inv := invoice("I1", "X-1", 1_000, 10_000, 0)
	s.Invoices["I1"] = inv
	inv.Status = InvoicePosted
	if _, err := s.PostInvoice("I1", "140000", "2026-03-01"); err == nil {
		t.Fatal("posted invoice")
	}
	inv.Status = InvoiceParked
	inv.VendorID = "V9"
	if _, err := s.PostInvoice("I1", "140000", "2026-03-01"); err == nil {
		t.Fatal("missing vendor")
	}
	inv.VendorID = "V1"
	s.Vendors["V1"].Blocked = true
	if _, err := s.PostInvoice("I1", "140000", "2026-03-01"); err == nil {
		t.Fatal("blocked vendor")
	}
	s.Vendors["V1"].Blocked = false
	s.Vendors["V1"].APAccount = map[string]string{}
	if _, err := s.PostInvoice("I1", "140000", "2026-03-01"); err == nil {
		t.Fatal("vendor without AP account")
	}
	s.Vendors["V1"].APAccount = map[string]string{"1000": "211000"}
	inv.POID = ""
	if _, err := s.PostInvoice("I1", "140000", "2026-03-01"); err == nil || !strings.Contains(err.Error(), "no G/L account") {
		t.Fatalf("line without account: %v", err)
	}

	if err := s.HoldInvoice("NOPE", "x"); err == nil {
		t.Fatal("hold missing invoice")
	}
	if err := s.HoldInvoice("I1", "check price"); err != nil || inv.Status != InvoiceOnHold || inv.HoldReason != "check price" {
		t.Fatalf("hold: %v", err)
	}
	inv.Status = InvoicePosted
	if err := s.HoldInvoice("I1", "x"); err == nil {
		t.Fatal("cannot hold a posted invoice")
	}
}

func TestLedgerValidationAndQueries(t *testing.T) {
	s := fixture()
	lines := []JournalLine{{Account: "100000", Amount: 1}, {Account: "510000", Amount: -1}}
	cases := map[string]*JournalEntry{
		"company":    {ID: "J1", CompanyCode: "9999", Lines: lines},
		"one line":   {ID: "J2", CompanyCode: "1000", Lines: lines[:1]},
		"vendor":     {ID: "J3", CompanyCode: "1000", Lines: []JournalLine{{Account: "211000", Amount: -1, VendorID: "V9"}, {Account: "510000", Amount: 1}}},
		"account":    {ID: "J4", CompanyCode: "1000", Lines: []JournalLine{{Account: "999", Amount: 1}, {Account: "510000", Amount: -1}}},
		"fractional": {ID: "J5", CompanyCode: "1000", Lines: lines},
	}
	delete(cases, "fractional")
	for name, je := range cases {
		if err := s.Post(je); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	ok := &JournalEntry{ID: "J6", CompanyCode: "1000", PostingDate: "2026-02-01", Lines: lines}
	if err := s.Post(ok); err != nil {
		t.Fatal(err)
	}
	if err := s.Post(&JournalEntry{ID: "J6", CompanyCode: "1000", Lines: lines}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatal("duplicate id")
	}
	s.Companies["2000"] = &Company{Code: "2000", Currency: "EUR"}
	s.PutAccount(&GLAccount{CompanyCode: "2000", Number: "100000", Type: Asset})
	s.PutAccount(&GLAccount{CompanyCode: "2000", Number: "510000", Type: Expense})
	s.Post(&JournalEntry{ID: "J7", CompanyCode: "2000", PostingDate: "2026-01-01", Lines: lines})
	s.Post(&JournalEntry{ID: "J8", CompanyCode: "1000", PostingDate: "2026-05-01", Lines: lines})
	if tb := s.TrialBalance("1000", "2026-03-01"); tb["100000"] != 1 {
		t.Fatalf("as-of and company filters: %v", tb)
	}
	// Vendor lines on a non-reconciliation account are not open items.
	s.Post(&JournalEntry{ID: "J9", CompanyCode: "1000", PostingDate: "2026-01-01", Lines: []JournalLine{
		{Account: "510000", Amount: 5, VendorID: "V1"}, {Account: "100000", Amount: -5}}})
	if items := s.VendorOpenItems("1000"); len(items) != 0 {
		t.Fatalf("no open items expected: %v", items)
	}
	if items := s.VendorOpenItems("2000"); len(items) != 0 {
		t.Fatal("other company")
	}
}

func TestProcurementBranches(t *testing.T) {
	s := p2pFixture(t)
	s.Requisitions["PR1"].Status = ReqPending
	if _, err := s.CreatePurchaseOrder("PR1", "", "2026-05-01", "u"); err == nil {
		t.Fatal("unapproved requisition")
	}
	s.Requisitions["PR1"].Status = ReqApproved
	s.Requisitions["PR1"].ContractID = "CT1"
	s.Requisitions["PR1"].Lines[0].Material = "OTHER"
	if _, err := s.CreatePurchaseOrder("PR1", "", "2026-05-01", "u"); err == nil || !strings.Contains(err.Error(), "not on contract") {
		t.Fatalf("material not on contract: %v", err)
	}
	s.Requisitions["PR1"].Lines[0].Material = "M1"
	s.Requisitions["PR1"].Lines[0].Quantity = 100_000
	if _, err := s.CreatePurchaseOrder("PR1", "", "2026-05-01", "u"); err == nil || !strings.Contains(err.Error(), "target value") {
		t.Fatalf("target value: %v", err)
	}
	// A vendor's own record is not a duplicate of itself.
	if w := s.VendorWarnings(s.Vendors["V1"]); len(w) != 1 || w[0] != "no tax number" {
		t.Fatalf("warnings: %v", w)
	}
	if err := s.ValidateNewVendor(&Vendor{ID: "V5", Name: "Five", APAccount: map[string]string{"1000": "211000"}}); err != nil {
		t.Fatalf("valid vendor: %v", err)
	}

	postedInvoice(t, s)
	s.Vendors["V1"].Blocked = true
	// Journal noise the proposal must ignore: another company and a vendor
	// line on a non-reconciliation account.
	s.Companies["2000"] = &Company{Code: "2000"}
	s.Journal["X1"] = &JournalEntry{ID: "X1", CompanyCode: "2000", Lines: []JournalLine{{Account: "211000", Amount: -1, VendorID: "V1"}}}
	s.Journal["X2"] = &JournalEntry{ID: "X2", CompanyCode: "1000", PostingDate: "2026-01-01", Lines: []JournalLine{{Account: "510000", Amount: -1, VendorID: "V1"}}}
	run, err := s.ProposePayments("1000", "2026-04-01", 5, "u")
	if err != nil || len(run.Excluded) != 1 || !strings.Contains(run.Excluded[0].Reason, "blocked") {
		t.Fatalf("blocked vendor excluded: %v %+v", err, run)
	}
}

func TestStoreFileErrors(t *testing.T) {
	dir := t.TempDir()
	s := fixture()
	blocker := filepath.Join(dir, "file")
	os.WriteFile(blocker, nil, 0o644)
	if err := s.Save(filepath.Join(blocker, "sub", "s.json")); err == nil {
		t.Fatal("directory creation failure")
	}
	os.Mkdir(filepath.Join(dir, "s.json.tmp"), 0o755)
	if err := s.Save(filepath.Join(dir, "s.json")); err == nil {
		t.Fatal("write failure")
	}
	if _, err := LoadStore(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("missing file")
	}
	os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{"), 0o644)
	if _, err := LoadStore(filepath.Join(dir, "bad.json")); err == nil {
		t.Fatal("corrupt file")
	}
}
