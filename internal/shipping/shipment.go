package shipping

import (
	"context"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Shipment is the shipment a transport management system planned a
// delivery in (S2r; ADR-0082 As built S2r): its number and carrier as the
// transport system names them, and the freight cost it reports, in minor
// units of its currency (ADR-0005). It is recorded, never posted: freight
// settlement and shipment costs on the stock's value are Later.
type Shipment struct {
	Number, Carrier string
	Freight         money.Minor
	Currency        string
	Exponent        int
	RecordedAt      time.Time
}

// ShipmentInput is a shipment as reported: the freight cost an API amount
// of its currency, a decimal string with exactly its minor-unit digits.
type ShipmentInput struct {
	Number, Carrier         string
	FreightAmount, Currency string
}

// maxShipmentText is the longest shipment number and carrier code a
// delivery holds (outbound_delivery_shipment_number_check,
// outbound_delivery_carrier_code_check).
const maxShipmentText = 40

// RecordShipment records the shipment a transport system reports for a
// delivery (S2r), once: the delivery read under its row lock (ADR-0008),
// of a company the caller may act in, whatever its status, as a freight
// cost may come after the goods issue; a delivery with a shipment already
// is refused (409 shipping.shipment_recorded), as is a number or carrier
// code that is empty or longer than 40 characters, an unknown currency or
// a negative cost (422 shipping.invalid_shipment) and an amount without
// exactly the currency's decimals (400 request.invalid_amount). The
// version moves; the audit record and OutboundDeliveryChanged are written
// in the caller's transaction. No If-Match: the transport system's report
// is not a change of what the caller read. It is shipping's published
// write for the exchange with transport systems, which the composition
// root wires; no operation of the API calls it.
func (e *Endpoints) RecordShipment(ctx context.Context, id uuid.UUID, in ShipmentInput) (Delivery, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Delivery{}, err
	}
	d, err := accessible(ctx, tx, id, true)
	if err != nil {
		return Delivery{}, err
	}
	if d.Shipment != nil {
		return Delivery{}, problem.Conflict("shipping.shipment_recorded", "Outbound delivery "+d.Number+" has shipment "+d.Shipment.Number+
			" recorded already; a shipment is recorded once.")
	}
	s, err := shipmentOf(ctx, tx, in)
	if err != nil {
		return Delivery{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE shipping.outbound_delivery SET shipment_number = $2, carrier_code = $3, freight_amount = $4, freight_currency = $5,
			shipment_recorded_at = $6, version = version + 1, updated_by = $7 WHERE id = $1`,
		d.ID, s.Number, s.Carrier, s.Freight, s.Currency, clock.Now(e.Now), p.ID); err != nil {
		return Delivery{}, fmt.Errorf("shipping: record the shipment of outbound delivery %s: %w", d.Number, err)
	}
	after, err := load(ctx, tx, d.ID, false)
	if err != nil {
		return Delivery{}, err
	}
	return after, e.record(ctx, "shipping.outbound_delivery.record_shipment", after, map[string]any{"shipment": shipmentEvent(after.Shipment)})
}

// shipmentOf checks a reported shipment and reads its freight cost in
// minor units of its currency.
func shipmentOf(ctx context.Context, tx pgx.Tx, in ShipmentInput) (Shipment, error) {
	for _, f := range []struct{ field, value string }{{"shipment_number", in.Number}, {"carrier_code", in.Carrier}} {
		if n := utf8.RuneCountInString(f.value); n == 0 || n > maxShipmentText {
			return Shipment{}, problem.Invalid("shipping.invalid_shipment", f.field, fmt.Sprintf("1 to %d characters.", maxShipmentText))
		}
	}
	exp, err := currency.Exponent(ctx, tx, in.Currency, problem.Invalid("shipping.invalid_shipment", "freight_cost.currency", "A known ISO 4217 code."))
	if err != nil {
		return Shipment{}, err
	}
	freight, err := currency.ParseAmount(in.FreightAmount, in.Currency, exp, "freight_cost.amount")
	if err != nil {
		return Shipment{}, err
	}
	if freight < 0 {
		return Shipment{}, problem.Invalid("shipping.invalid_shipment", "freight_cost.amount", "A freight cost is not negative.")
	}
	return Shipment{Number: in.Number, Carrier: in.Carrier, Freight: freight, Currency: in.Currency, Exponent: exp}, nil
}

// shipmentEvent is a shipment as OutboundDeliveryChanged and its audit
// record carry it; nil before one is recorded.
func shipmentEvent(s *Shipment) map[string]any {
	if s == nil {
		return nil
	}
	return map[string]any{"shipment_number": s.Number, "carrier_code": s.Carrier, "freight_cost": money.EventAmount(s.Freight, s.Currency, s.Exponent),
		"recorded_at": s.RecordedAt}
}
