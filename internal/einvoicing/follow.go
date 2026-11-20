package einvoicing

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Following a completed submission (ADR-0061 §9, As built E4): the send's
// step goes on reading what Peppol and the authority report of it, the
// buyer's access point's message level response and the tax data
// document's status, from pollFirst after completion doubling to pollMost,
// until both are final or maxPolls reads were made; and it makes a
// person's request of the tax data document (retry or withdraw), which the
// request wrote on the row as the outbox of the call. A request is claimed
// as a send is (its call counted and held for claimLease), and the call
// is made only if the provider does not show it made already, so a step
// run twice, or one whose claim lapsed, asks the provider once.

// claimFollow decides the step of a completed submission, read FOR UPDATE:
// no longer followed (done); a request whose Operation was cancelled
// (dropped once its claim lapsed, its Operation left cancelled); not due
// (the wait); moved since it was prepared (taken again at once); refused
// since it was prepared (a request ends with the refusal, a read counts as
// a failed one); a read (no claim: reading twice changes nothing); or a
// request's call, claimed.
func (e *Endpoints) claimFollow(ctx context.Context, tx pgx.Tx, s submission, operation uuid.UUID, prep *call) (*call, Step, error) {
	now := clock.Now(e.Now)
	if s.FollowCheckAt == nil {
		return nil, Step{Done: true}, nil
	}
	if s.TaxDataRequest != nil {
		cancelled, err := e.TaxData.Cancelled(ctx, operation, true)
		switch {
		case err != nil:
			return nil, Step{}, fmt.Errorf("einvoicing: the tax data request of outgoing e-invoice %s: %w", s.Filename, err)
		case cancelled && now.Before(*s.FollowCheckAt): // a claimed call may be under way, or its backoff runs
			return nil, Step{Wait: s.FollowCheckAt.Sub(now)}, nil
		case cancelled:
			return e.dropRequest(ctx, tx, s, now)
		}
	}
	switch {
	case now.Before(*s.FollowCheckAt):
		return nil, Step{Wait: s.FollowCheckAt.Sub(now)}, nil
	case prep == nil || prep.s.Version != s.Version:
		return nil, Step{}, nil
	}
	var err error
	if prep.company, err = e.companyNamed(ctx, s.LegalEntityID); err != nil {
		return nil, Step{}, err
	}
	switch {
	case prep.refused != nil && s.TaxDataRequest != nil:
		step, err := e.endRequest(ctx, tx, s, prep.company, now, "", false, prep.refused)
		return nil, step, err
	case prep.refused != nil: // the connector disabled since, its credentials refused: the provider holds the document still
		step, err := e.delivered(ctx, tx, s, prep.company, Delivery{}, prep.refused)
		return nil, step, err
	case s.TaxDataRequest == nil:
		return prep, Step{}, nil
	case s.TaxDataAttempts >= maxAttempts: // its last call's claim lapsed
		step, err := e.endRequest(ctx, tx, s, prep.company, now, "", false, requestUnavailable(s))
		return nil, step, err
	}
	if err := tx.QueryRow(ctx, `UPDATE einvoicing.outgoing_submission s SET tax_data_attempts = tax_data_attempts + 1, follow_check_at = $2,
		version = version + 1, updated_by = $3 WHERE id = $1 RETURNING `+submissionColumns, s.ID, now.Add(claimLease), SenderActor).Scan(prep.s.fields()...); err != nil {
		return nil, Step{}, fmt.Errorf("einvoicing: claim the tax data request of outgoing e-invoice %s: %w", s.Filename, err)
	}
	return prep, Step{}, nil
}

// askFollow reads the submission's delivery status and, for a person's
// request, makes it unless the tax data document shows it made already.
func (c *call) askFollow(ctx context.Context) (answer, error) {
	d, err := c.prov.Delivery(ctx, *c.s.TrackingID)
	if err != nil || c.s.TaxDataRequest == nil || requestMade(*c.s.TaxDataRequest, d.TaxData) {
		return answer{delivery: d}, err
	}
	if err := c.prov.TaxData(ctx, *c.s.TrackingID, *c.s.TaxDataRequest); err != nil {
		return answer{delivery: d}, err
	}
	return answer{delivery: d, acted: true}, nil
}

// requestMade reports whether the tax data document's status shows the
// request made: a retry once it is reported and no longer TDD_FAILED, a
// withdrawal once it is WITHDRAW_SEND; a document not reported (none) is
// not taken as one made.
func requestMade(request, status string) bool {
	if request == TaxDataRetry {
		return status != "" && status != TaxDataFailed
	}
	return status == TaxDataWithdrawSent
}

// followed records the answer of a completed submission's call, the row
// at the version the claim left it.
func (e *Endpoints) followed(ctx context.Context, tx pgx.Tx, s submission, co Company, got answer, callErr error) (Step, error) {
	if s.TaxDataRequest == nil {
		return e.delivered(ctx, tx, s, co, got.delivery, callErr)
	}
	now := clock.Now(e.Now)
	if callErr == nil { // made, or found made (its status kept): final, whatever the Operation says since
		return e.endRequest(ctx, tx, s, co, now, got.delivery.TaxData, got.acted, nil)
	}
	if p, refused := refusal(callErr); refused { // final too
		return e.endRequest(ctx, tx, s, co, now, "", false, p)
	}
	cancelled, err := e.TaxData.Cancelled(ctx, s.OperationID, true)
	switch {
	case err != nil:
		return Step{}, fmt.Errorf("einvoicing: the tax data request of outgoing e-invoice %s: %w", s.Filename, err)
	case cancelled:
		_, step, err := e.dropRequest(ctx, tx, s, now)
		return step, err
	case s.TaxDataAttempts >= maxAttempts:
		return e.endRequest(ctx, tx, s, co, now, "", false, requestUnavailable(s))
	}
	return e.move(ctx, tx, s, co, `follow_check_at = $3`, now.Add(sendBackoff[s.TaxDataAttempts-1]))
}

// delivered records a delivery read: what it reports (a status not
// reported keeps the one recorded), the reads counted, and the next read
// after the backoff, none once both statuses are final or after maxPolls
// reads; after a request made, not before the tax data document's status
// moves from the one the call left (tax_data_awaited: the provider may
// show the outcome later). A read that failed, or that the connector or
// the provider refused, records nothing but the count.
func (e *Endpoints) delivered(ctx context.Context, tx pgx.Tx, s submission, co Company, d Delivery, callErr error) (Step, error) {
	now := clock.Now(e.Now)
	response, detail, taxData := s.DeliveryStatus, s.DeliveryDetail, s.TaxDataStatus
	if callErr == nil && d.Response != "" {
		response, detail = &d.Response, nonEmpty(d.Detail)
	}
	if callErr == nil && d.TaxData != "" {
		taxData = &d.TaxData
	}
	var next *time.Time
	awaiting := s.TaxDataAwaited != nil && deref(taxData) == *s.TaxDataAwaited
	if final := responseFinal(deref(response)) && taxDataFinal(deref(taxData)) && !awaiting; !final && s.FollowPolls+1 < maxPolls {
		at := now.Add(pollWait(s.FollowPolls + 1))
		next = &at
	}
	return e.move(ctx, tx, s, co, `delivery_status = $3, delivery_detail = $4,
		delivered_at = CASE WHEN (delivery_status, delivery_detail) IS DISTINCT FROM ($3, $4) THEN $5 ELSE delivered_at END, tax_data_status = $6,
		tax_data_at = CASE WHEN tax_data_status IS DISTINCT FROM $6 THEN $5 ELSE tax_data_at END, follow_polls = follow_polls + 1, follow_check_at = $7,
		tax_data_awaited = CASE WHEN $8 THEN tax_data_awaited END`, response, detail, now, taxData, next, awaiting)
}

// endRequest ends a person's request of the tax data document: cleared,
// the tax data document's status as the step read it kept (none: the one
// recorded), the delivery status read again at once and its reads counted
// afresh, and its Operation succeeded (why nil) or failed with why. A
// request this step made (acted) awaits a move of the status it found.
func (e *Endpoints) endRequest(ctx context.Context, tx pgx.Tx, s submission, co Company, now time.Time, taxData string, acted bool, why *problem.Problem) (
	Step, error) {
	step, err := e.move(ctx, tx, s, co, `tax_data_request = NULL, tax_data_attempts = 0, follow_polls = 0, follow_check_at = $3,
		tax_data_status = coalesce($4, tax_data_status), tax_data_at = CASE WHEN tax_data_status IS DISTINCT FROM coalesce($4, tax_data_status) THEN $3
		ELSE tax_data_at END, tax_data_awaited = CASE WHEN $5 THEN coalesce($4, tax_data_status) END`, now, nonEmpty(taxData), acted)
	if err != nil {
		return Step{}, err
	}
	if why != nil {
		err = e.TaxData.Fail(ctx, s.OperationID, why)
	} else {
		err = e.TaxData.Succeed(ctx, s.OperationID, outgoingPath(s.OutgoingID),
			map[string]any{"outgoing_einvoice_id": s.OutgoingID, "tax_data_request": *s.TaxDataRequest})
	}
	if problem.From(err).Code == "platform.operation_finished" {
		return step, nil
	}
	return step, err
}

// dropRequest clears a request whose Operation was cancelled before its
// call was made, or after one that failed: the Operation stays cancelled,
// and the delivery status is read again at once.
func (e *Endpoints) dropRequest(ctx context.Context, tx pgx.Tx, s submission, now time.Time) (*call, Step, error) {
	co, err := e.companyNamed(ctx, s.LegalEntityID)
	if err != nil {
		return nil, Step{}, err
	}
	step, err := e.move(ctx, tx, s, co, `tax_data_request = NULL, tax_data_attempts = 0, follow_polls = 0, follow_check_at = $3`, now)
	return nil, step, err
}

// requestUnavailable is why a request fails after its last call failed.
func requestUnavailable(s submission) *problem.Problem {
	return problem.Unprocessable("einvoicing.provider_unavailable", fmt.Sprintf("The provider did not take the %s of e-invoice %s's tax data document in %d calls; ask again.",
		*s.TaxDataRequest, s.Filename, s.TaxDataAttempts))
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
