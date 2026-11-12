package sales

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// deliveryAPI is a delivery as the API gives it.
func deliveryAPI(d Delivery) gen.SalesDelivery {
	out := gen.SalesDelivery{ID: d.ID, DeliveryNo: d.No, SalesOrderID: d.OrderID, SalesOrderNumber: d.OrderNumber, DeliveryDate: d.Date,
		Items: make([]gen.SalesDeliveryItem, len(d.Items)), CreatedAt: d.CreatedAt, CreatedBy: d.CreatedBy}
	for i, it := range d.Items {
		out.Items[i] = gen.SalesDeliveryItem{ItemNo: it.ItemNo, ItemKey: apitypes.OptString(it.ItemKey), Description: it.Description,
			Quantity: gen.Quantity{Value: it.Quantity, Unit: it.Unit}}
	}
	return out
}

// ConfirmSalesDelivery implements POST /v1/sales-orders/{id}/deliveries.
func (e *Endpoints) ConfirmSalesDelivery(ctx context.Context, req *gen.SalesDeliveryCreate, params gen.ConfirmSalesDeliveryParams) (gen.ConfirmSalesDeliveryRes, error) {
	in := DeliveryInput{Date: req.DeliveryDate, Items: make([]DeliveryItemInput, len(req.Items))}
	for i, it := range req.Items {
		in.Items[i] = DeliveryItemInput{ItemNo: it.ItemNo, Quantity: it.Quantity.Value, Unit: it.Quantity.Unit}
	}
	d, err := e.ConfirmDelivery(ctx, params.ID, in)
	if err != nil {
		return nil, err
	}
	return &gen.SalesDeliveryHeaders{Response: deliveryAPI(d)}, nil
}

var deliverySort = pagination.Sort{Default: "delivery_date", Fields: []pagination.Field{
	{Name: "delivery_date", Column: "d.delivery_date", Type: "date"},
}}

// ListSalesDeliveries implements GET /v1/sales-orders/{id}/deliveries: the
// deliveries of an order of a company the caller may act in, by date.
func (e *Endpoints) ListSalesDeliveries(ctx context.Context, params gen.ListSalesDeliveriesParams) (gen.ListSalesDeliveriesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	o, err := order(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	ks, err := e.Pages.Keyset(deliverySort, fmt.Sprintf("listSalesDeliveries|%s", o.ID), apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("sales: page of deliveries: %w", err)
	}
	query, args := ks.SQL(deliverySelect+` WHERE d.sales_order_id = $1`, []any{o.ID})
	rows, _ := tx.Query(ctx, query, args...)
	found, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Delivery, error) { return scanDelivery(r) })
	if err != nil {
		return nil, fmt.Errorf("sales: list the deliveries of sales order %s: %w", o.ID, err)
	}
	items, next := apitypes.PageOf(ks, found, func(d Delivery) (string, uuid.UUID) { return d.Date.Format(time.DateOnly), d.ID }, deliveryAPI)
	return &gen.SalesDeliveryPage{Items: items, NextPageToken: next}, nil
}
