package sales

import (
	"context"
	"fmt"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// changeInput is a change request as the domain takes it.
func changeInput(req *gen.SalesOrderChangeRequest) OrderChangeInput {
	in := OrderChangeInput{Reason: apitypes.Ptr[string](req.Reason), DeliveryDateSet: req.RequestedDeliveryDate.Set,
		Items: make([]ItemChangeInput, len(req.Items))}
	if d, ok := req.RequestedDeliveryDate.Get(); ok {
		in.DeliveryDate = &d
	}
	for i, it := range req.Items {
		in.Items[i] = ItemChangeInput{ItemNo: it.ItemNo, PriceUnit: it.PriceUnit.Or(0)}
		if q, ok := it.Quantity.Get(); ok {
			in.Items[i].Quantity, in.Items[i].Unit = &q.Value, &q.Unit
		}
		if p, ok := it.UnitPrice.Get(); ok {
			in.Items[i].UnitPrice = &Amount{Value: p.Amount, Currency: p.Currency}
		}
	}
	return in
}

// ChangeSalesOrder implements POST /v1/sales-orders/{id}:change.
func (e *Endpoints) ChangeSalesOrder(ctx context.Context, req *gen.SalesOrderChangeRequest, params gen.ChangeSalesOrderParams) (gen.ChangeSalesOrderRes, error) {
	o, err := e.ChangeOrder(ctx, params.ID, etag.IfMatch(params.IfMatch), changeInput(req))
	if err != nil {
		return nil, err
	}
	out, err := e.orderAnswer(ctx, o)
	if err != nil {
		return nil, err
	}
	return &gen.CancelSalesOrderOKHeaders{ETag: gen.NewOptString(etag.Format(o.Version)), Response: out}, nil
}

// CancelSalesOrder implements POST /v1/sales-orders/{id}:cancel.
func (e *Endpoints) CancelSalesOrder(ctx context.Context, req *gen.ReasonInput, params gen.CancelSalesOrderParams) (gen.CancelSalesOrderRes, error) {
	o, err := e.CancelOrder(ctx, params.ID, etag.IfMatch(params.IfMatch), req.Reason)
	if err != nil {
		return nil, err
	}
	out, err := e.orderAnswer(ctx, o)
	if err != nil {
		return nil, err
	}
	return &gen.CancelSalesOrderOKHeaders{ETag: gen.NewOptString(etag.Format(o.Version)), Response: out}, nil
}

// changeAPI is a change of an order as the API gives it.
func changeAPI(h OrderChange) gen.SalesOrderChange {
	amount := func(m money.Minor) gen.Money { return apitypes.Money(m, h.Currency, h.Exponent) }
	out := gen.SalesOrderChange{ID: h.ID, ChangeNo: h.No, SalesOrderID: h.OrderID, Version: h.Version, Reason: apitypes.OptString(h.Reason),
		NetAmountBefore: amount(h.NetBefore), NetAmountAfter: amount(h.NetAfter),
		RequestedDeliveryDateBefore: apitypes.OptDate(h.DeliveryBefore), RequestedDeliveryDateAfter: apitypes.OptDate(h.DeliveryAfter),
		CreditBlockBefore: h.BlockBefore, CreditBlockAfter: h.BlockAfter, Items: make([]gen.SalesOrderChangeItem, len(h.Items)),
		CreatedAt: h.CreatedAt, CreatedBy: h.CreatedBy}
	for i, it := range h.Items {
		out.Items[i] = gen.SalesOrderChangeItem{ItemNo: it.ItemNo, QuantityBefore: gen.Quantity{Value: it.QuantityBefore, Unit: it.Unit},
			QuantityAfter: gen.Quantity{Value: it.QuantityAfter, Unit: it.Unit}, UnitPriceBefore: amount(it.UnitPriceBefore),
			UnitPriceAfter: amount(it.UnitPriceAfter), PriceUnitBefore: it.PriceUnitBefore, PriceUnitAfter: it.PriceUnitAfter,
			NetAmountBefore: amount(it.NetBefore), NetAmountAfter: amount(it.NetAfter)}
	}
	return out
}

var changeSort = pagination.Sort{Default: "change_no", Fields: []pagination.Field{{Name: "change_no", Column: "h.change_no", Type: "integer"}}}

// ListSalesOrderChanges implements GET /v1/sales-orders/{id}/changes: the
// changes of an order of a company the caller may act in, by number.
func (e *Endpoints) ListSalesOrderChanges(ctx context.Context, params gen.ListSalesOrderChangesParams) (gen.ListSalesOrderChangesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	o, err := order(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	ks, err := e.Pages.Keyset(changeSort, "listSalesOrderChanges|"+o.ID.String(), apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("sales: page of the changes of sales order %s: %w", o.ID, err)
	}
	query, args := ks.SQL(changeSelect+` WHERE h.sales_order_id = $1`, []any{o.ID})
	rows, _ := tx.Query(ctx, query, args...)
	found, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (OrderChange, error) { return scanChange(r) })
	if err != nil {
		return nil, fmt.Errorf("sales: list the changes of sales order %s: %w", o.ID, err)
	}
	items, next := apitypes.PageOf(ks, found, func(h OrderChange) (string, uuid.UUID) { return strconv.Itoa(int(h.No)), h.ID }, changeAPI)
	return &gen.SalesOrderChangePage{Items: items, NextPageToken: next}, nil
}
