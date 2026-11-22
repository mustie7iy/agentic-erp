package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
)

// Outbound deliveries to the warehouse system (S2q1): the shipping
// context publishes OutboundDeliveryChanged through its outbox (ADR-0007)
// and knows nothing of warehouse systems; this consumer reads the event
// of a delivery's creation on the bus, at least once, and, when an active
// wms connector manages the delivery's plant, queues the message that
// gives the WMS the delivery to pick: a pending outbound row of
// integration.wms_message, the outbox of the call, which the exchange
// sends after this commit. A delivery is queued once
// (wms_message_once), so a second reading adds nothing. A delivery
// created before its plant had a WMS is not sent (Later: send one by
// hand).

// WMSConsumer is the durable JetStream consumer deliveries are queued
// for warehouse systems with; replicas share it.
const WMSConsumer = "wms-outbound"

// deliveryChanged is the event type the consumer acts on.
const deliveryChanged = "com.example.erp.shipping.outbound_delivery_changed.v1"

// WMSContexts are the public names of the contexts whose streams the
// consumer reads.
func WMSContexts() []string { return []string{"shipping"} }

// WMSActor is the principal of the WMS exchange's changes, in every
// tenant: the system's, acting for the company of each delivery.
var WMSActor = uuid.NewSHA1(uuid.NameSpaceURL, []byte("urn:agentic-erp:wms-exchange"))

// wmsActor is the exchange's principal in the tenant.
func wmsActor(tenant uuid.UUID) reqctx.Principal {
	return reqctx.Principal{ID: WMSActor, Kind: reqctx.KindSystem, TenantID: tenant}
}

// WMSQueue turns shipping's bus events into messages to warehouse
// systems.
type WMSQueue struct {
	Pool    *database.Pool
	AppRole string // the tenant-scoped role: a message is queued under the event's tenant
	Log     *slog.Logger
	Begin   database.BeginFunc // nil means Pool.Begin; tests inject failures
	Now     func() time.Time   // the time a message is first due; nil: the wall clock
}

// deliveryCreated is an OutboundDeliveryChanged event as the consumer
// reads it: what the message to the WMS says of the delivery.
type deliveryCreated struct {
	Delivery        uuid.UUID `json:"outbound_delivery_id"`
	Number          string    `json:"number"`
	LegalEntity     uuid.UUID `json:"legal_entity_id"`
	Company         string    `json:"legal_entity_code"`
	OrderNumber     string    `json:"sales_order_number"`
	CustomerNumber  string    `json:"customer_number"`
	Plant           string    `json:"plant_code"`
	StorageLocation string    `json:"storage_location_code"`
	Status          string    `json:"status"`
	Version         int32     `json:"version"`
	Items           []struct {
		ItemNo   int32  `json:"item_no"`
		Material string `json:"material_number"`
		Quantity struct {
			Value string `json:"value"`
			Unit  string `json:"unit"`
		} `json:"quantity"`
	} `json:"items"`
}

// WMSDelivery is the message that gives a warehouse system an outbound
// delivery to pick (message type outbound_delivery, version 1): its id,
// which the WMS deduplicates on and names in what it reports back, the
// company, plant and storage location by code, the delivery by id and
// number with its order's and customer's numbers (never the customer's
// name, PO 2026-11-05), and each item by number with its material's
// number and quantity in the item's unit.
type WMSDelivery struct {
	MessageID       string            `json:"message_id"`
	Type            string            `json:"type"`
	SchemaVersion   int               `json:"schema_version"`
	CompanyCode     string            `json:"company_code"`
	PlantCode       string            `json:"plant_code"`
	StorageLocation string            `json:"storage_location_code"`
	DeliveryID      uuid.UUID         `json:"outbound_delivery_id"`
	DeliveryNumber  string            `json:"outbound_delivery_number"`
	OrderNumber     string            `json:"sales_order_number"`
	CustomerNumber  string            `json:"customer_number"`
	Items           []WMSDeliveryItem `json:"items"`
}

// WMSDeliveryItem is an item of a delivery as a WMS is sent it.
type WMSDeliveryItem struct {
	ItemNo         int32  `json:"item_no"`
	MaterialNumber string `json:"material_number"`
	Quantity       string `json:"quantity"`
	Unit           string `json:"unit"`
}

// Kinds of a WMS message.
const (
	WMSOutboundDelivery = "outbound_delivery"
	WMSGoodsIssue       = "goods_issue"
)

// Handle queues the message of the delivery an OutboundDeliveryChanged
// event says was created (its first version) at a plant an active wms
// connector manages. Another event, or a delivery of a plant no WMS
// manages, is skipped. A malformed event, or one whose subject names
// another tenant than its tenantid, is refused and logged: reading it
// again cannot change the answer, so it is acknowledged (nil). An error
// is a database failure: the event is read again.
func (q *WMSQueue) Handle(ctx context.Context, m outbox.Message) error {
	if m.Header["ce-type"] != deliveryChanged {
		return nil
	}
	tenant, ev, err := parseDeliveryCreated(m)
	if err != nil {
		q.Log.WarnContext(ctx, "the wms queue refused an event", "subject", m.Subject, "ce_id", m.Header["ce-id"], "error", err)
		return nil
	}
	if ev.Version != 1 || ev.Status != "created" {
		return nil
	}
	job := uow.Job{Begin: q.begin, Scope: database.Scope{Role: q.AppRole, TenantID: tenant}, Now: q.Now,
		Name: "integration: wms message of outbound delivery " + ev.Delivery.String()}
	var tx pgx.Tx // the job's transaction, which the unit of work's context also holds
	err = job.As(ctx, func(ctx context.Context, begun pgx.Tx) (reqctx.Principal, bool, error) {
		tx = begun
		return wmsActor(tenant), true, nil
	}, func(ctx context.Context) error { return q.queue(ctx, tx, ev) })
	if err != nil {
		return fmt.Errorf("integration: wms message of outbound delivery %s: %w", ev.Delivery, err)
	}
	return nil
}

func (q *WMSQueue) begin(ctx context.Context, s database.Scope) (pgx.Tx, error) {
	return database.BeginOr(ctx, q.Pool, q.Begin, s)
}

// parseDeliveryCreated reads an OutboundDeliveryChanged bus event: its
// tenant (the subject's, which must be its tenantid) and its data.
func parseDeliveryCreated(m outbox.Message) (uuid.UUID, deliveryCreated, error) {
	var ev deliveryCreated
	tenant, err := m.Tenant("shipping")
	if err != nil {
		return uuid.Nil, ev, err
	}
	if err := json.Unmarshal(m.Data, &ev); err != nil {
		return uuid.Nil, ev, fmt.Errorf("data: %w", err)
	}
	return tenant, ev, nil
}

// queue writes the delivery's message for the WMS managing its plant,
// once, with its audit record; none when no WMS manages it.
func (q *WMSQueue) queue(ctx context.Context, tx pgx.Tx, ev deliveryCreated) error {
	c, ok, err := wmsOf(ctx, tx, ev.Company, ev.Plant)
	if err != nil || !ok {
		return err
	}
	id := uuid.Must(uuid.NewV7())
	msg := WMSDelivery{MessageID: id.String(), Type: WMSOutboundDelivery, SchemaVersion: 1, CompanyCode: ev.Company, PlantCode: ev.Plant,
		StorageLocation: ev.StorageLocation, DeliveryID: ev.Delivery, DeliveryNumber: ev.Number, OrderNumber: ev.OrderNumber,
		CustomerNumber: ev.CustomerNumber, Items: make([]WMSDeliveryItem, len(ev.Items))}
	for i, it := range ev.Items {
		msg.Items[i] = WMSDeliveryItem{ItemNo: it.ItemNo, MaterialNumber: it.Material, Quantity: it.Quantity.Value, Unit: it.Quantity.Unit}
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("integration: render the wms message of outbound delivery %s: %w", ev.Number, err)
	}
	tag, err := tx.Exec(ctx, `INSERT INTO integration.wms_message (tenant_id, id, connector_id, direction, message_id, kind, legal_entity_id, document_id,
			document_number, plant_code, body, status, next_attempt_at)
		VALUES (platform.require_tenant(), $1, $2, 'outbound', $3, $4, $5, $6, $7, $8, $9, 'pending', $10)
		ON CONFLICT (tenant_id, kind, document_id) WHERE direction = 'outbound' DO NOTHING`,
		id, c.ID, id.String(), WMSOutboundDelivery, ev.LegalEntity, ev.Delivery, ev.Number, ev.Plant, body, clock.Now(q.Now))
	if err != nil {
		return fmt.Errorf("integration: queue the wms message of outbound delivery %s: %w", ev.Number, err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	return audit.Record(ctx, "integration.wms_message.queue", "integration.wms_message", id, map[string]any{"connector_id": c.ID,
		"connector_name": c.Name, "kind": WMSOutboundDelivery, "outbound_delivery_id": ev.Delivery, "outbound_delivery_number": ev.Number,
		"company_code": ev.Company, "plant_code": ev.Plant})
}
