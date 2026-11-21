package sales

import (
	"context"
	"fmt"
	"math/big"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// What outbound deliveries hold of a sales order (ADR-0083 §4): the
// shipping context takes quantities of an order's items into a delivery
// (Ship) and, at its goods issue, gives them back and delivers what it
// issued (S2b1). Sales keeps them as assignments
// (sales.delivery_assignment), which the database adds to each item's
// in_delivery_quantity, so an order change, an item's removal and the
// order's cancellation are judged on them as on what is delivered.

func init() {
	// The application checks these under the order's row lock first;
	// they are the database's answers should one pass it.
	problem.RegisterConstraint("sales_order_item_in_delivery", func() *problem.Problem {
		return problem.Conflict("sales.exceeds_open_quantity", "An order item's quantity is at least what is delivered and what outbound deliveries hold.")
	})
	problem.RegisterConstraint("sales_order_item_confirmed", func() *problem.Problem {
		return problem.Conflict("sales.exceeds_open_quantity", "An order item's deliveries take at most what its availability check confirmed.")
	})
	problem.RegisterConstraint("sales_order_item_removed_unshipped", func() *problem.Problem {
		return problem.Conflict("sales.order_item_delivered", "An order item an outbound delivery holds is not removed.")
	})
	problem.RegisterConstraint("sales_order_cancelled_unshipped", func() *problem.Problem {
		return problem.Conflict("sales.order_delivered", "A sales order an outbound delivery holds is not cancelled.")
	})
}

// ShipInput is what an outbound delivery takes of a sales order: the
// delivery by id and number, the plant it ships from, and its items, each
// a quantity of an order item in its unit, or, when Items is nil, every
// open quantity of the order items Open names.
type ShipInput struct {
	OrderID        uuid.UUID
	DeliveryID     uuid.UUID
	DeliveryNumber string
	// PlantID is the plant the delivery ships from, required: what it
	// holds there of an order without a plant counts in that plant's
	// availability (S2j).
	PlantID uuid.UUID
	Items   []ItemQuantity
	Open    []int32
}

// Ship takes quantities of a confirmed sales order's items into an
// outbound delivery in the request's transaction. The order is read
// under its row lock (ADR-0008), as a delivery confirmation reads it, so
// two deliveries of one order, or one and an order change, wait for one
// another and the second is judged on what the first left; an order of a
// company the caller may not act in is not found. Only a confirmed order
// not blocked for credit is shipped (409 sales.order_not_confirmed,
// sales.order_credit_blocked). Named items are checked as a delivery's
// are (taking), the refusals naming the request's items[i].order_item_no
// and items[i].quantity: each at most what is open, ordered less
// delivered less what deliveries hold (422 sales.exceeds_open_quantity);
// without items each item of Open takes what is open of it, and an order
// with nothing open of them is refused (422 sales.nothing_to_deliver
// naming sales_order_id). Each quantity is an assignment, which adds it
// to the item's quantity in delivery and moves the order's version, and
// the audit record names the delivery.
func (e *Endpoints) Ship(ctx context.Context, in ShipInput) (Shipped, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Shipped{}, err
	}
	o, err := order(ctx, tx, in.OrderID, true)
	if err != nil {
		return Shipped{}, problem.Reference(err, "sales_order_id", "sales order", "no sales order with this id that you can access")
	}
	if err := shippable(o); err != nil {
		return Shipped{}, err
	}
	items := in.Items
	if items == nil {
		items = openItems(o, in.Open)
		if len(items) == 0 {
			return Shipped{}, problem.Invalid("sales.nothing_to_deliver", "sales_order_id", "Sales order "+o.DocumentNumber+
				" has nothing open to deliver of the items a delivery takes: what was ordered is delivered or held by other deliveries.")
		}
	} else if err := takeable(o, items, "An outbound delivery", "order_item_no"); err != nil {
		return Shipped{}, err
	}
	names, err := e.Partners.Names(ctx, []uuid.UUID{o.CustomerID})
	if err != nil {
		return Shipped{}, fmt.Errorf("sales: customer of sales order %s: %w", o.ID, err)
	}
	if err := assign(ctx, tx, p, held{order: o.ID, delivery: in.DeliveryID, number: in.DeliveryNumber, plant: in.PlantID}, items, false); err != nil {
		return Shipped{}, err
	}
	detail := map[string]any{"document_number": o.DocumentNumber, "outbound_delivery_id": in.DeliveryID, "outbound_delivery_number": in.DeliveryNumber,
		"items": shippedDetail(o, items)}
	if err := audit.Record(ctx, "sales.sales_order.ship", "sales.sales_order", o.ID, detail); err != nil {
		return Shipped{}, fmt.Errorf("sales: record the shipping of sales order %s: %w", o.ID, err)
	}
	return Shipped{Order: o, CustomerNumber: names[o.CustomerID].Number, Items: items}, nil
}

// Shipped is what Ship took: the order as read under its lock, its
// customer's number and the quantities taken, by item number.
type Shipped struct {
	Order          Order
	CustomerNumber string
	Items          []ItemQuantity
}

// shippable refuses an order that takes no delivery: one not confirmed,
// or one blocked for credit until its block is released (ADR-0066).
func shippable(o Order) error {
	switch {
	case o.Status != Confirmed:
		return problem.Conflict("sales.order_not_confirmed",
			"Sales order "+o.DocumentNumber+" is "+o.Status+"; a delivery is made of a confirmed order.")
	case o.CreditBlock == Blocked:
		return problem.Conflict("sales.order_credit_blocked",
			"Sales order "+o.DocumentNumber+" is blocked for credit; it takes a delivery once its credit block is released.")
	}
	return nil
}

// open is what is open of an order item to deliver: what was ordered,
// or for an item the availability check confirmed what it confirmed
// (S2j), less what is delivered and what outbound deliveries hold.
func (it OrderItem) open() *big.Rat {
	promised := it.Quantity
	if it.Confirmed != nil {
		promised = *it.Confirmed
	}
	open := new(big.Rat).Sub(money.ParseRat(promised), money.ParseRat(it.Delivered))
	return open.Sub(open, money.ParseRat(it.InDelivery))
}

// openItems are the open quantities of the order's items that nos name,
// by item number, leaving out a removed item and one with nothing open.
func openItems(o Order, nos []int32) []ItemQuantity {
	var out []ItemQuantity
	for _, it := range o.Items {
		if it.RemovedInChange != nil || !slices.Contains(nos, it.ItemNo) {
			continue
		}
		if open := it.open(); open.Sign() > 0 {
			out = append(out, ItemQuantity{ItemNo: it.ItemNo, Quantity: money.PlainQuantity(open), Unit: it.Unit})
		}
	}
	return out
}

// held is whose quantities an assignment holds: the order's, for the
// outbound delivery by id and number, at the plant it ships from.
type held struct {
	order, delivery uuid.UUID
	number          string
	plant           uuid.UUID
}

// assign writes an assignment per quantity, in the order given, each
// adding it to its order item's quantity in delivery, negated when back
// is set (the goods issue gives a delivery's quantities back).
func assign(ctx context.Context, tx pgx.Tx, p reqctx.Principal, h held, items []ItemQuantity, back bool) error {
	if h.plant == uuid.Nil {
		return fmt.Errorf("sales: outbound delivery %s of sales order %s names no plant", h.number, h.order)
	}
	for _, it := range items {
		q := it.Quantity
		if back {
			q = money.PlainQuantity(new(big.Rat).Neg(money.ParseRat(q)))
		}
		if _, err := tx.Exec(ctx, `INSERT INTO sales.delivery_assignment (tenant_id, id, sales_order_id, order_item_no, outbound_delivery_id,
				outbound_delivery_number, quantity, created_by, plant_id) VALUES ($1, $2, $3, $4, $5, $6, $7::numeric, $8, $9)`,
			p.TenantID, uuid.Must(uuid.NewV7()), h.order, it.ItemNo, h.delivery, h.number, q, p.ID, h.plant); err != nil {
			return fmt.Errorf("sales: assign item %d of sales order %s to outbound delivery %s: %w", it.ItemNo, h.order, h.number, err)
		}
	}
	return nil
}

// shippedDetail is the audit detail of the quantities a delivery takes,
// each with its order item's unit.
func shippedDetail(o Order, items []ItemQuantity) []map[string]any {
	out := make([]map[string]any, len(items))
	for i, it := range items {
		out[i] = map[string]any{"item_no": it.ItemNo, "quantity": it.Quantity, "unit": o.Items[it.ItemNo-1].Unit} // items are numbered from 1 in order (insertItems)
	}
	return out
}

// IssuedItem is an order item an outbound delivery's goods issue
// delivered: Held, what the delivery held of it, which goes back to the
// order, and Issued, what its goods issue issued of it, in the item's
// unit, with Cost, what that took from the stock in the company's
// currency; nothing issued: no cost.
type IssuedItem struct {
	ItemNo       int32
	Held, Issued string
	Cost         money.Minor
}

// GoodsIssue is an outbound delivery's goods issue as sales records it:
// the delivery and its material document by id and number, the plant it
// shipped from, the day it posted on, the cost's currency and the items.
type GoodsIssue struct {
	OrderID                uuid.UUID
	DeliveryID             uuid.UUID
	DeliveryNumber         string
	PlantID                uuid.UUID
	MaterialDocumentID     uuid.UUID
	MaterialDocumentNumber string
	Date                   time.Time
	Currency               string
	Items                  []IssuedItem
}

// Issue records an outbound delivery's goods issue on its sales order
// (ADR-0083 §5) in the request's transaction, the order read under its
// row lock (ADR-0008): one blocked for credit since the delivery was made
// is refused (409 sales.order_credit_blocked). What the delivery held of
// each item goes back to the order, and what it issued is delivered: a
// delivery of the order (T78c's record) naming the outbound delivery and
// the material document, each item with its cost, which billing then
// bills. Its audit record and SalesOrderDelivered are written as a
// confirmation's are. Shipping issues something of at least one item.
func (e *Endpoints) Issue(ctx context.Context, in GoodsIssue) (Delivery, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Delivery{}, err
	}
	o, err := order(ctx, tx, in.OrderID, true)
	if err != nil {
		return Delivery{}, fmt.Errorf("sales: sales order %s of outbound delivery %s: %w", in.OrderID, in.DeliveryNumber, err)
	}
	if o.CreditBlock == Blocked {
		return Delivery{}, problem.Conflict("sales.order_credit_blocked",
			"Sales order "+o.DocumentNumber+" is blocked for credit; its goods issue is posted once its credit block is released.")
	}
	back := make([]ItemQuantity, len(in.Items))
	for i, it := range in.Items {
		back[i] = ItemQuantity{ItemNo: it.ItemNo, Quantity: it.Held}
	}
	if err := assign(ctx, tx, p, held{order: o.ID, delivery: in.DeliveryID, number: in.DeliveryNumber, plant: in.PlantID}, back, true); err != nil {
		return Delivery{}, err
	}
	le, _, err := e.LegalEntities.Entity(ctx, o.LegalEntityID) // a stored order's legal entity exists
	if err != nil {
		return Delivery{}, fmt.Errorf("sales: company of sales order %s: %w", o.ID, err)
	}
	names, err := e.Partners.Names(ctx, []uuid.UUID{o.CustomerID})
	if err != nil {
		return Delivery{}, fmt.Errorf("sales: customer of sales order %s: %w", o.ID, err)
	}
	d, err := insertIssue(ctx, tx, p, in)
	if err != nil {
		return Delivery{}, err
	}
	after, err := loadOrder(ctx, tx, o.ID, false)
	if err != nil {
		return Delivery{}, err
	}
	return d, recordDelivery(ctx, d, after, le, names[o.CustomerID].Number)
}

// insertIssue writes the goods issue's delivery of the order, numbered
// next within it, with an item per order item it issued something of,
// and reads it back.
func insertIssue(ctx context.Context, tx pgx.Tx, p reqctx.Principal, in GoodsIssue) (Delivery, error) {
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO sales.delivery (tenant_id, id, sales_order_id, delivery_no, delivery_date, outbound_delivery_id,
			outbound_delivery_number, material_document_id, material_document_number, created_by)
		SELECT $1, $2, $3, coalesce(max(delivery_no), 0) + 1, $4, $5, $6, $7, $8, $9 FROM sales.delivery WHERE sales_order_id = $3`,
		p.TenantID, id, in.OrderID, in.Date, in.DeliveryID, in.DeliveryNumber, in.MaterialDocumentID, in.MaterialDocumentNumber, p.ID); err != nil {
		return Delivery{}, fmt.Errorf("sales: delivery of outbound delivery %s: %w", in.DeliveryNumber, err)
	}
	for _, it := range in.Items {
		if money.ParseRat(it.Issued).Sign() == 0 {
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO sales.delivery_item (tenant_id, delivery_id, sales_order_id, order_item_no, quantity, cost_amount,
				cost_currency) VALUES ($1, $2, $3, $4, $5::numeric, $6, $7)`, p.TenantID, id, in.OrderID, it.ItemNo, it.Issued, it.Cost, in.Currency); err != nil {
			return Delivery{}, fmt.Errorf("sales: item %d of the delivery of outbound delivery %s: %w", it.ItemNo, in.DeliveryNumber, err)
		}
	}
	d, err := scanDelivery(tx.QueryRow(ctx, deliverySelect+` WHERE d.id = $1`, id))
	if err != nil {
		return Delivery{}, fmt.Errorf("sales: read delivery %s: %w", id, err)
	}
	return d, nil
}
