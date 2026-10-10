package procurement

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Orders answers payables' questions about purchase orders, in the
// request's transaction.
type Orders struct{}

// OrderItem is a live item of a purchase order with its received quantity.
// Quantities are decimal strings with up to six decimals.
type OrderItem struct {
	ItemNo    int32     `json:"item_no"`
	Quantity  string    `json:"quantity"`
	Unit      string    `json:"unit"`
	UnitPrice int64     `json:"unit_price"` // minor units of the order currency per PriceUnit
	PriceUnit int32     `json:"price_unit"`
	AccountID uuid.UUID `json:"account_id"`
	TaxCode   *string   `json:"tax_code"`
	Received  string    `json:"received"` // 101 receipts minus 102 reversals
}

// Order is a purchase order as payables sees it.
type Order struct {
	ID              uuid.UUID
	DocumentNumber  string
	LegalEntityID   uuid.UUID
	SupplierID      uuid.UUID
	Currency        string
	PaymentTermCode string
	Status          string
	Items           []OrderItem
}

// Order returns the purchase order, or ok = false when there is none.
func (Orders) Order(ctx context.Context, id uuid.UUID) (Order, bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Order{}, false, err
	}
	o := Order{ID: id}
	err = tx.QueryRow(ctx, `SELECT po.document_number, po.legal_entity_id, po.supplier_id, po.currency, po.payment_term_code, po.status,
			coalesce((SELECT json_agg(json_build_object('item_no', i.item_no, 'quantity', trim_scale(i.quantity)::text, 'unit', i.uom_code,
				'unit_price', i.unit_price, 'price_unit', i.price_unit, 'account_id', i.account_id, 'tax_code', i.tax_code,
				'received', trim_scale(coalesce((SELECT sum(g.quantity) FROM procurement.goods_movement_item g WHERE g.tenant_id = i.tenant_id
					AND g.purchase_order_id = i.purchase_order_id AND g.po_item_no = i.item_no), 0))::text) ORDER BY i.item_no)
			 FROM procurement.purchase_order_item i WHERE i.tenant_id = po.tenant_id AND i.purchase_order_id = po.id AND NOT i.is_removed), '[]')
		FROM procurement.purchase_order po WHERE po.id = $1`, id).
		Scan(&o.DocumentNumber, &o.LegalEntityID, &o.SupplierID, &o.Currency, &o.PaymentTermCode, &o.Status, &o.Items)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, false, nil
	}
	return o, err == nil, err
}

// OrderByNumber returns the id of the legal entity's purchase order with
// the document number, or ok = false.
func (Orders) OrderByNumber(ctx context.Context, legalEntity uuid.UUID, number string) (uuid.UUID, bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return uuid.Nil, false, err
	}
	var id uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM procurement.purchase_order WHERE legal_entity_id = $1 AND document_number = $2`, legalEntity, number).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	return id, err == nil, err
}
