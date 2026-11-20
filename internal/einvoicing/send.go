package einvoicing

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Sending outgoing e-invoices (ADR-0061 §3, §9, As built E3): a person
// asks for a valid e-invoice to be sent through an einvoice_provider
// connector; the request writes its submission, the outbox of the call,
// with the Operation that carries it out, and the send workflow then
// advances it one step at a time (AdvanceSubmission): it sends, reads the
// provider's status with backoff, sends again after SENDING_FAILED, and
// ends it completed or failed, the failed ones a worklist.

// KindSend is the kind of a send's Operation.
const KindSend = "einvoicing.send"

// Statuses of a submission.
const (
	SubmissionQueued    = "queued"
	SubmissionSending   = "sending"
	SubmissionSubmitted = "submitted"
	SubmissionCompleted = "completed"
	SubmissionFailed    = "failed"
)

// SenderActor is the principal the send workflow acts as, in every
// tenant: a system principal acting for the e-invoice's company.
var SenderActor = uuid.NewSHA1(uuid.NameSpaceURL, []byte("urn:agentic-erp:einvoice-sender"))

// Sender is the principal a send acts as in the tenant.
func Sender(tenant uuid.UUID) reqctx.Principal {
	return reqctx.Principal{ID: SenderActor, Kind: reqctx.KindSystem, TenantID: tenant}
}

// The send's bounds: five calls with backoff between them (sendBackoff),
// then failed; three sends again after SENDING_FAILED; status reads from
// pollFirst doubling to pollMost, at most maxPolls of them (about three
// weeks), then failed as unconfirmed; a claimed call lapses after
// claimLease, its outcome unknown, and the next step looks the document up.
var sendBackoff = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour}

const (
	maxAttempts = 5
	maxResends  = 3
	maxPolls    = 500
	pollFirst   = 30 * time.Second
	pollMost    = time.Hour
	claimLease  = 5 * CallTimeout // a login, a look-up and the send, each up to CallTimeout, with room to record
)

// pollWait is the wait before the status read after polls reads.
func pollWait(polls int32) time.Duration {
	wait := pollFirst
	for i := int32(0); i < polls && wait < pollMost; i++ {
		wait *= 2
	}
	return min(wait, pollMost)
}

// submission is a row of einvoicing.outgoing_submission.
type submission struct {
	ID, OutgoingID, LegalEntityID, ConnectorID, OperationID uuid.UUID
	Filename, BusinessType, Status                          string
	ProviderStatus, TrackingID                              *string
	Failed                                                  []string
	Attempts, Resends                                       int16
	Polls                                                   int32
	LookUp                                                  bool
	NextCheckAt, SubmittedAt, CompletedAt                   *time.Time
	ProblemCode, ProblemDetail                              *string
	Version                                                 int32
	// What Peppol and the authority reported once completed (As built E4).
	DeliveryStatus, DeliveryDetail, TaxDataStatus, TaxDataRequest *string
	DeliveredAt, TaxDataAt, FollowCheckAt                         *time.Time
	TaxDataAttempts                                               int16
	FollowPolls                                                   int32
	Outcome                                                       *string // the delivery's outcome, as a read of the e-invoices gives it
}

const submissionColumns = `s.id, s.outgoing_einvoice_id, s.legal_entity_id, s.connector_id, s.operation_id, s.filename, s.business_type, s.status,
	s.provider_status, s.tracking_id, s.failed_tracking_ids, s.attempts, s.resends, s.polls, s.look_up, s.next_check_at, s.submitted_at, s.completed_at,
	s.problem_code, s.problem_detail, s.version, s.delivery_status, s.delivery_detail, s.tax_data_status, s.tax_data_request, s.delivered_at, s.tax_data_at,
	s.follow_check_at, s.tax_data_attempts, s.follow_polls`

func (s *submission) fields() []any {
	return []any{&s.ID, &s.OutgoingID, &s.LegalEntityID, &s.ConnectorID, &s.OperationID, &s.Filename, &s.BusinessType, &s.Status, &s.ProviderStatus,
		&s.TrackingID, &s.Failed, &s.Attempts, &s.Resends, &s.Polls, &s.LookUp, &s.NextCheckAt, &s.SubmittedAt, &s.CompletedAt, &s.ProblemCode,
		&s.ProblemDetail, &s.Version, &s.DeliveryStatus, &s.DeliveryDetail, &s.TaxDataStatus, &s.TaxDataRequest, &s.DeliveredAt, &s.TaxDataAt,
		&s.FollowCheckAt, &s.TaxDataAttempts, &s.FollowPolls}
}

// submissionOf reads the e-invoice's submission, FOR UPDATE when lock;
// found false for none.
func submissionOf(ctx context.Context, tx pgx.Tx, outgoing uuid.UUID, lock bool) (submission, bool, error) {
	q := `SELECT ` + submissionColumns + ` FROM einvoicing.outgoing_submission s WHERE s.outgoing_einvoice_id = $1`
	if lock {
		q += ` FOR UPDATE`
	}
	var s submission
	err := tx.QueryRow(ctx, q, outgoing).Scan(s.fields()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, false, nil
	}
	if err != nil {
		return s, false, fmt.Errorf("einvoicing: read the submission of outgoing e-invoice %s: %w", outgoing, err)
	}
	return s, true, nil
}

// UnfinishedSends lists, in the transaction's tenant, the submissions the
// in-process sender of a cell without a workflow engine advances (As
// built E3), by e-invoice with its Operation: those queued, sending or
// submitted whose step is due at now, or whose Operation was cancelled, so
// that their next step ends them; and those completed whose delivery read
// or tax data request is due, or whose request's Operation was cancelled
// (As built E4); those a workflow carries out are left to it.
func (e *Endpoints) UnfinishedSends(ctx context.Context, tx pgx.Tx, now time.Time) (map[uuid.UUID]uuid.UUID, error) {
	rows, _ := tx.Query(ctx, `SELECT s.outgoing_einvoice_id, s.operation_id FROM einvoicing.outgoing_submission s
		JOIN platform.operation o ON o.tenant_id = s.tenant_id AND o.id = s.operation_id
		WHERE o.workflow_id IS NULL AND ((s.status IN ('queued', 'sending', 'submitted') AND (s.next_check_at <= $1 OR o.status = 'cancelled'))
		  OR (s.status = 'completed' AND s.follow_check_at IS NOT NULL
		    AND (s.follow_check_at <= $1 OR (s.tax_data_request IS NOT NULL AND o.status = 'cancelled'))))`, now)
	out := map[uuid.UUID]uuid.UUID{}
	var outgoing, op uuid.UUID
	if _, err := pgx.ForEachRow(rows, []any{&outgoing, &op}, func() error { out[outgoing] = op; return nil }); err != nil {
		return nil, fmt.Errorf("einvoicing: the sends to advance: %w", err)
	}
	return out, nil
}

// outgoingPath is an e-invoice's resource path: a send's Operation names it.
func outgoingPath(id uuid.UUID) string { return "/v1/outgoing-e-invoices/" + id.String() }

// SendOutgoingEInvoice implements POST /v1/outgoing-e-invoices/{id}:send
// (As built E3): the connector is read and checked first, without a call
// and with no row locked, so one that cannot send for the e-invoice's
// company is refused before anything is queued; its credentials are not
// resolved here, where the request's transaction is open, but by the
// send's first step, which fails the submission when they do not resolve.
// Then the valid e-invoice, read FOR UPDATE so two sends of it wait for
// each other, is queued for its provider: its submission written (or a
// failed one queued again in its row) with the Operation the send workflow
// carries out, audited and published, before any call.
func (e *Endpoints) SendOutgoingEInvoice(ctx context.Context, req *gen.OutgoingEInvoiceSend, params gen.SendOutgoingEInvoiceParams) (gen.SendOutgoingEInvoiceRes, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	if e.Objects == nil {
		return nil, noStore()
	}
	if req.BusinessType != "B2B" && req.BusinessType != "B2G" {
		return nil, problem.Invalid("einvoicing.unknown_business_type", "business_type", "B2B or B2G: B2C is outside the UAE mandate")
	}
	o, err := outgoing(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	company, err := e.sender(ctx, o, req.ConnectorID)
	if err != nil {
		return nil, err
	}
	if err := tx.QueryRow(ctx, `SELECT `+outgoingColumns+outgoingFrom+` WHERE o.id = $1 FOR UPDATE OF o`, o.ID).Scan(o.fields()...); err != nil {
		return nil, fmt.Errorf("einvoicing: lock outgoing e-invoice %s: %w", o.ID, err) // read again under the lock its checks rely on
	}
	if err := sendable(ctx, tx, o, params.IfMatch); err != nil {
		return nil, err
	}
	op, err := e.Sends.Start(ctx, outgoingPath(o.ID), req)
	if err != nil {
		return nil, fmt.Errorf("einvoicing: operation of the send of outgoing e-invoice %s: %w", o.ID, err)
	}
	s, before, err := queue(ctx, tx, p, o, req, op.ID, clock.Now(e.Now))
	if err != nil {
		return nil, err
	}
	return &gen.OperationHeaders{Location: gen.NewOptString("/v1/operations/" + op.ID.String()), Response: op},
		s.record(ctx, "einvoicing.outgoing.send", company, before)
}

// sendable refuses an e-invoice, locked, that cannot be sent: read at
// another ETag than the caller's (its version and its submission's, which
// moves with its transmission), invalid, or queued or sent already.
func sendable(ctx context.Context, tx pgx.Tx, o OutgoingEInvoice, ifMatch string) error {
	s, found, err := submissionOf(ctx, tx, o.ID, true)
	if err != nil {
		return err
	}
	if err := etag.Check(ifMatch, o.Version+s.Version); err != nil { // s.Version is 0 for none
		return err
	}
	switch {
	case o.Status != StatusValid:
		return problem.Conflict("einvoicing.document_invalid", "Outgoing e-invoice "+o.CustomerInvoiceNumber+
			" is invalid; correct what its problems name and render it again before it is sent.")
	case found && s.Status == SubmissionCompleted:
		return problem.Conflict("einvoicing.already_transmitted", "Outgoing e-invoice "+o.CustomerInvoiceNumber+
			" is transmitted already (tracking id "+*s.TrackingID+"); a correction is a credit note.")
	case found && s.Status != SubmissionFailed:
		return problem.Conflict("einvoicing.transmission_in_progress", "Outgoing e-invoice "+o.CustomerInvoiceNumber+" is "+s.Status+
			"; follow operation "+s.OperationID.String()+".")
	}
	return nil
}

// sender refuses a connector that cannot send for the e-invoice's
// company: none of the tenant's (422 request.reference_not_found, as the
// body names it), not an active einvoice_provider with a secret_ref, an
// adapter that sends nothing, or no participant for the company. It
// answers the company.
func (e *Endpoints) sender(ctx context.Context, o OutgoingEInvoice, connector uuid.UUID) (Company, error) {
	c, err := e.Connectors.Provider(ctx, connector)
	if problem.From(err).Status == http.StatusNotFound {
		return Company{}, problem.ReferenceNotFound("connector_id", "connector", "no connector with this id")
	}
	if err != nil {
		return Company{}, err
	}
	if !Sends(c.Adapter) {
		return Company{}, cannotSend()
	}
	co, err := e.companyNamed(ctx, o.LegalEntityID)
	if err != nil {
		return Company{}, err
	}
	if c.Participants[co.Code] == "" {
		return Company{}, problem.Invalid("einvoicing.not_a_participant", "connector_id",
			"connector "+c.Name+" names no participant for company "+co.Code+", whose e-invoices it would send")
	}
	return co, nil
}

// companyNamed is a company by id.
func (e *Endpoints) companyNamed(ctx context.Context, id uuid.UUID) (Company, error) {
	co, err := e.companyOf(ctx, EInvoice{LegalEntityID: &id})
	if err != nil {
		return Company{}, err
	}
	return *co, nil
}

// queue writes the e-invoice's submission queued for the operation: a new
// one, or a failed one queued again in its row, looked up first, as a call
// of it may have reached the provider (its tracking id kept among those
// the provider failed when the provider failed it, so a submission the
// provider still holds otherwise is taken as found, not sent twice). It
// answers the status before (nil when new).
func queue(ctx context.Context, tx pgx.Tx, p reqctx.Principal, o OutgoingEInvoice, req *gen.OutgoingEInvoiceSend, op uuid.UUID, now time.Time) (
	submission, *string, error) {
	var s submission
	err := tx.QueryRow(ctx, `INSERT INTO einvoicing.outgoing_submission AS s (tenant_id, id, outgoing_einvoice_id, legal_entity_id, connector_id,
			operation_id, filename, business_type, status, next_check_at, requested_by, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'queued', $9, $10, $10, $10)
		ON CONFLICT ON CONSTRAINT outgoing_submission_once DO UPDATE SET connector_id = EXCLUDED.connector_id, operation_id = EXCLUDED.operation_id,
			business_type = EXCLUDED.business_type, status = 'queued', provider_status = NULL, tracking_id = NULL,
			failed_tracking_ids = s.failed_tracking_ids || CASE WHEN s.provider_status IN ('SENDING_FAILED', 'FAILED', 'PERMANENT_FAILED')
				THEN ARRAY[s.tracking_id] ELSE '{}' END, attempts = 0, resends = 0, polls = 0, look_up = true,
			next_check_at = EXCLUDED.next_check_at, problem_code = NULL, problem_detail = NULL, requested_by = EXCLUDED.requested_by,
			version = s.version + 1, updated_by = EXCLUDED.updated_by
		WHERE s.status = 'failed' RETURNING `+submissionColumns,
		p.TenantID, uuid.Must(uuid.NewV7()), o.ID, o.LegalEntityID, req.ConnectorID, op, o.CustomerInvoiceNumber, string(req.BusinessType), now, p.ID).
		Scan(s.fields()...)
	if err != nil { // the e-invoice is locked and its submission failed or none: the guard holds
		return s, nil, fmt.Errorf("einvoicing: queue the send of outgoing e-invoice %s: %w", o.ID, err)
	}
	if s.Version == 1 {
		return s, nil, nil
	}
	was := SubmissionFailed
	return s, &was, nil
}

// record audits a change of the submission and publishes
// OutgoingEInvoiceTransmissionChanged (api/asyncapi.yaml): no content and
// no TRN.
func (s submission) record(ctx context.Context, action string, company Company, before *string) error {
	payload := map[string]any{"outgoing_einvoice_id": s.OutgoingID, "customer_invoice_number": s.Filename, "legal_entity_id": s.LegalEntityID,
		"legal_entity_code": company.Code, "connector_id": s.ConnectorID, "operation_id": s.OperationID, "business_type": s.BusinessType,
		"status": s.Status, "status_before": before, "provider_status": s.ProviderStatus, "tracking_id": s.TrackingID, "attempts": s.Attempts,
		"resends": s.Resends, "problem_code": s.ProblemCode, "delivery_status": s.DeliveryStatus, "tax_data_status": s.TaxDataStatus,
		"tax_data_request": s.TaxDataRequest, "version": s.Version}
	return audit.Record(ctx, action, "einvoicing.outgoing_einvoice", s.OutgoingID, payload,
		outbox.Event{Context: "einvoicing", Name: "outgoing_einvoice_transmission_changed", AggregateType: "einvoicing.outgoing_einvoice",
			AggregateID: s.OutgoingID, Payload: payload})
}
