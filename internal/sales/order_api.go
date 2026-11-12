package sales

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// orderRefs reads what the orders name by id.
func (e *Endpoints) orderRefs(ctx context.Context, orders []Order) (references, error) {
	docs := make([]named, len(orders))
	for i, o := range orders {
		items := make([]Item, len(o.Items))
		for j, it := range o.Items {
			items[j] = it.Item
		}
		docs[i] = named{legalEntity: o.LegalEntityID, customer: o.CustomerID, items: items}
	}
	return e.references(ctx, docs)
}

// orderAPI is a sales order as the API gives it, with what it names by
// id.
func orderAPI(o Order, refs references) gen.SalesOrder {
	le, customer := refs.companies[o.LegalEntityID], refs.partners[o.CustomerID]
	out := gen.SalesOrder{ID: o.ID, Version: o.Version, CreatedAt: o.CreatedAt, CreatedBy: o.CreatedBy, UpdatedAt: o.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(o.UpdatedBy), DocumentNumber: o.DocumentNumber, LegalEntityID: o.LegalEntityID, CompanyCode: le.Code,
		CompanyName: le.Name, CustomerID: o.CustomerID, CustomerNumber: customer.Number, CustomerName: customer.Name,
		QuotationID: apitypes.OptUUID(o.QuotationID), QuotationNumber: apitypes.OptString(o.QuotationNumber), OrderDate: o.OrderDate,
		Currency: o.Currency, PaymentTermCode: o.PaymentTermCode, NetAmount: apitypes.Money(o.Net, o.Currency, o.Exponent), Status: o.Status,
		CreditBlock: o.CreditBlock, CreditReleasedBy: apitypes.OptUUID(o.CreditReleasedBy),
		CreditReleasedByName: apitypes.OptString(o.CreditReleasedByName), Items: make([]gen.SalesOrderItem, len(o.Items))}
	if o.CreditReleasedAt != nil {
		out.CreditReleasedAt.SetTo(*o.CreditReleasedAt)
	}
	for i, it := range o.Items {
		base := itemAPI(it.Item, o.Currency, o.Exponent, refs)
		out.Items[i] = gen.SalesOrderItem{ItemNo: base.ItemNo, ItemKey: base.ItemKey, Description: base.Description, Quantity: base.Quantity,
			UnitPrice: base.UnitPrice, PriceUnit: base.PriceUnit, PriceSource: base.PriceSource, Conditions: base.Conditions, NetAmount: base.NetAmount, AccountID: base.AccountID,
			AccountNumber: base.AccountNumber, AccountName: base.AccountName, TaxCode: base.TaxCode,
			DeliveredQuantity: gen.Quantity{Value: it.Delivered, Unit: it.Unit}, BilledQuantity: gen.Quantity{Value: it.Billed, Unit: it.Unit}}
	}
	return out
}

// orderAnswer is an order as an action answers it.
func (e *Endpoints) orderAnswer(ctx context.Context, o Order) (gen.SalesOrder, error) {
	refs, err := e.orderRefs(ctx, []Order{o})
	if err != nil {
		return gen.SalesOrder{}, err
	}
	return orderAPI(o, refs), nil
}

// CreateSalesOrder implements POST /v1/sales-orders.
func (e *Endpoints) CreateSalesOrder(ctx context.Context, req *gen.SalesOrderCreate, _ gen.CreateSalesOrderParams) (gen.CreateSalesOrderRes, error) {
	in := OrderInput{QuotationID: apitypes.Ptr[uuid.UUID](req.QuotationID), LegalEntityID: apitypes.Ptr[uuid.UUID](req.LegalEntityID),
		CustomerID: apitypes.Ptr[uuid.UUID](req.CustomerID), Currency: apitypes.Ptr[string](req.Currency),
		PaymentTermCode: apitypes.Ptr[string](req.PaymentTermCode)}
	if req.Items != nil {
		in.Items = itemInputs(req.Items)
	}
	o, err := e.MakeOrder(ctx, in)
	if err != nil {
		return nil, err
	}
	out, err := e.orderAnswer(ctx, o)
	if err != nil {
		return nil, err
	}
	return &gen.SalesOrderHeaders{ETag: gen.NewOptString(etag.Format(o.Version)), Location: gen.NewOptString("/v1/sales-orders/" + o.ID.String()),
		Response: out}, nil
}

// GetSalesOrder implements GET /v1/sales-orders/{id}.
func (e *Endpoints) GetSalesOrder(ctx context.Context, params gen.GetSalesOrderParams) (gen.GetSalesOrderRes, error) {
	o, err := GetOrder(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	out, err := e.orderAnswer(ctx, o)
	if err != nil {
		return nil, err
	}
	return &gen.GetSalesOrderOKHeaders{ETag: gen.NewOptString(etag.Format(o.Version)), Response: out}, nil
}

// ConfirmSalesOrder implements POST /v1/sales-orders/{id}:confirm.
func (e *Endpoints) ConfirmSalesOrder(ctx context.Context, params gen.ConfirmSalesOrderParams) (gen.ConfirmSalesOrderRes, error) {
	o, err := e.ConfirmOrder(ctx, params.ID, etag.IfMatch(params.IfMatch))
	if err != nil {
		return nil, err
	}
	out, err := e.orderAnswer(ctx, o)
	if err != nil {
		return nil, err
	}
	return &gen.ConfirmSalesOrderOKHeaders{ETag: gen.NewOptString(etag.Format(o.Version)), Response: out}, nil
}

// ReleaseSalesOrderCreditBlock implements POST
// /v1/sales-orders/{id}:releaseCreditBlock.
func (e *Endpoints) ReleaseSalesOrderCreditBlock(ctx context.Context, params gen.ReleaseSalesOrderCreditBlockParams) (gen.ReleaseSalesOrderCreditBlockRes, error) {
	o, err := e.ReleaseCreditBlock(ctx, params.ID, etag.IfMatch(params.IfMatch))
	if err != nil {
		return nil, err
	}
	out, err := e.orderAnswer(ctx, o)
	if err != nil {
		return nil, err
	}
	return &gen.ConfirmSalesOrderOKHeaders{ // ogen gives the two actions one answer type
		ETag: gen.NewOptString(etag.Format(o.Version)), Response: out}, nil
}

var orderSort = pagination.Sort{Default: "order_date", Fields: []pagination.Field{
	{Name: "order_date", Column: "o.order_date", Type: "date"}, {Name: "document_number", Column: "o.document_number", Type: "text"},
}}

// ListSalesOrders implements GET /v1/sales-orders: the orders of the
// companies the caller may act in.
func (e *Endpoints) ListSalesOrders(ctx context.Context, params gen.ListSalesOrdersParams) (gen.ListSalesOrdersRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, customer, quotation := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.CustomerID), apitypes.Ptr[uuid.UUID](params.QuotationID)
	scope := fmt.Sprintf("listSalesOrders|%s|%s|%s|%s|%s|%s", le, customer, params.Status.Value, params.DocumentNumber.Value, quotation, params.CreditBlock.Value)
	ks, err := e.Pages.Keyset(orderSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("sales: page of sales orders: %w", err)
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, fmt.Errorf("sales: company access for sales orders: %w", err)
	}
	query, args := ks.SQL(orderSelect+` WHERE ($1 OR o.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR o.legal_entity_id = $3)
		AND ($4::uuid IS NULL OR o.customer_id = $4) AND ($5 = '' OR o.status = $5) AND ($6 = '' OR o.document_number = $6)
		AND ($7::uuid IS NULL OR o.quotation_id = $7) AND ($8 = '' OR o.credit_block = $8)`,
		[]any{access.All, access.IDs, le, customer, params.Status.Value, params.DocumentNumber.Value, quotation, params.CreditBlock.Value})
	rows, _ := tx.Query(ctx, query, args...)
	orders, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Order, error) { return scanOrder(r) })
	if err != nil {
		return nil, fmt.Errorf("sales: list sales orders: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, orders, func(o Order) (string, uuid.UUID) {
		if ks.Field() == "document_number" {
			return o.DocumentNumber, o.ID
		}
		return o.OrderDate.Format(time.DateOnly), o.ID
	}, func(page []Order) (references, error) { return e.orderRefs(ctx, page) }, orderAPI)
	if err != nil {
		return nil, err
	}
	return &gen.SalesOrderPage{Items: items, NextPageToken: next}, nil
}
