package shipping

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/inventory"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/sales"
)

// PostGoodsIssue posts a delivery's goods issue (ADR-0083 §5), once: the
// delivery read under its row lock (ADR-0008) at the version the caller
// read, while it is created (409 shipping.goods_issued, or
// shipping.goods_issue_reversed once reversed, S2b3), its trade
// compliance released when it is screened (409
// shipping.trade_compliance_block: pending or blocked, S2s), with
// something picked (409 shipping.nothing_picked). Inventory posts each item's
// picked quantity as a material document of movement type 601 from the
// delivery's storage location, dated the company's date today, its value
// posted to the cost of goods sold (IssueForDelivery, which locks the
// valuations and stock after the delivery and refuses a shortage); each
// item's cost is what its line took from the stock. Sales then gives what
// the delivery held back to the order and delivers what was issued, with
// its cost, under the order's lock (Issue: 409 sales.order_credit_blocked
// for an order blocked since), so billing bills it. The delivery is
// goods-issued, naming the day, the material document and the cost's
// currency; its audit record and OutboundDeliveryChanged are written in
// the request's transaction. Locks are taken delivery, valuations and
// stock, then order; nothing takes them the other way round.
func (e *Endpoints) PostGoodsIssue(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (Delivery, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Delivery{}, err
	}
	d, err := open(ctx, tx, id, ifMatch)
	if err != nil {
		return Delivery{}, err
	}
	if err := tradeHeld(d); err != nil {
		return Delivery{}, err
	}
	var items []inventory.DeliveryItem
	var picked []int // the delivery's items picked, in the material document's item order
	for i, it := range d.Items {
		if money.ParseRat(it.Picked).Sign() > 0 {
			items = append(items, inventory.DeliveryItem{MaterialID: it.MaterialID, PlantID: d.PlantID, StorageLocationID: d.StorageLocationID,
				Quantity: it.Picked, Unit: it.Unit})
			picked = append(picked, i)
		}
	}
	if len(items) == 0 {
		return Delivery{}, problem.Conflict("shipping.nothing_picked", "Outbound delivery "+d.Number+" has nothing picked; its goods issue issues what is picked.")
	}
	doc, err := e.Stock.IssueForDelivery(ctx, d.Number, items)
	if err != nil {
		return Delivery{}, err
	}
	issued := sales.GoodsIssue{OrderID: d.OrderID, DeliveryID: d.ID, DeliveryNumber: d.Number, PlantID: d.PlantID, MaterialDocumentID: doc.ID,
		MaterialDocumentNumber: doc.Number, Date: doc.PostingDate, Currency: doc.Currency, Items: make([]sales.IssuedItem, len(d.Items))}
	for i, it := range d.Items {
		issued.Items[i] = sales.IssuedItem{ItemNo: it.OrderItemNo, Held: it.Quantity, Issued: it.Picked}
	}
	for _, l := range doc.Lines { // one line per item: an issue's item moves one location
		issued.Items[picked[l.ItemNo-1]].Cost = -l.Amount
	}
	if _, err := e.Shipments.Issue(ctx, issued); err != nil {
		return Delivery{}, err
	}
	after, err := goodsIssued(ctx, tx, p, d, issued)
	if err != nil {
		return Delivery{}, err
	}
	return after, e.record(ctx, "shipping.outbound_delivery.goods_issue", after, map[string]any{"journal_entry_number": doc.EntryNumber})
}

// goodsIssued writes each picked item's cost and the delivery's goods
// issue, moving its version, and reads it back.
func goodsIssued(ctx context.Context, tx pgx.Tx, p reqctx.Principal, d Delivery, in sales.GoodsIssue) (Delivery, error) {
	for i, it := range in.Items {
		if money.ParseRat(it.Issued).Sign() == 0 {
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE shipping.outbound_delivery_item SET cost_amount = $3 WHERE outbound_delivery_id = $1 AND item_no = $2`,
			d.ID, d.Items[i].ItemNo, it.Cost); err != nil {
			return Delivery{}, fmt.Errorf("shipping: cost of item %d of outbound delivery %s: %w", d.Items[i].ItemNo, d.Number, err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE shipping.outbound_delivery SET status = $2, goods_issue_date = $3, material_document_id = $4,
			material_document_number = $5, currency = $6, version = version + 1, updated_by = $7 WHERE id = $1`,
		d.ID, GoodsIssued, in.Date.Format(time.DateOnly), in.MaterialDocumentID, in.MaterialDocumentNumber, in.Currency, p.ID); err != nil {
		return Delivery{}, fmt.Errorf("shipping: goods issue of outbound delivery %s: %w", d.Number, err)
	}
	return load(ctx, tx, d.ID, false)
}
