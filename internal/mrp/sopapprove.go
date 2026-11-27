package mrp

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Approval of the consensus plan (ADR-0084 As built S2n2): where a
// company's configuration names sop_approval steps, a planner submits a
// draft version instead of fixing it. The workflow context's engine opens
// its request (object type approvals.SopPlan) with the steps, each a role
// and a number of distinct approvers, never the requester; the version
// waits, submitted, its consensus fixed by the database while it waits.
// Approved at its last step it is fixed by that approver; rejected or
// withdrawn it is a draft again, to change and submit once more. A company
// without steps, the default, fixes a submitted version at once, as
// before approval existed.
//
// Locks: submitting takes the version's row lock and then inserts its
// request; the engine's decision locks the request and then, through
// SopApprovals, the version. No path takes a version's lock before an
// existing request's, so the two orders cannot meet.

// SopRequests opens S&OP plan versions' approval requests and says
// whether a company's versions need one (the workflow context).
type SopRequests interface {
	Open(ctx context.Context, in approvals.NewRequest) (uuid.UUID, bool, error)
	Required(ctx context.Context, in approvals.NewRequest) (bool, error)
}

// sopRequest is a version's approval request, routed by its company's
// sop_approval steps.
func sopRequest(p SopPlan, company string) approvals.NewRequest {
	return approvals.NewRequest{ObjectType: approvals.SopPlan, ObjectID: p.ID, LegalEntityID: p.LegalEntityID, Company: company,
		Title: fmt.Sprintf("S&OP plan %d of company %s, %s to %s", p.Number, company, p.FirstDay.Format(time.DateOnly), p.LastDay.Format(time.DateOnly))}
}

// sopCompany is the code of a version's company.
func (e *Endpoints) sopCompany(ctx context.Context, p SopPlan) (string, error) {
	codes, err := e.sopCompanies(ctx, []SopPlan{p})
	return codes[p.LegalEntityID], err
}

// SubmitSop submits a draft version for approval at its version (412
// otherwise; 409 mrp.sop_plan_fixed once fixed, mrp.sop_plan_submitted
// while submitted): its request opened by its company's steps, it is
// submitted, naming the request; a company without steps fixes it at
// once (fixSop). Audited.
func (e *Endpoints) SubmitSop(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (SopPlan, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return SopPlan{}, err
	}
	before, err := draft(ctx, tx, id, ifMatch)
	if err != nil {
		return SopPlan{}, err
	}
	code, err := e.sopCompany(ctx, before)
	if err != nil {
		return SopPlan{}, err
	}
	request, needed, err := e.Requests.Open(ctx, sopRequest(before, code))
	if err != nil {
		return SopPlan{}, fmt.Errorf("mrp: approval of S&OP plan %d: %w", before.Number, err)
	}
	if !needed {
		return fixSop(ctx, tx, p.ID, before, nil)
	}
	if _, err := tx.Exec(ctx, `UPDATE mrp.sop_plan SET status = 'submitted', approval_request_id = $2, version = version + 1, updated_by = $3
		WHERE id = $1`, id, request, p.ID); err != nil {
		return SopPlan{}, fmt.Errorf("mrp: submit S&OP plan %d: %w", before.Number, err)
	}
	out, err := loadSop(ctx, tx, id, false)
	if err != nil {
		return SopPlan{}, err
	}
	return out, audit.Record(ctx, "mrp.sop_plan.submit", "mrp.sop_plan", id, map[string]any{"legal_entity_id": before.LegalEntityID,
		"company_code": code, "number": before.Number, "approval_request_id": request})
}

// decideSop moves a submitted version on as its request completed:
// approved, fixed by the deciding approver; rejected or withdrawn, a draft
// again, its request kept as its latest, the reason in the audit record.
// A version not waiting for this request is a fault of the engine's
// wiring, not of the decision, so it fails as one (500).
func decideSop(ctx context.Context, id, request uuid.UUID, outcome, comment string) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	before, err := loadSop(ctx, tx, id, true)
	if err != nil {
		return err
	}
	if before.Status != SopSubmitted || before.ApprovalRequestID == nil || *before.ApprovalRequestID != request {
		return fmt.Errorf("mrp: S&OP plan %d is %s, not waiting for approval request %s", before.Number, before.Status, request)
	}
	if outcome == approvals.Approved {
		_, err := fixSop(ctx, tx, p.ID, before, &request)
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE mrp.sop_plan SET status = 'draft', version = version + 1, updated_by = $2 WHERE id = $1`, id, p.ID); err != nil {
		return fmt.Errorf("mrp: S&OP plan %d %s: %w", before.Number, outcome, err)
	}
	action := map[string]string{approvals.Rejected: "reject", approvals.Cancelled: "withdraw"}[outcome]
	return audit.Record(ctx, "mrp.sop_plan."+action, "mrp.sop_plan", id, map[string]any{"number": before.Number, "approval_request_id": request,
		"comment": comment})
}

// SopApprovals moves S&OP plan versions on when their approval requests
// complete (approvals.Handler, approvals.Locator).
type SopApprovals struct{}

// Approved fixes the version.
func (SopApprovals) Approved(ctx context.Context, plan, request uuid.UUID) error {
	return decideSop(ctx, plan, request, approvals.Approved, "")
}

// Rejected makes the version a draft again, its consensus to change.
func (SopApprovals) Rejected(ctx context.Context, plan, request uuid.UUID, comment string) error {
	return decideSop(ctx, plan, request, approvals.Rejected, comment)
}

// Cancelled makes the version a draft again: its requester withdrew the
// request.
func (SopApprovals) Cancelled(ctx context.Context, plan, request uuid.UUID, comment string) error {
	return decideSop(ctx, plan, request, approvals.Cancelled, comment)
}

// Resources is where the API reads each version.
func (SopApprovals) Resources(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	return approvals.Paths("/v1/sop-plans/%s", ids), nil
}
