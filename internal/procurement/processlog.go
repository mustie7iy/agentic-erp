package procurement

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// ProcessLog reads a company's purchase orders with the documents linked
// to them for the processmining context's event logs (T103a, ADR-0080):
// each order with the requisition it was made from and its goods
// movements. It reads in the caller's transaction and changes nothing.
type ProcessLog struct{}

// DocumentRef is a document by id and number.
type DocumentRef struct {
	ID             uuid.UUID `json:"id"`
	DocumentNumber string    `json:"document_number"`
}

// LoggedOrder is a purchase order as an event log's case.
type LoggedOrder struct {
	ID             uuid.UUID
	DocumentNumber string
	OrderDate      time.Time
	Requisition    *DocumentRef  // the requisition it was made from
	Movements      []DocumentRef // its goods receipts, returns and reversals, as posted
}

// Orders returns up to limit orders of the company dated from one day to
// another, both included, by order date and id, after the order dated
// afterDate with id afterID (nil: from the first).
func (ProcessLog) Orders(ctx context.Context, legalEntity uuid.UUID, from, to time.Time, afterDate *time.Time, afterID uuid.UUID, limit int) ([]LoggedOrder, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT o.id, o.document_number, o.order_date,
			CASE WHEN r.id IS NOT NULL THEN json_build_object('id', r.id, 'document_number', r.document_number) END,
			coalesce((SELECT json_agg(json_build_object('id', m.id, 'document_number', m.document_number) ORDER BY m.posted_at, m.id)
				FROM procurement.goods_movement m WHERE m.tenant_id = o.tenant_id AND m.id IN (SELECT i.goods_movement_id FROM procurement.goods_movement_item i
					WHERE i.tenant_id = o.tenant_id AND i.purchase_order_id = o.id)), '[]')
		FROM procurement.purchase_order o
		LEFT JOIN procurement.requisition r ON r.tenant_id = o.tenant_id AND r.id = o.requisition_id
		WHERE o.legal_entity_id = $1 AND o.order_date BETWEEN $2 AND $3 AND ($4::date IS NULL OR (o.order_date, o.id) > ($4, $5))
		ORDER BY o.order_date, o.id LIMIT $6`, legalEntity, from, to, afterDate, afterID, limit)
	orders, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (LoggedOrder, error) {
		var o LoggedOrder
		err := r.Scan(&o.ID, &o.DocumentNumber, &o.OrderDate, &o.Requisition, &o.Movements)
		return o, errs.Wrapf(err, "scan an order")
	})
	if err != nil {
		return nil, fmt.Errorf("procurement: orders of %s from %s for an event log: %w", legalEntity, from.Format(time.DateOnly), err)
	}
	return orders, nil
}

// CountOrders is how many orders of the company are dated from one day to
// another, both included.
func (ProcessLog) CountOrders(ctx context.Context, legalEntity uuid.UUID, from, to time.Time) (int, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return 0, err
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM procurement.purchase_order WHERE legal_entity_id = $1 AND order_date BETWEEN $2 AND $3`,
		legalEntity, from, to).Scan(&n); err != nil {
		return 0, fmt.Errorf("procurement: count the orders of %s for an event log: %w", legalEntity, err)
	}
	return n, nil
}
