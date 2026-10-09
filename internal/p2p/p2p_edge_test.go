package p2p

import (
	"errors"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/governance"
	"github.com/mustie7iy/agentic-erp/internal/workflow"
)

func smallReq() RequisitionInput {
	return RequisitionInput{CompanyCode: "1000", VendorID: "100001", Lines: []core.RequisitionLine{line("PAPER", 1, 100)}}
}

func TestRequisitionRoutingEdges(t *testing.T) {
	svc, cfg := setup(t)
	// A tier with no steps approves at once.
	cfg.Purchasing.RequisitionApproval = []config.ApprovalTier{{Steps: nil}}
	req, err := svc.CreateRequisition("buyer1", smallReq())
	if err != nil || req.Status != core.ReqApproved {
		t.Fatalf("no-step tier: %v %+v", err, req)
	}
	// Unusable routing configuration is reported, not ignored.
	cfg.Purchasing.RequisitionApproval = []config.ApprovalTier{{UpTo: "bad", Steps: nil}}
	if _, err := svc.CreateRequisition("buyer1", smallReq()); err == nil {
		t.Fatal("bad tier amount")
	}
	cfg.Purchasing.RequisitionApproval = []config.ApprovalTier{{Steps: []config.ApprovalStep{{Role: "", Approvals: 1}}}}
	if _, err := svc.CreateRequisition("buyer1", smallReq()); err == nil {
		t.Fatal("step without role")
	}
}

func TestPurchaseOrderAndReceiptEdges(t *testing.T) {
	svc, cfg := setup(t)
	if _, _, err := svc.CreatePurchaseOrder("buyer1", "PR-NONE", ""); err == nil {
		t.Fatal("missing requisition")
	}
	req, _ := svc.CreateRequisition("buyer1", smallReq())
	svc.DecideRequisition("purchmgr1", req.ID, true, "")
	if _, _, err := svc.CreatePurchaseOrder("apclerk1", req.ID, ""); !errors.Is(err, ErrPermission) {
		t.Fatalf("clerk cannot order: %v", err)
	}
	po, _, err := svc.CreatePurchaseOrder("buyer1", req.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PostGoodsReceipt("whclerk1", "PO-NONE", 10, 1, false); err == nil {
		t.Fatal("missing PO")
	}
	cfg.Company("1000").AutoPostLimit = "x"
	if _, err := svc.PostGoodsReceipt("whclerk1", po.ID, 10, 1, false); err == nil {
		t.Fatal("unusable settings")
	}
}

func TestMasterDataEdges(t *testing.T) {
	svc, cfg := setup(t)
	if _, err := svc.RequestUnblock("buyer1", "100001", "x"); !errors.Is(err, ErrPermission) {
		t.Fatal("buyer cannot request an unblock")
	}
	// An approval route without a role cannot be created.
	cfg.Payments.VendorApproverRole = ""
	if _, err := svc.RequestBankChange("mdclerk1", "100001", "GB82WEST12345698765432", "x"); err == nil {
		t.Fatal("empty approver role")
	}
	cfg.Payments.VendorApproverRole = "vendor_master_approver"
	ch, err := svc.RequestVendor("mdclerk1", core.Vendor{ID: "400001", Name: "Late Co", TaxID: "9", APAccount: map[string]string{"1000": "211000"}})
	if err != nil {
		t.Fatal(err)
	}
	// The vendor appears by another route before approval: applying fails and the decision is undone.
	svc.Store.Vendors["400001"] = &core.Vendor{ID: "400001", Name: "Late Co", APAccount: map[string]string{"1000": "211000"}}
	if _, err := svc.DecideMasterData("mdmgr1", ch.ID, true, ""); err == nil {
		t.Fatal("applying over an existing vendor must fail")
	}
	if wf := svc.Store.Workflows[ch.WorkflowID]; wf.Status != workflow.Pending || svc.Store.MDChanges[ch.ID].Status != core.ChangePending {
		t.Fatal("failed apply must leave the change pending")
	}
	delete(svc.Store.Vendors, "400001")
	svc.DecideMasterData("mdmgr1", ch.ID, true, "")
	if _, err := svc.DecideMasterData("mdmgr1", ch.ID, true, ""); err == nil {
		t.Fatal("deciding an applied change")
	}
}

func TestPaymentEdges(t *testing.T) {
	svc, cfg := setup(t)
	if _, err := svc.ProposePaymentRun("treas1", "1000", "next week"); err == nil {
		t.Fatal("bad run date")
	}
	cfg.Payments.ReleaseApprovals = 0
	if _, err := svc.ProposePaymentRun("treas1", "1000", "2027-06-30"); err == nil {
		t.Fatal("route with zero approvals")
	}
	if len(svc.Store.PaymentRuns) != 0 {
		t.Fatal("an unroutable proposal must be discarded so its items stay payable")
	}
	cfg.Payments.ReleaseApprovals = 1
	run, err := svc.ProposePaymentRun("treas1", "1000", "2027-06-30")
	if err != nil {
		t.Fatal(err)
	}
	// A broken house bank: the bank file fails before anything is posted.
	cfg.Company("1000").HouseBank.IBAN = "GB00BROKEN"
	journal := len(svc.Store.Journal)
	if _, err := svc.DecidePaymentRun("treasmgr1", run.ID, true, ""); err == nil || !strings.Contains(err.Error(), "IBAN") {
		t.Fatalf("bad house bank: %v", err)
	}
	if len(svc.Store.Journal) != journal || run.Status != core.RunProposed || svc.Store.Workflows[run.WorkflowID].Status != workflow.Pending {
		t.Fatal("nothing may be posted when the bank file cannot be built")
	}
	cfg.Company("1000").HouseBank.IBAN = "GB33BUKB20201555555555"
	cfg.Company("1000").AutoPostLimit = "x"
	if _, err := svc.DecidePaymentRun("treasmgr1", run.ID, true, ""); err == nil {
		t.Fatal("unusable settings")
	}
	cfg.Company("1000").AutoPostLimit = "50000.00"
	if _, err := svc.DecidePaymentRun("treasmgr1", run.ID, true, ""); err != nil {
		t.Fatalf("release after the fix: %v", err)
	}
}

func TestWithoutAuditLog(t *testing.T) {
	svc, _ := setup(t)
	svc.Audit = nil
	if err := svc.BlockVendor("mdclerk1", "100002", "x"); err != nil {
		t.Fatal(err)
	}
	var _ = governance.ActVendorChange
}
