package extract

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// The sales lines (dataset sales_lines v1, T91a; ADR-0044 As built T91a):
// the items of confirmed sales orders by order date and of posted billing
// documents by billing date, as the semantic layer's database path reads
// them from /v1/sales-orders and /v1/billing-documents. The run reads the
// sales context's tables directly and read-only, as it reads the others:
// it is the store's copy of what the API returns, held to it by the
// parity test.

// salesLine is a row of dataset sales_lines v1
// (analytics/datasets/v1/sales_lines.yaml).
type salesLine struct {
	TenantID      uuid.UUID   `parquet:"tenant_id,uuid"`
	DocumentID    uuid.UUID   `parquet:"document_id,uuid"`
	ItemNo        int32       `parquet:"item_no"`
	SalesDocument string      `parquet:"sales_document"`
	Company       string      `parquet:"company"`
	DocumentDate  int32       `parquet:"document_date,date"`
	Customer      string      `parquet:"customer"`
	CustomerName  string      `parquet:"customer_name"`
	ItemKey       *string     `parquet:"item_key,optional"`
	AmountMinor   money.Minor `parquet:"amount_minor"`
	Currency      string      `parquet:"currency"`
	Exponent      int32       `parquet:"exponent"`
}

// salesLinesSQL reads both kinds of item in month order, then by kind,
// document and item, so the same rows always give the same file. A billing
// document's item names what is sold by its order's item, which nothing
// changes. A draft order and a billing document not posted are left out.
const salesLinesSQL = `SELECT s.tenant_id, s.id, s.item_no, s.kind, le.code, s.document_date, bp.number, bp.name, s.item_key, s.net_amount, s.currency, c.exponent
	FROM (
		SELECT o.tenant_id, o.id, i.item_no, 'order' AS kind, o.legal_entity_id, o.customer_id, o.order_date AS document_date, i.item_key, i.net_amount, o.currency
		FROM sales.sales_order o
		JOIN sales.sales_order_item i ON i.tenant_id = o.tenant_id AND i.sales_order_id = o.id
		WHERE o.status = 'confirmed'
		UNION ALL
		SELECT d.tenant_id, d.id, b.item_no, 'billing', d.legal_entity_id, d.customer_id, d.billing_date, i.item_key, b.net_amount, d.currency
		FROM sales.billing_document d
		JOIN sales.billing_document_item b ON b.tenant_id = d.tenant_id AND b.billing_document_id = d.id
		JOIN sales.sales_order_item i ON i.tenant_id = b.tenant_id AND i.sales_order_id = b.sales_order_id AND i.item_no = b.order_item_no
		WHERE d.status = 'posted'
	) s
	JOIN tenancy.legal_entity le ON le.tenant_id = s.tenant_id AND le.id = s.legal_entity_id
	JOIN partner.business_partner bp ON bp.tenant_id = s.tenant_id AND bp.id = s.customer_id
	JOIN ref.currency c ON c.code = s.currency
	ORDER BY s.document_date, s.kind, s.id, s.item_no`

func (r *run) salesLines(ctx context.Context, tx pgx.Tx) error {
	var l salesLine
	var date time.Time
	var exponent int16
	return snapshot(ctx, tx, r, "sales_lines", salesLinesSQL, []any{&l.TenantID, &l.DocumentID, &l.ItemNo, &l.SalesDocument, &l.Company, &date,
		&l.Customer, &l.CustomerName, &l.ItemKey, &l.AmountMinor, &l.Currency, &exponent}, func() (salesLine, string, error) {
		row := l
		l.ItemKey = nil // the scan may reuse a non-nil pointer, which row holds
		row.DocumentDate, row.Exponent = days(date), int32(exponent)
		return row, date.Format(monthLayout), nil
	})
}
