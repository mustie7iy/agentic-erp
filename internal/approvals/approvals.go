// Package approvals is the workflow context (03-data-model §3.11): approval
// requests for requisitions, partner changes, configuration changes,
// supplier invoice exceptions, period unlocks, manual journal entries,
// purchase orders and their changes, and cost and profit center changes,
// with sequential steps that each need a number of distinct approvals from
// holders of a role. Only people approve or reject, never the requester,
// and nobody approves a request twice; the requester may withdraw it. (The prototype's engine is
// internal/workflow; its rules are kept here.)
//
// The owning context opens a request through Approvals in its own
// transaction and is called back (Handler) in the deciding transaction when
// the request completes, so an approved requisition, an applied partner
// change and the decision that caused them commit together. Steps come
// from the active configuration document, which is the approval policy of
// T4: requisitions by amount tier (purchasing.requisition_approval),
// partner changes by payments.vendor_approver_role, configuration changes
// by their tier, invoice exceptions by DefaultExceptionRole, period unlocks
// by DefaultPeriodUnlockRole, manual journal entries above their
// company's manual_entry_approval_limit by DefaultManualEntryRole (T35),
// and purchase orders released without an approved requisition, and
// changes raising an order's total, by amount tier
// (purchasing.order_release_approval, T47), and the posting of a matched
// bank statement line by its company's bank_statements.approval_tiers, or
// by DefaultBankClearingRole until the company has tiers (ADR-0047, T49f),
// and center changes by DefaultCenterChangeRole (T56a).
// Roles are the identity context's roles with the document's role names as
// codes.
package approvals

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/autonomy"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/telemetry"
)

// Object types.
const (
	Requisition      = "requisition"
	PartnerChange    = "partner_change"
	ConfigChange     = "config_change"
	InvoiceException = "invoice_exception"
	PeriodUnlock     = "period_unlock"
	ManualEntry      = "manual_entry"
	PurchaseOrder    = "purchase_order" // the release of an order without an approved requisition (T47)
	OrderChange      = "order_change"   // a change that raises a released order's total (T47)
	BankClearing     = "bank_clearing"  // the posting of a matched bank statement line (ADR-0047, T49f)
	CenterChange     = "center_change"  // a change of a cost or profit center that needs a controller (T56a)
)

// Request statuses.
const (
	Pending   = "pending"
	Approved  = "approved"
	Rejected  = "rejected"
	Cancelled = "cancelled"
)

// DefaultExceptionRole decides supplier invoice exceptions until the
// configuration document has a setting for them.
const DefaultExceptionRole = "process_owner"

// DefaultPeriodUnlockRole decides the unlock of a locked posting period
// (T32): a fixed role, so that reopening the books after the lock is never
// a matter of configuration.
const DefaultPeriodUnlockRole = "controller"

// DefaultManualEntryRole approves a manual journal entry above its
// company's manual_entry_approval_limit (T35): a fixed role, as for period
// unlocks.
const DefaultManualEntryRole = "controller"

// DefaultBankClearingRole approves the posting of every matched bank
// statement line of a company without bank_statements.approval_tiers: there
// is no default tier, so a company that never set them posts nothing
// unchecked (ADR-0047, fail closed).
const DefaultBankClearingRole = "controller"

// DefaultCenterChangeRole approves a center change (T56a, ADR-0050 §11):
// creating a cost or profit center, its validity, locks, profit center,
// segment and companies, and every change to a hierarchy's tree (T56b),
// are a controller's decision, a fixed role as for period unlocks.
const DefaultCenterChangeRole = "controller"

// ObjectPermissions is the approval permission of each object type
// (x-object-permissions of approveApprovalRequest).
var ObjectPermissions = map[string]string{
	Requisition: "procurement.requisition.approve", PartnerChange: "partner.change.approve",
	InvoiceException: "payables.invoice.approve", ConfigChange: "config.change.approve",
	PeriodUnlock: "ledger.period.unlock.approve", ManualEntry: "ledger.entry.approve",
	PurchaseOrder: "procurement.order.approve", OrderChange: "procurement.order.approve",
	BankClearing: "payments.statement.clearing.approve",
	CenterChange: "ledger.cost_center.approve",
}

func init() {
	problem.RegisterConstraint("approval_request_one_pending", func() *problem.Problem {
		return problem.Conflict("workflow.request_pending", "The object already has a pending approval request.")
	})
	problem.RegisterConstraint("decision_one_approval_per_actor", func() *problem.Problem {
		return problem.Conflict("workflow.already_approved", "You have already approved this request.")
	})
}

// Settings reads the active configuration (configuration context).
type Settings interface {
	Document(ctx context.Context) (*config.Config, int32, error)
}

// Roles answers questions about roles (identity context).
type Roles interface {
	RoleByCode(ctx context.Context, code string) (uuid.UUID, bool, error)
	HoldsRole(ctx context.Context, principal, role uuid.UUID) (bool, error)
	// RolesHeld returns every role HoldsRole would report the principal
	// holds today.
	RolesHeld(ctx context.Context, principal uuid.UUID) ([]uuid.UUID, error)
}

// Handler is the context that owns an object type. It moves the object on
// when its request completes, in the same transaction.
type Handler interface {
	Approved(ctx context.Context, objectID, requestID uuid.UUID) error
	Rejected(ctx context.Context, objectID, requestID uuid.UUID, comment string) error
	Cancelled(ctx context.Context, objectID, requestID uuid.UUID, comment string) error
}

// Modifier is a Handler whose objects a person may change while their
// request is pending. Modified reports whether one did, so that an agent's
// proposal approved after a person changed it counts as an override
// (ADR-0043). It is asked before the Handler hears of the approval, so the
// approval's own change to the object is not taken for one. A Handler
// that is not one has objects nobody changes while they wait.
type Modifier interface {
	Modified(ctx context.Context, objectID, requestID uuid.UUID) (bool, error)
}

// AgentKind reads what metrics call a principal (identity.AgentKind).
type AgentKind func(ctx context.Context, tx pgx.Tx, principal uuid.UUID) (string, error)

// Barrer is a Handler whose objects some people may not approve besides
// their requester, by a segregation of duties the owning context knows:
// whoever matched a bank statement line by hand does not approve its
// posting (ADR-0047). Barred is asked before each approval.
type Barrer interface {
	Barred(ctx context.Context, objectID, actor uuid.UUID) (bool, error)
}

// Step is one approval step of a request.
type Step struct {
	No        int16     `json:"step_no"`
	RoleID    uuid.UUID `json:"role_id"`
	Required  int16     `json:"required"`
	Approvals int16     `json:"approvals"`
}

// Decision is one recorded decision.
type Decision struct {
	ID        uuid.UUID `json:"id"`
	StepNo    int16     `json:"step_no"`
	ActorID   uuid.UUID `json:"actor_id"`
	Decision  string    `json:"decision"`
	Comment   *string   `json:"comment"`
	DecidedAt time.Time `json:"decided_at"`
}

// Request is a row of workflow.approval_request with its steps and decisions.
type Request struct {
	ID          uuid.UUID
	ObjectType  string
	ObjectID    uuid.UUID
	Title       string
	RequesterID uuid.UUID
	Amount      *money.Minor
	Currency    *string
	Exponent    int
	PolicyID    *uuid.UUID
	Status      string
	CurrentStep int16
	CompletedAt *time.Time
	Escalations int16
	EscalatedAt *time.Time
	Steps       []Step
	Decisions   []Decision
	Version     int32
	CreatedAt   time.Time
	CreatedBy   uuid.UUID
	UpdatedAt   time.Time
}

const requestSelect = `SELECT r.id, r.object_type, r.object_id, r.title, r.requester_id, r.amount, r.currency, coalesce(c.exponent, 0), r.policy_id,
		r.status, r.current_step, r.completed_at, r.escalations, r.escalated_at, r.version, r.created_at, r.created_by, r.updated_at,
		coalesce((SELECT json_agg(json_build_object('step_no', s.step_no, 'role_id', s.role_id, 'required', s.required,
			'approvals', (SELECT count(*) FROM workflow.decision d WHERE d.tenant_id = s.tenant_id AND d.request_id = s.request_id
				AND d.step_no = s.step_no AND d.decision = 'approve')) ORDER BY s.step_no)
		 FROM workflow.approval_step s WHERE s.tenant_id = r.tenant_id AND s.request_id = r.id), '[]'),
		coalesce((SELECT json_agg(json_build_object('id', d.id, 'step_no', d.step_no, 'actor_id', d.actor_id, 'decision', d.decision,
			'comment', d.comment, 'decided_at', d.decided_at) ORDER BY d.decided_at, d.id)
		 FROM workflow.decision d WHERE d.tenant_id = r.tenant_id AND d.request_id = r.id), '[]')
	FROM workflow.approval_request r LEFT JOIN ref.currency c ON c.code = r.currency`

func scanRequest(row pgx.Row) (Request, error) {
	var r Request
	err := row.Scan(&r.ID, &r.ObjectType, &r.ObjectID, &r.Title, &r.RequesterID, &r.Amount, &r.Currency, &r.Exponent, &r.PolicyID,
		&r.Status, &r.CurrentStep, &r.CompletedAt, &r.Escalations, &r.EscalatedAt, &r.Version, &r.CreatedAt, &r.CreatedBy, &r.UpdatedAt, &r.Steps, &r.Decisions)
	return r, errs.Wrapf(err, "scan approval request")
}

func getRequest(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (Request, error) {
	if lock {
		if _, err := tx.Exec(ctx, `SELECT FROM workflow.approval_request WHERE id = $1 FOR UPDATE`, id); err != nil {
			return Request{}, fmt.Errorf("approvals: lock request %s: %w", id, err)
		}
	}
	r, err := scanRequest(tx.QueryRow(ctx, requestSelect+` WHERE r.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Request{}, problem.NotFoundOf("approval request")
	}
	if err != nil {
		return r, fmt.Errorf("approvals: read request %s: %w", id, err)
	}
	return r, nil
}

// GetRequest returns an approval request.
func GetRequest(ctx context.Context, id uuid.UUID) (Request, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Request{}, err
	}
	return getRequest(ctx, tx, id, false)
}

// Rates converts a requisition's or manual entry's amount into its
// company's currency (ledger context): the approval thresholds and limits
// are in that currency (F12). It
// returns the amount's exact value in major units of the company's
// currency, unrounded, and refuses when no rate is valid on the date.
type Rates interface {
	ExactToCompany(ctx context.Context, legalEntity uuid.UUID, from string, date time.Time, amount money.Minor) (*big.Rat, error)
}

// Approvals opens and withdraws approval requests for the contexts that
// own the objects.
type Approvals struct {
	Settings Settings
	Roles    Roles
	Rates    Rates
}

// NewRequest is what an owning context asks approval for.
type NewRequest struct {
	ObjectType string
	ObjectID   uuid.UUID
	Title      string
	Amount     *money.Minor // minor units of Currency; selects the requisition tier, or is a manual entry's debits
	Currency   *string      // with Amount
	Tier       int          // configuration change tier
	// LegalEntityID and Date place a requisition or manual entry (its
	// posting date): its amount is converted into the legal entity's
	// currency at the rate valid on the date before it is compared with the
	// thresholds or the limit.
	LegalEntityID uuid.UUID
	Date          time.Time
	// Company is the configuration's code of LegalEntityID (its tenancy
	// code), whose manual_entry_approval_limit a manual entry is measured
	// against, and whose bank_statements.approval_tiers route a bank
	// clearing; the owner passes both from the one legal entity it posts in.
	Company string
}

// policyStep is a step as the configuration names it.
type policyStep struct {
	role     string
	required int
}

// requisitionSteps are the steps of the first requisition tier whose
// threshold the requisition's amount, in its company's currency, does not
// exceed.
func requisitionSteps(ctx context.Context, rates Rates, doc *config.Config, in NewRequest) ([]policyStep, error) {
	return tierSteps(ctx, rates, doc.Purchasing.RequisitionApproval, in)
}

// tierSteps are the steps of the first of the tiers whose threshold the
// request's amount, in its company's currency, does not exceed.
func tierSteps(ctx context.Context, rates Rates, tiers []config.ApprovalTier, in NewRequest) ([]policyStep, error) {
	var out []policyStep
	// The converted amount is compared exactly, unrounded: rounded to the
	// company's digits first, 1000.004 would fall under an "up to 1000.00"
	// tier and need fewer approvals. A threshold is validated with each
	// company currency's digits, which may be more than this company's, so
	// it is read exactly too rather than at the company's digits.
	amount, err := rates.ExactToCompany(ctx, in.LegalEntityID, *in.Currency, in.Date, *in.Amount)
	if err != nil {
		return nil, fmt.Errorf("approvals: policy steps: %w", err)
	}
	for _, t := range tiers {
		if t.UpTo != "" { // "" is the last tier
			limit, err := money.ParseDecimalRat(t.UpTo) // the grammar the document was validated with
			if err != nil {
				return nil, fmt.Errorf("approvals: policy steps: threshold: %w", err)
			}
			if amount.Cmp(limit) > 0 {
				continue
			}
		}
		for _, s := range t.Steps {
			out = append(out, policyStep{s.Role, s.Approvals})
		}
		break
	}
	return out, nil
}

// orderSteps route an order's release, or a change that raises its
// total, by the order's total through purchasing.order_release_approval
// (T47). A document without those tiers, one from before T47c, routes
// nothing: the order is refused rather than released unapproved.
func orderSteps(ctx context.Context, rates Rates, doc *config.Config, in NewRequest) ([]policyStep, error) {
	if len(doc.Purchasing.OrderReleaseApproval) == 0 {
		return nil, problem.Unprocessable("workflow.no_policy",
			"The active configuration has no purchasing.order_release_approval tiers; add them to release or change purchase orders.")
	}
	return tierSteps(ctx, rates, doc.Purchasing.OrderReleaseApproval, in)
}

// manualEntrySteps are none when the entry's debits, converted exactly
// into its company's currency, are within the company's
// manual_entry_approval_limit, and the controller's approval otherwise. A
// company the configuration lacks, or one without a limit (a document
// older than T35), has a limit of 0: its manual entries are approved
// first, never posted unchecked.
func manualEntrySteps(ctx context.Context, rates Rates, doc *config.Config, in NewRequest) ([]policyStep, error) {
	amount, err := rates.ExactToCompany(ctx, in.LegalEntityID, *in.Currency, in.Date, *in.Amount)
	if err != nil {
		return nil, fmt.Errorf("approvals: policy steps: %w", err)
	}
	limit := new(big.Rat)
	if co := doc.Company(in.Company); co != nil && strings.TrimSpace(co.ManualEntryApprovalLimit) != "" {
		if limit, err = money.ParseDecimalRat(co.ManualEntryApprovalLimit); err != nil {
			return nil, fmt.Errorf("approvals: policy steps: manual entry approval limit of company %s: %w", in.Company, err)
		}
	}
	if amount.Cmp(limit) <= 0 {
		return nil, nil
	}
	return []policyStep{{DefaultManualEntryRole, 1}}, nil
}

// bankClearingSteps route a matched bank statement line's posting by the
// line's amount, in its company's currency, through the company's
// bank_statements.approval_tiers; a tier without steps posts without
// approval. A company without tiers, or one the configuration lacks, has
// every posting approved by DefaultBankClearingRole (ADR-0047).
func bankClearingSteps(ctx context.Context, rates Rates, doc *config.Config, in NewRequest) ([]policyStep, error) {
	co := doc.Company(in.Company)
	if co == nil || len(co.BankStatements.ApprovalTiers) == 0 {
		return []policyStep{{DefaultBankClearingRole, 1}}, nil
	}
	return tierSteps(ctx, rates, co.BankStatements.ApprovalTiers, in)
}

// steps resolves the steps the active configuration requires.
func steps(ctx context.Context, rates Rates, doc *config.Config, in NewRequest) ([]policyStep, error) {
	var out []policyStep
	switch in.ObjectType {
	case Requisition:
		return requisitionSteps(ctx, rates, doc, in)
	case PartnerChange:
		out = append(out, policyStep{doc.Payments.VendorApproverRole, 1})
	case ConfigChange:
		for _, s := range config.ApprovalSteps(config.Tier(in.Tier)) {
			out = append(out, policyStep{s.Role, s.Approvals})
		}
	case InvoiceException:
		out = append(out, policyStep{DefaultExceptionRole, 1})
	case PeriodUnlock:
		out = append(out, policyStep{DefaultPeriodUnlockRole, 1})
	case CenterChange:
		out = append(out, policyStep{DefaultCenterChangeRole, 1})
	case ManualEntry:
		return manualEntrySteps(ctx, rates, doc, in)
	case PurchaseOrder, OrderChange:
		return orderSteps(ctx, rates, doc, in)
	case BankClearing:
		return bankClearingSteps(ctx, rates, doc, in)
	default:
		return nil, fmt.Errorf("approvals: unknown object type %q", in.ObjectType)
	}
	return out, nil
}

// Open opens a pending approval request for the object with the steps the
// active configuration requires. When it requires none, no request is
// opened and needed is false: the object is approved at once.
func (a Approvals) Open(ctx context.Context, in NewRequest) (id uuid.UUID, needed bool, err error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return uuid.Nil, false, err
	}
	doc, _, err := a.Settings.Document(ctx)
	if err != nil {
		return uuid.Nil, false, err
	}
	policy, err := steps(ctx, a.Rates, doc, in)
	if err != nil || len(policy) == 0 {
		return uuid.Nil, false, err
	}
	roles := make([]uuid.UUID, len(policy))
	for i, s := range policy {
		role, ok, err := a.Roles.RoleByCode(ctx, s.role)
		if err != nil {
			return uuid.Nil, false, err
		}
		if !ok {
			return uuid.Nil, false, problem.Unprocessable("workflow.unknown_role",
				fmt.Sprintf("The approval policy names role %s, which is not an active role; create it or change the configuration.", s.role))
		}
		roles[i] = role
	}
	id = uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO workflow.approval_request (tenant_id, id, object_type, object_id, title, requester_id, amount, currency, status, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'pending', $6)`, p.TenantID, id, in.ObjectType, in.ObjectID, in.Title, p.ID, in.Amount, in.Currency); err != nil {
		return uuid.Nil, false, fmt.Errorf("approvals: insert request %s for %s %s: %w", id, in.ObjectType, in.ObjectID, err)
	}
	nos, required := make([]int16, len(policy)), make([]int16, len(policy))
	for i, s := range policy {
		nos[i], required[i] = int16(i+1), int16(s.required)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO workflow.approval_step (tenant_id, request_id, step_no, role_id, required)
		SELECT $1, $2, n, r, q FROM unnest($3::smallint[], $4::uuid[], $5::smallint[]) AS x(n, r, q)`, p.TenantID, id, nos, roles, required); err != nil {
		return uuid.Nil, false, fmt.Errorf("approvals: insert steps of request %s: %w", id, err)
	}
	r, err := getRequest(ctx, tx, id, false)
	if err != nil {
		return uuid.Nil, false, err
	}
	stepPayload := make([]map[string]any, len(r.Steps))
	for i, s := range r.Steps {
		stepPayload[i] = map[string]any{"step_no": s.No, "role_id": s.RoleID, "required": s.Required}
	}
	return id, true, record(ctx, "workflow.approval_request.open", r, map[string]any{"object_type": in.ObjectType, "object_id": in.ObjectID},
		outbox.Event{Context: "workflow", Name: "approval_requested", AggregateType: "workflow.approval_request", AggregateID: id,
			Payload: map[string]any{"approval_request_id": id, "object_type": r.ObjectType, "object_id": r.ObjectID, "title": r.Title,
				"requester_id": r.RequesterID, "amount": moneyOf(r), "policy_id": nil, "steps": stepPayload}})
}

// Required reports whether the active configuration requires approval of
// the object, without opening a request: the ledger refuses a direct
// manual entry that needs one (T35).
func (a Approvals) Required(ctx context.Context, in NewRequest) (bool, error) {
	doc, _, err := a.Settings.Document(ctx)
	if err != nil {
		return false, fmt.Errorf("approvals: required for %s: %w", in.ObjectType, err)
	}
	policy, err := steps(ctx, a.Rates, doc, in)
	return len(policy) > 0, err
}

// Withdraw cancels the object's pending request, if there is one, because
// the owning context decided the object otherwise (it was cancelled,
// released by hand, changed). The owner is not called back.
func (Approvals) Withdraw(ctx context.Context, objectType string, objectID uuid.UUID, comment string) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	var id uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM workflow.approval_request WHERE object_type = $1 AND object_id = $2 AND status = 'pending' FOR UPDATE`,
		objectType, objectID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("approvals: find pending request of %s %s: %w", objectType, objectID, err)
	}
	return complete(ctx, tx, p, id, Cancelled, comment)
}

func moneyOf(r Request) any {
	if r.Amount == nil {
		return nil
	}
	return map[string]any{"amount": r.Amount.Plain(r.Exponent), "currency": *r.Currency}
}

// complete ends a pending request with the outcome and publishes
// ApprovalCompleted.
func complete(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID, outcome, comment string) error {
	if _, err := tx.Exec(ctx, `UPDATE workflow.approval_request SET status = $2, completed_at = now(), version = version + 1 WHERE id = $1`,
		id, outcome); err != nil {
		return fmt.Errorf("approvals: complete request %s as %s: %w", id, outcome, err)
	}
	r, err := getRequest(ctx, tx, id, false)
	if err != nil {
		return err
	}
	var c *string
	if comment != "" {
		c = &comment
	}
	return record(ctx, "workflow.approval_request."+outcome, r, map[string]any{"comment": c},
		outbox.Event{Context: "workflow", Name: "approval_completed", AggregateType: "workflow.approval_request", AggregateID: id,
			Payload: map[string]any{"approval_request_id": id, "object_type": r.ObjectType, "object_id": r.ObjectID, "outcome": outcome,
				"decided_by": p.ID, "decided_by_kind": p.Kind, "comment": c, "completed_at": r.CompletedAt.UTC().Format(time.RFC3339Nano)}})
}

func record(ctx context.Context, action string, r Request, detail map[string]any, events ...outbox.Event) error {
	return audit.Record(ctx, action, "workflow.approval_request", r.ID, detail, events...)
}

// Decider records decisions and calls the owning contexts back.
type Decider struct {
	Roles    Roles
	Handlers map[string]Handler
	// AgentKind and Metrics count decisions on agents' proposals
	// (erp_agent_proposals_decided_total, ADR-0043); either nil: none.
	AgentKind AgentKind
	Metrics   *telemetry.Metrics
}

// decide loads a pending request for a decision by the caller and checks
// what every approval or rejection requires: a person who is not the
// requester, holding the object type's approval permission and the current
// step's role.
func (d Decider) decide(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (pgx.Tx, reqctx.Principal, Request, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, p, Request{}, err
	}
	r, err := getRequest(ctx, tx, id, true)
	if err != nil {
		return nil, p, Request{}, err
	}
	if err := ifMatch(r.Version); err != nil {
		return nil, p, Request{}, err
	}
	if r.Status != Pending {
		return nil, p, Request{}, problem.Conflict("workflow.not_pending", "The approval request is "+r.Status+".")
	}
	if p.Kind != reqctx.KindHuman {
		return nil, p, Request{}, problem.Forbidden("identity.agent_not_allowed", "Only a person may approve or reject.")
	}
	if p.ID == r.RequesterID {
		return nil, p, Request{}, problem.Forbidden("workflow.own_request", "Requesters cannot decide their own request.")
	}
	if err := authz.Require(ctx, ObjectPermissions[r.ObjectType]); err != nil {
		return nil, p, Request{}, err
	}
	step := r.Steps[r.CurrentStep-1]
	holds, err := d.Roles.HoldsRole(ctx, p.ID, step.RoleID)
	if err != nil {
		return nil, p, Request{}, err
	}
	if !holds {
		return nil, p, Request{}, problem.Forbidden("workflow.role_missing", fmt.Sprintf("Step %d needs a holder of its role.", step.No))
	}
	return tx, p, r, nil
}

func (d Decider) insertDecision(ctx context.Context, tx pgx.Tx, p reqctx.Principal, r Request, decision, comment string) error {
	var c *string
	if comment != "" {
		c = &comment
	}
	_, err := tx.Exec(ctx, `INSERT INTO workflow.decision (tenant_id, id, request_id, step_no, actor_id, actor_kind, decision, comment)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, p.TenantID, uuid.Must(uuid.NewV7()), r.ID, r.CurrentStep, p.ID, p.Kind, decision, c)
	if err != nil {
		return fmt.Errorf("approvals: insert %s decision on request %s: %w", decision, r.ID, err)
	}
	return nil
}

// notify calls the owning context of the request's object type.
func (d Decider) notify(r Request, call func(Handler) error) error {
	h, ok := d.Handlers[r.ObjectType]
	if !ok {
		return fmt.Errorf("approvals: no handler for object type %q", r.ObjectType)
	}
	return call(h)
}

// observe counts a request an agent raised and a person completed, once
// the decision commits: approved, rejected, or approved after a person
// modified its object (the override rate, ADR-0043). Requests people
// raised are not counted.
func (d Decider) observe(ctx context.Context, tx pgx.Tx, r Request, decision string) error {
	if d.AgentKind == nil || d.Metrics == nil {
		return nil
	}
	kind, err := d.AgentKind(ctx, tx, r.RequesterID)
	if err != nil {
		return fmt.Errorf("approvals: read the requester of request %s: %w", r.ID, err)
	}
	if kind == autonomy.NotAgent {
		return nil
	}
	if m, ok := d.Handlers[r.ObjectType].(Modifier); ok && decision == telemetry.DecisionApproved {
		changed, err := m.Modified(ctx, r.ObjectID, r.ID)
		if err != nil {
			return fmt.Errorf("approvals: was %s %s modified: %w", r.ObjectType, r.ObjectID, err)
		}
		if changed {
			decision = telemetry.DecisionModified
		}
	}
	reqctx.OnCommit(ctx, func() { d.Metrics.ProposalDecided(kind, decision) })
	return nil
}

// barred refuses an approval by someone the owning context bars from
// approving the object (Barrer).
func (d Decider) barred(ctx context.Context, r Request, actor uuid.UUID) error {
	b, ok := d.Handlers[r.ObjectType].(Barrer)
	if !ok {
		return nil
	}
	barred, err := b.Barred(ctx, r.ObjectID, actor)
	if err != nil {
		return err
	}
	if barred {
		return problem.Forbidden("workflow.segregation_of_duties", "Segregation of duties bars you from approving this request.")
	}
	return nil
}

// Approve records the caller's approval for the current step. When the
// step has its approvals the request moves to the next step, or is
// approved after the last one and the owning context moves the object on.
func (d Decider) Approve(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, comment string) (Request, error) {
	tx, p, r, err := d.decide(ctx, id, ifMatch)
	if err != nil {
		return Request{}, err
	}
	for _, dec := range r.Decisions {
		if dec.ActorID == p.ID && dec.Decision == "approve" {
			return Request{}, problem.Conflict("workflow.already_approved", "You have already approved this request.")
		}
	}
	if err := d.barred(ctx, r, p.ID); err != nil {
		return Request{}, err
	}
	if err := d.insertDecision(ctx, tx, p, r, "approve", comment); err != nil {
		return Request{}, err
	}
	step := r.Steps[r.CurrentStep-1]
	switch {
	case step.Approvals+1 < step.Required:
	case int(r.CurrentStep) < len(r.Steps):
		if _, err := tx.Exec(ctx, `UPDATE workflow.approval_request SET current_step = current_step + 1, version = version + 1 WHERE id = $1`, id); err != nil {
			return Request{}, fmt.Errorf("approvals: advance request %s: %w", id, err)
		}
	default:
		if err := complete(ctx, tx, p, id, Approved, comment); err != nil {
			return Request{}, err
		}
		if err := d.observe(ctx, tx, r, telemetry.DecisionApproved); err != nil {
			return Request{}, err
		}
		if err := d.notify(r, func(h Handler) error { return h.Approved(ctx, r.ObjectID, id) }); err != nil {
			return Request{}, err
		}
	}
	updated, err := getRequest(ctx, tx, id, false)
	if err != nil {
		return Request{}, err
	}
	return updated, record(ctx, "workflow.approval_request.approve", updated, map[string]any{"step_no": r.CurrentStep, "comment": comment})
}

// Reject ends the request; the owning context rejects the object.
func (d Decider) Reject(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, comment string) (Request, error) {
	tx, p, r, err := d.decide(ctx, id, ifMatch)
	if err != nil {
		return Request{}, err
	}
	if err := d.insertDecision(ctx, tx, p, r, "reject", comment); err != nil {
		return Request{}, err
	}
	if err := complete(ctx, tx, p, id, Rejected, comment); err != nil {
		return Request{}, err
	}
	if err := d.observe(ctx, tx, r, telemetry.DecisionRejected); err != nil {
		return Request{}, err
	}
	if err := d.notify(r, func(h Handler) error { return h.Rejected(ctx, r.ObjectID, id, comment) }); err != nil {
		return Request{}, err
	}
	return getRequest(ctx, tx, id, false)
}

// Cancel lets the requester withdraw the request; the owning context is
// told so.
func (d Decider) Cancel(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, comment string) (Request, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Request{}, err
	}
	r, err := getRequest(ctx, tx, id, true)
	if err != nil {
		return Request{}, err
	}
	if err := ifMatch(r.Version); err != nil {
		return Request{}, err
	}
	if r.Status != Pending {
		return Request{}, problem.Conflict("workflow.not_pending", "The approval request is "+r.Status+".")
	}
	if p.ID != r.RequesterID {
		return Request{}, problem.Forbidden("workflow.not_requester", "Only the requester can withdraw an approval request.")
	}
	if err := d.insertDecision(ctx, tx, p, r, "cancel", comment); err != nil {
		return Request{}, err
	}
	if err := complete(ctx, tx, p, id, Cancelled, comment); err != nil {
		return Request{}, err
	}
	if err := d.notify(r, func(h Handler) error { return h.Cancelled(ctx, r.ObjectID, id, comment) }); err != nil {
		return Request{}, err
	}
	return getRequest(ctx, tx, id, false)
}
