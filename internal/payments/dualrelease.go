package payments

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Two-person release (ADR-0038, product owner 2026-10-22). A run whose
// total, converted exactly into its company's currency at the run date's
// rate (the rate its payments post at), is above the company's
// release_dual_limit is released only after a second person approved the
// release: a releaser asks for it (:submit, the approval request's
// requester), one holder of payments.release_role who did not propose the
// run approves it through the workflow context (approvals.
// PaymentRunRelease), and a releaser who is not that approver releases it
// with :release, which stays its own human step: the approval releases
// nothing. The run records the latest request, its outcome, the total it
// was asked for and the approver, so :release reads all of it from the run
// it locks. A run at or below the limit, or of a company without one, is
// released by one person as before.

// Outcomes of a release approval as the run records it
// (payments.payment_run.release_approval).
const (
	ReleasePending   = "pending"
	ReleaseApproved  = "approved"
	ReleaseRejected  = "rejected"
	ReleaseWithdrawn = "withdrawn"
)

// ReleaseApproval is the latest request for a second person's approval of
// a run's release.
type ReleaseApproval struct {
	RequestID  uuid.UUID
	Status     string
	Total      money.Minor // the run's total when it was asked for
	ApprovedBy *uuid.UUID
	ApprovedAt *time.Time
}

// releaseRequest is what the workflow context measures the run's release
// against: its total in its currency, on its run date, in its company.
func (e *Endpoints) releaseRequest(ctx context.Context, r Run) (approvals.NewRequest, error) {
	le, err := e.entityOf(ctx, r)
	if err != nil {
		return approvals.NewRequest{}, err
	}
	total, currency := r.Total, r.Currency
	return approvals.NewRequest{ObjectType: approvals.PaymentRunRelease, ObjectID: r.ID,
		Title:  fmt.Sprintf("Release of payment run %s of %s: %s %s", r.DocumentNumber, r.RunDate.Format(time.DateOnly), r.Total.Plain(r.Exponent), r.Currency),
		Amount: &total, Currency: &currency, LegalEntityID: r.LegalEntityID, Company: le.Code, Date: r.RunDate}, nil
}

// SubmitRelease asks for a second person's approval of a proposed run's
// release. The caller is checked as a releaser (decider) and becomes the
// request's requester, so the workflow context keeps them from approving
// it; the run's proposer is barred too (ReleaseApprovals.Barred).
func (e *Endpoints) SubmitRelease(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (Run, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Run{}, err
	}
	r, err := get(ctx, tx, id, true)
	if err != nil {
		return Run{}, err
	}
	if err := ifMatch(r.Version); err != nil {
		return Run{}, err
	}
	if err := requireProposed(r, "submitted for the approval of its release"); err != nil {
		return Run{}, err
	}
	if err := e.decider(ctx, r, p, true); err != nil {
		return Run{}, err
	}
	if err := resubmittable(r); err != nil {
		return Run{}, err
	}
	in, err := e.releaseRequest(ctx, r)
	if err != nil {
		return Run{}, err
	}
	request, needed, err := e.Approvals.Open(ctx, in)
	if err != nil {
		return Run{}, err
	}
	if !needed {
		return Run{}, problem.Conflict("payments.release_approval_not_required",
			"The run's total is within its company's release_dual_limit: one releaser releases it, with no second approval.")
	}
	if _, err := tx.Exec(ctx, `UPDATE payments.payment_run SET release_request_id = $2, release_approval = 'pending', release_request_total = $3,
		release_approved_by = NULL, release_approved_at = NULL, version = version + 1, updated_by = $4 WHERE id = $1`, r.ID, request, r.Total, p.ID); err != nil {
		return Run{}, fmt.Errorf("payments: submit the release of run %s: %w", r.ID, err)
	}
	submitted, err := load(ctx, tx, r.ID, false)
	if err != nil {
		return Run{}, err
	}
	return submitted, record(ctx, "payments.payment_run.submit", r.ID, map[string]any{"approval_request_id": request,
		"total_amount": money.EventAmount(r.Total, r.Currency, r.Exponent)})
}

// resubmittable refuses a second request while one waits, and one for a
// release already approved for the run's total.
func resubmittable(r Run) error {
	switch a := r.Release; {
	case a == nil:
		return nil
	case a.Status == ReleasePending:
		return problem.Conflict("payments.release_approval_pending", "The run's release already waits for approval request "+a.RequestID.String()+".")
	case a.Status == ReleaseApproved && r.Total <= a.Total:
		return problem.Conflict("payments.release_already_approved", "The run's release is approved already (approval request "+a.RequestID.String()+"); release it.")
	}
	return nil
}

// releaseApproved refuses the release of a run that needs a second
// person's approval and has none for its total, and its release by the
// person who approved it. A run that needs none is released by one person;
// a request still pending for it (the limit was raised, or the total fell
// to it) is withdrawn, so it is never approved after the release. A
// release resumed after its operation failed (the run releasing) was
// decided when it started, with the approval it needed then, and has
// payments posted: it is not measured again, or a limit set or lowered
// since would leave it half posted with no way to finish or reject it.
func (e *Endpoints) releaseApproved(ctx context.Context, tx pgx.Tx, r Run, p reqctx.Principal) error {
	a := r.Release
	if a != nil && a.ApprovedBy != nil && *a.ApprovedBy == p.ID {
		return problem.Forbidden("identity.sod_conflict", "Segregation of duties: whoever approved a payment run's release can never release it.")
	}
	if r.Status == Releasing {
		return nil
	}
	in, err := e.releaseRequest(ctx, r)
	if err != nil {
		return err
	}
	needed, err := e.Approvals.Required(ctx, in)
	if err != nil {
		return err
	}
	switch {
	case !needed && a != nil && a.Status == ReleasePending:
		return e.withdrawRelease(ctx, tx, p, r, "released by one person: the run is within its company's release_dual_limit")
	case !needed:
		return nil
	case a == nil || a.Status == ReleaseRejected || a.Status == ReleaseWithdrawn:
		return problem.Conflict("payments.release_approval_required",
			"The run's total is above its company's release_dual_limit: a second person approves its release first (POST /v1/payment-runs/"+r.ID.String()+":submit).")
	case a.Status == ReleasePending:
		return problem.Conflict("payments.release_approval_pending", "The run's release waits for approval request "+a.RequestID.String()+".")
	case r.Total > a.Total:
		return problem.Conflict("payments.release_approval_outdated", fmt.Sprintf("The release was approved for %s %s and the run's total is now %s %s; ask for its approval again.",
			a.Total.Plain(r.Exponent), r.Currency, r.Total.Plain(r.Exponent), r.Currency))
	}
	return nil
}

// withdrawRelease withdraws the run's pending release request: the run no
// longer waits for it (rejected, or released by one person). The workflow
// context does not call back (approvals.Withdraw), so the run records it
// here.
func (e *Endpoints) withdrawRelease(ctx context.Context, tx pgx.Tx, p reqctx.Principal, r Run, comment string) error {
	if err := e.Approvals.Withdraw(ctx, approvals.PaymentRunRelease, r.ID, comment); err != nil {
		return err
	}
	return endRelease(ctx, tx, p, r.ID, r.Release.RequestID, ReleaseWithdrawn, comment)
}

// endRelease records the end of the run's pending release request with
// the outcome, guarded by the request and its pending state: a second
// call changes nothing and fails, so a decision is never recorded twice.
func endRelease(ctx context.Context, tx pgx.Tx, p reqctx.Principal, run, request uuid.UUID, outcome, comment string) error {
	tag, err := tx.Exec(ctx, `UPDATE payments.payment_run SET release_approval = $3, version = version + 1, updated_by = $4
		WHERE id = $1 AND release_request_id = $2 AND release_approval = 'pending'`, run, request, outcome, p.ID)
	if err != nil {
		return fmt.Errorf("payments: record the %s release request %s of run %s: %w", outcome, request, run, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("payments: run %s does not wait for release request %s", run, request)
	}
	return record(ctx, "payments.payment_run.release_"+outcome, run, map[string]any{"approval_request_id": request, "comment": comment})
}

// requestOf is the release request a run's event names: the latest one
// when its outcome is one of the outcomes, or nil (none was asked for, or
// it ended otherwise).
func requestOf(r Run, outcomes ...string) *uuid.UUID {
	if r.Release == nil || !slices.Contains(outcomes, r.Release.Status) {
		return nil
	}
	return &r.Release.RequestID
}

// ReleaseApprovals records the outcome of a run's release request on the
// run (approvals.Handler for approvals.PaymentRunRelease), in the deciding
// transaction. The approval does not release the run: the release stays
// its own human step. A rejection rejects the run (product owner
// 2026-10-25), which needs the endpoints that free its items.
type ReleaseApprovals struct {
	Endpoints *Endpoints
}

// Approved records the second person's approval. Only a proposed run
// waiting for this request is changed (the run is locked, the update
// guarded), so a second callback for the request fails the decision.
func (ReleaseApprovals) Approved(ctx context.Context, run, request uuid.UUID) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	// The approver may not see the run's company: the request is what
	// authorises the decision, so the run is read without the filter.
	if _, err := load(ctx, tx, run, true); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE payments.payment_run SET release_approval = 'approved', release_approved_by = $3, release_approved_at = now(),
		version = version + 1, updated_by = $3 WHERE id = $1 AND release_request_id = $2 AND release_approval = 'pending' AND status = 'proposed'`, run, request, p.ID)
	if err != nil {
		return fmt.Errorf("payments: approve release request %s of run %s: %w", request, run, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("payments: run %s does not wait for release request %s", run, request)
	}
	return record(ctx, "payments.payment_run.release_approved", run, map[string]any{"approval_request_id": request})
}

// Rejected records the refusal and rejects the run, freeing its items, so
// a new run is proposed rather than the same one put to approver after
// approver (product owner 2026-10-25). Only a proposed run that waits for
// this request is changed: a callback run again after its commit finds
// the run rejected and changes nothing, and a request the run no longer
// waits for (it was withdrawn, or another asked for) fails the decision.
func (h ReleaseApprovals) Rejected(ctx context.Context, run, request uuid.UUID, comment string) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	r, err := load(ctx, tx, run, true)
	if err != nil {
		return err
	}
	if r.Status == Rejected && r.Release != nil && r.Release.Status == ReleaseRejected && r.Release.RequestID == request {
		return nil // this rejection was recorded already
	}
	if err := requireProposed(r, "rejected"); err != nil {
		return err
	}
	if err := endRelease(ctx, tx, p, run, request, ReleaseRejected, comment); err != nil {
		return err
	}
	_, err = h.Endpoints.reject(ctx, tx, p, run, "its release was rejected (approval request "+request.String()+"): "+comment)
	return err
}

// Cancelled records that the requester withdrew the request: the run
// stays proposed, to be asked for again or rejected.
func (ReleaseApprovals) Cancelled(ctx context.Context, run, request uuid.UUID, comment string) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	if _, err := load(ctx, tx, run, true); err != nil {
		return err
	}
	return endRelease(ctx, tx, p, run, request, ReleaseWithdrawn, comment)
}

// Barred bars the run's proposer from approving its release (segregation
// of duties: approver, proposer and releaser are three people); the
// workflow context bars the requester.
func (ReleaseApprovals) Barred(ctx context.Context, run, actor uuid.UUID) (bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return false, err
	}
	var barred bool
	// Runs are never deleted: the request's run is there.
	if err := tx.QueryRow(ctx, `SELECT proposed_by = $2 FROM payments.payment_run WHERE id = $1`, run, actor).Scan(&barred); err != nil {
		return false, fmt.Errorf("payments: who proposed run %s: %w", run, err)
	}
	return barred, nil
}
