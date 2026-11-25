package shipping

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/inventory"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/sales"
)

// Reversal is a delivery's goods issue's reversal (S2b3): the company's
// date it posted on and its material document (602) by id and number.
type Reversal struct {
	Date                   time.Time
	MaterialDocumentID     uuid.UUID
	MaterialDocumentNumber string
}

// ReverseGoodsIssue reverses a delivery's goods issue (ADR-0083 As built
// S2b3; SAP VL09, from memory), once: the delivery read under its row
// lock (ADR-0008) at the version the caller read, while it is
// goods-issued (409 shipping.goods_issue_not_posted before,
// shipping.goods_issue_reversed after). Inventory posts the 601's
// reversal, a 602 naming it and, as its reference, the delivery's
// number, on the company's date today, each line giving back what its
// 601 line took (ReverseForDelivery, which locks the valuations and
// stock after the delivery). Sales then reverses the goods issue's
// delivery of the order under the order's lock and the availability
// check's locks of its materials at the plant (ReverseIssue: 409
// sales.delivery_billed when an item stays billed beyond what is
// delivered), so the order's items are open again. Its trade compliance
// is not asked again: the stock comes back. The delivery is
// goods_issue_reversed, naming the reversal; its audit record and
// OutboundDeliveryChanged are written in the request's transaction.
// Locks are taken as the goods issue takes them: delivery, valuations
// and stock, then order, then the availability locks, which the check
// takes after the order's.
func (e *Endpoints) ReverseGoodsIssue(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (Delivery, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Delivery{}, err
	}
	d, err := issued(ctx, tx, id, ifMatch)
	if err != nil {
		return Delivery{}, err
	}
	doc, err := e.Stock.ReverseForDelivery(ctx, *d.MaterialDocumentID, d.Number)
	if err != nil {
		return Delivery{}, err
	}
	if _, err := e.Shipments.ReverseIssue(ctx, sales.IssueReversal{OrderID: d.OrderID, DeliveryID: d.ID, DeliveryNumber: d.Number, PlantID: d.PlantID,
		MaterialDocumentID: doc.ID, MaterialDocumentNumber: doc.Number, Date: doc.PostingDate}); err != nil {
		return Delivery{}, err
	}
	after, err := reversed(ctx, tx, p, d, doc)
	if err != nil {
		return Delivery{}, err
	}
	return after, e.record(ctx, "shipping.outbound_delivery.reverse_goods_issue", after, map[string]any{"journal_entry_number": doc.EntryNumber,
		"reversal_material_document_number": doc.Number})
}

// issued reads a delivery to reverse under its row lock, at the version
// the caller read, refusing one whose goods issue is not posted or is
// reversed already.
func issued(ctx context.Context, tx pgx.Tx, id uuid.UUID, ifMatch func(int32) error) (Delivery, error) {
	d, err := accessible(ctx, tx, id, true)
	if err != nil {
		return Delivery{}, err
	}
	if err := ifMatch(d.Version); err != nil {
		return Delivery{}, err
	}
	switch d.Status {
	case Created:
		return Delivery{}, problem.Conflict("shipping.goods_issue_not_posted", "Outbound delivery "+d.Number+"'s goods issue is not posted; there is "+
			"nothing to reverse.")
	case GoodsIssueReversed:
		return Delivery{}, reversedAlready(d)
	}
	return d, nil
}

// reversedAlready refuses a change of a delivery whose goods issue is
// reversed (409 shipping.goods_issue_reversed): it changes no more.
func reversedAlready(d Delivery) error {
	return problem.Conflict("shipping.goods_issue_reversed", "Outbound delivery "+d.Number+"'s goods issue is reversed ("+
		d.Reversal.MaterialDocumentNumber+"); it changes no more.")
}

// reversed writes the delivery's reversal, moving its version, and reads
// it back.
func reversed(ctx context.Context, tx pgx.Tx, p reqctx.Principal, d Delivery, doc inventory.Document) (Delivery, error) {
	if _, err := tx.Exec(ctx, `UPDATE shipping.outbound_delivery SET status = $2, goods_issue_reversal_date = $3, reversal_material_document_id = $4,
			reversal_material_document_number = $5, version = version + 1, updated_by = $6 WHERE id = $1`,
		d.ID, GoodsIssueReversed, doc.PostingDate.Format(time.DateOnly), doc.ID, doc.Number, p.ID); err != nil {
		return Delivery{}, fmt.Errorf("shipping: reverse the goods issue of outbound delivery %s: %w", d.Number, err)
	}
	return load(ctx, tx, d.ID, false)
}

// reversalEvent is the reversal as OutboundDeliveryChanged gives it; nil
// while there is none.
func reversalEvent(rv *Reversal) map[string]any {
	if rv == nil {
		return nil
	}
	return map[string]any{"date": rv.Date.Format(time.DateOnly), "material_document_id": rv.MaterialDocumentID,
		"material_document_number": rv.MaterialDocumentNumber}
}
