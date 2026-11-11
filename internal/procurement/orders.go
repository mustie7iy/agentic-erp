package procurement

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Orders answers payables' questions about purchase orders, and the
// ledger's about their numbers, in the request's transaction.
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
	// CostCenterID is the item's cost center (T56e), nil when it is
	// assigned to none: an invoice item on it takes it.
	CostCenterID *uuid.UUID `json:"cost_center_id"`
	// DeliveryDate is the item's expected delivery date, nil for none
	// (T53g): when the liquidity forecast expects it invoiced.
	DeliveryDate *time.Time `json:"delivery_date"`
}

// Order is a purchase order as payables sees it.
type Order struct {
	ID              uuid.UUID
	DocumentNumber  string
	LegalEntityID   uuid.UUID
	SupplierID      uuid.UUID
	Currency        string
	OrderDate       time.Time
	PaymentTermCode string
	Status          string
	Valuation       string // ValuationDirect or ValuationGRIR (T33)
	Items           []OrderItem
}

// payablesOrder selects an order as payables sees it, from
// procurement.purchase_order po.
const payablesOrder = `SELECT po.id, po.document_number, po.legal_entity_id, po.supplier_id, po.currency, po.order_date, po.payment_term_code, po.status,
		po.valuation, coalesce((SELECT json_agg(json_build_object('item_no', i.item_no, 'quantity', trim_scale(i.quantity)::text, 'unit', i.uom_code,
			'unit_price', i.unit_price, 'price_unit', i.price_unit, 'account_id', i.account_id, 'tax_code', i.tax_code,
			-- an order's items are valued as it is released, a released one's always
			-- (value_released_items, purchase_order_item_valued): payables posts
			-- only on released orders
			'valuation_price', coalesce(i.valuation_price, 0), 'valuation_price_unit', coalesce(i.valuation_price_unit, 1),
			'cost_center_id', i.cost_center_id, 'delivery_date', ` + deliveryDate + `,
			'received', trim_scale(coalesce((SELECT sum(g.quantity) FROM procurement.goods_movement_item g WHERE g.tenant_id = i.tenant_id
				AND g.purchase_order_id = i.purchase_order_id AND g.po_item_no = i.item_no), 0))::text) ORDER BY i.item_no)
		 FROM procurement.purchase_order_item i WHERE i.tenant_id = po.tenant_id AND i.purchase_order_id = po.id AND NOT i.is_removed), '[]')
	FROM procurement.purchase_order po`

// scanPayablesOrder scans a row of payablesOrder.
func scanPayablesOrder(row pgx.Row) (Order, error) {
	var o Order
	if err := row.Scan(&o.ID, &o.DocumentNumber, &o.LegalEntityID, &o.SupplierID, &o.Currency, &o.OrderDate, &o.PaymentTermCode, &o.Status, &o.Valuation,
		&o.Items); err != nil {
		return Order{}, fmt.Errorf("scan purchase order: %w", err)
	}
	return o, nil
}

// Order returns the purchase order, or ok = false when there is none.
func (Orders) Order(ctx context.Context, id uuid.UUID) (Order, bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Order{}, false, err
	}
	o, err := scanPayablesOrder(tx.QueryRow(ctx, payablesOrder+` WHERE po.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, false, nil
	}
	if err != nil {
		return Order{}, false, fmt.Errorf("procurement: read purchase order %s: %w", id, err)
	}
	return o, true, nil
}

// Released are the legal entity's released purchase orders, which receipts
// and invoices may still come for: what the liquidity forecast reads the
// open order values of (ADR-0051 §5, T53g), by document number. A closed,
// cancelled, draft or pending order expects nothing.
func (Orders) Released(ctx context.Context, legalEntity uuid.UUID) ([]Order, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, payablesOrder+` WHERE po.legal_entity_id = $1 AND po.status = 'released' ORDER BY po.document_number`, legalEntity)
	orders, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Order, error) { return scanPayablesOrder(r) })
	if err != nil {
		return nil, fmt.Errorf("procurement: released purchase orders of %s: %w", legalEntity, err)
	}
	return orders, nil
}

// LockItems locks the order's items an invoice posts against (payables),
// FOR UPDATE in item order, as a goods movement locks them (lockItems),
// until the caller's transaction ends: a receipt, a return or the reversal
// of one on them, and another invoice's posting, wait for it, so the
// received quantities and the postings of other invoices the caller reads
// after the lock stay as it read them until it commits (a document's
// reversal takes the lock too). The posting
// takes the lock after its invoice's row and before the ledger's GR/IR
// lock, which goods movements take after the items too, so no two wait in
// a cycle.
func (Orders) LockItems(ctx context.Context, id uuid.UUID, items []int32) error {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT FROM procurement.purchase_order_item WHERE purchase_order_id = $1 AND item_no = ANY($2)
		ORDER BY item_no FOR UPDATE`, id, items); err != nil {
		return fmt.Errorf("procurement: lock %d items of purchase order %s: %w", len(items), id, err)
	}
	return nil
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

// Numbers returns the purchase orders' document numbers, keyed by id: what
// the ledger's GR/IR report shows beside an order's id. An id that names
// no order is left out.
func (Orders) Numbers(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT id, document_number FROM procurement.purchase_order WHERE id = ANY($1)`, ids)
	out := map[uuid.UUID]string{}
	var id uuid.UUID
	var number string
	_, err = pgx.ForEachRow(rows, []any{&id, &number}, func() error { out[id] = number; return nil })
	if err != nil {
		return out, fmt.Errorf("procurement: numbers of %d purchase orders: %w", len(ids), err)
	}
	return out, nil
}

// ItemAccount is a purchase order item with the account it is assigned
// to: what a report on order items shows beside the ledger's amounts.
type ItemAccount struct {
	PurchaseOrderID uuid.UUID
	DocumentNumber  string
	ItemNo          int32
	Description     string
	AccountID       uuid.UUID
}

// ItemAccounts returns the items of the purchase orders with their
// accounts, by order and item number, removed items included: a posting
// made on an item before a change removed it still names it. An id that
// names no order has no items.
func (Orders) ItemAccounts(ctx context.Context, ids []uuid.UUID) ([]ItemAccount, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT po.id, po.document_number, i.item_no, i.description, i.account_id
		FROM procurement.purchase_order po
		JOIN procurement.purchase_order_item i ON i.tenant_id = po.tenant_id AND i.purchase_order_id = po.id
		WHERE po.id = ANY($1) ORDER BY po.id, i.item_no`, ids)
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ItemAccount, error) {
		var a ItemAccount
		return a, r.Scan(&a.PurchaseOrderID, &a.DocumentNumber, &a.ItemNo, &a.Description, &a.AccountID)
	})
	if err != nil {
		return nil, fmt.Errorf("procurement: items of %d purchase orders: %w", len(ids), err)
	}
	return out, nil
}

// CreateOrder creates a draft purchase order, as POST /v1/purchase-orders
// does, for loads that go through the application services.
func (e *Endpoints) CreateOrder(ctx context.Context, in NewOrder) (PurchaseOrder, error) {
	return e.createOrder(ctx, in)
}

// ReleaseLoadedOrder releases a draft purchase order a migration run
// loaded, valued as the source system valued it: ValuationDirect, or
// ValuationGRIR for an order whose GR/IR positions the run takes over
// (T72f), which the legal entity's GR/IR account must exist for (422
// procurement.no_gr_ir_account).
func (e *Endpoints) ReleaseLoadedOrder(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, valuation string) (PurchaseOrder, error) {
	if valuation != ValuationDirect && valuation != ValuationGRIR {
		return PurchaseOrder{}, fmt.Errorf("procurement: release loaded order %s: valuation %q is neither direct nor gr_ir", id, valuation)
	}
	return e.releaseOrder(ctx, id, ifMatch, valuation)
}

// PostReceipt posts a goods receipt, as POST /v1/goods-receipts does.
func (e *Endpoints) PostReceipt(ctx context.Context, in ReceiptInput) (GoodsReceipt, error) {
	return e.postReceipt(ctx, in, true)
}

// PostLoadedReceipt records a goods receipt a migration run loaded, as
// PostReceipt does but without its WE entry: the source system's journal,
// which the run loads, carries the receipt's value, and on an order valued
// through GR/IR the run takes its open position over (T72f). A reversal of
// it on the platform posts its entry as any reversal does.
func (e *Endpoints) PostLoadedReceipt(ctx context.Context, in ReceiptInput) (GoodsReceipt, error) {
	return e.postReceipt(ctx, in, false)
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
