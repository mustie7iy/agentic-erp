package procurement

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
)

// An award to a purchase order (T82f, ADR-0069 As built): the winning
// quotation becomes a draft purchase order through the order's own code
// path (createOrder: its checks, number, audit record and, for an RFQ from
// a requisition, the requisition moved from in_sourcing to ordered), in
// the award's transaction. The order is released afterwards as any order.

// awardedOrder creates the draft purchase order of an award of RFQ r to
// the row's quotation, dated today.
func (e *Endpoints) awardedOrder(ctx context.Context, tx pgx.Tx, r RFQ, win Compared, today time.Time) (awarded, error) {
	in, err := orderOf(ctx, tx, r, win, today)
	if err != nil {
		return awarded{}, err
	}
	o, err := e.createOrder(ctx, in)
	if err != nil {
		return awarded{}, err
	}
	return awarded{order: &o.ID, event: func(stored RFQ, win Compared, reason *string) outbox.Event {
		payload := awardPayload(stored, win, reason)
		payload["purchase_order_id"], payload["purchase_order_document_number"] = o.ID, o.DocumentNumber
		payload["order_date"] = o.OrderDate.Format(time.DateOnly)
		if stored.RequisitionID != nil {
			payload["requisition_id"], payload["requisition_document_number"] = *stored.RequisitionID, *stored.RequisitionNumber
		}
		return event("rfq_awarded_to_order", "rfq", stored.ID, payload)
	}}, nil
}

// orderOf is the purchase order an award of RFQ r to the row's quotation
// creates: with the quotation's supplier (and its payment terms), in the
// RFQ's currency, dated today, its items the RFQ's at the quoted prices
// and price units with their delivery dates; for an RFQ from a
// requisition, naming it, each item with its requisition item's account
// assignment. Amounts are written as the API's decimals, which
// createOrder parses and checks as it does a buyer's. The requisition is
// read without a lock: it is in sourcing, so its items are fixed, and
// createOrder locks it before it orders it.
func orderOf(ctx context.Context, tx pgx.Tx, r RFQ, win Compared, today time.Time) (NewOrder, error) {
	assigned := map[int32]Item{}
	if r.RequisitionID != nil {
		req, err := getRequisition(ctx, tx, *r.RequisitionID, false)
		if err != nil {
			return NewOrder{}, err
		}
		for _, it := range req.Items {
			assigned[it.No] = it
		}
	}
	items := make([]ItemInput, len(win.Priced))
	for i, it := range win.Priced {
		items[i] = ItemInput{Description: it.Description, Quantity: it.Quantity, Unit: it.Unit, Price: it.UnitPrice.Plain(r.Exponent),
			Currency: r.Currency, PriceUnit: it.PriceUnit, AccountID: it.AccountID, DeliveryDate: it.DeliveryDate}
		if it.RequisitionItemNo != nil {
			source := assigned[*it.RequisitionItemNo]
			assignment := source.assignment()
			items[i].MaterialID, items[i].AccountAssignment, items[i].CostCenterID = source.MaterialID, &assignment, source.CostCenterID
		}
	}
	return NewOrder{LegalEntityID: r.LegalEntityID, SupplierID: win.Supplier.ID, Currency: r.Currency, OrderDate: today,
		RequisitionID: r.RequisitionID, Items: items, sourcedBy: &r.ID}, nil
}

// sourcedFrom is the requisition status an order may be created from: an
// approved requisition, or one in sourcing when the award of the RFQ
// sourcing it creates the order (sourcedBy).
func (in NewOrder) sourcedFrom() string {
	if in.sourcedBy != nil {
		return InSourcing
	}
	return Approved
}
