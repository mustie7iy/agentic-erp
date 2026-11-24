package integration

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// The trade compliance connector (S2s; ADR-0082 As built S2s): an
// external trade compliance service a tenant connects for some of its
// plants, as a WMS or a TMS is (wms.go), to screen the parties and
// materials of their outbound deliveries (sanctioned party lists, export
// classification; SAP GTS's role, from memory, for the specialist).
// Shipping creates a delivery at such a plant pending (TradeScreening,
// which the composition root wires into shipping), so its goods issue
// waits; the delivery is sent to the service once to screen, a screening
// request (wms_queue.go), on the WMS's queue and exchange; the service's
// answer, released or blocked, is reported back and recorded on the
// delivery once per message id (wms_receive.go's inbox), through
// shipping's published API (GTSDeliveries, wired by the composition root,
// so integration imports no context). Nothing is posted.

// GTS is the kind of a trade compliance service's connector (S2s).
const GTS = "gts"

// Kinds of a GTS message: the delivery sent to screen (outbound) and the
// service's answer (inbound).
const (
	GTSScreeningRequest = "screening_request"
	GTSScreeningResult  = "screening_result"
)

// gtsScreening is the message type of the service's answer.
const gtsScreening = "screening"

// GTSDeliveries records what a trade compliance service answers of a
// delivery (shipping's Endpoints.RecordScreening), in the caller's
// transaction.
type GTSDeliveries interface {
	// ScreeningReported records the service's latest answer.
	ScreeningReported(ctx context.Context, delivery uuid.UUID, s GTSScreening) error
}

// GTSScreening is a trade compliance service's answer for a delivery:
// released or blocked, with the service's reason code, if any.
type GTSScreening struct {
	Result, ReasonCode string
}

// GTSScreeningReport is the message a trade compliance service answers a
// delivery's screening with (message type screening, version 1): its own
// message id, the delivery it was sent by id, its result (released or
// blocked) and, optionally, its reason code.
type GTSScreeningReport struct {
	MessageID  string    `json:"message_id"`
	Type       string    `json:"type"`
	DeliveryID uuid.UUID `json:"outbound_delivery_id"`
	Result     string    `json:"result"`
	ReasonCode string    `json:"reason_code"`
}

// TradeScreening tells shipping whether a trade compliance service
// screens a company's plant: an active gts connector names it, read in
// the request's transaction (shipping's Screening port, S2s). A connector
// whose settings no longer resolve screens nothing.
type TradeScreening struct{}

// Screens implements shipping's Screening.
func (TradeScreening) Screens(ctx context.Context, company, plant string) (bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return false, err
	}
	_, ok, err := managing(ctx, tx, GTS, company, plant)
	return ok, err
}

func (sc *GTSScreeningReport) head() (string, string, uuid.UUID) {
	return sc.MessageID, sc.Type, sc.DeliveryID
}

// readScreening reads a gts connector's confirmation, which is a
// screening naming the message id it was listed under and a delivery
// (readDeliveryReport); its result is shipping's to judge.
func readScreening(id string, raw []byte, readErr error) (GTSScreeningReport, error) {
	var sc GTSScreeningReport
	err := readDeliveryReport(id, raw, readErr, gtsScreening, "GTS", "a trade compliance service", &sc)
	return sc, err
}

// postScreening records the service's answer for a delivery it was sent,
// in a savepoint, acting for the delivery's company alone; shipping
// refuses one of a delivery whose goods issue is posted, and an answer it
// cannot hold.
func (w *WMSExchange) postScreening(ctx context.Context, tx pgx.Tx, in *inbound, sc GTSScreeningReport) error {
	var sent WMSDelivery
	if err := w.sentTo(ctx, tx, in, GTSScreeningRequest, sc.DeliveryID, &sent, problem.Unprocessable("integration.gts_unknown_delivery",
		"Confirmation "+in.messageID+" names an outbound delivery this trade compliance service was not sent.")); err != nil {
		return err
	}
	return w.inSavepoint(ctx, tx, in, func(ctx context.Context) (*WMSPosted, error) {
		return nil, w.Screenings.ScreeningReported(authz.ActingFor(ctx, *in.legalEntity), sc.DeliveryID, GTSScreening{Result: sc.Result,
			ReasonCode: sc.ReasonCode})
	})
}
