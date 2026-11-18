package procurement

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// An RFQ ended without an award (T82g, ADR-0069 As built), and the
// sourcing an award's cancelled order undoes (T82h): a buyer
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

// RFQOrderCancelled is the status of an RFQ whose award's purchase order
// was cancelled (T82h): the RFQ still names the order, which no longer
// orders anything.
const RFQOrderCancelled = "order_cancelled"

// ReopenedByOrderCancelled is the cause of a requisition reopened because
// its purchase order was cancelled.
const ReopenedByOrderCancelled = "purchase_order_cancelled"

// unsourced undoes what an order cancelled in this transaction had
// sourced (T82h): the RFQ whose award created it, if any, moves from
// awarded to order_cancelled, and its requisition, if any, is reopened.
// Locks: the order (the caller's), then the RFQ, then the requisition,
// the order an award takes the RFQ and the requisition in; the award's
// order is a new row no other transaction holds.
func (e *Endpoints) unsourced(ctx context.Context, tx pgx.Tx, p reqctx.Principal, o PurchaseOrder) error {
	var rfq uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM procurement.rfq WHERE purchase_order_id = $1 FOR UPDATE`, o.ID).Scan(&rfq)
	switch {
	case errors.Is(err, pgx.ErrNoRows): // an order no award created
	case err != nil:
		return fmt.Errorf("procurement: cancel purchase order %s: lock its RFQ: %w", o.ID, err)
	default:
		// Only an awarded RFQ names an order that is not cancelled (000365).
		if _, err := tx.Exec(ctx, `UPDATE procurement.rfq SET status = $2, version = version + 1, updated_by = $3 WHERE id = $1 AND status = $4`,
			rfq, RFQOrderCancelled, p.ID, RFQAwarded); err != nil {
			return fmt.Errorf("procurement: cancel purchase order %s: its RFQ %s: %w", o.ID, rfq, err)
		}
		if _, err := e.recorded(ctx, tx, rfq, "order_cancelled", RFQOrderCancelled, map[string]any{"from_status": RFQAwarded,
			"purchase_order_id": o.ID}); err != nil {
			return err
		}
	}
	if o.RequisitionID == nil {
		return nil
	}
	return e.reopen(ctx, tx, p, *o.RequisitionID, ReopenedByOrderCancelled, map[string]any{"purchase_order_id": o.ID,
		"purchase_order_document_number": o.DocumentNumber})
}
