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

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Step is where a send stands after AdvanceSubmission: finished (Done), or
// to be advanced again after Wait.
type Step struct {
	Done bool
	Wait time.Duration
}

// call is what a step asks the provider, decided in the claim's
// transaction and made with none open: a send (with the document looked
// up first, lookUp) or a status read, of the submission at its version.
type call struct {
	s       submission
	company Company
	send    bool
	lookUp  bool
}

// AdvanceSubmission does the next step of the e-invoice's submission for
// the send's operation (As built E3), each part in a transaction of in,
// none open while the connector's secret is resolved or the provider is
// called: a queued submission (or one whose claim lapsed, its outcome
// unknown) is claimed, its call counted, and sent, looked up first when a
// call was made before; a submitted one has its status read once due.
// What the provider answered is recorded only if the row is as the claim
// left it (its version), so a step run twice, or after another, records
// nothing twice. A finished submission ends its Operation, which the step
// does again harmlessly. A submission another send owns now (a person's
// new send after it failed) is not this operation's: the step is done.
func (e *Endpoints) AdvanceSubmission(ctx context.Context, outgoing, operation uuid.UUID, in Within) (Step, error) {
	var c *call
	var step Step
	if err := inTx(ctx, in, func(ctx context.Context, tx pgx.Tx, _ reqctx.Principal) error {
		var err error
		c, step, err = e.claim(ctx, tx, outgoing, operation)
		return err
	}); err != nil || c == nil {
		return step, err
	}
	got, callErr := e.ask(ctx, c, in)
	if ctx.Err() != nil { // the worker stopping: the claim lapses, and the next step looks the document up
		return Step{}, fmt.Errorf("einvoicing: advance the submission of outgoing e-invoice %s: %w", outgoing, ctx.Err())
	}
	err := inTx(ctx, in, func(ctx context.Context, tx pgx.Tx, _ reqctx.Principal) error {
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

// claim reads the submission FOR UPDATE and decides its step: finished
// (its Operation ended), not due (the wait), or a call to make, claimed.
func (e *Endpoints) claim(ctx context.Context, tx pgx.Tx, outgoing, operation uuid.UUID) (*call, Step, error) {
	s, found, err := submissionOf(ctx, tx, outgoing, true)
	if err != nil {
		return nil, Step{}, err
	}
	if !found || s.OperationID != operation {
		return nil, Step{Done: true}, e.endOperation(ctx, operation, uuid.Nil, notOwned(operation))
	}
	now := e.now()
	switch {
	case s.Status == SubmissionCompleted || s.Status == SubmissionFailed:
		return nil, Step{Done: true}, e.ended(ctx, s)
	case now.Before(*s.NextCheckAt):
		return nil, Step{Wait: s.NextCheckAt.Sub(now)}, nil
	}
	company, err := e.companyNamed(ctx, s.LegalEntityID)
	if err != nil {
		return nil, Step{}, err
	}
	c := &call{s: s, company: company, send: s.Status != SubmissionSubmitted}
	if !c.send {
		return c, Step{}, nil
	}
	if s.Attempts >= maxAttempts { // its last call's claim lapsed
		return nil, Step{Done: true}, e.fail(ctx, tx, s, company, unavailable(s), nil)
	}
	return c, Step{}, e.claimSend(ctx, tx, c, now)
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
	sent   Submission
	report Report
}

// ask makes the call with no transaction open: the connector resolved in
// a transaction of its own, then the send of the XML as kept (its SHA-256
// compared) or the status read.
func (e *Endpoints) ask(ctx context.Context, c *call, in Within) (answer, error) {
	conn, prov, _, err := e.provider(ctx, c.s.ConnectorID, in)
	if err != nil {
		return answer{}, err
	}
	if !c.send {
		r, err := prov.Status(ctx, *c.s.TrackingID)
		return answer{report: r}, err
	}
	o, content, err := e.kept(ctx, c.s.OutgoingID, in)
	if err != nil {
		return answer{}, err
	}
	sent, err := prov.Send(ctx, Outgoing{Filename: o.CustomerInvoiceNumber, Participant: conn.Participants[c.company.Code],
		BusinessType: c.s.BusinessType, Content: content, LookUp: c.lookUp, Failed: c.s.Failed})
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
