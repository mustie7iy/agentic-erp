package einvoicing

import (
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// A person's requests of a transmitted e-invoice's tax data document
// (ADR-0061 §9, As built E4; the coordinator plan's default 8): send a
// failed one again, or withdraw a reported one, through the provider the
// e-invoice was sent through. The request writes itself on the
// submission, the outbox of the call, with the Operation that carries it
// out, which takes over the submission's operation_id, so the send's
// workflow (or a new one, started for the Operation) makes the call after
// the commit (claimFollow). People only: agents never ask (ADR-0061 §13).

// KindTaxData is the kind of a tax data request's Operation.
const KindTaxData = "einvoicing.tax_data"

// withdrawable are the tax data document's statuses a withdrawal may be
// asked from: reported (or being reported) to the authority, or a
// withdrawal that failed.
var withdrawable = []string{TaxDataSent, TaxDataApproved, TaxDataAcknowledged, TaxDataWithdrawFailed}

// RetryOutgoingEInvoiceTaxData implements POST
// /v1/outgoing-e-invoices/{id}:retry-tax-data.
func (e *Endpoints) RetryOutgoingEInvoiceTaxData(ctx context.Context, params gen.RetryOutgoingEInvoiceTaxDataParams) (gen.RetryOutgoingEInvoiceTaxDataRes, error) {
	return e.requestTaxData(ctx, params.ID, params.IfMatch, TaxDataRetry)
}

// WithdrawOutgoingEInvoiceTaxData implements POST
// /v1/outgoing-e-invoices/{id}:withdraw-tax-data.
func (e *Endpoints) WithdrawOutgoingEInvoiceTaxData(ctx context.Context, params gen.WithdrawOutgoingEInvoiceTaxDataParams) (gen.WithdrawOutgoingEInvoiceTaxDataRes, error) {
	return e.requestTaxData(ctx, params.ID, params.IfMatch, TaxDataWithdraw)
}

// requestTaxData queues a person's request of the e-invoice's tax data
// document: the connector checked first, with no row locked, as a send's
// is; then the e-invoice and its submission read FOR UPDATE (in the send's
// order), checked at the caller's ETag, and the request written with its
// Operation, audited and published, before any call.
func (e *Endpoints) requestTaxData(ctx context.Context, id uuid.UUID, ifMatch, request string) (*gen.OperationHeaders, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	o, err := outgoing(ctx, id)
	if err != nil {
		return nil, err
	}
	s, found, err := submissionOf(ctx, tx, o.ID, false)
	if err != nil {
		return nil, err
	}
	if !found || s.Status != SubmissionCompleted {
		return nil, notTransmitted(o)
	}
	company, err := e.sender(ctx, o, s.ConnectorID)
	if err != nil {
		return nil, err
	}
	if err := tx.QueryRow(ctx, `SELECT `+outgoingColumns+outgoingFrom+` WHERE o.id = $1 FOR UPDATE OF o`, o.ID).Scan(o.fields()...); err != nil {
		return nil, fmt.Errorf("einvoicing: lock outgoing e-invoice %s: %w", o.ID, err)
	}
	if s, _, err = submissionOf(ctx, tx, o.ID, true); err != nil { // a submission is never deleted
		return nil, err
	}
	if err := requestable(o, s, ifMatch, request); err != nil {
		return nil, err
	}
	op, err := e.TaxData.Start(ctx, outgoingPath(o.ID), map[string]string{"tax_data_request": request})
	if err != nil {
		return nil, fmt.Errorf("einvoicing: operation of the tax data request of outgoing e-invoice %s: %w", o.ID, err)
	}
	if err := tx.QueryRow(ctx, `UPDATE einvoicing.outgoing_submission s SET operation_id = $2, tax_data_request = $3, tax_data_attempts = 0,
		follow_check_at = $4, requested_by = $5, version = version + 1, updated_by = $5 WHERE id = $1 RETURNING `+submissionColumns,
		s.ID, op.ID, request, clock.Now(e.Now), p.ID).Scan(s.fields()...); err != nil {
		return nil, fmt.Errorf("einvoicing: queue the tax data request of outgoing e-invoice %s: %w", o.ID, err)
	}
	completed := SubmissionCompleted
	return &gen.OperationHeaders{Location: gen.NewOptString("/v1/operations/" + op.ID.String()), Response: op},
		s.record(ctx, "einvoicing.outgoing.tax_data", company, &completed)
}

// requestable refuses a request the locked submission cannot take: read at
// another ETag than the caller's, not completed, a request not yet made,
// or a tax data document in a status the request does not apply to.
func requestable(o OutgoingEInvoice, s submission, ifMatch, request string) error {
	if err := etag.Check(ifMatch, o.Version+s.Version); err != nil {
		return err
	}
	status := deref(s.TaxDataStatus)
	switch {
	case s.Status != SubmissionCompleted:
		return notTransmitted(o)
	case s.TaxDataRequest != nil:
		return problem.Conflict("einvoicing.tax_data_request_pending", "The "+*s.TaxDataRequest+" of e-invoice "+o.CustomerInvoiceNumber+
			"'s tax data document is not made yet; follow operation "+s.OperationID.String()+".")
	case request == TaxDataRetry && status != TaxDataFailed:
		return problem.Conflict("einvoicing.tax_data_not_failed", "The tax data document of e-invoice "+o.CustomerInvoiceNumber+" is "+
			orNone(status)+", not TDD_FAILED: only a failed one is sent again.")
	case request == TaxDataWithdraw && !slices.Contains(withdrawable, status):
		return problem.Conflict("einvoicing.tax_data_not_withdrawable", "The tax data document of e-invoice "+o.CustomerInvoiceNumber+" is "+
			orNone(status)+": only one reported (TDD_SEND, TDD_APPROVED, TDD_ACKNOWLEDGED) or whose withdrawal failed is withdrawn.")
	}
	return nil
}

func notTransmitted(o OutgoingEInvoice) error {
	return problem.Conflict("einvoicing.not_transmitted", "Outgoing e-invoice "+o.CustomerInvoiceNumber+
		" is not transmitted: its tax data document is the provider's once its transmission is completed.")
}

func orNone(status string) string {
	if status == "" {
		return "not reported yet"
	}
	return status
}
