package core

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

func fixture() *Store {
	s := NewStore()
	s.Companies["1000"] = &Company{Code: "1000", Name: "Test", Currency: "USD"}
	for _, a := range []*GLAccount{
		{CompanyCode: "1000", Number: "100000", Name: "Bank", Type: Asset},
		{CompanyCode: "1000", Number: "140000", Name: "Input tax", Type: Asset},
		{CompanyCode: "1000", Number: "211000", Name: "AP", Type: Liability, VendorRecon: true},
		{CompanyCode: "1000", Number: "510000", Name: "Materials", Type: Expense},
	} {
		s.PutAccount(a)
	}
	s.Vendors["V1"] = &Vendor{ID: "V1", Name: "Vendor One", APAccount: map[string]string{"1000": "211000"}}
	s.PurchaseOrders["PO1"] = &PurchaseOrder{
		ID: "PO1", CompanyCode: "1000", VendorID: "V1", Currency: "USD",
		Lines: []POLine{{LineNo: 10, Quantity: 10_000, UnitPrice: 10_000, PriceUnit: 1, GLAccount: "510000"}},
	}
	s.GoodsReceipts["GR1"] = &GoodsReceipt{ID: "GR1", POID: "PO1", POLineNo: 10, Quantity: 10_000}
	return s
}

func invoice(id, ref string, qty int64, amount, tax int64) *SupplierInvoice {
	return &SupplierInvoice{
		ID: id, CompanyCode: "1000", VendorID: "V1", VendorReference: ref, POID: "PO1", Currency: "USD",
		Lines:     []InvoiceLine{{POLineNo: 10, Quantity: moneyQty(qty), Amount: moneyMinor(amount)}},
		TaxAmount: moneyMinor(tax), GrossAmount: moneyMinor(amount + tax), Status: InvoiceParked,
	}
}

func TestPostRejectsUnbalancedAndMissingVendor(t *testing.T) {
	s := fixture()
	err := s.Post(&JournalEntry{ID: "J1", CompanyCode: "1000", Lines: []JournalLine{
		{Account: "100000", Amount: 100}, {Account: "510000", Amount: -99},
	}})
	if err == nil || !strings.Contains(err.Error(), "unbalanced") {
		t.Fatalf("want unbalanced error, got %v", err)
	}
	err = s.Post(&JournalEntry{ID: "J2", CompanyCode: "1000", Lines: []JournalLine{
		{Account: "510000", Amount: 100}, {Account: "211000", Amount: -100},
	}})
	if err == nil || !strings.Contains(err.Error(), "requires a vendor") {
		t.Fatalf("want vendor error, got %v", err)
	}
	if len(s.Journal) != 0 {
		t.Fatal("rejected entries must not be stored")
	}
}

func TestThreeWayMatchCleanAndPost(t *testing.T) {
	s := fixture()
	inv := invoice("I1", "A-1", 10_000, 100_000, 8_000)
	s.Invoices[inv.ID] = inv
	if m := s.ThreeWayMatch(inv, DefaultTolerance); !m.Clean {
		t.Fatalf("expected clean match, got %s", m.Summary())
	}
	je, err := s.PostInvoice("I1", "140000", "2026-03-01")
	if err != nil {
		t.Fatal(err)
	}
	if inv.Status != InvoicePosted || inv.JournalEntryID != je.ID {
		t.Fatalf("invoice not marked posted: %+v", inv)
	}
	tb := s.TrialBalance("1000", "")
	if tb["510000"] != 100_000 || tb["140000"] != 8_000 || tb["211000"] != -108_000 {
		t.Fatalf("unexpected trial balance %v", tb)
	}
	open := s.VendorOpenItems("1000")["V1"]
	if len(open) != 1 || open[0].Amount != -108_000 {
		t.Fatalf("unexpected open items %v", open)
	}
}

func TestThreeWayMatchExceptions(t *testing.T) {
	s := fixture()
	posted := invoice("I0", "DUP-1", 4_000, 40_000, 0)
	posted.Status = InvoicePosted
	s.Invoices[posted.ID] = posted

	cases := map[string]struct {
		inv  *SupplierInvoice
		want IssueKind
	}{
		"price":     {invoice("I1", "P-1", 5_000, 60_000, 0), IssuePriceVariance},
		"quantity":  {invoice("I2", "Q-1", 7_000, 70_000, 0), IssueQtyOverReceived}, // 4 already invoiced + 7 > 10
		"duplicate": {invoice("I3", "dup 1", 1_000, 10_000, 0), IssueDuplicate},
		"total":     {func() *SupplierInvoice { i := invoice("I4", "T-1", 1_000, 10_000, 800); i.GrossAmount++; return i }(), IssueTotalMismatch},
	}
	for name, c := range cases {
		m := s.ThreeWayMatch(c.inv, DefaultTolerance)
		found := false
		for _, is := range m.Issues {
			found = found || is.Kind == c.want
		}
		if m.Clean || !found {
			t.Errorf("%s: want issue %s, got %s", name, c.want, m.Summary())
		}
	}
}

func TestPriceWithinToleranceIsClean(t *testing.T) {
	s := fixture()
	inv := invoice("I1", "OK-1", 10_000, 100_400, 0) // +4.00 on 1,000.00; tolerance is max(5.00, 2%)
	s.Invoices[inv.ID] = inv
	if m := s.ThreeWayMatch(inv, DefaultTolerance); !m.Clean {
		t.Fatalf("expected clean, got %s", m.Summary())
	}
}

func TestPostInvoiceRefusesBadTotals(t *testing.T) {
	s := fixture()
	inv := invoice("I1", "X-1", 1_000, 10_000, 800)
	inv.GrossAmount = 99_999
	s.Invoices[inv.ID] = inv
	if _, err := s.PostInvoice("I1", "140000", "2026-03-01"); err == nil {
		t.Fatal("ledger must reject an invoice whose gross != net + tax")
	}
	if inv.Status != InvoiceParked {
		t.Fatalf("status changed to %s", inv.Status)
	}
}

func TestStoreRoundTrip(t *testing.T) {
	s := fixture()
	path := filepath.Join(t.TempDir(), "store.json")
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Account("1000", "211000") == nil || !got.Account("1000", "211000").VendorRecon || got.Vendors["V1"] == nil {
		t.Fatal("round trip lost data")
	}
}

func moneyQty(v int64) money.Qty     { return money.Qty(v) }
func moneyMinor(v int64) money.Minor { return money.Minor(v) }

func TestIndexFollowsNewRecords(t *testing.T) {
	s := fixture()
	first := invoice("I1", "DUP-9", 1_000, 10_000, 0)
	first.Status = InvoicePosted
	s.Invoices["I1"] = first
	second := invoice("I2", "dup 9", 1_000, 10_000, 0)
	if m := s.ThreeWayMatch(second, DefaultTolerance); !hasKind(m, IssueDuplicate) {
		t.Fatal("index must include invoices added before the first lookup")
	}
	// A receipt and an invoice added after the index was built are seen.
	s.GoodsReceipts["GR2"] = &GoodsReceipt{ID: "GR2", POID: "PO1", POLineNo: 10, Quantity: 5_000}
	if got := len(s.ReceiptsFor("PO1")); got != 2 {
		t.Fatalf("receipts after add: %d", got)
	}
	third := invoice("I3", "NEW-1", 1_000, 10_000, 0)
	third.Status = InvoicePosted
	s.Invoices["I3"] = third
	fourth := invoice("I4", "new 1", 1_000, 10_000, 0)
	if m := s.ThreeWayMatch(fourth, DefaultTolerance); !hasKind(m, IssueDuplicate) {
		t.Fatal("index must refresh when invoices are added")
	}
	// Status changes are read live.
	third.Status = InvoiceRejected
	if m := s.ThreeWayMatch(fourth, DefaultTolerance); hasKind(m, IssueDuplicate) {
		t.Fatal("rejected invoices are not duplicates")
	}
}

func hasKind(m MatchResult, k IssueKind) bool {
	for _, is := range m.Issues {
		if is.Kind == k {
			return true
		}
	}
	return false
}
