package shipping

import (
	"context"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// A delivery's trade compliance (S2s; ADR-0082 As built S2s): an external
// trade compliance service a gts connector names for the delivery's plant
// screens its parties and materials (SAP GTS's sanctioned party list
// screening and export control, from memory, for the specialist). The
// delivery is created pending (Create, when the connector screens its
// plant), the service's latest answer releases or blocks it
// (RecordScreening, the exchange's), and a person holding
// shipping.trade_compliance.release may release a pending or blocked one
// (ReleaseTradeComplianceBlock). Its goods issue waits while it is pending
// or blocked (tradeHeld), so nothing leaves the stock unscreened, as
// SAP GTS's legal control blocks a delivery's goods issue (from memory).

// Statuses of a delivery's trade compliance.
const (
	TradePending  = "pending"
	TradeReleased = "released"
	TradeBlocked  = "blocked"
)

// TradeCompliance is a delivery's screening: its status, the service's
// latest reason code and when it answered, and who released it by hand
// and when.
type TradeCompliance struct {
	Status         string
	ReasonCode     *string
	ScreenedAt     *time.Time
	ReleasedBy     *uuid.UUID
	ReleasedByName *string
	ReleasedAt     *time.Time
}

// ScreeningInput is a trade compliance service's answer for a delivery:
// released or blocked, with the service's reason code, if any, and when
// the service screened it.
type ScreeningInput struct {
	Result, ReasonCode string
	ScreenedAt         time.Time
}

// screenedAhead is how far past the ERP's clock a service's screening
// time may be: a little clock drift, but not a date in the future, which
// would make every later answer, a block among them, superseded.
const screenedAhead = 5 * time.Minute

// maxReasonCode is the longest reason code a delivery holds
// (outbound_delivery_trade_compliance_reason_check).
const maxReasonCode = 40

// tradeHeld refuses the goods issue of a delivery whose screening is not
// released (409 shipping.trade_compliance_block): pending, or blocked by
// the service, until the service or a person releases it.
func tradeHeld(d Delivery) error {
	tc := d.TradeCompliance
	if tc == nil || tc.Status == TradeReleased {
		return nil
	}
	detail := "Outbound delivery " + d.Number + " waits for its trade compliance screening; its goods issue is posted once the service, or a person, releases it."
	if tc.Status == TradeBlocked {
		reason := ""
		if tc.ReasonCode != nil {
			reason = " (" + *tc.ReasonCode + ")"
		}
		detail = "Outbound delivery " + d.Number + " is blocked by its trade compliance screening" + reason +
			"; its goods issue is posted once the service, or a person, releases it."
	}
	return problem.Conflict("shipping.trade_compliance_block", detail)
}

// RecordScreening records a trade compliance service's answer for a
// delivery (S2s): the delivery read under its row lock (ADR-0008), of a
// company the caller may act in, while its goods issue is not posted (409
// shipping.goods_issued: what is issued is no longer held); the answer
// released or blocked, its reason code at most 40 characters (422
// shipping.invalid_screening), naming when the service screened it (422
// shipping.invalid_screening without, or more than screenedAhead past the
// ERP's clock). The service's latest answer
// counts, by that time, not by when it is read: one screened before the
// answer recorded already is refused (409 shipping.screening_superseded),
// so an answer read late cannot undo a later block. A newer one replaces
// the status, reason code and time, and a person's release before it. A
// delivery created before its plant was screened takes it all the same:
// a block is honoured whenever it comes. The version moves; the audit
// record and OutboundDeliveryChanged are written in the caller's
// transaction. No If-Match: the service's answer is not a change of what
// its caller read. It is shipping's published write for the exchange with
// trade compliance services, which the composition root wires; no
// operation of the API calls it.
func (e *Endpoints) RecordScreening(ctx context.Context, id uuid.UUID, in ScreeningInput) (Delivery, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Delivery{}, err
	}
	if in.Result != TradeReleased && in.Result != TradeBlocked {
		return Delivery{}, problem.Invalid("shipping.invalid_screening", "result", "released or blocked.")
	}
	var reason *string
	if in.ReasonCode != "" {
		if utf8.RuneCountInString(in.ReasonCode) > maxReasonCode {
			return Delivery{}, problem.Invalid("shipping.invalid_screening", "reason_code", fmt.Sprintf("At most %d characters.", maxReasonCode))
		}
		reason = &in.ReasonCode
	}
	if in.ScreenedAt.IsZero() || in.ScreenedAt.After(clock.Now(e.Now).Add(screenedAhead)) {
		return Delivery{}, problem.Invalid("shipping.invalid_screening", "screened_at",
			"When the service screened the delivery, at most "+screenedAhead.String()+" past the ERP's clock.")
	}
	d, err := open(ctx, tx, id, func(int32) error { return nil })
	if err != nil {
		return Delivery{}, err
	}
	at := in.ScreenedAt.Truncate(time.Microsecond) // as the column holds it, so a time compares with its own
	if err := superseded(d, at); err != nil {
		return Delivery{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE shipping.outbound_delivery SET trade_compliance_status = $2, trade_compliance_reason = $3,
			trade_compliance_screened_at = $4, trade_compliance_released_by = NULL, trade_compliance_released_by_name = NULL, trade_compliance_released_at = NULL, version = version + 1,
			updated_by = $5 WHERE id = $1`, d.ID, in.Result, reason, at, p.ID); err != nil {
		return Delivery{}, fmt.Errorf("shipping: record the screening of outbound delivery %s: %w", d.Number, err)
	}
	return e.traded(ctx, tx, "shipping.outbound_delivery.record_screening", d)
}

// superseded refuses an answer the service screened no later than the one
// the delivery holds (409 shipping.screening_superseded): the service's
// answers are ordered by when it screened, read in whatever order.
func superseded(d Delivery, at time.Time) error {
	tc := d.TradeCompliance
	if tc == nil || tc.ScreenedAt == nil || at.After(*tc.ScreenedAt) {
		return nil
	}
	return problem.Conflict("shipping.screening_superseded", "Outbound delivery "+d.Number+" holds the service's answer of "+
		tc.ScreenedAt.UTC().Format(time.RFC3339Nano)+"; one screened at "+at.UTC().Format(time.RFC3339Nano)+" is older and changes nothing.")
}

// ReleaseTradeComplianceBlock releases a delivery's trade compliance by
// hand (S2s): the delivery read under its row lock at the version the
// caller read, while its goods issue is not posted (409
// shipping.goods_issued), pending or blocked (409
// shipping.not_trade_compliance_blocked otherwise); it is released,
// naming the caller and when, the service's reason code and time kept.
// Its goods issue may then be posted; a later answer of the service still
// counts. The version moves; audited and published as
// OutboundDeliveryChanged.
func (e *Endpoints) ReleaseTradeComplianceBlock(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (Delivery, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Delivery{}, err
	}
	d, err := open(ctx, tx, id, ifMatch)
	if err != nil {
		return Delivery{}, err
	}
	if tc := d.TradeCompliance; tc == nil || tc.Status == TradeReleased {
		status := "not screened"
		if tc != nil {
			status = "released already"
		}
		return Delivery{}, problem.Conflict("shipping.not_trade_compliance_blocked", "Outbound delivery "+d.Number+"'s trade compliance is "+status+
			"; only a pending or blocked delivery is released.")
	}
	people, err := e.People.PersonNames(ctx, []uuid.UUID{p.ID})
	if err != nil {
		return Delivery{}, fmt.Errorf("shipping: name of the person releasing outbound delivery %s: %w", d.Number, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE shipping.outbound_delivery SET trade_compliance_status = $2, trade_compliance_released_by = $3,
			trade_compliance_released_by_name = $5, trade_compliance_released_at = $4, version = version + 1, updated_by = $3 WHERE id = $1`,
		d.ID, TradeReleased, p.ID, clock.Now(e.Now), people[p.ID]); err != nil {
		return Delivery{}, fmt.Errorf("shipping: release the trade compliance block of outbound delivery %s: %w", d.Number, err)
	}
	return e.traded(ctx, tx, "shipping.outbound_delivery.release_trade_compliance_block", d)
}

// traded reads the delivery back after a change of its trade compliance
// and records it, the audit record naming the status before.
func (e *Endpoints) traded(ctx context.Context, tx pgx.Tx, action string, before Delivery) (Delivery, error) {
	after, err := load(ctx, tx, before.ID, false)
	if err != nil {
		return Delivery{}, err
	}
	var was any // nil: not screened
	if before.TradeCompliance != nil {
		was = before.TradeCompliance.Status
	}
	return after, e.record(ctx, action, after, map[string]any{"trade_compliance": tradeEvent(after.TradeCompliance), "trade_compliance_before": was})
}

// tradeEvent is a delivery's trade compliance as OutboundDeliveryChanged
// and its audit record carry it; nil when it is not screened.
func tradeEvent(tc *TradeCompliance) map[string]any {
	if tc == nil {
		return nil
	}
	return map[string]any{"status": tc.Status, "reason_code": tc.ReasonCode, "screened_at": tc.ScreenedAt, "released_by": tc.ReleasedBy,
		"released_by_name": tc.ReleasedByName, "released_at": tc.ReleasedAt}
}
