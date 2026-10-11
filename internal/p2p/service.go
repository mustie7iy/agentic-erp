// Package p2p is the procure-to-pay service: requisitions, contracts,
// purchase orders, goods receipts, supplier master changes and payment runs.
// Every operation checks the caller's permissions and company access in the
// active configuration, routes approvals through the workflow engine, and is
// recorded in the audit log.
package p2p

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/bankfile"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/governance"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/workflow"
)

type Service struct {
	Store  *core.Store
	Config func() (*config.Config, error)
	Audit  *governance.AuditLog
	Now    func() time.Time
}

var ErrPermission = errors.New("permission denied")

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) today() string { return s.now().Format(time.DateOnly) }

// authorize resolves the caller and checks a permission and company access.
func (s *Service) authorize(userID, perm, company string) (*config.Config, governance.Identity, error) {
	cfg, err := s.Config()
	if err != nil {
		return nil, governance.Identity{}, err
	}
	id, err := cfg.Identity(userID)
	if err != nil {
		return nil, id, err
	}
	if !id.Can(perm) {
		return nil, id, fmt.Errorf("%w: %s lacks %s", ErrPermission, userID, perm)
	}
	// Defense in depth: activation already rejects conflicting identities,
	// but a conflict is refused here too, whatever the configuration says.
	if err := (governance.Policy{SoDConflicts: cfg.SoDConflicts}).ValidateIdentity(id); err != nil {
		return nil, id, fmt.Errorf("%w: %v", ErrPermission, err)
	}
	if company != "" && !cfg.CanAccess(userID, company) {
		return nil, id, fmt.Errorf("%w: %s has no access to company %s", ErrPermission, userID, company)
	}
	return cfg, id, nil
}

func (s *Service) audit(actor, action, subject, outcome string, detail map[string]any) {
	if s.Audit == nil {
		return
	}
	if _, err := s.Audit.Append(actor, action, subject, outcome, detail); err != nil {
		panic(fmt.Sprintf("audit log write failed: %v", err))
	}
}

// --- Requisitions -----------------------------------------------------------

type RequisitionInput struct {
	CompanyCode string
	VendorID    string
	ContractID  string
	NeededBy    string
	Lines       []core.RequisitionLine
}

// CreateRequisition submits a requisition; its approval route follows the
// configured tiers for its value.
func (s *Service) CreateRequisition(userID string, in RequisitionInput) (*core.Requisition, error) {
	cfg, id, err := s.authorize(userID, governance.ActRequisition, in.CompanyCode)
	if err != nil {
		return nil, err
	}
	co := cfg.Company(in.CompanyCode)
	if co == nil {
		return nil, fmt.Errorf("company %s is not configured", in.CompanyCode)
	}
	if len(in.Lines) == 0 {
		return nil, errors.New("a requisition needs at least one line")
	}
	s.Store.Mu.Lock()
	defer s.Store.Mu.Unlock()
	if in.ContractID != "" {
		c := s.Store.Contracts[in.ContractID]
		if c == nil || c.CompanyCode != in.CompanyCode {
			return nil, fmt.Errorf("contract %s not found in company %s", in.ContractID, in.CompanyCode)
		}
		if in.VendorID == "" {
			in.VendorID = c.VendorID
		}
	}
	req := &core.Requisition{
		ID: s.Store.NextID("PR"), CompanyCode: in.CompanyCode, Requester: id.ID, VendorID: in.VendorID,
		ContractID: in.ContractID, Currency: co.Currency, NeededBy: in.NeededBy, Status: core.ReqPending, CreatedAt: s.today(),
	}
	for i, l := range in.Lines {
		if l.Quantity <= 0 || l.UnitPrice < 0 || strings.TrimSpace(l.Description) == "" {
			return nil, fmt.Errorf("line %d needs a description, a positive quantity and a non-negative price", i+1)
		}
		if s.Store.Account(in.CompanyCode, l.GLAccount) == nil && in.ContractID == "" {
			return nil, fmt.Errorf("line %d: G/L account %s does not exist in company %s", i+1, l.GLAccount, in.CompanyCode)
		}
		l.LineNo = i + 1
		req.Lines = append(req.Lines, l)
	}
	steps, err := cfg.RequisitionSteps(req.Total())
	if err != nil {
		return nil, err
	}
	wf, err := workflow.New(s.Store.NextID("WF"), "requisition", req.ID, fmt.Sprintf("Requisition %s (%s %s)", req.ID, req.Total(), req.Currency), id.ID, req.Total(), steps, s.now())
	if err != nil {
		return nil, err
	}
	req.WorkflowID = wf.ID
	if wf.Status == workflow.Approved {
		req.Status = core.ReqApproved
	}
	s.Store.Workflows[wf.ID] = wf
	s.Store.Requisitions[req.ID] = req
	s.audit(id.ID, governance.ActRequisition, req.ID, string(req.Status), map[string]any{"total": req.Total(), "company": req.CompanyCode})
	return req, nil
}

// DecideRequisition approves or rejects the requisition's current step.
func (s *Service) DecideRequisition(userID, reqID string, approve bool, comment string) (*core.Requisition, error) {
	cfg, err := s.Config()
	if err != nil {
		return nil, err
	}
	actor, err := cfg.Actor(userID)
	if err != nil {
		return nil, err
	}
	s.Store.Mu.Lock()
	defer s.Store.Mu.Unlock()
	req := s.Store.Requisitions[reqID]
	if req == nil {
		return nil, fmt.Errorf("requisition %s not found", reqID)
	}
	if !cfg.CanAccess(userID, req.CompanyCode) {
		return nil, fmt.Errorf("%w: %s has no access to company %s", ErrPermission, userID, req.CompanyCode)
	}
	wf := s.Store.Workflows[req.WorkflowID]
	if approve {
		err = wf.Approve(actor, comment, s.now())
	} else {
		err = wf.Reject(actor, comment, s.now())
	}
	if err != nil {
		return nil, err
	}
	switch wf.Status {
	case workflow.Approved:
		req.Status = core.ReqApproved
	case workflow.Rejected:
		req.Status = core.ReqRejected
	}
	s.audit(actor.ID, governance.ActRequisitionApp, req.ID, string(req.Status), map[string]any{"comment": comment, "approve": approve})
	return req, nil
}

// --- Contracts and purchase orders -----------------------------------------

type ContractInput struct {
	CompanyCode string
	VendorID    string
	ValidFrom   string
	ValidTo     string
	TargetValue money.Minor
	Items       []core.ContractItem
}

func (s *Service) CreateContract(userID string, in ContractInput) (*core.Contract, error) {
	cfg, id, err := s.authorize(userID, governance.ActContractManage, in.CompanyCode)
	if err != nil {
		return nil, err
	}
	co := cfg.Company(in.CompanyCode)
	if co == nil {
		return nil, fmt.Errorf("company %s is not configured", in.CompanyCode)
	}
	if in.ValidFrom == "" || in.ValidTo < in.ValidFrom {
		return nil, errors.New("contract needs a validity period with valid_to on or after valid_from")
	}
	if in.TargetValue <= 0 || len(in.Items) == 0 {
		return nil, errors.New("contract needs a positive target value and at least one item")
	}
	s.Store.Mu.Lock()
	defer s.Store.Mu.Unlock()
	v := s.Store.Vendors[in.VendorID]
	if v == nil || v.APAccount[in.CompanyCode] == "" {
		return nil, fmt.Errorf("vendor %s is not set up for company %s", in.VendorID, in.CompanyCode)
	}
	seen := map[string]bool{}
	for i, it := range in.Items {
		if it.Material == "" || seen[it.Material] || it.UnitPrice <= 0 {
			return nil, fmt.Errorf("item %d needs a unique material and a positive price", i+1)
		}
		seen[it.Material] = true
		if s.Store.Account(in.CompanyCode, it.GLAccount) == nil {
			return nil, fmt.Errorf("item %d: G/L account %s does not exist", i+1, it.GLAccount)
		}
		if it.PriceUnit <= 0 {
			in.Items[i].PriceUnit = 1
		}
	}
	c := &core.Contract{
		ID: s.Store.NextID("CT"), CompanyCode: in.CompanyCode, VendorID: in.VendorID, Currency: co.Currency,
		ValidFrom: in.ValidFrom, ValidTo: in.ValidTo, TargetValue: in.TargetValue, Items: in.Items, CreatedBy: id.ID,
	}
	s.Store.Contracts[c.ID] = c
	s.audit(id.ID, governance.ActContractManage, c.ID, "created", map[string]any{"vendor": c.VendorID, "target": c.TargetValue})
	return c, nil
}

// CreatePurchaseOrder orders an approved requisition and sends the PO to the
// vendor through the configured output channel.
func (s *Service) CreatePurchaseOrder(userID, reqID, vendorID string) (*core.PurchaseOrder, *core.OutputDocument, error) {
	cfg, err := s.Config()
	if err != nil {
		return nil, nil, err
	}
	s.Store.Mu.Lock()
	req := s.Store.Requisitions[reqID]
	company := ""
	if req != nil {
		company = req.CompanyCode
	}
	s.Store.Mu.Unlock()
	if req == nil {
		return nil, nil, fmt.Errorf("requisition %s not found", reqID)
	}
	_, id, err := s.authorize(userID, governance.ActPOCreate, company)
	if err != nil {
		return nil, nil, err
	}
	s.Store.Mu.Lock()
	defer s.Store.Mu.Unlock()
	po, err := s.Store.CreatePurchaseOrder(reqID, vendorID, s.today(), id.ID)
	if err != nil {
		return nil, nil, err
	}
	out := renderPO(po, s.Store.Vendors[po.VendorID], cfg.Output.PurchaseOrderChannel)
	out.ID, out.CreatedAt = s.Store.NextID("OUT"), s.today()
	s.Store.Outputs[out.ID] = out
	s.audit(id.ID, governance.ActPOCreate, po.ID, "created", map[string]any{"requisition": reqID, "vendor": po.VendorID, "output": out.ID})
	return po, out, nil
}

// PostGoodsReceipt records a receipt or reversal for a PO line.
func (s *Service) PostGoodsReceipt(userID, poID string, lineNo int, qty money.Qty, reverse bool) (*core.GoodsReceipt, error) {
	s.Store.Mu.Lock()
	po := s.Store.PurchaseOrders[poID]
	s.Store.Mu.Unlock()
	if po == nil {
		return nil, fmt.Errorf("purchase order %s not found", poID)
	}
	cfg, id, err := s.authorize(userID, governance.ActGoodsReceipt, po.CompanyCode)
	if err != nil {
		return nil, err
	}
	set, err := cfg.Settings(po.CompanyCode)
	if err != nil {
		return nil, err
	}
	s.Store.Mu.Lock()
	defer s.Store.Mu.Unlock()
	gr, err := s.Store.PostGoodsReceipt(poID, lineNo, qty, reverse, set.OverDelivery, s.today(), id.ID)
	if err != nil {
		return nil, err
	}
	s.audit(id.ID, governance.ActGoodsReceipt, gr.ID, gr.MovementType, map[string]any{"po": poID, "line": lineNo, "quantity": gr.Quantity.String()})
	return gr, nil
}

// --- Supplier master data ---------------------------------------------------

// RequestVendor proposes a new vendor; it is created only after approval.
func (s *Service) RequestVendor(userID string, v core.Vendor) (*core.MasterDataChange, error) {
	cfg, id, err := s.authorize(userID, governance.ActVendorCreate, "")
	if err != nil {
		return nil, err
	}
	s.Store.Mu.Lock()
	defer s.Store.Mu.Unlock()
	if err := s.Store.ValidateNewVendor(&v); err != nil {
		return nil, err
	}
	for _, c := range s.Store.MDChanges {
		if c.Kind == core.VendorCreate && c.Status == core.ChangePending && c.VendorID == v.ID {
			return nil, fmt.Errorf("vendor %s already has a pending creation request (%s)", v.ID, c.ID)
		}
	}
	v.BankChangedAt = ""
	return s.newChange(cfg, id, core.MasterDataChange{Kind: core.VendorCreate, VendorID: v.ID, Proposed: &v, Warnings: s.Store.VendorWarnings(&v)})
}

// RequestBankChange proposes new bank details. Bank changes are the main
// payment-fraud vector: they always need a different person to approve, and
// payments to the vendor are held while one is pending.
func (s *Service) RequestBankChange(userID, vendorID, newBank, reason string) (*core.MasterDataChange, error) {
	cfg, id, err := s.authorize(userID, governance.ActVendorBank, "")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(newBank) == "" || strings.TrimSpace(reason) == "" {
		return nil, errors.New("a bank change needs the new account and a reason")
	}
	s.Store.Mu.Lock()
	defer s.Store.Mu.Unlock()
	v := s.Store.Vendors[vendorID]
	if v == nil {
		return nil, fmt.Errorf("vendor %s not found", vendorID)
	}
	if v.BankAccount == newBank {
		return nil, errors.New("the new bank account is the same as the current one")
	}
	return s.newChange(cfg, id, core.MasterDataChange{Kind: core.VendorBankChange, VendorID: vendorID, NewBankAccount: newBank, Reason: reason})
}

// BlockVendor blocks a vendor immediately (a safety action needs no
// approval); unblocking goes through approval.
func (s *Service) BlockVendor(userID, vendorID, reason string) error {
	_, id, err := s.authorize(userID, governance.ActVendorChange, "")
	if err != nil {
		return err
	}
	s.Store.Mu.Lock()
	defer s.Store.Mu.Unlock()
	v := s.Store.Vendors[vendorID]
	if v == nil {
		return fmt.Errorf("vendor %s not found", vendorID)
	}
	if reason == "" {
		return errors.New("blocking a vendor needs a reason")
	}
	v.Blocked = true
	s.audit(id.ID, "vendor.block", vendorID, "blocked", map[string]any{"reason": reason})
	return nil
}

func (s *Service) RequestUnblock(userID, vendorID, reason string) (*core.MasterDataChange, error) {
	cfg, id, err := s.authorize(userID, governance.ActVendorChange, "")
	if err != nil {
		return nil, err
	}
	s.Store.Mu.Lock()
	defer s.Store.Mu.Unlock()
	v := s.Store.Vendors[vendorID]
	if v == nil || !v.Blocked {
		return nil, fmt.Errorf("vendor %s is not blocked", vendorID)
	}
	return s.newChange(cfg, id, core.MasterDataChange{Kind: core.VendorUnblock, VendorID: vendorID, Reason: reason})
}

// newChange stores a change request with its approval workflow. Caller holds the lock.
func (s *Service) newChange(cfg *config.Config, id governance.Identity, c core.MasterDataChange) (*core.MasterDataChange, error) {
	c.ID, c.Requester, c.Status, c.CreatedAt = s.Store.NextID("MDC"), id.ID, core.ChangePending, s.today()
	steps := []workflow.Step{{Role: cfg.Payments.VendorApproverRole, Required: 1}}
	wf, err := workflow.New(s.Store.NextID("WF"), "vendor_change", c.ID, string(c.Kind)+" "+c.VendorID, id.ID, 0, steps, s.now())
	if err != nil {
		return nil, err
	}
	c.WorkflowID = wf.ID
	s.Store.Workflows[wf.ID] = wf
	s.Store.MDChanges[c.ID] = &c
	s.audit(id.ID, "vendor.change_request", c.ID, string(c.Kind), map[string]any{"vendor": c.VendorID, "warnings": c.Warnings})
	return &c, nil
}

// DecideMasterData approves or rejects a supplier change; approval applies it.
func (s *Service) DecideMasterData(userID, changeID string, approve bool, comment string) (*core.MasterDataChange, error) {
	cfg, ident, err := s.authorize(userID, governance.ActVendorApprove, "")
	if err != nil {
		return nil, err
	}
	actor, _ := cfg.Actor(userID)
	s.Store.Mu.Lock()
	defer s.Store.Mu.Unlock()
	c := s.Store.MDChanges[changeID]
	if c == nil {
		return nil, fmt.Errorf("change %s not found", changeID)
	}
	wf := s.Store.Workflows[c.WorkflowID]
	saved := snapshot(wf)
	if approve {
		err = wf.Approve(actor, comment, s.now())
	} else {
		err = wf.Reject(actor, comment, s.now())
	}
	if err != nil {
		return nil, err
	}
	switch wf.Status {
	case workflow.Rejected:
		c.Status = core.ChangeRejected
	case workflow.Approved:
		if err := s.apply(c); err != nil {
			s.Store.Workflows[c.WorkflowID] = saved // the decision did not take effect
			return nil, err
		}
		c.Status, c.AppliedAt = core.ChangeApplied, s.today()
	}
	s.audit(ident.ID, governance.ActVendorApprove, c.ID, string(c.Status), map[string]any{"kind": c.Kind, "vendor": c.VendorID, "comment": comment})
	return c, nil
}

func (s *Service) apply(c *core.MasterDataChange) error {
	switch c.Kind {
	case core.VendorCreate:
		if err := s.Store.ValidateNewVendor(c.Proposed); err != nil {
			return err
		}
		v := *c.Proposed
		s.Store.Vendors[v.ID] = &v
	case core.VendorBankChange:
		v := s.Store.Vendors[c.VendorID]
		v.BankAccount, v.BankChangedAt = c.NewBankAccount, s.today()
	case core.VendorUnblock:
		s.Store.Vendors[c.VendorID].Blocked = false
	}
	return nil
}

// --- Payments ---------------------------------------------------------------

// ProposePaymentRun builds a payment proposal and routes it for release.
func (s *Service) ProposePaymentRun(userID, company, runDate string) (*core.PaymentRun, error) {
	cfg, id, err := s.authorize(userID, governance.ActPaymentPropose, company)
	if err != nil {
		return nil, err
	}
	s.Store.Mu.Lock()
	defer s.Store.Mu.Unlock()
	run, err := s.Store.ProposePayments(company, runDate, cfg.Payments.BankChangeHoldDays, id.ID)
	if err != nil {
		return nil, err
	}
	if len(run.Payments) == 0 {
		delete(s.Store.PaymentRuns, run.ID)
		return run, errors.New("nothing is due for payment")
	}
	steps := []workflow.Step{{Role: cfg.Payments.ReleaseRole, Required: cfg.Payments.ReleaseApprovals}}
	wf, err := workflow.New(s.Store.NextID("WF"), "payment_run", run.ID, fmt.Sprintf("Payment run %s: %s", run.ID, run.Total), id.ID, run.Total, steps, s.now())
	if err != nil {
		delete(s.Store.PaymentRuns, run.ID) // an unroutable proposal must not keep items reserved
		return nil, err
	}
	run.WorkflowID = wf.ID
	s.Store.Workflows[wf.ID] = wf
	s.audit(id.ID, governance.ActPaymentPropose, run.ID, "proposed", map[string]any{"total": run.Total, "vendors": len(run.Payments), "excluded": len(run.Excluded)})
	return run, nil
}

// DecidePaymentRun records a release decision. When the release is complete
// the run is posted and the bank file is generated.
func (s *Service) DecidePaymentRun(userID, runID string, approve bool, comment string) (*core.PaymentRun, error) {
	s.Store.Mu.Lock()
	run := s.Store.PaymentRuns[runID]
	s.Store.Mu.Unlock()
	if run == nil {
		return nil, fmt.Errorf("payment run %s not found", runID)
	}
	cfg, ident, err := s.authorize(userID, governance.ActPaymentRun, run.CompanyCode)
	if err != nil {
		return nil, err
	}
	actor, _ := cfg.Actor(userID)
	set, err := cfg.Settings(run.CompanyCode)
	if err != nil {
		return nil, err
	}
	s.Store.Mu.Lock()
	defer s.Store.Mu.Unlock()
	wf := s.Store.Workflows[run.WorkflowID]
	saved := snapshot(wf)
	if approve {
		err = wf.Approve(actor, comment, s.now())
	} else {
		err = wf.Reject(actor, comment, s.now())
	}
	if err != nil {
		return nil, err
	}
	switch wf.Status {
	case workflow.Rejected:
		run.Status = core.RunRejected
	case workflow.Approved:
		// Build the bank file first: if it cannot be produced, nothing is
		// posted and the release can be retried once the cause is fixed.
		file, err := s.bankFile(run, set, cfg.Company(run.CompanyCode).Name, s.today())
		if err == nil {
			_, err = s.Store.ExecutePaymentRun(run.ID, set.Bank, s.today())
		}
		if err != nil {
			s.Store.Workflows[run.WorkflowID] = saved
			return nil, err
		}
		run.BankFile = string(file)
	}
	s.audit(ident.ID, governance.ActPaymentRun, run.ID, string(run.Status), map[string]any{"comment": comment, "total": run.Total})
	return run, nil
}

func (s *Service) bankFile(run *core.PaymentRun, set config.Settings, companyName, executionDate string) ([]byte, error) {
	b := bankfile.Batch{
		MessageID: run.ID, InitiatorName: companyName, DebtorName: companyName,
		DebtorIBAN: set.HouseBank.IBAN, DebtorBIC: set.HouseBank.BIC, Currency: set.Currency, Decimals: 2,
		ExecutionDate: executionDate, Created: s.now(),
	}
	for _, p := range run.Payments {
		refs := make([]string, 0, len(p.Items))
		for _, it := range p.Items {
			refs = append(refs, it.Reference)
		}
		acct := p.BankAccount
		if i := strings.Index(acct, ":"); i >= 0 {
			acct = acct[i+1:] // migrated accounts carry a country prefix
		}
		b.Payments = append(b.Payments, bankfile.Payment{
			EndToEndID: run.ID + "-" + p.VendorID, CreditorName: p.VendorName, CreditorAccount: acct,
			Amount: p.Total, Remittance: strings.Join(refs, " "),
		})
	}
	return bankfile.Pain001(b)
}

// --- Output -------------------------------------------------------------------

// renderPO produces the purchase order message for the channel.
func renderPO(po *core.PurchaseOrder, v *core.Vendor, channel string) *core.OutputDocument {
	var b strings.Builder
	var total money.Minor
	if channel == "edi" {
		// ANSI X12 850 purchase order (transaction set only; the VAN adds envelopes).
		fmt.Fprintf(&b, "ST*850*0001~\nBEG*00*SA*%s**%s~\nCUR*BY*%s~\nN1*SE*%s*92*%s~\n", po.ID, strings.ReplaceAll(po.OrderDate, "-", ""), po.Currency, v.Name, v.ID)
		for _, l := range po.Lines {
			total += money.Extend(l.Quantity, l.UnitPrice, l.PriceUnit)
			fmt.Fprintf(&b, "PO1*%d*%s*EA*%s**VP*%s~\n", l.LineNo, l.Quantity, l.UnitPrice.Decimal(), l.Material)
		}
		segments := 5 + len(po.Lines)
		fmt.Fprintf(&b, "CTT*%d~\nSE*%d*0001~\n", len(po.Lines), segments+1)
	} else {
		fmt.Fprintf(&b, "PURCHASE ORDER %s\nDate: %s\nTo: %s (%s)\nCurrency: %s\n\n", po.ID, po.OrderDate, v.Name, v.ID, po.Currency)
		fmt.Fprintf(&b, "%-6s %-12s %-30s %10s %14s %14s\n", "Line", "Material", "Description", "Qty", "Unit price", "Amount")
		for _, l := range po.Lines {
			amount := money.Extend(l.Quantity, l.UnitPrice, l.PriceUnit)
			total += amount
			fmt.Fprintf(&b, "%-6d %-12s %-30.30s %10s %14s %14s\n", l.LineNo, l.Material, l.Description, l.Quantity, l.UnitPrice, amount)
		}
		fmt.Fprintf(&b, "\nTotal: %s %s\nPlease quote %s on your invoice.\n", total, po.Currency, po.ID)
	}
	return &core.OutputDocument{
		Type: "purchase_order", Reference: po.ID, Channel: channel, Recipient: "vendor:" + v.ID,
		Subject: fmt.Sprintf("Purchase order %s from %s", po.ID, po.CompanyCode), Body: b.String(),
	}
}

// snapshot deep-copies a workflow request so a decision can be undone when
// applying it fails.
func snapshot(r *workflow.Request) *workflow.Request {
	c := *r
	c.Steps = make([]workflow.Step, len(r.Steps))
	for i, st := range r.Steps {
		st.ApprovedBy = append([]string(nil), st.ApprovedBy...)
		c.Steps[i] = st
	}
	c.History = append([]workflow.Event(nil), r.History...)
	return &c
}
