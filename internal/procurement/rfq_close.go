package procurement

import (
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// An RFQ ended without an award (T82g, ADR-0069 As built): a buyer
// cancels a draft or published RFQ that is no longer needed, or closes a
// published one whose quotations do not lead to an award, with a reason.
// Its recorded quotations are not awarded, and a requisition it was
// created from leaves sourcing: it is approved again (reopen), available
// to a new RFQ, an order or its cancellation.

// The statuses of an RFQ ended without an award.
const (
	RFQCancelled = "cancelled"
	RFQClosed    = "closed"
)

// What reopened a requisition (RequisitionReopened's cause).
const (
	ReopenedByRFQCancelled = "rfq_cancelled"
	ReopenedByRFQClosed    = "rfq_closed"
)

// rfqEnding is how an RFQ ends without an award: the status it moves to,
// the statuses it may move from, the verb its refusal and audit action
// name, and the cause its requisition is reopened by.
type rfqEnding struct {
	status, verb, cause string
	from                []string
}

var (
	rfqCancelling = rfqEnding{status: RFQCancelled, verb: "cancel", cause: ReopenedByRFQCancelled, from: []string{Draft, RFQPublished}}
	rfqClosing    = rfqEnding{status: RFQClosed, verb: "close", cause: ReopenedByRFQClosed, from: []string{RFQPublished}}
)

// endRFQ ends an RFQ without an award, once: it is read under its row
// lock at the version the caller read, so of two requests the second
// meets another version or finds it ended (409). Its recorded quotations
// are not awarded first, since the database ends an RFQ only with none
// left open (000363); then the RFQ moves, keeping the reason, and its
// requisition, if any, is reopened: the RFQ, then the requisition, as an
// award takes them.
func (e *Endpoints) endRFQ(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, how rfqEnding, reason string) (RFQ, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return RFQ{}, err
	}
	if err := problem.RequireText("reason", reason); err != nil {
		return RFQ{}, err
	}
	r, err := getRFQ(ctx, tx, id, true)
	if err != nil {
		return RFQ{}, err
	}
	if err := ifMatch(r.Version); err != nil {
		return RFQ{}, err
	}
	if !slices.Contains(how.from, r.Status) {
		return RFQ{}, statusConflict("RFQ", r.Status, how.verb, how.from...)
	}
	rows, _ := tx.Query(ctx, `UPDATE procurement.supplier_quotation SET status = $2, version = version + 1, updated_by = $3
		WHERE rfq_id = $1 AND status = $4 RETURNING id`, id, QuotationNotAwarded, p.ID, Recorded)
	settled, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return RFQ{}, fmt.Errorf("procurement: %s RFQ %s: settle its quotations: %w", how.verb, id, err)
	}
	// UPDATE … RETURNING has no order: the audit record lists the
	// quotations by id, which is time-ordered (uuid v7).
	slices.SortFunc(settled, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	if _, err := tx.Exec(ctx, `UPDATE procurement.rfq SET status = $2, closing_reason = $3, version = version + 1, updated_by = $4
		WHERE id = $1 AND status = $5`, id, how.status, reason, p.ID, r.Status); err != nil {
		return RFQ{}, fmt.Errorf("procurement: %s RFQ %s: %w", how.verb, id, err)
	}
	if r.RequisitionID != nil {
		if err := e.reopen(ctx, tx, p, *r.RequisitionID, how.cause, map[string]any{"rfq_id": r.ID, "rfq_document_number": r.DocumentNumber}); err != nil {
			return RFQ{}, err
		}
	}
	return e.recorded(ctx, tx, id, how.verb, how.status, map[string]any{"from_status": r.Status, "reason": reason,
		"not_awarded_quotation_ids": settled})
}

// reopen moves a requisition back to approved, read under its row lock:
// what sourced or ordered it ended (cause, with what extra names of it),
// and it can be sourced, ordered or cancelled anew. Audited as
// procurement.requisition.reopened, not .approved: it is not approved a
// second time, and the event log tells the two apart (ADR-0080 §3).
func (e *Endpoints) reopen(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID, cause string, extra map[string]any) error {
	req, err := getRequisition(ctx, tx, id, true)
	if err != nil {
		return err
	}
	extra["cause"] = cause
	_, err = e.moveRequisition(ctx, tx, p, req, Approved, "reopened", req.WorkflowRequestID, "requisition_reopened", extra)
	return err
}
