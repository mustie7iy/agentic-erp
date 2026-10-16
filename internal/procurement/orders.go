package procurement

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Orders answers payables' questions about purchase orders, in the
// request's transaction.
type Orders struct{}

// OrderItem is a live item of a purchase order with its received quantity.
// Quantities are decimal strings with up to six decimals.
type OrderItem struct {
	ItemNo    int32       `json:"item_no"`
	Quantity  string      `json:"quantity"`
	Unit      string      `json:"unit"`
	UnitPrice money.Minor `json:"unit_price"` // minor units of the order currency per PriceUnit
	PriceUnit int32       `json:"price_unit"`
	AccountID uuid.UUID   `json:"account_id"`
	TaxCode   *string     `json:"tax_code"`
	Received  string      `json:"received"` // net of every goods movement: 101 and 123 in, 102 and 122 out
	// ValuationPrice per ValuationPriceUnit is what the item's GR/IR
	// position is valued at, fixed at its release (T47a); UnitPrice is the
	// price an invoice is matched against.
	ValuationPrice     money.Minor `json:"valuation_price"`
	ValuationPriceUnit int32       `json:"valuation_price_unit"`
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
	Valuation       string // ValuationDirect or ValuationGRIR (T33)
	Items           []OrderItem
}

// Order returns the purchase order, or ok = false when there is none.
func (Orders) Order(ctx context.Context, id uuid.UUID) (Order, bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Order{}, false, err
	}
	o := Order{ID: id}
	err = tx.QueryRow(ctx, `SELECT po.document_number, po.legal_entity_id, po.supplier_id, po.currency, po.payment_term_code, po.status, po.valuation,
			coalesce((SELECT json_agg(json_build_object('item_no', i.item_no, 'quantity', trim_scale(i.quantity)::text, 'unit', i.uom_code,
				'unit_price', i.unit_price, 'price_unit', i.price_unit, 'account_id', i.account_id, 'tax_code', i.tax_code,
				-- an order's items are valued as it is released, a released one's always
				-- (value_released_items, purchase_order_item_valued): payables posts
				-- only on released orders
				'valuation_price', coalesce(i.valuation_price, 0), 'valuation_price_unit', coalesce(i.valuation_price_unit, 1),
				'received', trim_scale(coalesce((SELECT sum(g.quantity) FROM procurement.goods_movement_item g WHERE g.tenant_id = i.tenant_id
					AND g.purchase_order_id = i.purchase_order_id AND g.po_item_no = i.item_no), 0))::text) ORDER BY i.item_no)
			 FROM procurement.purchase_order_item i WHERE i.tenant_id = po.tenant_id AND i.purchase_order_id = po.id AND NOT i.is_removed), '[]')
		FROM procurement.purchase_order po WHERE po.id = $1`, id).
		Scan(&o.DocumentNumber, &o.LegalEntityID, &o.SupplierID, &o.Currency, &o.PaymentTermCode, &o.Status, &o.Valuation, &o.Items)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, false, nil
	}
	if err != nil {
		return Order{}, false, fmt.Errorf("procurement: read purchase order %s: %w", id, err)
	}
	return o, true, nil
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
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("procurement: find purchase order %s: %w", number, err)
	}
	return id, true, nil
}

// CreateOrder creates a draft purchase order, as POST /v1/purchase-orders
// does, for loads that go through the application services.
func (e *Endpoints) CreateOrder(ctx context.Context, in NewOrder) (PurchaseOrder, error) {
	return e.createOrder(ctx, in)
}

// ReleaseLoadedOrder releases a draft purchase order a migration run
// loaded.
func (e *Endpoints) ReleaseLoadedOrder(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (PurchaseOrder, error) {
	return e.releaseOrder(ctx, id, ifMatch, true)
}

// PostReceipt posts a goods receipt, as POST /v1/goods-receipts does.
func (e *Endpoints) PostReceipt(ctx context.Context, in ReceiptInput) (GoodsReceipt, error) {
	return e.postReceipt(ctx, in)
}

// Value is the number of the purchase orders and the total of their live
// items, in minor units of their currencies.
func (Orders) Value(ctx context.Context, ids []uuid.UUID) (int, money.Minor, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return 0, 0, err
	}
	var n int
	var value money.Minor
	err = tx.QueryRow(ctx, `SELECT count(*), coalesce(sum((SELECT coalesce(sum(round(i.quantity * i.unit_price / i.price_unit)), 0)
			FROM procurement.purchase_order_item i WHERE i.tenant_id = o.tenant_id AND i.purchase_order_id = o.id AND NOT i.is_removed)), 0)::bigint
		FROM procurement.purchase_order o WHERE o.id = ANY($1)`, ids).Scan(&n, &value)
	if err != nil {
		return 0, 0, fmt.Errorf("procurement: value %d purchase orders: %w", len(ids), err)
	}
	return n, value, nil
}
