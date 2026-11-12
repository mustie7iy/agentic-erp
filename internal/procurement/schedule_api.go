package procurement

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// The handlers of a scheduling agreement's delivery schedule (ADR-0070),
// in a file of their own beside schedule.go.

// scheduleLineToAPI shows line l beside its agreement c: the agreement's
// number, the item asked for, its price and the line's value.
func scheduleLineToAPI(c Contract, l ScheduleLine) gen.ScheduleLine {
	it := c.scheduled(l)
	return gen.ScheduleLine{ID: l.ID, Version: l.Version, CreatedAt: l.CreatedAt, CreatedBy: l.CreatedBy, UpdatedAt: l.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(l.UpdatedBy), ContractID: c.ID, ContractDocumentNumber: c.DocumentNumber, LineNo: l.No, ContractItemNo: l.ItemNo,
		MaterialKey: it.MaterialKey, Description: it.Description, DeliveryDate: l.DeliveryDate, Quantity: gen.Quantity{Value: l.Quantity, Unit: it.Unit},
		UnitPrice: apitypes.Money(it.UnitPrice, c.Currency, c.Exponent), PriceUnit: it.PriceUnit,
		Amount: apitypes.Money(total([]Item{it}), c.Currency, c.Exponent), Status: l.Status(), PurchaseOrderID: apitypes.OptUUID(l.OrderID),
		PurchaseOrderDocumentNumber: apitypes.OptString(l.OrderNumber)}
}

// schedulePath is where the schedule of contract id is listed.
func schedulePath(id uuid.UUID) string { return "/v1/contracts/" + id.String() + "/schedule-lines" }

// CreateScheduleLine implements POST /v1/contracts/{id}/schedule-lines.
// The line has no address of its own: Location is the agreement's
// schedule filtered to its number, which holds this line alone.
func (e *Endpoints) CreateScheduleLine(ctx context.Context, req *gen.ScheduleLineCreate, params gen.CreateScheduleLineParams) (gen.CreateScheduleLineRes, error) {
	c, l, err := e.createScheduleLine(ctx, params.ID, NewScheduleLine{ItemNo: req.ContractItemNo, DeliveryDate: req.DeliveryDate,
		Quantity: req.Quantity.Value, Unit: req.Quantity.Unit})
	if err != nil {
		return nil, err
	}
	return &gen.ScheduleLineHeaders{ETag: gen.NewOptString(etag.Format(l.Version)),
		Location: gen.NewOptString(fmt.Sprintf("%s?line_no=%d", schedulePath(c.ID), l.No)), Response: scheduleLineToAPI(c, l)}, nil
}

var scheduleSort = pagination.Sort{Default: "delivery_date", Fields: []pagination.Field{
	{Name: "delivery_date", Column: "l.delivery_date", Type: "date"},
}}

// ListScheduleLines implements GET /v1/contracts/{id}/schedule-lines:
// by delivery date, lines of one date in the order entered (a line's id
// is a UUIDv7).
func (e *Endpoints) ListScheduleLines(ctx context.Context, params gen.ListScheduleLinesParams) (gen.ListScheduleLinesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	c, err := getContract(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	item, no := apitypes.Ptr[int32](params.ContractItemNo), apitypes.Ptr[int32](params.LineNo)
	ks, err := e.Pages.Keyset(scheduleSort, fmt.Sprintf("listScheduleLines|%s|%v|%v|%s", c.ID, params.ContractItemNo, params.LineNo, params.Status.Value),
		apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	// A line's status is derived: called off once it names its order.
	q, args := ks.SQL(scheduleLineSelect+` WHERE l.contract_id = $1 AND ($2::int IS NULL OR l.contract_item_no = $2) AND ($3::int IS NULL OR l.line_no = $3)
		AND ($4 = '' OR $4 = CASE WHEN l.purchase_order_id IS NULL THEN $5::text ELSE $6::text END)`,
		[]any{c.ID, item, no, params.Status.Value, LineScheduled, LineCalledOff})
	rows, _ := tx.Query(ctx, q, args...)
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ScheduleLine, error) { return scanScheduleLine(row) })
	if err != nil {
		return nil, fmt.Errorf("procurement: list schedule lines of contract %s: %w", c.ID, err)
	}
	items, next := apitypes.PageOf(ks, list, func(l ScheduleLine) (string, uuid.UUID) { return l.DeliveryDate.Format(time.DateOnly), l.ID },
		func(l ScheduleLine) gen.ScheduleLine { return scheduleLineToAPI(c, l) })
	return &gen.ScheduleLinePage{Items: items, NextPageToken: next}, nil
}

// CallOffScheduleLine implements POST
// /v1/contracts/{id}/schedule-lines/{line_no}:callOff.
func (e *Endpoints) CallOffScheduleLine(ctx context.Context, params gen.CallOffScheduleLineParams) (gen.CallOffScheduleLineRes, error) {
	c, l, err := e.callOff(ctx, params.ID, params.LineNo, etag.IfMatch(params.IfMatch))
	if err != nil {
		return nil, err
	}
	return &gen.CallOffScheduleLineOKHeaders{ETag: gen.NewOptString(etag.Format(l.Version)), Response: scheduleLineToAPI(c, l)}, nil
}
