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

// receiptAPI is a return receipt as the API gives it.
func receiptAPI(c ReturnReceipt) gen.ReturnReceipt {
	out := gen.ReturnReceipt{ID: c.ID, ReceiptNo: c.No, ReturnsOrderID: c.ReturnsOrderID, ReturnsOrderNumber: c.ReturnsOrderNumber, ReceiptDate: c.Date,
		Items: make([]gen.ReturnReceiptItem, len(c.Items)), CreatedAt: c.CreatedAt, CreatedBy: c.CreatedBy}
	for i, it := range c.Items {
		out.Items[i] = gen.ReturnReceiptItem{ItemNo: it.ItemNo, BillingItemNo: it.BillingItemNo, ItemKey: apitypes.OptString(it.ItemKey),
			Description: it.Description, Quantity: gen.Quantity{Value: it.Quantity, Unit: it.Unit}}
	}
	return out
}

// ConfirmReturnReceipt implements POST /v1/returns-orders/{id}/receipts.
func (e *Endpoints) ConfirmReturnReceipt(ctx context.Context, req *gen.ReturnReceiptCreate, params gen.ConfirmReturnReceiptParams) (gen.ConfirmReturnReceiptRes, error) {
	in := ReceiptInput{Date: req.ReceiptDate, Items: make([]ItemQuantity, len(req.Items))}
	for i, it := range req.Items {
		in.Items[i] = ItemQuantity{ItemNo: it.ItemNo, Quantity: it.Quantity.Value, Unit: it.Quantity.Unit}
	}
	c, err := e.ConfirmReceipt(ctx, params.ID, in)
	if err != nil {
		return nil, err
	}
	return &gen.ReturnReceiptHeaders{Response: receiptAPI(c)}, nil
}

// receiptList lists a returns order's receipts by date.
var receiptList = listed[ReturnReceipt, gen.ReturnReceipt]{what: "return receipts", query: receiptSelect + ` WHERE c.returns_order_id = $1`, scan: scanReceipt,
	sort: pagination.Sort{Default: "receipt_date", Fields: []pagination.Field{{Name: "receipt_date", Column: "c.receipt_date", Type: "date"}}},
	key:  func(c ReturnReceipt) (time.Time, uuid.UUID) { return c.Date, c.ID }, api: receiptAPI}

// ListReturnReceipts implements GET /v1/returns-orders/{id}/receipts: the
// receipts of a returns order of a company the caller may act in, by
// date.
func (e *Endpoints) ListReturnReceipts(ctx context.Context, params gen.ListReturnReceiptsParams) (gen.ListReturnReceiptsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	r, err := returnsOrder(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	items, next, err := receiptList.page(ctx, e, tx, "listReturnReceipts", r.ID, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	return &gen.ReturnReceiptPage{Items: items, NextPageToken: next}, nil
}
