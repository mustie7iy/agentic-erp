// Package agent is the System of Action: deterministic pipelines handle the
// routine path and Claude agents handle exceptions. Every action goes through
// Service, which enforces policy and writes the audit trail; agents have no
// other way to change data.
package agent

import (
	"fmt"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/governance"
)

type Service struct {
	Store  *core.Store
	Policy governance.Policy
	Audit  *governance.AuditLog
	// InputTaxAccount per company code.
	InputTaxAccount map[string]string
	Tolerance       core.MatchTolerance
	Today           func() string
	// Config, when set, supplies per-company tolerances, tax accounts and
	// auto-post limits from the active configuration instead of the defaults.
	Config *config.Config
}

// rules returns the matching tolerance, input tax account and policy for a company.
func (svc *Service) rules(company string) (core.MatchTolerance, string, governance.Policy, error) {
	if svc.Config == nil {
		return svc.Tolerance, svc.InputTaxAccount[company], svc.Policy, nil
	}
	set, err := svc.Config.Settings(company)
	if err != nil {
		return core.MatchTolerance{}, "", governance.Policy{}, err
	}
	pol, err := svc.Config.Policy(company)
	return set.Tolerance, set.InputTax, pol, err
}

func NewService(s *core.Store, audit *governance.AuditLog) *Service {
	return &Service{
		Store:           s,
		Policy:          governance.DefaultPolicy(),
		Audit:           audit,
		InputTaxAccount: map[string]string{"1000": "140000", "2000": "140000"},
		Tolerance:       core.DefaultTolerance,
		Today:           func() string { return time.Now().Format(time.DateOnly) },
	}
}

// Match runs the three-way match under the store lock.
func (svc *Service) Match(invoiceID string) (core.MatchResult, error) {
	svc.Store.Mu.Lock()
	defer svc.Store.Mu.Unlock()
	inv := svc.Store.Invoices[invoiceID]
	if inv == nil {
		return core.MatchResult{}, fmt.Errorf("invoice %s not found", invoiceID)
	}
	tol, _, _, err := svc.rules(inv.CompanyCode)
	if err != nil {
		return core.MatchResult{}, err
	}
	return svc.Store.ThreeWayMatch(inv, tol), nil
}

// PostResult reports what happened to a posting request.
type PostResult struct {
	Outcome        governance.Outcome `json:"outcome"`
	Reason         string             `json:"reason"`
	JournalEntryID string             `json:"journalEntryId,omitempty"`
	TaskID         string             `json:"taskId,omitempty"`
}

// PostInvoice posts an invoice if policy allows. Non-human identities may
// only post invoices with a clean three-way match; exceptions always reach a
// person. Escalations create a review task instead of posting.
func (svc *Service) PostInvoice(id governance.Identity, invoiceID, note string) (PostResult, error) {
	svc.Store.Mu.Lock()
	defer svc.Store.Mu.Unlock()
	inv := svc.Store.Invoices[invoiceID]
	if inv == nil {
		return PostResult{}, fmt.Errorf("invoice %s not found", invoiceID)
	}
	if !open(inv) {
		return PostResult{}, fmt.Errorf("invoice %s is %s and cannot be posted", invoiceID, inv.Status)
	}
	tol, inputTax, policy, err := svc.rules(inv.CompanyCode)
	if err != nil {
		return PostResult{}, err
	}
	match := svc.Store.ThreeWayMatch(inv, tol)
	detail := map[string]any{"gross": inv.GrossAmount, "match": match.Summary(), "note": note}

	var res PostResult
	switch {
	case !match.Clean && id.Kind != "human":
		res = PostResult{Outcome: governance.Escalate, Reason: "match exceptions need human review: " + match.Summary()}
	default:
		d := policy.Decide(id, governance.ActInvoicePost, inv.GrossAmount)
		res = PostResult{Outcome: d.Outcome, Reason: d.Reason}
	}

	switch res.Outcome {
	case governance.Allow:
		je, err := svc.Store.PostInvoice(invoiceID, inputTax, svc.Today())
		if err != nil {
			svc.audit(id.ID, governance.ActInvoicePost, invoiceID, "error", map[string]any{"error": err.Error()})
			return PostResult{}, err
		}
		res.JournalEntryID = je.ID
		detail["journalEntry"] = je.ID
	case governance.Escalate:
		res.TaskID = svc.openTask(inv, res.Reason, "", id.ID)
		detail["task"] = res.TaskID
	}
	svc.audit(id.ID, governance.ActInvoicePost, invoiceID, string(res.Outcome), detail)
	return res, nil
}

// Escalate puts an invoice on hold and opens a review task with the
// agent's recommendation.
func (svc *Service) Escalate(id governance.Identity, invoiceID, reason, recommendation string) (string, error) {
	svc.Store.Mu.Lock()
	defer svc.Store.Mu.Unlock()
	if !id.Can(governance.ActInvoiceReview) {
		return "", fmt.Errorf("%s lacks permission %s", id.ID, governance.ActInvoiceReview)
	}
	inv := svc.Store.Invoices[invoiceID]
	if inv == nil {
		return "", fmt.Errorf("invoice %s not found", invoiceID)
	}
	if !open(inv) {
		return "", fmt.Errorf("invoice %s is %s; only parked or held invoices can be escalated", invoiceID, inv.Status)
	}
	taskID := svc.openTask(inv, reason, recommendation, id.ID)
	svc.audit(id.ID, governance.ActInvoiceReview, invoiceID, "escalated",
		map[string]any{"task": taskID, "reason": reason, "recommendation": recommendation})
	return taskID, nil
}

// open reports whether an invoice is still waiting for a decision.
func open(inv *core.SupplierInvoice) bool {
	return inv.Status == core.InvoiceParked || inv.Status == core.InvoiceOnHold
}

// openTask holds the invoice and creates (or updates) its open review task.
// Caller holds the store lock and has checked the invoice is open, so the
// hold cannot fail.
func (svc *Service) openTask(inv *core.SupplierInvoice, reason, recommendation, createdBy string) string {
	_ = svc.Store.HoldInvoice(inv.ID, reason)
	for _, tid := range core.SortedKeys(svc.Store.Tasks) {
		t := svc.Store.Tasks[tid]
		if t.InvoiceID == inv.ID && t.Status == core.TaskOpen {
			t.Reason = reason
			if recommendation != "" {
				t.Recommendation = recommendation
				t.CreatedBy = createdBy
			}
			return t.ID
		}
	}
	t := &core.ReviewTask{
		ID: svc.Store.NextID("TASK"), InvoiceID: inv.ID, Reason: reason,
		Recommendation: recommendation, CreatedBy: createdBy, Status: core.TaskOpen,
	}
	svc.Store.Tasks[t.ID] = t
	return t.ID
}

// Decide closes a review task as a human: approve posts the invoice
// (accepting any variance), reject marks it rejected.
func (svc *Service) Decide(human governance.Identity, taskID string, approve bool, comment string) (PostResult, error) {
	if human.Kind != "human" {
		return PostResult{}, fmt.Errorf("only humans can close review tasks")
	}
	if !human.Can(governance.ActInvoiceReview) {
		return PostResult{}, fmt.Errorf("%s lacks %s", human.ID, governance.ActInvoiceReview)
	}
	svc.Store.Mu.Lock()
	t := svc.Store.Tasks[taskID]
	if t == nil || t.Status != core.TaskOpen {
		svc.Store.Mu.Unlock()
		return PostResult{}, fmt.Errorf("no open task %s", taskID)
	}
	if !approve {
		t.Status = core.TaskRejected
		if inv := svc.Store.Invoices[t.InvoiceID]; inv != nil {
			inv.Status = core.InvoiceRejected
			inv.HoldReason = comment
		}
		svc.audit(human.ID, governance.ActInvoiceReview, t.InvoiceID, "rejected", map[string]any{"task": taskID, "comment": comment})
		svc.Store.Mu.Unlock()
		return PostResult{Outcome: governance.Deny, Reason: "rejected by " + human.ID}, nil
	}
	svc.Store.Mu.Unlock()

	res, err := svc.PostInvoice(human, t.InvoiceID, "approved task "+taskID+": "+comment)
	if err != nil {
		return res, err
	}
	if res.Outcome == governance.Allow {
		svc.Store.Mu.Lock()
		t.Status = core.TaskApproved
		svc.Store.Mu.Unlock()
	}
	return res, nil
}

func (svc *Service) audit(actor, action, subject, outcome string, detail map[string]any) {
	if svc.Audit == nil {
		return
	}
	if _, err := svc.Audit.Append(actor, action, subject, outcome, detail); err != nil {
		// An action that cannot be audited must not look successful.
		panic(fmt.Sprintf("audit log write failed: %v", err))
	}
}
