package nfr

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/governance"
	"github.com/mustie7iy/agentic-erp/internal/p2p"
	"github.com/mustie7iy/agentic-erp/internal/testkit"
	"github.com/mustie7iy/agentic-erp/internal/workflow"
)

// Every procure-to-pay operation is refused for every configured user who
// lacks its permission (an exhaustive authorization matrix).
func TestAuthorizationMatrix(t *testing.T) {
	svc, cfg := p2pService(t)
	ops := []struct {
		perm string
		call func(user string) error
	}{
		{governance.ActRequisition, func(u string) error {
			_, err := svc.CreateRequisition(u, p2p.RequisitionInput{CompanyCode: "1000", Lines: []core.RequisitionLine{{Description: "x", Quantity: 1000, UnitPrice: 1, GLAccount: "520000"}}})
			return err
		}},
		{governance.ActContractManage, func(u string) error {
			_, err := svc.CreateContract(u, p2p.ContractInput{CompanyCode: "1000", VendorID: "100001", ValidFrom: "2026-01-01", ValidTo: "2026-12-31", TargetValue: 1,
				Items: []core.ContractItem{{Material: "M", UnitPrice: 1, GLAccount: "520000"}}})
			return err
		}},
		{governance.ActGoodsReceipt, func(u string) error {
			_, err := svc.PostGoodsReceipt(u, core.SortedKeys(svc.Store.PurchaseOrders)[0], 10, 1, false)
			return err
		}},
		{governance.ActVendorCreate, func(u string) error {
			_, err := svc.RequestVendor(u, core.Vendor{ID: "SEC-" + u, Name: "Sec " + u, APAccount: map[string]string{"1000": "211000"}})
			return err
		}},
		{governance.ActVendorBank, func(u string) error {
			_, err := svc.RequestBankChange(u, "100001", "GB82WEST12345698765432", "x")
			return err
		}},
		{governance.ActVendorChange, func(u string) error { return svc.BlockVendor(u, "100002", "x") }},
		{governance.ActPaymentPropose, func(u string) error {
			_, err := svc.ProposePaymentRun(u, "1000", "2027-06-30")
			return err
		}},
	}
	checked := 0
	for _, op := range ops {
		for _, u := range cfg.Users {
			if slices.Contains(cfg.UserPermissions(u.ID), op.perm) {
				continue
			}
			checked++
			if err := op.call(u.ID); !errors.Is(err, p2p.ErrPermission) {
				t.Errorf("%s without %s: want permission denied, got %v", u.ID, op.perm, err)
			}
		}
	}
	if checked < 50 {
		t.Fatalf("matrix too small: %d checks", checked)
	}
	// Approval-type decisions also refuse users without the permission.
	ch, _ := svc.RequestVendor("mdclerk1", core.Vendor{ID: "SEC-V", Name: "Sec V", TaxID: "1", APAccount: map[string]string{"1000": "211000"}})
	for _, u := range cfg.Users {
		if slices.Contains(cfg.UserPermissions(u.ID), governance.ActVendorApprove) {
			continue
		}
		if _, err := svc.DecideMasterData(u.ID, ch.ID, true, ""); !errors.Is(err, p2p.ErrPermission) {
			t.Errorf("%s approved a vendor change: %v", u.ID, err)
		}
	}
}

// Agents never approve anything: not requisitions, vendor changes, payment
// runs or configuration, whatever roles a configuration tries to give them.
func TestAgentsCannotApprove(t *testing.T) {
	svc, cfg := p2pService(t)
	req, _ := svc.CreateRequisition("buyer1", p2p.RequisitionInput{CompanyCode: "1000", Lines: []core.RequisitionLine{{Description: "x", Quantity: 1000, UnitPrice: 1, GLAccount: "520000"}}})
	if _, err := svc.DecideRequisition("agent:configuration", req.ID, true, ""); !errors.Is(err, workflow.ErrNotHuman) {
		t.Fatalf("agent approved a requisition: %v", err)
	}
	ch, _ := svc.RequestVendor("mdclerk1", core.Vendor{ID: "AG-V", Name: "AG", TaxID: "2", APAccount: map[string]string{"1000": "211000"}})
	if _, err := svc.DecideMasterData("agent:configuration", ch.ID, true, ""); err == nil {
		t.Fatal("agent approved a vendor change")
	}
	run, _ := svc.ProposePaymentRun("treas1", "1000", "2027-06-30")
	if _, err := svc.DecidePaymentRun("agent:ap-exceptions", run.ID, true, ""); err == nil {
		t.Fatal("agent released a payment run")
	}
	// Configuration that grants an agent approval rights is rejected outright.
	cfg.Agents[0].Permissions = append(cfg.Agents[0].Permissions, governance.ActPaymentRun)
	fs := config.Validate(cfg)
	if !slices.ContainsFunc(fs, func(f config.Finding) bool {
		return strings.Contains(f.Message, "agents may not hold approval permission")
	}) {
		t.Fatal("agent approval permission must fail validation")
	}
	repo := testkit.Repo(t)
	c := testkit.Config(t)
	c.PaymentTerms[0].NetDays = 21
	cr, _ := repo.Propose("agent:configuration", "terms", "", testkit.YAML(t, c))
	if _, err := repo.Approve(cr.ID, "agent:configuration", ""); err == nil {
		t.Fatal("agent approved configuration")
	}
}

// A segregation-of-duties conflict is refused at run time even if a
// configuration containing it were loaded without validation.
func TestSoDEnforcedAtRuntime(t *testing.T) {
	svc, cfg := p2pService(t)
	cfg.User("treas1").Roles = append(cfg.User("treas1").Roles, "Z_TREASURY_MGR") // propose + release
	if !config.HasErrors(config.Validate(cfg)) {
		t.Fatal("validation must flag the conflict")
	}
	if _, err := svc.ProposePaymentRun("treas1", "1000", "2027-06-30"); !errors.Is(err, p2p.ErrPermission) || !strings.Contains(err.Error(), "segregation of duties") {
		t.Fatalf("conflicting identity must be refused at run time: %v", err)
	}
}

// Tampering with the audit trail or a configuration version is detected.
func TestTamperDetection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	audit, _ := governance.NewAuditLog(path)
	cfg := testkit.Config(t)
	svc := &p2p.Service{Store: testkit.Store(t), Config: func() (*config.Config, error) { return cfg, nil }, Audit: audit}
	svc.BlockVendor("mdclerk1", "100002", "fraud check")
	svc.RequestBankChange("mdclerk1", "100001", "GB82WEST12345698765432", "letter")
	b, _ := os.ReadFile(path)
	os.WriteFile(path, []byte(strings.Replace(string(b), "fraud check", "routine", 1)), 0o644)
	if _, err := governance.NewAuditLog(path); err == nil {
		t.Fatal("edited audit entry must be detected")
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	os.WriteFile(path, []byte(lines[1]+"\n"), 0o644) // first entry deleted
	if _, err := governance.NewAuditLog(path); err == nil {
		t.Fatal("deleted audit entry must be detected")
	}
}

// Bank details never reach agents in full, and agents' tool surfaces stay
// read-and-propose only (checked per agent in their own packages).
func TestBankDetailsNotInAgentContext(t *testing.T) {
	// Covered in internal/agent (masking) and the agent packages' tool-surface tests;
	// here we assert the invariant the masking relies on: every vendor bank
	// account is longer than the four characters left visible.
	s := testkit.Store(t)
	for _, v := range s.Vendors {
		if v.BankAccount != "" && len(v.BankAccount) <= 4 {
			t.Fatalf("vendor %s bank account too short to mask", v.ID)
		}
	}
}
