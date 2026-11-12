package sales

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// DeliveryItemInput is a quantity delivered of one item of the order, as
// a client sends it: the order item's number and a plain decimal string
// in the order item's unit.
type DeliveryItemInput struct {
	ItemNo         int32
	Quantity, Unit string
}

// DeliveryInput is a delivery confirmation as a client sends it.
type DeliveryInput struct {
	Date  time.Time
	Items []DeliveryItemInput
}

// DeliveryItem is an item of a delivery: the quantity delivered of the
// order item ItemNo, a decimal string in that item's unit, with the order
// item's key and description.
type DeliveryItem struct {
	ItemNo      int32   `json:"item_no"`
	ItemKey     *string `json:"item_key"`
	Description string  `json:"description"`
	Quantity    string  `json:"quantity"`
	Unit        string  `json:"unit"`
}

// Delivery is a row of sales.delivery with its items: a confirmation that
// quantities of a sales order's items were delivered on a date (ADR-0067
// §8). No is its number within the order. It moves no stock and posts
// nothing.
type Delivery struct {
	ID          uuid.UUID
	No          int32
	OrderID     uuid.UUID
	OrderNumber string
	Date        time.Time
	Items       []DeliveryItem
	CreatedAt   time.Time
	CreatedBy   uuid.UUID
}

// ItemDelivery is one delivery of an order item as Orders gives it: the
// day and the quantity, a decimal string in the item's unit. Revenue
// recognition reads it (T81c).
type ItemDelivery struct {
	Date     time.Time `json:"date"`
	Quantity string    `json:"quantity"`
}

// itemDeliveries is the key an order item's deliveries are read under,
// for itemsJSON: by date, then in the order they were confirmed. The date
// is written as an instant at midnight UTC, which is what a date is
// everywhere in the context.
const itemDeliveries = `, 'deliveries', coalesce((SELECT json_agg(json_build_object('date', to_char(d.delivery_date, 'YYYY-MM-DD"T00:00:00Z"'),
		'quantity', trim_scale(i.quantity)::text) ORDER BY d.delivery_date, d.delivery_no)
	FROM sales.delivery_item i JOIN sales.delivery d ON d.tenant_id = i.tenant_id AND d.id = i.delivery_id
	WHERE i.tenant_id = t.tenant_id AND i.sales_order_id = t.sales_order_id AND i.order_item_no = t.item_no), '[]')`

// deliverySelect names the order by a subquery, not a join, for the
// reason orderSelect names the quotation by one.
const deliverySelect = `SELECT d.id, d.delivery_no, d.sales_order_id,
		(SELECT o.document_number FROM sales.sales_order o WHERE o.tenant_id = d.tenant_id AND o.id = d.sales_order_id), d.delivery_date,
		(SELECT json_agg(json_build_object('item_no', i.order_item_no, 'item_key', t.item_key, 'description', t.description,
			'quantity', trim_scale(i.quantity)::text, 'unit', t.uom_code) ORDER BY i.order_item_no)
		 FROM sales.delivery_item i JOIN sales.sales_order_item t ON t.tenant_id = i.tenant_id AND t.sales_order_id = i.sales_order_id
			AND t.item_no = i.order_item_no
		 WHERE i.tenant_id = d.tenant_id AND i.delivery_id = d.id), d.created_at, d.created_by
	FROM sales.delivery d`

func scanDelivery(row pgx.Row) (Delivery, error) {
	var d Delivery
	err := row.Scan(&d.ID, &d.No, &d.OrderID, &d.OrderNumber, &d.Date, &d.Items, &d.CreatedAt, &d.CreatedBy)
	return d, errs.Wrapf(err, "scan delivery")
}

// ConfirmDelivery records that quantities of a confirmed order's items
// were delivered on a date (ADR-0067 §8). The order's row is read FOR
// UPDATE (ADR-0008) and its items are read after the lock is held
// (loadOrder), so two confirmations of one order wait for one another
// and the second is judged on what the first left open. Only a
// confirmed order takes one (409 sales.order_not_confirmed); the date is
// not after the company's date today (422 sales.delivery_date_in_future);
// each item is one of the order's, named once, with a quantity in its
// unit of at most what is open (422, naming the request's field). The
// database adds each quantity to the order item's delivered quantity and
// moves the order's version (migration 000278). The audit record and
// SalesOrderDelivered are written in the request's transaction. No stock
// moves and nothing is posted.
func (e *Endpoints) ConfirmDelivery(ctx context.Context, orderID uuid.UUID, in DeliveryInput) (Delivery, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Delivery{}, err
	}
	o, err := order(ctx, tx, orderID, true)
	if err != nil {
		return Delivery{}, err
	}
	if o.Status != Confirmed {
		return Delivery{}, problem.Conflict("sales.order_not_confirmed",
			"Sales order "+o.DocumentNumber+" is "+o.Status+"; a delivery is confirmed against a confirmed order.")
	}
	le, _, err := e.LegalEntities.Entity(ctx, o.LegalEntityID) // a stored order's legal entity exists
	if err != nil {
		return Delivery{}, fmt.Errorf("sales: company of sales order %s: %w", o.ID, err)
	}
	if err := e.notAfterToday(ctx, le.Code, in.Date); err != nil {
		return Delivery{}, err
	}
	if err := deliverable(o, in.Items); err != nil {
		return Delivery{}, err
	}
	names, err := e.Partners.Names(ctx, []uuid.UUID{o.CustomerID})
	if err != nil {
		return Delivery{}, fmt.Errorf("sales: customer of sales order %s: %w", o.ID, err)
	}
	d, err := insertDelivery(ctx, tx, p, o.ID, in)
	if err != nil {
		return Delivery{}, err
	}
	after, err := loadOrder(ctx, tx, o.ID, false)
	if err != nil {
		return Delivery{}, err
	}
	return d, recordDelivery(ctx, d, after, le, names[o.CustomerID].Number)
}

// notAfterToday refuses a delivery dated after the company's date today
// in its time zone (422 sales.delivery_date_in_future naming
// delivery_date): what is confirmed has happened.
func (e *Endpoints) notAfterToday(ctx context.Context, company string, date time.Time) error {
	day, err := e.today(ctx, company)
	if err != nil {
		return err
	}
	if date.After(day.Date) {
		return problem.Invalid("sales.delivery_date_in_future", "delivery_date", "A delivery is confirmed on or after the day it was made: "+
			date.Format(time.DateOnly)+" is after "+day.Date.Format(time.DateOnly)+", the company's date in "+day.Zone.String()+".")
	}
	return nil
}

// deliverable checks a delivery's items against the order's, read under
// its lock: each names an item of the order, once, with a quantity more
// than zero in the item's unit that is at most the item's open quantity,
// what was ordered less what is delivered. Every refusal names the
// request's own field, and the last the item with what is open of it.
func deliverable(o Order, in []DeliveryItemInput) error {
	if len(in) == 0 {
		return problem.Invalid("sales.items_required", "items", "A delivery has at least one item.")
	}
	ordered := make(map[int32]OrderItem, len(o.Items))
	for _, it := range o.Items {
		ordered[it.ItemNo] = it
	}
	named := make(map[int32]bool, len(in))
	for i, it := range in {
		field := fmt.Sprintf("items[%d]", i)
		oi, ok := ordered[it.ItemNo]
		switch {
		case !ok:
			return problem.Invalid("sales.unknown_order_item", field+".item_no", fmt.Sprintf("Sales order %s has no item %d.", o.DocumentNumber, it.ItemNo))
		case named[it.ItemNo]:
			return problem.Invalid("sales.item_repeated", field+".item_no", fmt.Sprintf("Item %d is named more than once; a delivery names an order item once.", it.ItemNo))
		case money.PositiveQuantity(it.Quantity) != nil:
			return problem.Invalid("sales.invalid_quantity", field+".quantity.value", fmt.Sprintf(
				"A quantity is a plain decimal more than zero of at most %d integer digits and %d decimals.", money.QuantityIntDigits, money.QuantityDecimals))
		case it.Unit != oi.Unit:
			return problem.Invalid("sales.unit_mismatch", field+".quantity.unit",
				fmt.Sprintf("Item %d of sales order %s is in %s; its delivery is confirmed in that unit.", it.ItemNo, o.DocumentNumber, oi.Unit))
		}
		named[it.ItemNo] = true
		open := new(big.Rat).Sub(money.ParseRat(oi.Quantity), money.ParseRat(oi.Delivered))
		if money.ParseRat(it.Quantity).Cmp(open) > 0 {
			return problem.Invalid("sales.exceeds_open_quantity", field+".quantity.value", fmt.Sprintf(
				"Item %d of sales order %s has %s %s open for delivery (%s ordered, %s delivered); %s is more.",
				it.ItemNo, o.DocumentNumber, money.PlainQuantity(open), oi.Unit, oi.Quantity, oi.Delivered, it.Quantity))
		}
	}
	return nil
}

// insertDelivery writes the delivery with its items and returns it as it
// reads. Its number is the next within the order, which the caller holds
// locked.
func insertDelivery(ctx context.Context, tx pgx.Tx, p reqctx.Principal, orderID uuid.UUID, in DeliveryInput) (Delivery, error) {
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO sales.delivery (tenant_id, id, sales_order_id, delivery_no, delivery_date, created_by)
		SELECT $1, $2, $3, coalesce(max(delivery_no), 0) + 1, $4, $5 FROM sales.delivery WHERE sales_order_id = $3`,
		p.TenantID, id, orderID, in.Date, p.ID); err != nil {
		return Delivery{}, fmt.Errorf("sales: insert a delivery of sales order %s: %w", orderID, err)
	}
	nos, quantities := make([]int32, len(in.Items)), make([]string, len(in.Items))
	for i, it := range in.Items {
		nos[i], quantities[i] = it.ItemNo, it.Quantity
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sales.delivery_item (tenant_id, delivery_id, sales_order_id, order_item_no, quantity)
		SELECT $1, $2, $3, n, q::numeric FROM unnest($4::int[], $5::text[]) AS x(n, q)`, p.TenantID, id, orderID, nos, quantities); err != nil {
		return Delivery{}, fmt.Errorf("sales: write items of delivery %s: %w", id, err)
	}
	d, err := scanDelivery(tx.QueryRow(ctx, deliverySelect+` WHERE d.id = $1`, id))
	if err != nil {
		return Delivery{}, fmt.Errorf("sales: read delivery %s: %w", id, err)
	}
	return d, nil
}

// recordDelivery adds the audit record of a delivery and publishes
// SalesOrderDelivered in the same transaction (ADR-0007, ADR-0011). The
// event is the order's (its subject is the order's id): it carries the
// delivery's items with what is delivered of each order item after it,
// names the customer by id and number, never by name (product owner,
// 2026-11-05), and gives the order's version after the delivery.
func recordDelivery(ctx context.Context, d Delivery, after Order, le tenancy.Entity, customerNumber string) error {
	delivered := make(map[int32]string, len(after.Items))
	for _, it := range after.Items {
		delivered[it.ItemNo] = it.Delivered
	}
	items := make([]map[string]any, len(d.Items))
	for i, it := range d.Items {
		items[i] = map[string]any{"item_no": it.ItemNo, "quantity": map[string]string{"value": it.Quantity, "unit": it.Unit},
			"delivered_quantity": map[string]string{"value": delivered[it.ItemNo], "unit": it.Unit}}
	}
	date := d.Date.Format(time.DateOnly)
	payload := map[string]any{"delivery_id": d.ID, "delivery_no": d.No, "sales_order_id": after.ID, "document_number": after.DocumentNumber,
		"legal_entity_id": after.LegalEntityID, "legal_entity_code": le.Code, "customer_id": after.CustomerID, "customer_number": customerNumber,
		"delivery_date": date, "items": items, "version": after.Version}
	detail := map[string]any{"sales_order_id": after.ID, "document_number": after.DocumentNumber, "delivery_no": d.No, "delivery_date": date, "items": items}
	if err := audit.Record(ctx, "sales.delivery.confirm", "sales.delivery", d.ID, detail,
		outbox.Event{Context: "sales", Name: "sales_order_delivered", AggregateType: "sales.sales_order", AggregateID: after.ID, Payload: payload}); err != nil {
		return fmt.Errorf("sales: record delivery %s of sales order %s: %w", d.ID, after.ID, err)
	}
	return nil
}
