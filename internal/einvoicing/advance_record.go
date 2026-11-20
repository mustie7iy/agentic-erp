package einvoicing

import (
	"context"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// answered records what the provider answered the call, if the row is as
// the claim left it; otherwise another step moved it since and the next
// step reads it as it is. A send cancelled during the call ends the
// submission with the answer kept (cancel).
func (e *Endpoints) answered(ctx context.Context, tx pgx.Tx, c *call, got answer, callErr error) (Step, error) {
	s, _, err := submissionOf(ctx, tx, c.s.OutgoingID, true) // a submission is never deleted
	if err != nil || s.Version != c.s.Version {
		return Step{}, err
	}
	if c.follow {
		return e.followed(ctx, tx, s, c.company, got.delivery, callErr)
	}
	cancelled, err := e.Sends.Cancelled(ctx, s.OperationID, true)
	switch {
	case err != nil:
		return Step{}, fmt.Errorf("einvoicing: the send of outgoing e-invoice %s: %w", s.Filename, err)
	case cancelled && !final(c.send, got, callErr):
		return e.cancel(ctx, tx, s, clock.Now(e.Now), got.kept(c.send, callErr))
	case c.send: // a final answer is recorded as it is, cancelled or not: its Operation stays cancelled (endOperation)
		return e.sent(ctx, tx, s, c.company, got.sent, callErr)
	}
	return e.read(ctx, tx, s, c.company, got.report, callErr)
}

// outcome is what the provider answered of a submission, kept on it when
// it fails: its status and tracking id, nil for those recorded already.
type outcome struct {
	status, tracking *string
}

// final reports whether the provider's answer ends the submission
// whatever the send's Operation says: Evatra refusing the document, or a
// status read of COMPLETED, FAILED or PERMANENT_FAILED. A cancel does not
// override it (review round 2): a delivered e-invoice stays completed.
func final(send bool, got answer, callErr error) bool {
	if send {
		_, refused := refusal(callErr)
		return refused
	}
	if callErr != nil {
		return false
	}
	switch got.report.Status {
	case ProviderCompleted, ProviderFailed, ProviderPermanentFailed:
		return true
	}
	return false
}

// kept is the outcome of the answer to a send (with its tracking id) or a
// status read; none when the call failed.
func (a answer) kept(send bool, callErr error) *outcome {
	switch {
	case callErr != nil:
		return &outcome{}
	case send:
		return &outcome{status: &a.sent.Status, tracking: &a.sent.TrackingID}
	}
	return &outcome{status: &a.report.Status}
}

// sent records a send's outcome: the tracking id (submitted, its status
// read first after pollFirst); a refusal or the last call's failure
// (failed); or another call after the backoff (queued).
func (e *Endpoints) sent(ctx context.Context, tx pgx.Tx, s submission, co Company, got Submission, callErr error) (Step, error) {
	now := clock.Now(e.Now)
	p, refused := refusal(callErr)
	switch {
	case callErr == nil:
		return e.move(ctx, tx, s, co, `status = 'submitted', tracking_id = $3, provider_status = $4, polls = 0, submitted_at = $5, next_check_at = $6`,
			got.TrackingID, got.Status, now, now.Add(pollWait(0)))
	case refused:
		return Step{Done: true}, e.fail(ctx, tx, s, co, p, outcome{})
	case s.Attempts >= maxAttempts:
		return Step{Done: true}, e.fail(ctx, tx, s, co, unavailable(s), outcome{})
	}
	return e.move(ctx, tx, s, co, `status = 'queued', next_check_at = $3`, now.Add(sendBackoff[s.Attempts-1]))
}

// read records a status read: COMPLETED completes the submission and its
// Operation; FAILED and PERMANENT_FAILED fail it; SENDING_FAILED queues it
// to be sent again (looked up first, the failed tracking id kept so it is
// not taken as found), up to maxResends times; any other status, or a read
// that failed, is read again after the backoff, up to maxPolls reads. A
// read refused (the connector disabled since, the provider refusing the
// credentials) is a read that failed too: the provider holds the document,
// so it is not failed for it, and a person may correct the connector
// meanwhile.
func (e *Endpoints) read(ctx context.Context, tx pgx.Tx, s submission, co Company, r Report, callErr error) (Step, error) {
	now := clock.Now(e.Now)
	status := s.ProviderStatus
	if callErr == nil {
		status = &r.Status
		switch r.Status {
		case ProviderCompleted:
			return e.complete(ctx, tx, s, co, r.Status, now)
		case ProviderPermanentFailed:
			return Step{Done: true}, e.fail(ctx, tx, s, co, problem.Unprocessable("einvoicing.permanent_failure", "The provider reports e-invoice "+
				s.Filename+" PERMANENT_FAILED: it will not deliver it; correct it (a credit note and a new invoice) or send it again."), outcome{status: status})
		case ProviderFailed:
			return Step{Done: true}, e.fail(ctx, tx, s, co, problem.Unprocessable("einvoicing.provider_failed", "The provider reports e-invoice "+
				s.Filename+" FAILED; check it with the provider and send it again."), outcome{status: status})
		case ProviderSendingFailed:
			return e.resend(ctx, tx, s, co, now)
		}
	}
	if s.Polls+1 >= maxPolls {
		return Step{Done: true}, e.fail(ctx, tx, s, co, problem.Unprocessable("einvoicing.status_unconfirmed", fmt.Sprintf(
			"The provider did not report e-invoice %s finished in %d status reads; check it with the provider (tracking id %s).", s.Filename, maxPolls,
			*s.TrackingID)), outcome{status: status})
	}
	return e.move(ctx, tx, s, co, `polls = polls + 1, provider_status = $3, next_check_at = $4`, status, now.Add(pollWait(s.Polls+1)))
}

// resend queues the submission to be sent again after the provider failed
// to send it, or fails it after the last resend.
func (e *Endpoints) resend(ctx context.Context, tx pgx.Tx, s submission, co Company, now time.Time) (Step, error) {
	failed := ProviderSendingFailed
	if s.Resends >= maxResends {
		return Step{Done: true}, e.fail(ctx, tx, s, co, problem.Unprocessable("einvoicing.sending_failed", fmt.Sprintf(
			"The provider failed to send e-invoice %s %d times; check it with the provider and send it again.", s.Filename, s.Resends+1)), outcome{status: &failed})
	}
	return e.move(ctx, tx, s, co, `status = 'queued', provider_status = $3, resends = resends + 1, attempts = 0, polls = 0, look_up = true,
		failed_tracking_ids = failed_tracking_ids || ARRAY[tracking_id], next_check_at = $4`, failed, now)
}

// complete records the provider's COMPLETED and succeeds the Operation;
// what Peppol and the authority report of it is read from pollFirst on
// (As built E4).
func (e *Endpoints) complete(ctx context.Context, tx pgx.Tx, s submission, co Company, status string, now time.Time) (Step, error) {
	step, err := e.move(ctx, tx, s, co, `status = 'completed', provider_status = $3, polls = polls + 1, completed_at = $4, next_check_at = NULL,
		follow_check_at = $5`, status, now, now.Add(pollFirst))
	if err != nil {
		return Step{}, err
	}
	return step, e.endOperation(ctx, s.OperationID, s.OutgoingID, nil)
}

// fail records the submission failed with the problem (and what the
// provider answered, kept) and fails its Operation with it.
func (e *Endpoints) fail(ctx context.Context, tx pgx.Tx, s submission, co Company, p *problem.Problem, kept outcome) error {
	if _, err := e.move(ctx, tx, s, co, `status = 'failed', problem_code = $3, problem_detail = $4, provider_status = coalesce($5, provider_status),
		tracking_id = coalesce($6, tracking_id), next_check_at = NULL`, p.Code, clipBytes(p.Detail, 1000), kept.status, kept.tracking); err != nil {
		return err
	}
	return e.endOperation(ctx, s.OperationID, uuid.Nil, p)
}

// clipBytes is text at most n bytes long, cut at a character boundary.
func clipBytes(text string, n int) string {
	if len(text) <= n {
		return text
	}
	for n > 0 && !utf8.RuneStart(text[n]) {
		n--
	}
	return text[:n]
}

// move updates the submission by set (whose own arguments start at $3),
// moving its version, and audits and publishes the change when its status
// or the provider's moved. It answers the step: done once finished, or the
// wait until its next check.
func (e *Endpoints) move(ctx context.Context, tx pgx.Tx, s submission, co Company, set string, args ...any) (Step, error) {
	var after submission
	if err := tx.QueryRow(ctx, `UPDATE einvoicing.outgoing_submission s SET `+set+`, version = version + 1, updated_by = $2 WHERE id = $1
		RETURNING `+submissionColumns, append([]any{s.ID, SenderActor}, args...)...).Scan(after.fields()...); err != nil {
		return Step{}, fmt.Errorf("einvoicing: record the submission of outgoing e-invoice %s: %w", s.Filename, err)
	}
	if after.Status != s.Status || !samePtr(after.ProviderStatus, s.ProviderStatus) || !samePtr(after.DeliveryStatus, s.DeliveryStatus) ||
		!samePtr(after.TaxDataStatus, s.TaxDataStatus) || !samePtr(after.TaxDataRequest, s.TaxDataRequest) {
		if err := after.record(ctx, "einvoicing.outgoing.transmission", co, &s.Status); err != nil {
			return Step{}, err
		}
	}
	return after.step(clock.Now(e.Now)), nil
}

// step is where the submission stands at now: done once nothing is due
// (finished, or completed and no longer followed), or the wait until its
// next check.
func (s submission) step(now time.Time) Step {
	next := s.NextCheckAt
	if s.Status == SubmissionCompleted {
		next = s.FollowCheckAt
	}
	if next == nil {
		return Step{Done: true}
	}
	return Step{Wait: max(next.Sub(now), 0)}
}

func samePtr(a, b *string) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

// endOperation succeeds the send's Operation with the e-invoice as its
// resource, or fails it with why; one already finished is left as it is.
func (e *Endpoints) endOperation(ctx context.Context, operation, outgoing uuid.UUID, why *problem.Problem) error {
	var err error
	if why != nil {
		err = e.Sends.Fail(ctx, operation, why)
	} else {
		err = e.Sends.Succeed(ctx, operation, outgoingPath(outgoing), map[string]any{"outgoing_einvoice_id": outgoing, "status": SubmissionCompleted})
	}
	if problem.From(err).Code == "platform.operation_finished" {
		return nil
	}
	return err
}
