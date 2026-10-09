package p2p

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/agent"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/governance"
	"github.com/mustie7iy/agentic-erp/internal/migration"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/sap"
	"github.com/mustie7iy/agentic-erp/internal/workflow"
)

type memSource struct{ e *sap.Extract }

func (m memSource) Name() string { return "memory" }
func (m memSource) Extract(_ context.Context, s sap.Scope) (*sap.Extract, error) {
	return sap.FilterScope(m.e, s), nil
}

var today = time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)

func setup(t *testing.T) (*Service, *config.Config) {
	t.Helper()
	s := core.NewStore()
	scope := sap.Scope{CompanyCodes: []string{"1000", "2000"}, FiscalYear: "2026"}
	if res, err := migration.Run(context.Background(), memSource{sap.GenerateMock(42, "2026")}, scope, migration.Options{}, s); err != nil || !res.Report.Passed {
		t.Fatalf("migration: %v", err)
	}
	cfg, err := config.Load("../config/testdata/valid.yaml")
	if err != nil {
		t.Fatal(err)
	}
	audit, _ := governance.NewAuditLog("")
	return &Service{Store: s, Config: func() (*config.Config, error) { return cfg, nil }, Audit: audit, Now: func() time.Time { return today }}, cfg
}

func line(desc string, qty int64, price money.Minor) core.RequisitionLine {
	return core.RequisitionLine{Description: desc, Quantity: money.Qty(qty * money.QtyScale), UnitPrice: price, GLAccount: "520000", Material: "MAT-" + desc}
}

// The complete happy path: request, approve, order, receive, invoice, post.
func TestRequisitionToInvoice(t *testing.T) {
	svc, cfg := setup(t)
	req, err := svc.CreateRequisition("buyer1", RequisitionInput{CompanyCode: "1000", VendorID: "100001", Lines: []core.RequisitionLine{line("PAPER", 10, 2_500)}})
	if err != nil {
		t.Fatal(err)
	}
	if req.Status != core.ReqPending || req.Total() != 25_000 || req.Requester != "user:buyer1" {
		t.Fatalf("requisition: %+v", req)
	}
	if _, _, err := svc.CreatePurchaseOrder("buyer1", req.ID, ""); err == nil {
		t.Fatal("unapproved requisition must not become a PO")
	}
	if _, err := svc.DecideRequisition("buyer1", req.ID, true, ""); err == nil {
		t.Fatal("requester must not approve own requisition")
	}
	req, err = svc.DecideRequisition("purchmgr1", req.ID, true, "ok")
	if err != nil || req.Status != core.ReqApproved {
		t.Fatalf("approve: %v %s", err, req.Status)
	}
	po, out, err := svc.CreatePurchaseOrder("buyer1", req.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if po.RequisitionID != req.ID || len(po.Lines) != 1 || svc.Store.Requisitions[req.ID].Status != core.ReqOrdered {
		t.Fatalf("po: %+v", po)
	}
	if out.Channel != "email" || !strings.Contains(out.Body, po.ID) || !strings.Contains(out.Body, "Total: 250.00 USD") {
		t.Fatalf("output: %+v", out)
	}
	if _, _, err := svc.CreatePurchaseOrder("buyer1", req.ID, ""); err == nil {
		t.Fatal("a requisition can only be ordered once")
	}

	ln := po.Lines[0].LineNo
	if _, err := svc.PostGoodsReceipt("buyer1", po.ID, ln, 10_000, false); !errors.Is(err, ErrPermission) {
		t.Fatalf("buyer cannot post receipts: %v", err)
	}
	if _, err := svc.PostGoodsReceipt("whclerk1", po.ID, ln, 11_000, false); err == nil {
		t.Fatal("over-delivery beyond tolerance must fail")
	}
	gr, err := svc.PostGoodsReceipt("whclerk1", po.ID, ln, 10_000, false)
	if err != nil || gr.MovementType != "101" {
		t.Fatalf("receipt: %v", err)
	}

	// Invoice the PO and let the configured invoice service post it.
	svc.Store.Invoices["INV-T1"] = &core.SupplierInvoice{
		ID: "INV-T1", CompanyCode: "1000", VendorID: "100001", VendorReference: "T-1", POID: po.ID, InvoiceDate: "2026-10-09",
		Currency: "USD", Lines: []core.InvoiceLine{{POLineNo: ln, Quantity: 10_000, Amount: 25_000}}, TaxAmount: 2_000, GrossAmount: 27_000,
		Status: core.InvoiceParked, Source: "test",
	}
	inv := agent.NewService(svc.Store, svc.Audit)
	inv.Config = cfg
	sum := agent.ProcessInvoices(context.Background(), inv, nil, io.Discard)
	if !contains(sum.Posted, "INV-T1") {
		t.Fatalf("clean invoice should post: %+v", sum)
	}
}

func TestLargeRequisitionNeedsTwoSteps(t *testing.T) {
	svc, _ := setup(t)
	req, err := svc.CreateRequisition("buyer1", RequisitionInput{CompanyCode: "1000", VendorID: "100002", Lines: []core.RequisitionLine{line("SERVERS", 5, 400_000)}})
	if err != nil {
		t.Fatal(err)
	}
	wf := svc.Store.Workflows[req.WorkflowID]
	if len(wf.Steps) != 2 {
		t.Fatalf("20,000.00 needs two steps, got %v", wf.Steps)
	}
	if _, err := svc.DecideRequisition("owner1", req.ID, true, ""); !errors.Is(err, workflow.ErrWrongRole) {
		t.Fatalf("process owner approves second: %v", err)
	}
	svc.DecideRequisition("purchmgr1", req.ID, true, "")
	if svc.Store.Requisitions[req.ID].Status != core.ReqPending {
		t.Fatal("still waiting for the process owner")
	}
	req, err = svc.DecideRequisition("owner1", req.ID, true, "")
	if err != nil || req.Status != core.ReqApproved {
		t.Fatalf("final approval: %v %s", err, req.Status)
	}
}

func TestRequisitionRejectAndValidation(t *testing.T) {
	svc, cfg := setup(t)
	req, _ := svc.CreateRequisition("buyer1", RequisitionInput{CompanyCode: "1000", Lines: []core.RequisitionLine{line("X", 1, 100)}})
	req, err := svc.DecideRequisition("purchmgr1", req.ID, false, "not budgeted")
	if err != nil || req.Status != core.ReqRejected {
		t.Fatalf("reject: %v", err)
	}
	cases := map[string]RequisitionInput{
		"no lines":         {CompanyCode: "1000"},
		"bad quantity":     {CompanyCode: "1000", Lines: []core.RequisitionLine{line("X", 0, 100)}},
		"bad account":      {CompanyCode: "1000", Lines: []core.RequisitionLine{{Description: "X", Quantity: 1000, UnitPrice: 1, GLAccount: "999"}}},
		"unknown company":  {CompanyCode: "9999", Lines: []core.RequisitionLine{line("X", 1, 100)}},
		"unknown contract": {CompanyCode: "1000", ContractID: "CT-NONE", Lines: []core.RequisitionLine{line("X", 1, 100)}},
	}
	for name, in := range cases {
		if _, err := svc.CreateRequisition("buyer1", in); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if _, err := svc.CreateRequisition("apclerk1", RequisitionInput{CompanyCode: "1000", Lines: []core.RequisitionLine{line("X", 1, 100)}}); !errors.Is(err, ErrPermission) {
		t.Fatalf("clerk cannot request: %v", err)
	}
	if _, err := svc.CreateRequisition("ghost", RequisitionInput{CompanyCode: "1000"}); err == nil {
		t.Fatal("unknown user")
	}
	cfg.User("buyer1").Companies = []string{"2000"}
	if _, err := svc.CreateRequisition("buyer1", RequisitionInput{CompanyCode: "1000", Lines: []core.RequisitionLine{line("X", 1, 100)}}); !errors.Is(err, ErrPermission) {
		t.Fatalf("company access: %v", err)
	}
	cfg.User("buyer1").Companies = nil
	if _, err := svc.DecideRequisition("purchmgr1", "PR-NONE", true, ""); err == nil {
		t.Fatal("missing requisition")
	}
	if _, err := svc.DecideRequisition("ghost", req.ID, true, ""); err == nil {
		t.Fatal("unknown approver")
	}
	cfg.User("purchmgr1").Companies = []string{"2000"}
	req2, _ := svc.CreateRequisition("buyer1", RequisitionInput{CompanyCode: "1000", Lines: []core.RequisitionLine{line("Y", 1, 100)}})
	if _, err := svc.DecideRequisition("purchmgr1", req2.ID, true, ""); !errors.Is(err, ErrPermission) {
		t.Fatalf("approver without company access: %v", err)
	}
}

func TestContracts(t *testing.T) {
	svc, _ := setup(t)
	in := ContractInput{CompanyCode: "1000", VendorID: "100003", ValidFrom: "2026-01-01", ValidTo: "2026-12-31", TargetValue: 100_000,
		Items: []core.ContractItem{{Material: "MAT-TONER", Description: "Toner", UnitPrice: 4_000, GLAccount: "520000"}}}
	c, err := svc.CreateContract("buyer1", in)
	if err != nil {
		t.Fatal(err)
	}
	if c.Items[0].PriceUnit != 1 {
		t.Fatal("price unit defaults to 1")
	}
	order := func(qty int64, material string) (*core.PurchaseOrder, error) {
		req, err := svc.CreateRequisition("buyer1", RequisitionInput{CompanyCode: "1000", ContractID: c.ID,
			Lines: []core.RequisitionLine{{Material: material, Description: "Toner", Quantity: money.Qty(qty * 1000), UnitPrice: 1}}})
		if err != nil {
			return nil, err
		}
		if _, err := svc.DecideRequisition("purchmgr1", req.ID, true, ""); err != nil {
			return nil, err
		}
		po, _, err := svc.CreatePurchaseOrder("buyer1", req.ID, "")
		return po, err
	}
	po, err := order(10, "MAT-TONER")
	if err != nil {
		t.Fatal(err)
	}
	if po.Lines[0].UnitPrice != 4_000 || po.ContractID != c.ID || svc.Store.Contracts[c.ID].ReleasedValue != 40_000 {
		t.Fatalf("contract price not applied: %+v", po.Lines[0])
	}
	if _, err := order(20, "MAT-TONER"); err == nil || !strings.Contains(err.Error(), "target value") {
		t.Fatalf("target value overrun: %v", err)
	}
	if _, err := order(1, "MAT-OTHER"); err == nil || !strings.Contains(err.Error(), "not on contract") {
		t.Fatalf("material not on contract: %v", err)
	}
	bad := []ContractInput{
		{CompanyCode: "1000", VendorID: "100003", ValidFrom: "2026-12-31", ValidTo: "2026-01-01", TargetValue: 1, Items: in.Items},
		{CompanyCode: "1000", VendorID: "100003", ValidFrom: "2026-01-01", ValidTo: "2026-12-31", TargetValue: 0, Items: in.Items},
		{CompanyCode: "1000", VendorID: "999999", ValidFrom: "2026-01-01", ValidTo: "2026-12-31", TargetValue: 1, Items: in.Items},
		{CompanyCode: "1000", VendorID: "100003", ValidFrom: "2026-01-01", ValidTo: "2026-12-31", TargetValue: 1,
			Items: []core.ContractItem{{Material: "A", UnitPrice: 1, GLAccount: "520000"}, {Material: "A", UnitPrice: 1, GLAccount: "520000"}}},
		{CompanyCode: "1000", VendorID: "100003", ValidFrom: "2026-01-01", ValidTo: "2026-12-31", TargetValue: 1,
			Items: []core.ContractItem{{Material: "A", UnitPrice: 1, GLAccount: "999"}}},
		{CompanyCode: "9999", VendorID: "100003", ValidFrom: "2026-01-01", ValidTo: "2026-12-31", TargetValue: 1, Items: in.Items},
	}
	for i, b := range bad {
		if _, err := svc.CreateContract("buyer1", b); err == nil {
			t.Errorf("bad contract %d accepted", i)
		}
	}
	if _, err := svc.CreateContract("apclerk1", in); !errors.Is(err, ErrPermission) {
		t.Fatal("clerk cannot manage contracts")
	}
}

func TestVendorCreationNeedsApproval(t *testing.T) {
	svc, _ := setup(t)
	v := core.Vendor{ID: "200001", Name: "Initech Components", TaxID: "", APAccount: map[string]string{"1000": "211000"}, PaymentTermsDays: 30, BankAccount: "GB33BUKB20201555555555"}
	ch, err := svc.RequestVendor("mdclerk1", v)
	if err != nil {
		t.Fatal(err)
	}
	if len(ch.Warnings) != 2 { // same name as 100001, no tax number
		t.Fatalf("warnings: %v", ch.Warnings)
	}
	if svc.Store.Vendors["200001"] != nil {
		t.Fatal("vendor must not exist before approval")
	}
	if _, err := svc.RequestVendor("mdclerk1", v); err == nil {
		t.Fatal("duplicate pending request")
	}
	if _, err := svc.DecideMasterData("mdclerk1", ch.ID, true, ""); !errors.Is(err, ErrPermission) {
		t.Fatalf("requester cannot approve: %v", err)
	}
	ch, err = svc.DecideMasterData("mdmgr1", ch.ID, true, "checked")
	if err != nil || ch.Status != core.ChangeApplied || svc.Store.Vendors["200001"] == nil {
		t.Fatalf("approve: %v", err)
	}
	for _, bad := range []core.Vendor{
		{ID: "", Name: "x", APAccount: map[string]string{"1000": "211000"}},
		{ID: "200001", Name: "x", APAccount: map[string]string{"1000": "211000"}},
		{ID: "200002", Name: " ", APAccount: map[string]string{"1000": "211000"}},
		{ID: "200003", Name: "x"},
		{ID: "200004", Name: "x", APAccount: map[string]string{"1000": "100000"}},
	} {
		if _, err := svc.RequestVendor("mdclerk1", bad); err == nil {
			t.Errorf("bad vendor %+v accepted", bad)
		}
	}
	rej, _ := svc.RequestVendor("mdclerk1", core.Vendor{ID: "200005", Name: "New Co", TaxID: "99-1", APAccount: map[string]string{"1000": "211000"}})
	rej, err = svc.DecideMasterData("mdmgr1", rej.ID, false, "not needed")
	if err != nil || rej.Status != core.ChangeRejected {
		t.Fatalf("reject: %v", err)
	}
	if _, err := svc.DecideMasterData("mdmgr1", "MDC-NONE", true, ""); err == nil {
		t.Fatal("missing change")
	}
	if _, err := svc.RequestVendor("buyer1", v); !errors.Is(err, ErrPermission) {
		t.Fatal("buyer cannot create vendors")
	}
}

func payAll(t *testing.T, svc *Service) *core.PaymentRun {
	t.Helper()
	run, err := svc.ProposePaymentRun("treas1", "1000", "2027-06-30")
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestPaymentRunEndToEnd(t *testing.T) {
	svc, _ := setup(t)
	before := svc.Store.VendorOpenItems("1000")
	run := payAll(t, svc)
	if len(run.Payments) == 0 || run.Total <= 0 {
		t.Fatalf("nothing proposed: %+v", run)
	}
	if !excludedFor(run, "100012", "blocked") {
		t.Fatalf("blocked vendor must be excluded: %+v", run.Excluded)
	}
	if _, err := svc.DecidePaymentRun("treas1", run.ID, true, ""); !errors.Is(err, ErrPermission) {
		t.Fatalf("proposer lacks release permission: %v", err)
	}
	// A second proposal cannot pick up items already in the open run.
	if _, err := svc.ProposePaymentRun("treas1", "1000", "2027-06-30"); err == nil {
		t.Fatal("reserved items must not be proposed twice")
	}
	run, err := svc.DecidePaymentRun("treasmgr1", run.ID, true, "released")
	if err != nil || run.Status != core.RunReleased {
		t.Fatalf("release: %v", err)
	}
	if len(run.JournalEntryIDs) != len(run.Payments) {
		t.Fatal("one payment entry per vendor")
	}
	after := svc.Store.VendorOpenItems("1000")
	for _, p := range run.Payments {
		if len(after[p.VendorID]) != 0 {
			t.Errorf("vendor %s still has open items after payment", p.VendorID)
		}
	}
	if len(after["100012"]) != len(before["100012"]) {
		t.Error("blocked vendor items must stay open")
	}
	var tb int64
	for _, v := range svc.Store.TrialBalance("1000", "") {
		tb += int64(v)
	}
	if tb != 0 {
		t.Fatalf("ledger out of balance by %d", tb)
	}

	var doc struct {
		Init struct {
			Hdr struct {
				N   int    `xml:"NbOfTxs"`
				Sum string `xml:"CtrlSum"`
			} `xml:"GrpHdr"`
			Pmt struct {
				Debtor string `xml:"DbtrAcct>Id>IBAN"`
				Txs    []struct {
					Amount string `xml:"Amt>InstdAmt"`
				} `xml:"CdtTrfTxInf"`
			} `xml:"PmtInf"`
		} `xml:"CstmrCdtTrfInitn"`
	}
	if err := xml.Unmarshal([]byte(run.BankFile), &doc); err != nil {
		t.Fatalf("bank file is not valid XML: %v", err)
	}
	if doc.Init.Hdr.N != len(run.Payments) || doc.Init.Hdr.Sum != run.Total.Decimal() || doc.Init.Pmt.Debtor != "GB33BUKB20201555555555" {
		t.Fatalf("bank file header: %+v", doc.Init.Hdr)
	}
	if _, err := svc.DecidePaymentRun("treasmgr1", run.ID, true, ""); err == nil {
		t.Fatal("a released run cannot be released again")
	}
	if _, err := svc.ProposePaymentRun("treas1", "1000", "2027-06-30"); err == nil || !strings.Contains(err.Error(), "nothing is due") {
		t.Fatalf("everything payable was paid: %v", err)
	}
}

func TestBankChangeHoldsPayments(t *testing.T) {
	svc, _ := setup(t)
	vendor := firstPayable(t, svc)
	ch, err := svc.RequestBankChange("mdclerk1", vendor, "GB82WEST12345698765432", "vendor letter of 1 Oct")
	if err != nil {
		t.Fatal(err)
	}
	run := payAll(t, svc)
	if !excludedFor(run, vendor, "awaiting approval") {
		t.Fatalf("pending bank change must hold payment: %+v", run.Excluded)
	}
	svc.DecidePaymentRun("treasmgr1", run.ID, false, "redo after bank change")
	if svc.Store.PaymentRuns[run.ID].Status != core.RunRejected {
		t.Fatal("rejected run")
	}
	if _, err := svc.DecideMasterData("mdmgr1", ch.ID, true, "called vendor to confirm"); err != nil {
		t.Fatal(err)
	}
	if v := svc.Store.Vendors[vendor]; v.BankAccount != "GB82WEST12345698765432" || v.BankChangedAt != "2026-10-09" {
		t.Fatalf("bank change not applied: %+v", v)
	}
	run, _ = svc.ProposePaymentRun("treas1", "1000", "2026-10-12")
	if run != nil && !excludedFor(run, vendor, "held for 5 days") {
		t.Fatalf("recent bank change must hold payment: %+v", run.Excluded)
	}
	if run != nil {
		svc.DecidePaymentRun("treasmgr1", run.ID, false, "x")
	}
	later, err := svc.ProposePaymentRun("treas1", "1000", "2027-06-30")
	if err != nil || excludedFor(later, vendor, "held") {
		t.Fatalf("after the hold the vendor is paid: %v %+v", err, later)
	}
	for _, bad := range [][3]string{{"999999", "X", "r"}, {vendor, "", "r"}, {vendor, "Y", ""}, {vendor, "GB82WEST12345698765432", "same"}} {
		if _, err := svc.RequestBankChange("mdclerk1", bad[0], bad[1], bad[2]); err == nil {
			t.Errorf("bad bank change %v accepted", bad)
		}
	}
	if _, err := svc.RequestBankChange("buyer1", vendor, "X", "r"); !errors.Is(err, ErrPermission) {
		t.Fatal("buyer cannot change bank details")
	}
}

func TestBlockAndUnblock(t *testing.T) {
	svc, _ := setup(t)
	vendor := firstPayable(t, svc)
	if err := svc.BlockVendor("mdclerk1", vendor, ""); err == nil {
		t.Fatal("reason required")
	}
	if err := svc.BlockVendor("mdclerk1", "999999", "x"); err == nil {
		t.Fatal("missing vendor")
	}
	if err := svc.BlockVendor("buyer1", vendor, "x"); !errors.Is(err, ErrPermission) {
		t.Fatal("buyer cannot block")
	}
	if err := svc.BlockVendor("mdclerk1", vendor, "fraud alert"); err != nil || !svc.Store.Vendors[vendor].Blocked {
		t.Fatalf("block: %v", err)
	}
	ch, err := svc.RequestUnblock("mdclerk1", vendor, "cleared")
	if err != nil {
		t.Fatal(err)
	}
	if !svc.Store.Vendors[vendor].Blocked {
		t.Fatal("still blocked until approved")
	}
	svc.DecideMasterData("mdmgr1", ch.ID, true, "")
	if svc.Store.Vendors[vendor].Blocked {
		t.Fatal("unblock not applied")
	}
	if _, err := svc.RequestUnblock("mdclerk1", vendor, "x"); err == nil {
		t.Fatal("vendor is not blocked")
	}
}

func TestReleaseFailureLeavesRunRetryable(t *testing.T) {
	svc, _ := setup(t)
	run := payAll(t, svc)
	// Someone clears an item behind the run's back.
	it := run.Payments[0].Items[0]
	svc.Store.Journal[it.JournalEntryID].Lines[it.Line].ClearedBy = "MANUAL"
	if _, err := svc.DecidePaymentRun("treasmgr1", run.ID, true, ""); err == nil || !strings.Contains(err.Error(), "changed since the proposal") {
		t.Fatalf("stale run must not post: %v", err)
	}
	if wf := svc.Store.Workflows[run.WorkflowID]; wf.Status != workflow.Pending || run.Status != core.RunProposed {
		t.Fatalf("decision must be undone: %s / %s", wf.Status, run.Status)
	}
	if _, err := svc.DecidePaymentRun("treasmgr1", "PAY-NONE", true, ""); err == nil {
		t.Fatal("missing run")
	}
}

func TestEDIOutput(t *testing.T) {
	svc, cfg := setup(t)
	cfg.Output.PurchaseOrderChannel = "edi"
	req, _ := svc.CreateRequisition("buyer1", RequisitionInput{CompanyCode: "1000", VendorID: "100001", Lines: []core.RequisitionLine{line("PAPER", 2, 1_000), line("PENS", 3, 200)}})
	svc.DecideRequisition("purchmgr1", req.ID, true, "")
	_, out, err := svc.CreatePurchaseOrder("buyer1", req.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.Body, "ST*850*0001~") || !strings.Contains(out.Body, "CTT*2~") || !strings.Contains(out.Body, "SE*8*0001~") {
		t.Fatalf("X12 850:\n%s", out.Body)
	}
}

func TestConfigUnavailable(t *testing.T) {
	svc, _ := setup(t)
	svc.Config = func() (*config.Config, error) { return nil, errors.New("no active configuration") }
	if _, err := svc.CreateRequisition("buyer1", RequisitionInput{}); err == nil {
		t.Fatal("must fail without configuration")
	}
	if _, err := svc.DecideRequisition("purchmgr1", "x", true, ""); err == nil {
		t.Fatal("must fail without configuration")
	}
	if _, _, err := svc.CreatePurchaseOrder("buyer1", "x", ""); err == nil {
		t.Fatal("must fail without configuration")
	}
}

func firstPayable(t *testing.T, svc *Service) string {
	t.Helper()
	for _, v := range core.SortedKeys(svc.Store.VendorOpenItems("1000")) {
		if !svc.Store.Vendors[v].Blocked {
			return v
		}
	}
	t.Fatal("no payable vendor")
	return ""
}

func excludedFor(run *core.PaymentRun, vendor, reason string) bool {
	for _, e := range run.Excluded {
		if e.VendorID == vendor && strings.Contains(e.Reason, reason) {
			return true
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
