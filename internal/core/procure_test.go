package core

import (
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// p2pFixture extends the core fixture with an approved requisition and a contract.
func p2pFixture(t *testing.T) *Store {
	t.Helper()
	s := fixture()
	s.Vendors["V1"].PaymentTermsDays = 30
	s.Vendors["V1"].BankAccount = "GB33BUKB20201555555555"
	s.Requisitions["PR1"] = &Requisition{ID: "PR1", CompanyCode: "1000", VendorID: "V1", Currency: "USD", Status: ReqApproved,
		Lines: []RequisitionLine{{LineNo: 1, Material: "M1", Description: "Item", Quantity: 5_000, UnitPrice: 1_000, GLAccount: "510000"}}}
	s.Contracts["CT1"] = &Contract{ID: "CT1", CompanyCode: "1000", VendorID: "V1", Currency: "USD", ValidFrom: "2026-01-01", ValidTo: "2026-12-31",
		TargetValue: 10_000, Items: []ContractItem{{Material: "M1", UnitPrice: 800, PriceUnit: 1, GLAccount: "510000"}}}
	return s
}

func TestCreatePurchaseOrderRules(t *testing.T) {
	s := p2pFixture(t)
	if _, err := s.CreatePurchaseOrder("NOPE", "", "2026-05-01", "u"); err == nil {
		t.Fatal("missing requisition")
	}
	s.Vendors["V1"].Blocked = true
	if _, err := s.CreatePurchaseOrder("PR1", "", "2026-05-01", "u"); err == nil {
		t.Fatal("blocked vendor")
	}
	s.Vendors["V1"].Blocked = false
	if _, err := s.CreatePurchaseOrder("PR1", "V9", "2026-05-01", "u"); err == nil {
		t.Fatal("unknown vendor")
	}
	s.Vendors["V2"] = &Vendor{ID: "V2", Name: "Two", APAccount: map[string]string{"2000": "211000"}}
	if _, err := s.CreatePurchaseOrder("PR1", "V2", "2026-05-01", "u"); err == nil {
		t.Fatal("vendor not set up for the company")
	}
	s.Requisitions["PR1"].Lines[0].GLAccount = "999999"
	if _, err := s.CreatePurchaseOrder("PR1", "", "2026-05-01", "u"); err == nil {
		t.Fatal("unknown G/L account")
	}
	s.Requisitions["PR1"].Lines[0].GLAccount = "510000"

	// Contract checks.
	s.Requisitions["PR1"].ContractID = "CT9"
	if _, err := s.CreatePurchaseOrder("PR1", "", "2026-05-01", "u"); err == nil {
		t.Fatal("missing contract")
	}
	s.Requisitions["PR1"].ContractID = "CT1"
	s.Contracts["CT1"].VendorID = "V2"
	if _, err := s.CreatePurchaseOrder("PR1", "V1", "2026-05-01", "u"); err == nil {
		t.Fatal("contract with another vendor")
	}
	s.Contracts["CT1"].VendorID = "V1"
	if _, err := s.CreatePurchaseOrder("PR1", "", "2027-01-15", "u"); err == nil {
		t.Fatal("outside contract validity")
	}
	po, err := s.CreatePurchaseOrder("PR1", "", "2026-05-01", "u")
	if err != nil {
		t.Fatal(err)
	}
	if po.Lines[0].UnitPrice != 800 || s.Contracts["CT1"].ReleasedValue != 4_000 || s.Requisitions["PR1"].Status != ReqOrdered {
		t.Fatalf("contract pricing and release: %+v", po.Lines[0])
	}
	if s.Requisitions["PR1"].Total() != 5_000 {
		t.Fatal("requisition total")
	}
}

func TestGoodsReceiptRules(t *testing.T) {
	s := fixture()
	if _, err := s.PostGoodsReceipt("PO1", 10, 0, false, 0, "2026-05-01", "u"); err == nil {
		t.Fatal("zero quantity")
	}
	if _, err := s.PostGoodsReceipt("PO9", 10, 1, false, 0, "2026-05-01", "u"); err == nil {
		t.Fatal("missing PO")
	}
	if _, err := s.PostGoodsReceipt("PO1", 99, 1, false, 0, "2026-05-01", "u"); err == nil {
		t.Fatal("missing line")
	}
	// The fixture already received the full 10 units.
	if _, err := s.PostGoodsReceipt("PO1", 10, 1_000, false, 0, "2026-05-01", "u"); err == nil {
		t.Fatal("over-delivery without tolerance")
	}
	if _, err := s.PostGoodsReceipt("PO1", 10, 1_000, false, 10, "2026-05-01", "u"); err != nil {
		t.Fatalf("10%% tolerance allows one more unit: %v", err)
	}
	if _, err := s.PostGoodsReceipt("PO1", 10, 20_000, true, 0, "2026-05-01", "u"); err == nil {
		t.Fatal("reversal above received")
	}
	gr, err := s.PostGoodsReceipt("PO1", 10, 2_000, true, 0, "2026-05-01", "u")
	if err != nil || gr.Quantity != -2_000 || gr.MovementType != "102" {
		t.Fatalf("reversal: %v %+v", err, gr)
	}
	ordered, received, _ := s.OpenQuantity("PO1", 10)
	if ordered != 10_000 || received != 9_000 {
		t.Fatalf("open quantity %s/%s", ordered, received)
	}
}

func TestVendorValidationAndWarnings(t *testing.T) {
	s := fixture()
	s.Vendors["V1"].TaxID = "12-345"
	cases := []*Vendor{
		{Name: "x", APAccount: map[string]string{"1000": "211000"}},
		{ID: "V1", Name: "x", APAccount: map[string]string{"1000": "211000"}},
		{ID: "V2", Name: " ", APAccount: map[string]string{"1000": "211000"}},
		{ID: "V2", Name: "x"},
		{ID: "V2", Name: "x", APAccount: map[string]string{"1000": "100000"}},
	}
	for i, v := range cases {
		if err := s.ValidateNewVendor(v); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
	w := s.VendorWarnings(&Vendor{ID: "V2", Name: "vendor  ONE", TaxID: ""})
	if len(w) != 2 || !strings.Contains(w[0], "same name") {
		t.Fatalf("warnings: %v", w)
	}
	w = s.VendorWarnings(&Vendor{ID: "V3", Name: "Other", TaxID: "12-345"})
	if len(w) != 1 || !strings.Contains(w[0], "tax number") {
		t.Fatalf("tax-number duplicate: %v", w)
	}
}

func postedInvoice(t *testing.T, s *Store) {
	t.Helper()
	inv := invoice("I1", "A-1", 10_000, 100_000, 8_000)
	s.Invoices["I1"] = inv
	if _, err := s.PostInvoice("I1", "140000", "2026-03-01"); err != nil {
		t.Fatal(err)
	}
}

func TestProposeAndExecutePayments(t *testing.T) {
	s := p2pFixture(t)
	postedInvoice(t, s)
	if _, err := s.ProposePayments("9999", "2026-04-01", 5, "u"); err == nil {
		t.Fatal("unknown company")
	}
	if _, err := s.ProposePayments("1000", "April", 5, "u"); err == nil {
		t.Fatal("bad date")
	}
	early, _ := s.ProposePayments("1000", "2026-03-15", 5, "u")
	if len(early.Payments) != 0 {
		t.Fatal("not yet due (net 30)")
	}
	run, err := s.ProposePayments("1000", "2026-04-01", 5, "u")
	if err != nil || len(run.Payments) != 1 || run.Total != 108_000 || run.Payments[0].Items[0].DueDate != "2026-03-31" {
		t.Fatalf("proposal: %v %+v", err, run)
	}
	if _, err := s.ExecutePaymentRun("NOPE", "100000", "2026-04-01"); err == nil {
		t.Fatal("missing run")
	}
	if _, err := s.ExecutePaymentRun(run.ID, "510000", "2026-04-01"); err == nil {
		t.Fatal("bank must be an asset account")
	}
	jes, err := s.ExecutePaymentRun(run.ID, "100000", "2026-04-01")
	if err != nil || len(jes) != 1 || run.Status != RunReleased {
		t.Fatalf("execute: %v", err)
	}
	if len(s.VendorOpenItems("1000")["V1"]) != 0 {
		t.Fatal("paid items must be cleared")
	}
	if _, err := s.ExecutePaymentRun(run.ID, "100000", "2026-04-01"); err == nil {
		t.Fatal("cannot execute twice")
	}
}

func TestPaymentExclusionsAndStaleRuns(t *testing.T) {
	reason := func(s *Store) string {
		run, err := s.ProposePayments("1000", "2026-04-01", 5, "u")
		if err != nil {
			t.Fatal(err)
		}
		if len(run.Excluded) != 1 {
			t.Fatalf("expected one exclusion, got %+v", run)
		}
		delete(s.PaymentRuns, run.ID)
		return run.Excluded[0].Reason
	}
	s := p2pFixture(t)
	postedInvoice(t, s)
	s.Vendors["V1"].BankAccount = ""
	if r := reason(s); !strings.Contains(r, "no bank account") {
		t.Fatal(r)
	}
	s.Vendors["V1"].BankAccount = "X"
	s.MDChanges["C1"] = &MasterDataChange{ID: "C1", Kind: VendorBankChange, VendorID: "V1", Status: ChangePending}
	if r := reason(s); !strings.Contains(r, "awaiting approval") {
		t.Fatal(r)
	}
	s.MDChanges["C1"].Status = ChangeApplied
	s.Vendors["V1"].BankChangedAt = "2026-03-30"
	if r := reason(s); !strings.Contains(r, "held for 5 days") {
		t.Fatal(r)
	}
	s.Vendors["V1"].BankChangedAt = "garbage"
	if r := reason(s); !strings.Contains(r, "held") {
		t.Fatal("an unreadable change date holds payment to be safe")
	}
	s.Vendors["V1"].BankChangedAt = ""
	// A debit (credit memo) larger than the invoice nets to a debit balance.
	s.Post(&JournalEntry{ID: "CM1", CompanyCode: "1000", PostingDate: "2026-03-02", DocumentType: "KG",
		Lines: []JournalLine{{Account: "211000", Amount: 200_000, VendorID: "V1"}, {Account: "510000", Amount: -200_000}}})
	if r := reason(s); !strings.Contains(r, "debit balance") {
		t.Fatal(r)
	}

	// A run whose items changed after the proposal is refused as a whole.
	s2 := p2pFixture(t)
	postedInvoice(t, s2)
	run, _ := s2.ProposePayments("1000", "2026-04-01", 5, "u")
	je := s2.Journal[run.Payments[0].Items[0].JournalEntryID]
	je.Lines[run.Payments[0].Items[0].Line].ClearedBy = "MANUAL"
	if _, err := s2.ExecutePaymentRun(run.ID, "100000", "2026-04-01"); err == nil {
		t.Fatal("stale item must stop the run")
	}
	run.Payments[0].Items[0].Line = 99
	if _, err := s2.ExecutePaymentRun(run.ID, "100000", "2026-04-01"); err == nil {
		t.Fatal("missing item must stop the run")
	}
	if len(s2.Journal) != 1 {
		t.Fatal("nothing may be posted for a refused run")
	}
	// A broken posting date on an open item surfaces as an error.
	s3 := p2pFixture(t)
	postedInvoice(t, s3)
	for _, je := range s3.Journal {
		je.PostingDate = "bad"
	}
	if _, err := s3.ProposePayments("1000", "2026-04-01", 5, "u"); err == nil {
		t.Fatal("bad posting date")
	}
	// An entry that fails validation stops the whole run before anything posts.
	s4 := p2pFixture(t)
	postedInvoice(t, s4)
	run4, _ := s4.ProposePayments("1000", "2026-04-01", 5, "u")
	s4.Vendors["V1"].APAccount["1000"] = "999999"
	if _, err := s4.ExecutePaymentRun(run4.ID, "100000", "2026-04-01"); err == nil || run4.Status != RunProposed {
		t.Fatal("invalid payment entry must abort the run")
	}
}

func TestReservedItemsAreNotProposedTwice(t *testing.T) {
	s := p2pFixture(t)
	postedInvoice(t, s)
	first, _ := s.ProposePayments("1000", "2026-04-01", 5, "u")
	second, _ := s.ProposePayments("1000", "2026-04-01", 5, "u")
	if len(first.Payments) != 1 || len(second.Payments) != 0 {
		t.Fatal("items in an open run are reserved")
	}
	first.Status = RunRejected
	third, _ := s.ProposePayments("1000", "2026-04-01", 5, "u")
	if len(third.Payments) != 1 {
		t.Fatal("rejected runs release their items")
	}
	_ = money.Minor(0)
}
