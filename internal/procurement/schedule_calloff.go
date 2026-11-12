package procurement

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// The call-off of a schedule line (ADR-0070 §4, T83b): a line of an
// active scheduling agreement is called off once, as a draft purchase
// order against the agreement created through the order's own code path
// (createOrder: its checks, validation rules, number and audit record).
// The line then names the order; from there the order is any order, and
// receipts, GR/IR and invoices know nothing of schedule lines.

// The statuses of a schedule line.
const (
	LineScheduled = "scheduled"
	LineCalledOff = "called_off"
)

// Status is LineCalledOff once the line names its purchase order.
func (l ScheduleLine) Status() string {
	if l.OrderID != nil {
		return LineCalledOff
	}
	return LineScheduled
}

// callOffOrder is the order a call-off of line l of agreement c creates
// on the day: of the agreement's legal entity, supplier and currency,
// against the agreement, its one item the agreement's item at the
// agreement's price in the line's quantity, due on the line's date. The
// price is written as the API's decimal, which createOrder parses and
// then replaces by the contract item's, as for any order on a contract.
func (c Contract) callOffOrder(l ScheduleLine, day time.Time) NewOrder {
	it := c.scheduled(l)
	return NewOrder{LegalEntityID: c.LegalEntityID, SupplierID: c.SupplierID, Currency: c.Currency, OrderDate: day, ContractID: &c.ID,
		Items: []ItemInput{{MaterialID: it.MaterialID, Description: it.Description, Quantity: l.Quantity, Unit: it.Unit,
			Price: it.UnitPrice.Plain(c.Exponent), Currency: c.Currency, PriceUnit: it.PriceUnit, AccountID: it.AccountID, ContractItemNo: &it.No,
			DeliveryDate: &l.DeliveryDate}}}
}

// callable reads line no of agreement c under its row lock: a line not
// yet called off, at the version the caller read, of an agreement valid
// on its legal entity's date today, which it returns.
func (e *Endpoints) callable(ctx context.Context, tx pgx.Tx, c Contract, no int32, ifMatch func(int32) error) (ScheduleLine, time.Time, error) {
	l, err := scanScheduleLine(tx.QueryRow(ctx, scheduleLineSelect+` WHERE l.contract_id = $1 AND l.line_no = $2 FOR UPDATE OF l`, c.ID, no))
	if errors.Is(err, pgx.ErrNoRows) {
		return ScheduleLine{}, time.Time{}, problem.NotFoundOf("schedule line")
	}
	if err != nil {
		return ScheduleLine{}, time.Time{}, fmt.Errorf("procurement: read schedule line %d of contract %s: %w", no, c.ID, err)
	}
	// Before the version: a line changes only by its call-off, so the
	// loser of two call-offs at the version both read is told what
	// happened and which order, not to read again and retry.
	if l.OrderID != nil {
		return ScheduleLine{}, time.Time{}, problem.Conflict("procurement.schedule_line_called_off",
			fmt.Sprintf("Schedule line %d of scheduling agreement %s was called off as purchase order %s.", l.No, c.DocumentNumber, *l.OrderNumber))
	}
	if err := ifMatch(l.Version); err != nil {
		return ScheduleLine{}, time.Time{}, err
	}
	today, zone, err := e.todayOf(ctx, c.LegalEntityID)
	if err != nil {
		return ScheduleLine{}, time.Time{}, err
	}
	if today.Before(c.ValidFrom) || today.After(c.ValidTo) {
		return ScheduleLine{}, time.Time{}, problem.Conflict("procurement.agreement_not_valid",
			fmt.Sprintf("Scheduling agreement %s is valid from %s to %s; the legal entity's date today is %s in %s.", c.DocumentNumber,
				c.ValidFrom.Format(time.DateOnly), c.ValidTo.Format(time.DateOnly), today.Format(time.DateOnly), zone))
	}
	return l, today, nil
}

// callOff calls off line no of scheduling agreement contract, once: the
// contract and the line are read under their row locks, so two call-offs
// run one after the other and the second finds the line called off
// (409). The locks are what make a call-off happen once; the update
// names the state it expects, and the database refuses a second
// call-off of a line whatever the writer (schedule_line_called_off_once).
// A call-off
// reserves the line for its order, more than a draft proposes (ADR-0036):
// the operation is not marked x-autonomy: propose, so an agent needs
// level 2 before it gets here.
func (e *Endpoints) callOff(ctx context.Context, contract uuid.UUID, no int32, ifMatch func(int32) error) (Contract, ScheduleLine, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Contract{}, ScheduleLine{}, err
	}
	c, err := agreement(ctx, tx, contract, "calling off a schedule line")
	if err != nil {
		return Contract{}, ScheduleLine{}, err
	}
	l, today, err := e.callable(ctx, tx, c, no, ifMatch)
	if err != nil {
		return Contract{}, ScheduleLine{}, err
	}
	o, err := e.createOrder(ctx, c.callOffOrder(l, today))
	if err != nil {
		return Contract{}, ScheduleLine{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE procurement.schedule_line SET purchase_order_id = $2, version = version + 1, updated_by = $3
		WHERE id = $1 AND purchase_order_id IS NULL`, l.ID, o.ID, p.ID); err != nil {
		return Contract{}, ScheduleLine{}, fmt.Errorf("procurement: call off schedule line %s: %w", l.ID, err)
	}
	called, err := scanScheduleLine(tx.QueryRow(ctx, scheduleLineSelect+` WHERE l.id = $1`, l.ID))
	if err != nil {
		return Contract{}, ScheduleLine{}, fmt.Errorf("procurement: read schedule line %s: %w", l.ID, err)
	}
	return c, called, e.scheduleChanged(ctx, c, called, "call_off", "called_off", func(payload map[string]any) outbox.Event {
		payload["order_date"] = o.OrderDate.Format(time.DateOnly)
		return event("schedule_line_called_off", "schedule_line", called.ID, payload)
	})
}
