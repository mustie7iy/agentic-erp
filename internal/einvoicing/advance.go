package einvoicing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Step is where a send stands after AdvanceSubmission: finished (Done), or
// to be advanced again after Wait.
type Step struct {
	Done bool
	Wait time.Duration
}

// call is what a step asks the provider, prepared with no transaction
// open (the connector resolved, the XML read) and decided in the claim's
// transaction: a send (with the document looked up first, lookUp), a
// status read, or, once completed, a delivery read and a person's request
// of the tax data document (follow, As built E4), of the submission at its
// version.
type call struct {
	s       submission
	company Company
	send    bool
	follow  bool
	lookUp  bool
	prov    Provider
	conn    Connector
	doc     OutgoingEInvoice
	content []byte
	refused *problem.Problem // the connector or the XML refused since: the submission fails
}

// AdvanceSubmission does the next step of the e-invoice's submission for
// the send's operation (As built E3), each part in a transaction of in,
// none open while the connector's secret is resolved, the store read or
// the provider called. The step is prepared first, from the submission as
// read: a failure of the machinery there changes nothing and is returned,
// to be tried again, so a send cancelled while the vault or the store
// fails stays in progress until they answer. Then, under the row's lock,
// a submission whose send was cancelled is ended (cancel); a queued
// submission (or one whose claim lapsed, its outcome unknown) is claimed,
// its call counted, and sent, looked up first when a call was made
// before; a submitted one has its status read once due. What the provider
// answered is recorded only if the row is as the claim left it (its
// version), so a step run twice, or after another, records nothing twice.
// A finished submission's Operation ended in the transaction that
// finished it. A submission another send owns now (a person's new send
// after it failed) is not this operation's: the step is done.
func (e *Endpoints) AdvanceSubmission(ctx context.Context, outgoing, operation uuid.UUID, in Within) (Step, error) {
	prep, err := e.prepare(ctx, outgoing, in)
	if err != nil {
		return Step{}, err
	}
	var c *call
	var step Step
	if err := inTx(ctx, in, func(ctx context.Context, tx pgx.Tx, _ reqctx.Principal) error {
		var err error
		c, step, err = e.claim(ctx, tx, outgoing, operation, prep)
		return err
	}); err != nil || c == nil {
		return step, err
	}
	got, callErr := c.ask(ctx)
	if ctx.Err() != nil { // the worker stopping: the claim lapses, and the next step looks the document up
		return Step{}, fmt.Errorf("einvoicing: advance the submission of outgoing e-invoice %s: %w", outgoing, ctx.Err())
	}
	err = inTx(ctx, in, func(ctx context.Context, tx pgx.Tx, _ reqctx.Principal) error {
		var err error
		step, err = e.answered(ctx, tx, c, got, callErr)
		return err
	})
	return step, err
}

// isProblem reports whether err is a problem, not a failure of the
// machinery.
func isProblem(err error) bool {
	var p *problem.Problem
	return errors.As(err, &p)
}

// refusal is err when it is a problem below 500, which another call would
// meet again.
func refusal(err error) (*problem.Problem, bool) {
	p := problem.From(err)
	return p, isProblem(err) && p.Status < http.StatusInternalServerError
}

// prepare reads the submission and, when it is due for a call, reads its
// connector, resolves its credentials once that read committed, and, for a
// send, reads the XML as kept: nil when no call is due (the claim decides
// what then). A refusal is kept on the call for the claim to decide on (a
// send fails, a status read counts as failed); a failure of the machinery
// is returned.
func (e *Endpoints) prepare(ctx context.Context, outgoing uuid.UUID, in Within) (*call, error) {
	var s submission
	var found bool
	if err := inTx(ctx, in, func(ctx context.Context, tx pgx.Tx, _ reqctx.Principal) error {
		var err error
		s, found, err = submissionOf(ctx, tx, outgoing, false)
		return err
	}); err != nil {
		return nil, err
	}
	due := s.NextCheckAt
	if s.Status == SubmissionCompleted {
		due = s.FollowCheckAt
	}
	if !found || due == nil || clock.Now(e.Now).Before(*due) {
		return nil, nil
	}
	c := &call{s: s, send: s.Status == SubmissionQueued || s.Status == SubmissionSending, follow: s.Status == SubmissionCompleted}
	var err error
	if c.conn, c.prov, _, err = e.provider(ctx, s.ConnectorID, in); err == nil && c.send {
		c.doc, c.content, err = e.kept(ctx, outgoing, in)
	}
	if p, refused := refusal(err); refused {
		c.refused = p
		return c, nil
	}
	return c, err
}

// claim reads the submission FOR UPDATE and decides its step: finished
// (its Operation ended), its send cancelled (ended at once, due or not,
// which is why UnfinishedSends lists the cancelled ones whatever their
// next check), not due (the wait),
// moved since it was prepared (taken again at once), refused since (a
// send failed, a status read counted as a failed read), or a call to
// make, claimed.
func (e *Endpoints) claim(ctx context.Context, tx pgx.Tx, outgoing, operation uuid.UUID, prep *call) (*call, Step, error) {
	s, found, err := submissionOf(ctx, tx, outgoing, true)
	if err != nil {
		return nil, Step{}, err
	}
	if !found || s.OperationID != operation {
		return nil, Step{Done: true}, e.endOperation(ctx, operation, uuid.Nil, notOwned(operation))
	}
	switch s.Status {
	case SubmissionFailed: // its Operation ended in the same transaction
		return nil, Step{Done: true}, nil
	case SubmissionCompleted:
		return e.claimFollow(ctx, tx, s, operation, prep)
	}
	now := clock.Now(e.Now)
	cancelled, err := e.Sends.Cancelled(ctx, operation, true)
	switch {
	case err != nil:
		return nil, Step{}, fmt.Errorf("einvoicing: the send of outgoing e-invoice %s: %w", s.Filename, err)
	case cancelled:
		step, err := e.cancel(ctx, tx, s, now, nil)
		return nil, step, err
	case now.Before(*s.NextCheckAt):
		return nil, Step{Wait: s.NextCheckAt.Sub(now)}, nil
	case prep == nil || prep.s.Version != s.Version:
		return nil, Step{}, nil
	}
	if prep.company, err = e.companyNamed(ctx, s.LegalEntityID); err != nil {
		return nil, Step{}, err
	}
	switch {
	case prep.refused != nil && !prep.send: // a status read refused (the connector disabled, the provider refusing): the provider holds it still
		step, err := e.read(ctx, tx, s, prep.company, Report{}, prep.refused)
		return nil, step, err
	case prep.refused != nil:
		return nil, Step{Done: true}, e.fail(ctx, tx, s, prep.company, prep.refused, outcome{})
	case !prep.send:
		return prep, Step{}, nil
	case s.Attempts >= maxAttempts: // its last call's claim lapsed
		return nil, Step{Done: true}, e.fail(ctx, tx, s, prep.company, unavailable(s), outcome{})
	}
	return prep, Step{}, e.claimSend(ctx, tx, prep, now)
}

// cancel ends a submission whose send's Operation was cancelled: one with
// a call claimed whose claim has not lapsed waits for it, as the call may
// be under way still; otherwise the submission fails with
// einvoicing.send_cancelled, keeping what the provider answered (got), so
// a person may send it again, looked up first since a call may have
// reached the provider. Its Operation stays cancelled.
func (e *Endpoints) cancel(ctx context.Context, tx pgx.Tx, s submission, now time.Time, got *outcome) (Step, error) {
	if got == nil && s.Status == SubmissionSending && now.Before(*s.NextCheckAt) {
		return Step{Wait: s.NextCheckAt.Sub(now)}, nil
	}
	co, err := e.companyNamed(ctx, s.LegalEntityID)
	if err != nil {
		return Step{}, err
	}
	var kept outcome
	if got != nil {
		kept = *got
	}
	return Step{Done: true}, e.fail(ctx, tx, s, co, problem.Conflict("einvoicing.send_cancelled", "Send "+s.OperationID.String()+" of e-invoice "+
		s.Filename+" was cancelled; it is not followed further. Send it again to transmit it: it is looked up at the provider first."), kept)
}

// claimSend counts the call and holds it for claimLease; a submission
// claimed before (its claim lapsed, the outcome unknown) or with a call
// made before is looked up before it is sent.
func (e *Endpoints) claimSend(ctx context.Context, tx pgx.Tx, c *call, now time.Time) error {
	before := c.s.Status
	c.lookUp = c.s.LookUp || before == SubmissionSending
	if err := tx.QueryRow(ctx, `UPDATE einvoicing.outgoing_submission s SET status = 'sending', attempts = attempts + 1, look_up = true,
		next_check_at = $2, version = version + 1, updated_by = $3 WHERE id = $1 RETURNING `+submissionColumns,
		c.s.ID, now.Add(claimLease), SenderActor).Scan(c.s.fields()...); err != nil {
		return fmt.Errorf("einvoicing: claim the submission of outgoing e-invoice %s: %w", c.s.Filename, err)
	}
	if before == SubmissionSending { // claimed again after a lapse: its status does not move
		return nil
	}
	return c.s.record(ctx, "einvoicing.outgoing.transmission", c.company, &before)
}

// notOwned is why a send's Operation ends when its e-invoice's submission
// belongs to another send.
func notOwned(operation uuid.UUID) *problem.Problem {
	return problem.Conflict("einvoicing.transmission_superseded", "Send "+operation.String()+" no longer owns the e-invoice's submission; a later send does.")
}

// unavailable is why a submission fails after its last call failed.
func unavailable(s submission) *problem.Problem {
	return problem.Unprocessable("einvoicing.provider_unavailable", fmt.Sprintf("The provider did not take e-invoice %s in %d calls; send it again.",
		s.Filename, s.Attempts))
}

// answer is the provider's answer to a call.
type answer struct {
	sent     Submission
	report   Report
	delivery Delivery
	acted    bool // a person's request made at the provider by this call
}

// ask makes the prepared call with no transaction open: the send of the
// XML as kept, or the status read.
func (c *call) ask(ctx context.Context) (answer, error) {
	if c.follow {
		return c.askFollow(ctx)
	}
	if !c.send {
		r, err := c.prov.Status(ctx, *c.s.TrackingID)
		return answer{report: r}, err
	}
	sent, err := c.prov.Send(ctx, Outgoing{Filename: c.doc.CustomerInvoiceNumber, Participant: c.conn.Participants[c.company.Code],
		BusinessType: c.s.BusinessType, Content: c.content, LookUp: c.lookUp, Failed: c.s.Failed})
	return answer{sent: sent}, err
}

// kept reads the valid e-invoice's XML from the store, compared with its
// SHA-256, in a read of its own: its row is final.
func (e *Endpoints) kept(ctx context.Context, id uuid.UUID, in Within) (OutgoingEInvoice, []byte, error) {
	if e.Objects == nil {
		return OutgoingEInvoice{}, nil, noStore()
	}
	o, err := outgoingRow(ctx, id, in)
	if err != nil {
		return o, nil, err
	}
	content, _, err := e.Objects.Get(ctx, *o.ContentRef) // a submission's e-invoice is valid, so it has its content_ref
	if err != nil {
		return o, nil, fmt.Errorf("einvoicing: read the XML of outgoing e-invoice %s: %w", id, err)
	}
	if sum := sha256.Sum256(content); !bytes.Equal(sum[:], o.SHA256) {
		return o, nil, problem.Conflict("einvoicing.document_changed", "The stored XML of outgoing e-invoice "+o.CustomerInvoiceNumber+
			" does not match the SHA-256 recorded when it was rendered; it is not sent.")
	}
	return o, content, nil
}

// outgoingRow reads an e-invoice by id with no company check: the send
// acts for its company.
func outgoingRow(ctx context.Context, id uuid.UUID, in Within) (OutgoingEInvoice, error) {
	var o OutgoingEInvoice
	err := inTx(ctx, in, func(ctx context.Context, tx pgx.Tx, _ reqctx.Principal) error {
		return tx.QueryRow(ctx, `SELECT `+outgoingColumns+outgoingFrom+` WHERE o.id = $1`, id).Scan(o.fields()...)
	})
	if err != nil {
		return o, fmt.Errorf("einvoicing: read outgoing e-invoice %s: %w", id, err)
	}
	return o, nil
}
