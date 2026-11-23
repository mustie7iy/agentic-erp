package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Inbound receipts to the warehouse system (S2q2; ADR-0082 As built S2q2):
// the procurement context publishes PurchaseOrderReleased through its
// outbox (ADR-0007) and knows nothing of warehouse systems; the wms queue
// reads it on the bus and, when an active wms connector manages the plant
// of a stock item of the order, queues the message that gives that WMS
// the receipts expected there to put away, once per order
// (wms_message_once, its document the order). The goods receipt the WMS
// reports back is recorded once per message id, as a goods issue is
// (wms_receive.go), and posted as a goods receipt (101) of the items it
// names through procurement's published API. Procurement is reached
// through WMSOrders, which the composition root wires, so integration
// imports no context.

// orderReleased is the event type of a released purchase order.
const orderReleased = "com.example.erp.procurement.purchase_order_released.v1"

// Kinds of a WMS message about a purchase order's receipts.
const (
	WMSInboundReceipt = "inbound_receipt"
	WMSGoodsReceipt   = "goods_receipt"
)

// WMSOrders reads the purchase orders whose receipts a warehouse system
// is sent, and posts the goods receipts it reports (procurement's
// published API, wired by the composition root), in the caller's
// transaction.
type WMSOrders interface {
	// Expected is the order's stock items still to be received, with the
	// codes a WMS knows them by; ok is false when there is no such order
	// or it is not released.
	Expected(ctx context.Context, order uuid.UUID) (o WMSExpectedOrder, ok bool, err error)
	// ReceiptReported posts a goods receipt (101) of the order's items at
	// the quantities received, dated the company's date.
	ReceiptReported(ctx context.Context, legalEntity, order uuid.UUID, reference string, received []WMSReceived) (WMSPosted, error)
}

// WMSExpectedOrder is a released purchase order as a WMS is sent it.
type WMSExpectedOrder struct {
	Number string
	Items  []WMSExpectedItem // its stock items with a quantity still to receive, by item number
}

// WMSExpectedItem is a stock item of an order still to be received.
type WMSExpectedItem struct {
	ItemNo                         int32
	MaterialNumber                 string
	PlantCode, StorageLocationCode string
	Quantity, Unit                 string // the quantity still to receive, in the item's unit
	DeliveryDate                   *time.Time
}

// WMSReceived is a quantity a WMS received of an order's item, in its
// unit.
type WMSReceived struct {
	ItemNo         int32
	Quantity, Unit string
}

// WMSReceipt is the message that gives a warehouse system the receipts
// expected at a plant to put away (message type inbound_receipt, version
// 1): its id, which the WMS deduplicates on, the company and plant by
// code, the purchase order by id and number with its supplier's number
// (never its name, PO 2026-11-05), and each of the order's stock items at
// the plant still to be received: number, material number, storage
// location code, open quantity and unit, and delivery date.
type WMSReceipt struct {
	MessageID      string           `json:"message_id"`
	Type           string           `json:"type"`
	SchemaVersion  int              `json:"schema_version"`
	CompanyCode    string           `json:"company_code"`
	PlantCode      string           `json:"plant_code"`
	OrderID        uuid.UUID        `json:"purchase_order_id"`
	OrderNumber    string           `json:"purchase_order_number"`
	SupplierNumber string           `json:"supplier_number"`
	Items          []WMSReceiptItem `json:"items"`
}

// WMSReceiptItem is an item of an order as a WMS is sent it.
type WMSReceiptItem struct {
	ItemNo              int32   `json:"item_no"`
	MaterialNumber      string  `json:"material_number"`
	StorageLocationCode string  `json:"storage_location_code"`
	Quantity            string  `json:"quantity"`
	Unit                string  `json:"unit"`
	DeliveryDate        *string `json:"delivery_date"` // YYYY-MM-DD, null when the item names none
}

// WMSReceiptReport is the message a warehouse system reports an order's
// goods receipt with (message type goods_receipt, version 1): its own
// message id, the purchase order it was sent by id, and what it received
// of each item it names, in the item's unit.
type WMSReceiptReport struct {
	MessageID string    `json:"message_id"`
	Type      string    `json:"type"`
	OrderID   uuid.UUID `json:"purchase_order_id"`
	Items     []struct {
		ItemNo   int32  `json:"item_no"`
		Quantity string `json:"quantity"`
		Unit     string `json:"unit"`
	} `json:"items"`
}

// orderReleasedEvent is a PurchaseOrderReleased event as the queue reads
// it: the order and the codes a WMS knows its company and supplier by.
type orderReleasedEvent struct {
	Order          uuid.UUID `json:"purchase_order_id"`
	LegalEntity    uuid.UUID `json:"legal_entity_id"`
	Company        string    `json:"legal_entity_code"`
	SupplierNumber string    `json:"supplier_number"`
}

// handleOrder queues the receipts of a released purchase order.
func (q *WMSQueue) handleOrder(ctx context.Context, m outbox.Message) error {
	var ev orderReleasedEvent
	tenant, err := parseWMSEvent(m, "procurement", &ev)
	if err == nil && ev.Order == uuid.Nil {
		err = errors.New("data: no purchase_order_id")
	}
	if err != nil {
		q.Log.WarnContext(ctx, "the wms queue refused an event", "subject", m.Subject, "ce_id", m.Header["ce-id"], "error", err)
		return nil
	}
	return q.inTenant(ctx, tenant, "purchase order "+ev.Order.String(), func(ctx context.Context, tx pgx.Tx) error { return q.queueOrder(ctx, tx, ev) })
}

// queueOrder writes the message of the order's stock items at the plant
// of its first stock item an active WMS manages, once, with its audit
// record; none when no WMS manages a plant of its stock items, or the
// order is no longer released (read as it is now). The items at other
// plants are not sent (Later: a message per plant), and the audit record
// names their plants.
func (q *WMSQueue) queueOrder(ctx context.Context, tx pgx.Tx, ev orderReleasedEvent) error {
	cs, err := activeLogistics(ctx, tx, WMS)
	if err != nil || !slices.ContainsFunc(cs, func(c wmsConnector) bool { return len(c.Plants[ev.Company]) > 0 }) {
		return err
	}
	o, ok, err := q.Orders.Expected(ctx, ev.Order)
	if err != nil {
		return fmt.Errorf("integration: the receipts of purchase order %s: %w", ev.Order, err)
	}
	if !ok {
		return nil
	}
	c, plant, found := receivingWMS(cs, ev.Company, o.Items)
	if !found {
		return nil
	}
	id := uuid.Must(uuid.NewV7())
	msg := WMSReceipt{MessageID: id.String(), Type: WMSInboundReceipt, SchemaVersion: 1, CompanyCode: ev.Company, PlantCode: plant, OrderID: ev.Order,
		OrderNumber: o.Number, SupplierNumber: ev.SupplierNumber, Items: []WMSReceiptItem{}}
	var others []string
	for _, it := range o.Items {
		if it.PlantCode != plant {
			if !slices.Contains(others, it.PlantCode) {
				others = append(others, it.PlantCode)
			}
			continue
		}
		msg.Items = append(msg.Items, WMSReceiptItem{ItemNo: it.ItemNo, MaterialNumber: it.MaterialNumber, StorageLocationCode: it.StorageLocationCode,
			Quantity: it.Quantity, Unit: it.Unit, DeliveryDate: dateOf(it.DeliveryDate)})
	}
	body, _ := json.Marshal(msg) // strings, numbers and a UUID always marshal
	return q.insertOutbound(ctx, tx, outboundRow{id: id, connector: c, kind: WMSInboundReceipt, legalEntity: ev.LegalEntity, document: ev.Order,
		number: o.Number, plant: plant, body: body, audit: map[string]any{"purchase_order_id": ev.Order, "purchase_order_number": o.Number,
			"company_code": ev.Company, "plant_code": plant, "plants_not_sent": others}})
}

// receivingWMS is the active WMS managing the plant of the order's first
// stock item at a plant one manages, and that plant.
func receivingWMS(cs []wmsConnector, company string, items []WMSExpectedItem) (wmsConnector, string, bool) {
	for _, it := range items {
		for _, c := range cs {
			if c.Manages(company, it.PlantCode) {
				return c, it.PlantCode, true
			}
		}
	}
	return wmsConnector{}, "", false
}

// dateOf is a day as a message gives it, or nil.
func dateOf(d *time.Time) *string {
	if d == nil {
		return nil
	}
	s := d.Format(time.DateOnly)
	return &s
}

// readGoodsReceipt reads a confirmation of type goods_receipt.
func readGoodsReceipt(id string, raw []byte) (WMSReceiptReport, error) {
	var gr WMSReceiptReport
	if err := json.Unmarshal(raw, &gr); err != nil {
		return gr, problem.Unprocessable("integration.wms_message_unreadable", "Confirmation "+id+" is not a JSON goods_receipt message of the WMS protocol.")
	}
	if gr.OrderID == uuid.Nil {
		return gr, problem.Unprocessable("integration.wms_message_unreadable", "Confirmation "+id+" names no purchase_order_id.")
	}
	return gr, nil
}

// postReceipt posts a goods receipt the WMS reports, of an order it was
// sent, naming only items it was sent, in a savepoint, acting for the
// order's company alone.
func (w *WMSExchange) postReceipt(ctx context.Context, tx pgx.Tx, in *inbound, gr WMSReceiptReport) error {
	var sent WMSReceipt
	if err := w.sentTo(ctx, tx, in, WMSInboundReceipt, gr.OrderID, &sent, problem.Unprocessable("integration.wms_unknown_order",
		"Confirmation "+in.messageID+" names a purchase order this warehouse system was not sent.")); err != nil {
		return err
	}
	received := make([]WMSReceived, len(gr.Items))
	for i, it := range gr.Items {
		if !slices.ContainsFunc(sent.Items, func(s WMSReceiptItem) bool { return s.ItemNo == it.ItemNo }) {
			return problem.Unprocessable("integration.wms_item_not_sent",
				fmt.Sprintf("Confirmation %s names item %d of purchase order %s, which this warehouse system was not sent.", in.messageID, it.ItemNo, sent.OrderNumber))
		}
		received[i] = WMSReceived{ItemNo: it.ItemNo, Quantity: it.Quantity, Unit: it.Unit}
	}
	return w.inSavepoint(ctx, tx, in, func(ctx context.Context) (*WMSPosted, error) {
		p, err := w.Orders.ReceiptReported(authz.ActingFor(ctx, *in.legalEntity), *in.legalEntity, gr.OrderID, in.messageID, received)
		return &p, err
	})
}
