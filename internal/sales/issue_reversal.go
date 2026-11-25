package sales

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// IssueReversal is the reversal of an outbound delivery's goods issue as
// sales records it (S2b3; ADR-0083 As built S2b3): the order, the
// delivery by id and number, the plant it shipped from, the reversal's
// material document (602) by id and number and the day it posted on.
type IssueReversal struct {
	OrderID                uuid.UUID
	DeliveryID             uuid.UUID
	DeliveryNumber         string
	PlantID                uuid.UUID
	MaterialDocumentID     uuid.UUID
	MaterialDocumentNumber string
	Date                   time.Time
}

// ReverseIssue records the reversal of an outbound delivery's goods issue
// on its sales order in the request's transaction: the order read under
// its row lock (ADR-0008), then the availability check's lock of each of
// the issue's materials at the plant (lockMaterials, in their order), as
// a check takes them after the order's, so a check never counts the
// stock the reversal gives back against the demand before it (the
// check reads the demand, then the stock). The goods issue's delivery of
// the order is reversed, unless an item of it is billed beyond what then
// stays delivered (409 sales.delivery_billed: its billing document is
// cancelled first); the database takes its quantities off the order's
// items, which are open again, and moves the order's version (migration
// 000453). Its audit record and SalesOrderDeliveryReversed are written
// in the request's transaction. Shipping reverses a goods issue once,
// under the delivery's row lock.
func (e *Endpoints) ReverseIssue(ctx context.Context, in IssueReversal) (Delivery, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Delivery{}, err
	}
	o, err := order(ctx, tx, in.OrderID, true)
	if err != nil {
		return Delivery{}, fmt.Errorf("sales: sales order %s of outbound delivery %s: %w", in.OrderID, in.DeliveryNumber, err)
	}
	d, err := issuedBy(ctx, tx, in)
	if err != nil {
		return Delivery{}, err
	}
	if err := lockMaterials(ctx, tx, in.PlantID, issuedMaterials(o, d)); err != nil {
		return Delivery{}, err
	}
	if err := unbilled(o, d, in.DeliveryNumber); err != nil {
		return Delivery{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sales.delivery_reversal (tenant_id, delivery_id, sales_order_id, reversal_date, material_document_id,
			material_document_number, created_by) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		p.TenantID, d.ID, o.ID, in.Date, in.MaterialDocumentID, in.MaterialDocumentNumber, p.ID); err != nil {
		return Delivery{}, fmt.Errorf("sales: reverse the delivery of outbound delivery %s: %w", in.DeliveryNumber, err)
	}
	return e.recordReversal(ctx, tx, o, d.ID)
}

// issuedBy reads the delivery of the order that the outbound delivery's
// goods issue wrote; shipping reverses only a goods issue it posted.
func issuedBy(ctx context.Context, tx pgx.Tx, in IssueReversal) (Delivery, error) {
	d, err := scanDelivery(tx.QueryRow(ctx, deliverySelect+` WHERE d.sales_order_id = $1 AND d.outbound_delivery_id = $2`, in.OrderID, in.DeliveryID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Delivery{}, fmt.Errorf("sales: sales order %s has no delivery by outbound delivery %s", in.OrderID, in.DeliveryNumber)
	}
	if err != nil {
		return Delivery{}, fmt.Errorf("sales: the delivery of outbound delivery %s: %w", in.DeliveryNumber, err)
	}
	return d, nil
}

// issuedMaterials are the materials of the order items the delivery
// delivered, once each, as PostgreSQL orders uuids (bytewise): the order
// lockMaterials takes their locks in, as checkPlan sorts them.
func issuedMaterials(o Order, d Delivery) []uuid.UUID {
	var ids []uuid.UUID
	for _, it := range d.Items {
		m := o.Items[it.ItemNo-1].MaterialID // items are numbered from 1 in order (insertItems); a goods issue's item is a material's
		if m != nil && !slices.Contains(ids, *m) {
			ids = append(ids, *m)
		}
	}
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	return ids
}

// unbilled refuses the reversal of a delivery an item of which is billed
// beyond what stays delivered without it (409 sales.delivery_billed):
// billing bills an item's delivered quantity, so its billing document is
// cancelled first (T79c), which makes it billable again.
func unbilled(o Order, d Delivery, number string) error {
	for _, it := range d.Items {
		item := o.Items[it.ItemNo-1]
		left := new(big.Rat).Sub(money.ParseRat(item.Delivered), money.ParseRat(it.Quantity))
		if money.ParseRat(item.Billed).Cmp(left) > 0 {
			return problem.Conflict("sales.delivery_billed", fmt.Sprintf("Sales order %s's item %d is billed (%s %s) beyond what stays delivered "+
				"once outbound delivery %s's goods issue is reversed; cancel its billing document first.", o.DocumentNumber, it.ItemNo, item.Billed,
				item.Unit, number))
		}
	}
	return nil
}

// recordReversal reads the order and the delivery after the reversal and
// writes its audit record and SalesOrderDeliveryReversed (ADR-0007,
// ADR-0011), naming the customer by number, never by name, and each
// item's quantity taken back with its cost and what is delivered of it
// after.
func (e *Endpoints) recordReversal(ctx context.Context, tx pgx.Tx, o Order, id uuid.UUID) (Delivery, error) {
	after, err := loadOrder(ctx, tx, o.ID, false)
	if err != nil {
		return Delivery{}, err
	}
	d, err := scanDelivery(tx.QueryRow(ctx, deliverySelect+` WHERE d.id = $1`, id))
	if err != nil {
		return Delivery{}, fmt.Errorf("sales: read delivery %s: %w", id, err)
	}
	le, _, err := e.LegalEntities.Entity(ctx, o.LegalEntityID) // a stored order's legal entity exists
	if err != nil {
		return Delivery{}, fmt.Errorf("sales: company of sales order %s: %w", o.ID, err)
	}
	names, err := e.Partners.Names(ctx, []uuid.UUID{o.CustomerID})
	if err != nil {
		return Delivery{}, fmt.Errorf("sales: customer of sales order %s: %w", o.ID, err)
	}
	return d, recordReversalEvent(ctx, d, after, le, names[o.CustomerID].Number)
}

// recordReversalEvent writes the reversal's audit record and event.
func recordReversalEvent(ctx context.Context, d Delivery, after Order, le tenancy.Entity, customerNumber string) error {
	items := make([]map[string]any, len(d.Items))
	for i, it := range d.Items {
		items[i] = map[string]any{"item_no": it.ItemNo, "quantity": map[string]string{"value": it.Quantity, "unit": it.Unit},
			"cost":               money.EventAmount(*it.Cost, *it.CostCurrency, it.CostExponent), // a goods issue's item has its cost (delivery_item_cost)
			"delivered_quantity": map[string]string{"value": after.Items[it.ItemNo-1].Delivered, "unit": it.Unit}}
	}
	date, reversed := d.Date.Format(time.DateOnly), d.ReversalDate.Format(time.DateOnly)
	payload := map[string]any{"delivery_id": d.ID, "delivery_no": d.No, "sales_order_id": after.ID, "document_number": after.DocumentNumber,
		"legal_entity_id": after.LegalEntityID, "legal_entity_code": le.Code, "customer_id": after.CustomerID, "customer_number": customerNumber,
		"delivery_date": date, "reversal_date": reversed, "outbound_delivery_id": d.OutboundDeliveryID, "outbound_delivery_number": d.OutboundDeliveryNumber,
		"material_document_id": d.MaterialDocumentID, "material_document_number": d.MaterialDocumentNumber,
		"reversal_material_document_id": d.ReversalMaterialDocumentID, "reversal_material_document_number": d.ReversalMaterialDocumentNumber,
		"items": items, "version": after.Version}
	detail := map[string]any{"sales_order_id": after.ID, "document_number": after.DocumentNumber, "delivery_no": d.No, "reversal_date": reversed,
		"items": items, "outbound_delivery_number": d.OutboundDeliveryNumber, "reversal_material_document_number": d.ReversalMaterialDocumentNumber}
	if err := audit.Record(ctx, "sales.delivery.reverse", "sales.delivery", d.ID, detail,
		outbox.Event{Context: "sales", Name: "sales_order_delivery_reversed", AggregateType: "sales.sales_order", AggregateID: after.ID,
			Payload: payload}); err != nil {
		return fmt.Errorf("sales: record the reversal of delivery %s of sales order %s: %w", d.ID, after.ID, err)
	}
	return nil
}
