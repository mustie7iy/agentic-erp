package governance

import (
	"fmt"
	"slices"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// Actions agents and users can request.
const (
	ActInvoicePost    = "invoice.post"
	ActInvoiceHold    = "invoice.hold"
	ActInvoiceReview  = "invoice.review"
	ActInvoiceCapture = "invoice.capture"
	ActVendorCreate   = "vendor.create"
	ActVendorChange   = "vendor.change"
	ActVendorBank     = "vendor.bank_change"
	ActVendorApprove  = "vendor.approve"
	ActPaymentPropose = "payment.propose"
	ActPaymentRun     = "payment.release"
	ActRequisition    = "requisition.create"
	ActRequisitionApp = "requisition.approve"
	ActPOCreate       = "po.create"
	ActContractManage = "contract.manage"
	ActGoodsReceipt   = "goods_receipt.post"
	ActConfigPropose  = "config.propose"
	ActConfigApprove  = "config.approve"
	ActConfigActivate = "config.activate"
	ActMigrationRun   = "migration.run"
	ActAnalyticsQuery = "analytics.query"
)

// Permissions lists every action the platform knows; configuration that
// grants anything else is rejected.
var Permissions = []string{
	ActInvoicePost, ActInvoiceHold, ActInvoiceReview, ActInvoiceCapture,
	ActVendorCreate, ActVendorChange, ActVendorBank, ActVendorApprove,
	ActPaymentPropose, ActPaymentRun, ActRequisition, ActRequisitionApp, ActPOCreate,
	ActContractManage, ActGoodsReceipt, ActConfigPropose, ActConfigApprove, ActConfigActivate,
	ActMigrationRun, ActAnalyticsQuery,
}

// ApprovalPermissions may only be held by people, never by agents.
var ApprovalPermissions = []string{ActRequisitionApp, ActVendorApprove, ActPaymentRun, ActConfigApprove, ActConfigActivate}

// Autonomy levels, from the plan: agents start low and earn more.
type Autonomy int

const (
	Suggest         Autonomy = iota // L0: may only recommend
	ActWithApproval                 // L1: every action needs a human
	ActWithinLimits                 // L2: acts alone under thresholds
	FullyAutonomous                 // L3: acts alone (still audited)
)

// Identity is a named, scoped principal. Every agent gets its own, so SOX
// controls and segregation of duties apply to agents like to employees.
type Identity struct {
	ID          string   `json:"id"`
	Kind        string   `json:"kind"` // "agent" | "human" | "rule"
	Permissions []string `json:"permissions"`
	Autonomy    Autonomy `json:"autonomy"`
}

func (i Identity) Can(action string) bool { return slices.Contains(i.Permissions, action) }

type Outcome string

const (
	Allow    Outcome = "allow"
	Escalate Outcome = "escalate" // needs human approval
	Deny     Outcome = "deny"
)

type Decision struct {
	Outcome Outcome `json:"outcome"`
	Reason  string  `json:"reason"`
}

// Policy is evaluated deterministically before any action executes.
type Policy struct {
	// AutoLimit is the largest amount (minor units) an L2 identity may act on alone.
	AutoLimit money.Minor `json:"autoLimit"`
	// AlwaysHuman actions are escalated regardless of autonomy (fraud-prone).
	AlwaysHuman []string `json:"alwaysHuman"`
	// SoDConflicts: one identity may not hold both permissions of a pair.
	SoDConflicts [][2]string `json:"sodConflicts"`
}

func DefaultPolicy() Policy {
	return Policy{
		AutoLimit:    5_000_000, // 50,000.00
		AlwaysHuman:  []string{ActVendorBank, ActPaymentRun},
		SoDConflicts: DefaultSoDConflicts(),
	}
}

// ValidateIdentity rejects identities whose permissions violate segregation of duties.
func (p Policy) ValidateIdentity(id Identity) error {
	for _, c := range p.SoDConflicts {
		if id.Can(c[0]) && id.Can(c[1]) {
			return fmt.Errorf("identity %s violates segregation of duties: holds both %s and %s", id.ID, c[0], c[1])
		}
	}
	return nil
}

// Decide says whether identity may perform action on an amount now.
func (p Policy) Decide(id Identity, action string, amount money.Minor) Decision {
	if err := p.ValidateIdentity(id); err != nil {
		return Decision{Deny, err.Error()}
	}
	if !id.Can(action) {
		return Decision{Deny, fmt.Sprintf("%s lacks permission %s", id.ID, action)}
	}
	if slices.Contains(p.AlwaysHuman, action) {
		return Decision{Escalate, fmt.Sprintf("%s always requires human approval", action)}
	}
	switch id.Autonomy {
	case Suggest:
		return Decision{Escalate, fmt.Sprintf("%s may only make recommendations (L0)", id.ID)}
	case ActWithApproval:
		return Decision{Escalate, fmt.Sprintf("%s requires approval for every action (L1)", id.ID)}
	case ActWithinLimits:
		if amount.Abs() > p.AutoLimit {
			return Decision{Escalate, fmt.Sprintf("amount %s exceeds autonomous limit %s", amount.Abs(), p.AutoLimit)}
		}
	}
	return Decision{Allow, "within policy"}
}

// DefaultSoDConflicts are the segregation-of-duties pairs every
// configuration starts from.
func DefaultSoDConflicts() [][2]string {
	return [][2]string{
		{ActVendorCreate, ActPaymentRun},
		{ActVendorBank, ActPaymentRun},
		{ActInvoicePost, ActPaymentRun},
		{ActPaymentPropose, ActPaymentRun},
		{ActRequisition, ActRequisitionApp},
		{ActVendorChange, ActVendorApprove},
		{ActConfigPropose, ActConfigApprove},
	}
}
