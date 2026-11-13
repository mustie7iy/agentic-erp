package extract

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// The spend lines (dataset spend_lines v1, T91b; ADR-0044 As built T91b):
// the items of posted supplier invoices, credit memos and subsequent
// debits and credits by posting date, as the semantic layer's database
// path reads them from /v1/supplier-invoices, with the account and cost
// center of an item on a purchase order and the order's contract as it
// reads them from /v1/purchase-orders, /v1/contracts and
// /v1/cost-centers.

// spendLine is a row of dataset spend_lines v1
// (analytics/datasets/v1/spend_lines.yaml).
type spendLine struct {
	TenantID      uuid.UUID   `parquet:"tenant_id,uuid"`
	InvoiceID     uuid.UUID   `parquet:"invoice_id,uuid"`
	ItemNo        int32       `parquet:"item_no"`
	Company       string      `parquet:"company"`
	PostingDate   int32       `parquet:"posting_date,date"`
	Vendor        string      `parquet:"vendor"`
	Account       *string     `parquet:"account,optional"`
	CostCenter    *string     `parquet:"cost_center,optional"`
	PurchaseOrder *string     `parquet:"purchase_order,optional"`
	Contract      *string     `parquet:"contract,optional"`
	OnContract    bool        `parquet:"on_contract"`
	AmountMinor   money.Minor `parquet:"amount_minor"`
	Currency      string      `parquet:"currency"`
	Exponent      int32       `parquet:"exponent"`
}

// spendLinesSQL reads the items in month order, then by document and
// item. An item on a purchase order takes the order item's account and
// cost center where it names none of its own (a removed order item is not
// one the API returns), and the order's contract; a credit memo's and a
// subsequent credit's amounts reduce spend. Only posted documents: a
// reversed one is no longer spend.
const spendLinesSQL = `SELECT i.tenant_id, i.id, it.item_no, le.code, i.posting_date, bp.number, a.number, cc.code, o.document_number,
		ct.document_number, ct.id IS NOT NULL,
		CASE WHEN i.kind IN ('credit_memo', 'subsequent_credit') THEN -it.amount ELSE it.amount END, i.currency, c.exponent
	FROM payables.supplier_invoice i
	JOIN payables.supplier_invoice_item it ON it.tenant_id = i.tenant_id AND it.supplier_invoice_id = i.id
	JOIN tenancy.legal_entity le ON le.tenant_id = i.tenant_id AND le.id = i.legal_entity_id
	JOIN partner.business_partner bp ON bp.tenant_id = i.tenant_id AND bp.id = i.supplier_id
	LEFT JOIN procurement.purchase_order o ON o.tenant_id = it.tenant_id AND o.id = it.purchase_order_id
	LEFT JOIN procurement.purchase_order_item oi ON oi.tenant_id = it.tenant_id AND oi.purchase_order_id = it.purchase_order_id
		AND oi.item_no = it.po_item_no AND NOT oi.is_removed
	LEFT JOIN ledger.gl_account a ON a.tenant_id = i.tenant_id AND a.id = coalesce(it.account_id, oi.account_id)
	LEFT JOIN ledger.cost_center cc ON cc.tenant_id = i.tenant_id AND cc.id = coalesce(it.cost_center_id, oi.cost_center_id)
	LEFT JOIN procurement.contract ct ON ct.tenant_id = o.tenant_id AND ct.id = o.contract_id
	JOIN ref.currency c ON c.code = i.currency
	WHERE i.status = 'posted'
	ORDER BY i.posting_date, i.id, it.item_no`

func (r *run) spendLines(ctx context.Context, tx pgx.Tx) error {
	var l spendLine
	var date time.Time
	var exponent int16
	return snapshot(ctx, tx, r, "spend_lines", spendLinesSQL, []any{&l.TenantID, &l.InvoiceID, &l.ItemNo, &l.Company, &date, &l.Vendor, &l.Account,
		&l.CostCenter, &l.PurchaseOrder, &l.Contract, &l.OnContract, &l.AmountMinor, &l.Currency, &exponent}, func() (spendLine, string, error) {
		row := l
		// The scan may reuse a non-nil pointer, which row holds.
		l.Account, l.CostCenter, l.PurchaseOrder, l.Contract = nil, nil, nil, nil
		row.PostingDate, row.Exponent = days(date), int32(exponent)
		return row, date.Format(monthLayout), nil
	})
}
