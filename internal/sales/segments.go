package sales

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// SegmentQuery selects what net revenue by profitability segment reads
// (ADR-0075 §6, T92a): a company's billing documents and credit memo
// requests posted from From to To, both days included, in Currency, the
// company's local currency, and of one customer when Customer is set.
type SegmentQuery struct {
	LegalEntityID uuid.UUID
	From, To      time.Time
	Currency      string
	Customer      *uuid.UUID
}

// Segment is a customer and item key's figures, minor units of the
// query's currency: the gross value billed, the discounts taken off it,
// the credit memos posted against it and Net, the first less the other
// two; Cost, the cost of goods sold its goods issues posted (ADR-0083
// §6) less what their reversals took back on their days (S2b3), and
// Margin, Net less Cost. ItemKey is nil for the customer's items
// without a key.
type Segment struct {
	CustomerID                     uuid.UUID
	ItemKey                        *string
	Gross, Discounts, Credits, Net money.Minor
	Cost, Margin                   money.Minor
}

// segmentsSQL adds up the segments. A billing item's gross value is its
// net value and its part of its order item's discount: the order item's
// price, its first condition (pricing's conditioned), less its net value,
// none without conditions, both as they were when the item was billed
// (migration 000356). An item billed in one part takes the whole; one
// billed in parts shares it as billing shares the net value (billable):
// the discount × the net billed of the item so far, this part included, ÷
// the item's net value, rounded half away from zero (numeric round), less
// the same for the parts billed before it, in the order their documents
// were created, a document cancelled before this one was created left
// out (T79c), as billing leaves it out. The parts of a fully billed item
// add up to its discount to the minor unit, but for a minor unit a
// cancellation may leave, and each part is fixed once billed. A document
// that posted counts on its posting date; its cancellation (T79c) counts
// on its own date, the document's gross and discount taken off. A credit
// counts its own posting date and its billing item's segment. A goods
// issue's cost counts on its date, the date its cost of goods sold
// posted, in the segment of the order item it delivered, for an order in
// the query's currency, as its revenue counts (ADR-0083 §6).
const segmentsSQL = `WITH parts AS (
	SELECT b.id, b.customer_id, b.posting_date, oi.item_key, i.net_amount,
		CASE WHEN d.discount = 0 OR i.net_amount = i.order_net_amount THEN d.discount
		ELSE (round(d.discount * (p.before + i.net_amount) / i.order_net_amount) - round(d.discount * p.before / i.order_net_amount))::bigint END AS discount
	FROM sales.billing_document b
	JOIN sales.billing_document_item i ON i.tenant_id = b.tenant_id AND i.billing_document_id = b.id
	JOIN sales.sales_order_item oi ON oi.tenant_id = i.tenant_id AND oi.sales_order_id = i.sales_order_id AND oi.item_no = i.order_item_no
	CROSS JOIN LATERAL (SELECT CASE WHEN i.conditions->0->>'kind' = 'price' THEN (i.conditions->0->>'amount')::bigint - i.order_net_amount ELSE 0 END
		AS discount) d
	LEFT JOIN LATERAL (SELECT coalesce(sum(e.net_amount), 0)::numeric AS before FROM sales.billing_document_item e
		JOIN sales.billing_document eb ON eb.tenant_id = e.tenant_id AND eb.id = e.billing_document_id
		WHERE d.discount <> 0 AND i.net_amount <> i.order_net_amount
			AND e.tenant_id = i.tenant_id AND e.sales_order_id = i.sales_order_id AND e.order_item_no = i.order_item_no
			AND (eb.created_at, eb.id) < (b.created_at, b.id)
			AND NOT EXISTS (SELECT FROM sales.billing_cancellation x WHERE x.tenant_id = eb.tenant_id AND x.billing_document_id = eb.id
				AND x.created_at < b.created_at)) p ON true
	WHERE b.legal_entity_id = $1 AND b.posting_date IS NOT NULL AND b.currency = $4 AND ($5::uuid IS NULL OR b.customer_id = $5)
), billed AS (
	SELECT customer_id, item_key, net_amount, discount FROM parts WHERE posting_date BETWEEN $2 AND $3
	UNION ALL
	SELECT p.customer_id, p.item_key, -p.net_amount, -p.discount FROM parts p
	JOIN sales.billing_cancellation x ON x.billing_document_id = p.id
	WHERE x.customer_invoice_id IS NOT NULL AND x.cancellation_date BETWEEN $2 AND $3
), credited AS (
	SELECT r.customer_id, oi.item_key, c.net_amount
	FROM sales.credit_memo_request r
	JOIN sales.billing_document b ON b.tenant_id = r.tenant_id AND b.id = r.billing_document_id
	JOIN sales.credit_memo_request_item c ON c.tenant_id = r.tenant_id AND c.request_id = r.id
	JOIN sales.billing_document_item i ON i.tenant_id = c.tenant_id AND i.billing_document_id = c.billing_document_id AND i.item_no = c.billing_item_no
	JOIN sales.sales_order_item oi ON oi.tenant_id = i.tenant_id AND oi.sales_order_id = i.sales_order_id AND oi.item_no = i.order_item_no
	WHERE r.legal_entity_id = $1 AND r.status = 'posted' AND r.posting_date BETWEEN $2 AND $3 AND b.currency = $4
		AND ($5::uuid IS NULL OR r.customer_id = $5)
), issued AS (
	SELECT o.customer_id, oi.item_key, i.cost_amount
	FROM sales.delivery d
	JOIN sales.sales_order o ON o.tenant_id = d.tenant_id AND o.id = d.sales_order_id
	JOIN sales.delivery_item i ON i.tenant_id = d.tenant_id AND i.delivery_id = d.id
	JOIN sales.sales_order_item oi ON oi.tenant_id = i.tenant_id AND oi.sales_order_id = i.sales_order_id AND oi.item_no = i.order_item_no
	WHERE o.legal_entity_id = $1 AND d.outbound_delivery_id IS NOT NULL AND d.delivery_date BETWEEN $2 AND $3 AND o.currency = $4
		AND ($5::uuid IS NULL OR o.customer_id = $5)
	UNION ALL
	SELECT o.customer_id, oi.item_key, -i.cost_amount
	FROM sales.delivery_reversal r
	JOIN sales.sales_order o ON o.tenant_id = r.tenant_id AND o.id = r.sales_order_id
	JOIN sales.delivery_item i ON i.tenant_id = r.tenant_id AND i.delivery_id = r.delivery_id
	JOIN sales.sales_order_item oi ON oi.tenant_id = i.tenant_id AND oi.sales_order_id = i.sales_order_id AND oi.item_no = i.order_item_no
	WHERE o.legal_entity_id = $1 AND r.reversal_date BETWEEN $2 AND $3 AND o.currency = $4 AND ($5::uuid IS NULL OR o.customer_id = $5)
)
SELECT customer_id, item_key, sum(gross)::bigint, sum(discount)::bigint, sum(credit)::bigint, (sum(gross) - sum(discount) - sum(credit))::bigint,
	sum(cost)::bigint, (sum(gross) - sum(discount) - sum(credit) - sum(cost))::bigint
FROM (SELECT customer_id, item_key, (net_amount + discount)::numeric AS gross, discount::numeric, 0::numeric AS credit, 0::numeric AS cost FROM billed
	UNION ALL SELECT customer_id, item_key, 0, 0, net_amount, 0 FROM credited
	UNION ALL SELECT customer_id, item_key, 0, 0, 0, cost_amount FROM issued) s
GROUP BY customer_id, item_key
ORDER BY customer_id, item_key NULLS FIRST`

// foreignSQL counts the posted billing documents, their cancellations
// with a credit memo, the credit memo requests and the goods issues and
// their reversals of orders the query selects but for their currency.
const foreignSQL = `SELECT
	(SELECT count(*) FROM sales.billing_document b
		WHERE b.legal_entity_id = $1 AND b.posting_date BETWEEN $2 AND $3 AND b.currency <> $4
			AND ($5::uuid IS NULL OR b.customer_id = $5))
	+ (SELECT count(*) FROM sales.billing_cancellation x JOIN sales.billing_document b ON b.tenant_id = x.tenant_id AND b.id = x.billing_document_id
		WHERE x.legal_entity_id = $1 AND x.customer_invoice_id IS NOT NULL AND x.cancellation_date BETWEEN $2 AND $3 AND b.currency <> $4
			AND ($5::uuid IS NULL OR b.customer_id = $5))
	+ (SELECT count(*) FROM sales.credit_memo_request r JOIN sales.billing_document b ON b.tenant_id = r.tenant_id AND b.id = r.billing_document_id
		WHERE r.legal_entity_id = $1 AND r.status = 'posted' AND r.posting_date BETWEEN $2 AND $3 AND b.currency <> $4
			AND ($5::uuid IS NULL OR r.customer_id = $5))
	+ (SELECT count(*) FROM sales.delivery d JOIN sales.sales_order o ON o.tenant_id = d.tenant_id AND o.id = d.sales_order_id
		WHERE o.legal_entity_id = $1 AND d.outbound_delivery_id IS NOT NULL AND d.delivery_date BETWEEN $2 AND $3 AND o.currency <> $4
			AND ($5::uuid IS NULL OR o.customer_id = $5))
	+ (SELECT count(*) FROM sales.delivery_reversal r JOIN sales.sales_order o ON o.tenant_id = r.tenant_id AND o.id = r.sales_order_id
		WHERE o.legal_entity_id = $1 AND r.reversal_date BETWEEN $2 AND $3 AND o.currency <> $4 AND ($5::uuid IS NULL OR o.customer_id = $5))`

// Segments are the company's figures by profitability segment, a
// customer and an item key (ADR-0075 §6, T92a), from the billing
// documents, credit memo requests and goods issues (ADR-0083 §6) the
// query selects, by customer id
// and then item key, the segment without one first, and how many of the
// documents posted in those days are in another currency, which are left
// out. A sum beyond a 64-bit amount fails the read, never wraps. Company
// access is not applied: the caller applies it.
func (Orders) Segments(ctx context.Context, q SegmentQuery) ([]Segment, int, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, 0, err
	}
	args := []any{q.LegalEntityID, q.From, q.To, q.Currency, q.Customer}
	rows, _ := tx.Query(ctx, segmentsSQL, args...)
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Segment])
	if err != nil {
		return nil, 0, fmt.Errorf("sales: segments of %s: %w", q.LegalEntityID, err)
	}
	var foreign int
	if err := tx.QueryRow(ctx, foreignSQL, args...).Scan(&foreign); err != nil {
		return nil, 0, fmt.Errorf("sales: documents of %s in other currencies: %w", q.LegalEntityID, err)
	}
	return out, foreign, nil
}
