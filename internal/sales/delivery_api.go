package sales

import (
	"context"
	"time"

	"github.com/google/uuid"

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

// deliveryList lists an order's deliveries by date.
var deliveryList = listed[Delivery, gen.SalesDelivery]{what: "deliveries", query: deliverySelect + ` WHERE d.sales_order_id = $1`, scan: scanDelivery,
	sort: pagination.Sort{Default: "delivery_date", Fields: []pagination.Field{{Name: "delivery_date", Column: "d.delivery_date", Type: "date"}}},
	key:  func(d Delivery) (time.Time, uuid.UUID) { return d.Date, d.ID }, api: deliveryAPI}

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
	items, next, err := deliveryList.page(ctx, e, tx, "listSalesDeliveries", o.ID, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	return &gen.SalesDeliveryPage{Items: items, NextPageToken: next}, nil
}
