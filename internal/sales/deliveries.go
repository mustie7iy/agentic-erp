package sales

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
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
type DeliveryItemInput = ItemQuantity

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
	// Cost is what a goods issue's item took from the stock, minor units
	// of CostCurrency with CostExponent digits (ADR-0083 §5); nil on a
	// confirmation by hand.
	Cost         *money.Minor `json:"cost_amount"`
	CostCurrency *string      `json:"cost_currency"`
	CostExponent int          `json:"cost_exponent"`
}

// Delivery is a row of sales.delivery with its items: a confirmation that
// quantities of a sales order's items were delivered on a date (ADR-0067
// §8). No is its number within the order. A confirmation by a person
// moves no stock and posts nothing; one an outbound delivery's goods issue
// wrote (ADR-0083 §5) names the delivery and its material document, nil
// otherwise.
type Delivery struct {
	ID                     uuid.UUID
	No                     int32
	OrderID                uuid.UUID
	OrderNumber            string
	Date                   time.Time
	OutboundDeliveryID     *uuid.UUID
	OutboundDeliveryNumber *string
	MaterialDocumentID     *uuid.UUID
	MaterialDocumentNumber *string
	Items                  []DeliveryItem
	CreatedAt              time.Time
	CreatedBy              uuid.UUID
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
		d.outbound_delivery_id, d.outbound_delivery_number, d.material_document_id, d.material_document_number,
		(SELECT json_agg(json_build_object('item_no', i.order_item_no, 'item_key', t.item_key, 'description', t.description,
			'quantity', trim_scale(i.quantity)::text, 'unit', t.uom_code, 'cost_amount', i.cost_amount, 'cost_currency', i.cost_currency,
			'cost_exponent', (SELECT c.exponent FROM ref.currency c WHERE c.code = i.cost_currency)) ORDER BY i.order_item_no)
		 FROM sales.delivery_item i JOIN sales.sales_order_item t ON t.tenant_id = i.tenant_id AND t.sales_order_id = i.sales_order_id
			AND t.item_no = i.order_item_no
		 WHERE i.tenant_id = d.tenant_id AND i.delivery_id = d.id), d.created_at, d.created_by
	FROM sales.delivery d`

func scanDelivery(row pgx.Row) (Delivery, error) {
	var d Delivery
	err := row.Scan(&d.ID, &d.No, &d.OrderID, &d.OrderNumber, &d.Date, &d.OutboundDeliveryID, &d.OutboundDeliveryNumber, &d.MaterialDocumentID,
		&d.MaterialDocumentNumber, &d.Items, &d.CreatedAt, &d.CreatedBy)
	return d, errs.Wrapf(err, "scan delivery")
}

// ConfirmDelivery records that quantities of a confirmed order's items
// were delivered on a date (ADR-0067 §8). The order's row is read FOR
// UPDATE (ADR-0008) and its items are read after the lock is held
// (loadOrder), so two confirmations of one order wait for one another
// and the second is judged on what the first left open. Only a
// confirmed order takes one (409 sales.order_not_confirmed), and one
// blocked for credit none until its block is released (409
// sales.order_credit_blocked; ADR-0066, As built T77b); the date is
// not after the company's date today (422 sales.delivery_date_in_future);
// each item is one of the order's, named once, with a quantity in its
// unit of at most what is open (422, naming the request's field). The
// database adds each quantity to the order item's delivered quantity and
// moves the order's version (migration 000292). An item of a material
// kept in stock is delivered by an outbound delivery's goods issue, not
// by hand (422 sales.delivery_by_goods_issue; ADR-0083 §5). The audit
// record and SalesOrderDelivered are written in the request's
// transaction. No stock moves and nothing is posted.
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
	if o.CreditBlock == Blocked {
		return Delivery{}, problem.Conflict("sales.order_credit_blocked",
			"Sales order "+o.DocumentNumber+" is blocked for credit; it takes a delivery once its credit block is released.")
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
	if err := e.byHand(ctx, o, in.Items); err != nil {
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

// afterToday says whether the date is after the company's date today in
// its time zone, with that day and the zone it is told in for the
// refusal: what a person confirms as delivered or received has happened.
func (e *Endpoints) afterToday(ctx context.Context, company string, date time.Time) (config.CompanyDay, bool, error) {
	day, err := e.today(ctx, company)
	if err != nil {
		return config.CompanyDay{}, false, err
	}
	return day, date.After(day.Date), nil
}

// notAfterToday refuses a delivery dated after the company's date today
// in its time zone (422 sales.delivery_date_in_future naming
// delivery_date).
func (e *Endpoints) notAfterToday(ctx context.Context, company string, date time.Time) error {
	day, after, err := e.afterToday(ctx, company, date)
	if err != nil {
		return err
	}
	if after {
		return problem.Invalid("sales.delivery_date_in_future", "delivery_date", "A delivery is confirmed on or after the day it was made: "+
			date.Format(time.DateOnly)+" is after "+day.Date.Format(time.DateOnly)+", the company's date in "+day.Zone.String()+".")
	}
	return nil
}

// deliverable checks a delivery confirmation's items against the order's
// (takeable), naming the request's items[i].item_no.
func deliverable(o Order, in []DeliveryItemInput) error {
	return takeable(o, in, "A delivery", "item_no")
}

// takeable checks the items a delivery or an outbound delivery takes of
// the order, read under its lock (taking): each names an item of the
// order, once, with a quantity more than zero in the item's unit that is
// at most the item's open quantity, what was ordered less what is
// delivered and what outbound deliveries hold (ADR-0083 §4); a removed
// item (T78e) takes none. Every refusal names the request's own field
// (items[i].noField or .quantity), and the last the item with what is
// open of it.
func takeable(o Order, in []ItemQuantity, document, noField string) error {
	ordered := make(map[int32]OrderItem, len(o.Items))
	for _, it := range o.Items {
		ordered[it.ItemNo] = it
	}
	for i, d := range in {
		if it, ok := ordered[d.ItemNo]; ok && it.RemovedInChange != nil {
			return itemRemoved(fmt.Sprintf("items[%d].%s", i, noField), o, it)
		}
	}
	short, err := taking{document: document, noField: noField,
		find: func(no int32) (string, *big.Rat, bool) {
			it, ok := ordered[no]
			if !ok {
				return "", nil, false
			}
			return it.Unit, it.open(), true
		},
		otherUnit: func(no int32, unit string) string {
			return fmt.Sprintf("Item %d of sales order %s is in %s; its delivery is in that unit.", no, o.DocumentNumber, unit)
		}}.check(in)
	switch {
	case err != nil || short == nil:
		return err
	case short.open == nil:
		return problem.Invalid("sales.unknown_order_item", short.noField, fmt.Sprintf("Sales order %s has no item %d.", o.DocumentNumber, short.no))
	}
	it := ordered[short.no]
	held := ""
	if money.ParseRat(it.InDelivery).Sign() != 0 {
		held = ", " + it.InDelivery + " in outbound deliveries"
	}
	return problem.Invalid("sales.exceeds_open_quantity", short.quantityField, fmt.Sprintf(
		"Item %d of sales order %s has %s %s open for delivery (%s ordered, %s delivered%s); %s is more.",
		short.no, o.DocumentNumber, money.PlainQuantity(short.open), it.Unit, it.Quantity, it.Delivered, held, short.quantity))
}

// byHand refuses confirming the delivery of an item of a material kept in
// stock (422 sales.delivery_by_goods_issue naming items[i].item_no): its
// delivery is an outbound delivery's goods issue (ADR-0083 §5). A
// service or free-text item, and one of a material kept in no stock, is
// confirmed by hand.
func (e *Endpoints) byHand(ctx context.Context, o Order, in []DeliveryItemInput) error {
	var ids []uuid.UUID
	for _, it := range o.Items {
		if it.MaterialID != nil {
			ids = append(ids, *it.MaterialID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	ms, err := e.Materials.Materials(ctx, ids)
	if err != nil {
		return fmt.Errorf("sales: materials of sales order %s: %w", o.DocumentNumber, err)
	}
	for i, d := range in {
		it := o.Items[d.ItemNo-1] // deliverable found each item; items are numbered from 1 in order (insertItems)
		if it.MaterialID == nil || !ms[*it.MaterialID].Stocked() {
			continue
		}
		return problem.Invalid("sales.delivery_by_goods_issue", fmt.Sprintf("items[%d].item_no", i), fmt.Sprintf(
			"Item %d of sales order %s is material %s, kept in stock: it is delivered by an outbound delivery's goods issue (createOutboundDelivery).",
			it.ItemNo, o.DocumentNumber, ms[*it.MaterialID].Number))
	}
	return nil
}

// deliveries are the tables a delivery is written to.
var deliveries = confirmation{table: "delivery", document: "sales_order_id", number: "delivery_no", date: "delivery_date",
	items: "delivery_item", parent: "delivery_id", item: "order_item_no"}

// insertDelivery writes the delivery with its items and returns it as it
// reads. Its number is the next within the order, which the caller holds
// locked.
func insertDelivery(ctx context.Context, tx pgx.Tx, p reqctx.Principal, orderID uuid.UUID, in DeliveryInput) (Delivery, error) {
	id, err := deliveries.insert(ctx, tx, p, orderID, in.Date, in.Items)
	if err != nil {
		return Delivery{}, err
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
		if it.Cost != nil {
			items[i]["cost"] = money.EventAmount(*it.Cost, *it.CostCurrency, it.CostExponent)
		}
	}
	date := d.Date.Format(time.DateOnly)
	payload := map[string]any{"delivery_id": d.ID, "delivery_no": d.No, "sales_order_id": after.ID, "document_number": after.DocumentNumber,
		"legal_entity_id": after.LegalEntityID, "legal_entity_code": le.Code, "customer_id": after.CustomerID, "customer_number": customerNumber,
		"delivery_date": date, "items": items, "version": after.Version, "outbound_delivery_id": d.OutboundDeliveryID,
		"outbound_delivery_number": d.OutboundDeliveryNumber, "material_document_id": d.MaterialDocumentID,
		"material_document_number": d.MaterialDocumentNumber}
	detail := map[string]any{"sales_order_id": after.ID, "document_number": after.DocumentNumber, "delivery_no": d.No, "delivery_date": date, "items": items,
		"outbound_delivery_number": d.OutboundDeliveryNumber, "material_document_number": d.MaterialDocumentNumber}
	if err := audit.Record(ctx, "sales.delivery.confirm", "sales.delivery", d.ID, detail,
		outbox.Event{Context: "sales", Name: "sales_order_delivered", AggregateType: "sales.sales_order", AggregateID: after.ID, Payload: payload}); err != nil {
		return fmt.Errorf("sales: record delivery %s of sales order %s: %w", d.ID, after.ID, err)
	}
	return nil
}
