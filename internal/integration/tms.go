package integration

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// The TMS connector (S2r; ADR-0082 As built S2r): an external transport
// management system a tenant connects for some of its plants, as a WMS
// is (wms.go). Each outbound delivery created at such a plant is sent to
// it once to plan, a transport request (wms_queue.go), on the WMS's queue
// and exchange; the shipment it plans the delivery in is reported back
// and recorded on the delivery once per message id (wms_receive.go's
// inbox), through shipping's published API (TMSDeliveries, which the
// composition root wires, so integration imports no context). Nothing is
// posted: freight settlement and shipment costs on the stock's value are
// Later (SAP TM's freight settlement, from memory, for the specialist).

// Kinds of a TMS message: the delivery sent to plan (outbound) and the
// shipment reported back (inbound).
const (
	TMSTransportRequest = "transport_request"
	TMSShipment         = "shipment"
)

// TMSDeliveries records what a transport system reports of a delivery
// (shipping's Endpoints.RecordShipment), in the caller's transaction.
type TMSDeliveries interface {
	// ShipmentReported records the delivery's shipment, once.
	ShipmentReported(ctx context.Context, delivery uuid.UUID, s TMSShipmentReported) error
}

// TMSShipmentReported is a shipment as a transport system reports it:
// its number and carrier's code at the transport system, and the freight
// cost as an API amount (a decimal with exactly its currency's minor-unit
// digits) of its currency.
type TMSShipmentReported struct {
	Number, Carrier         string
	FreightAmount, Currency string
}

// TMSShipmentReport is the message a transport system reports a
// delivery's shipment with (message type shipment, version 1): its own
// message id, the delivery it was sent by id, the shipment's number and
// carrier's code, and the freight cost.
type TMSShipmentReport struct {
	MessageID   string    `json:"message_id"`
	Type        string    `json:"type"`
	DeliveryID  uuid.UUID `json:"outbound_delivery_id"`
	Number      string    `json:"shipment_number"`
	Carrier     string    `json:"carrier_code"`
	FreightCost struct {
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
	} `json:"freight_cost"`
}

func (sh *TMSShipmentReport) head() (string, string, uuid.UUID) {
	return sh.MessageID, sh.Type, sh.DeliveryID
}

// readShipment reads a tms connector's confirmation, which is a shipment
// naming the message id it was listed under and a delivery; any other is
// unreadable (the exchange's integration.wms_message_unreadable).
func readShipment(id string, raw []byte, readErr error) (TMSShipmentReport, error) {
	var sh TMSShipmentReport
	err := readDeliveryReport(id, raw, readErr, TMSShipment, "TMS", "a transport system", &sh)
	return sh, err
}

// deliveryReport is a confirmation reporting on a delivery it was sent:
// its message id, its message type and the delivery.
type deliveryReport interface {
	head() (messageID, typ string, delivery uuid.UUID)
}

// readDeliveryReport reads a confirmation of a transport system or a
// trade compliance service into r: a JSON message of the type the system
// reports, of the protocol, naming the message id it was listed under and
// a delivery; any other is unreadable (the exchange's
// integration.wms_message_unreadable).
func readDeliveryReport(id string, raw []byte, readErr error, typ, protocol, system string, r deliveryReport) error {
	if readErr != nil {
		return readErr
	}
	if err := json.Unmarshal(raw, r); err != nil {
		return problem.Unprocessable("integration.wms_message_unreadable", "Confirmation "+id+" is not a JSON "+typ+" message of the "+protocol+" protocol.")
	}
	messageID, got, delivery := r.head()
	switch {
	case got != typ:
		return problem.Unprocessable("integration.wms_message_unreadable", "Confirmation "+id+" is of type "+fmt.Sprintf("%q", got)+
			": only "+typ+" is read from "+system+".")
	case messageID != id:
		return problem.Unprocessable("integration.wms_message_unreadable", "Confirmation "+id+" names another message id, "+fmt.Sprintf("%q", messageID)+".")
	case delivery == uuid.Nil:
		return problem.Unprocessable("integration.wms_message_unreadable", "Confirmation "+id+" names no outbound_delivery_id.")
	}
	return nil
}

// postShipment records a shipment the TMS reports, of a delivery it was
// sent, in a savepoint, acting for the delivery's company alone; shipping
// refuses a second shipment of the delivery and one it cannot hold.
func (w *WMSExchange) postShipment(ctx context.Context, tx pgx.Tx, in *inbound, sh TMSShipmentReport) error {
	var sent WMSDelivery
	if err := w.sentTo(ctx, tx, in, TMSTransportRequest, sh.DeliveryID, &sent, problem.Unprocessable("integration.tms_unknown_delivery",
		"Confirmation "+in.messageID+" names an outbound delivery this transport system was not sent.")); err != nil {
		return err
	}
	return w.inSavepoint(ctx, tx, in, func(ctx context.Context) (*WMSPosted, error) {
		return nil, w.Shipments.ShipmentReported(authz.ActingFor(ctx, *in.legalEntity), sh.DeliveryID, TMSShipmentReported{Number: sh.Number,
			Carrier: sh.Carrier, FreightAmount: sh.FreightCost.Amount, Currency: sh.FreightCost.Currency})
	})
}
