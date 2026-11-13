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

// returnsRefs reads what the returns orders name by id: their companies
// and their customers. A returns order's items name no account.
func (e *Endpoints) returnsRefs(ctx context.Context, orders []ReturnsOrder) (references, error) {
	docs := make([]named, len(orders))
	for i, r := range orders {
		docs[i] = named{legalEntity: r.LegalEntityID, customer: r.CustomerID}
	}
	return e.references(ctx, docs)
}

// returnsAPI is a returns order as the API gives it, with what it names
// by id.
func returnsAPI(r ReturnsOrder, refs references) gen.ReturnsOrder {
	le, customer := refs.companies[r.LegalEntityID], refs.partners[r.CustomerID]
	out := gen.ReturnsOrder{ID: r.ID, Version: r.Version, CreatedAt: r.CreatedAt, CreatedBy: r.CreatedBy, UpdatedAt: r.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(r.UpdatedBy), DocumentNumber: r.DocumentNumber, LegalEntityID: r.LegalEntityID, CompanyCode: le.Code,
		CompanyName: le.Name, CustomerID: r.CustomerID, CustomerNumber: customer.Number, CustomerName: customer.Name,
		BillingDocumentID: r.BillingDocumentID, BillingDocumentNumber: r.BillingDocumentNumber, SalesOrderID: r.OrderID,
		SalesOrderNumber: r.OrderNumber, ReturnsDate: r.ReturnsDate, Reason: r.Reason, ReasonText: apitypes.OptString(r.ReasonText),
		Status: r.Status, Items: make([]gen.ReturnsOrderItem, len(r.Items))}
	for i, it := range r.Items {
		out.Items[i] = gen.ReturnsOrderItem{ItemNo: it.ItemNo, BillingItemNo: it.BillingItemNo, OrderItemNo: it.OrderItemNo,
			ItemKey: apitypes.OptString(it.ItemKey), Description: it.Description, BilledQuantity: gen.Quantity{Value: it.Billed, Unit: it.Unit},
			Quantity: gen.Quantity{Value: it.Quantity, Unit: it.Unit}}
	}
	return out
}

// returnsAnswer is a returns order as an operation answers it.
func (e *Endpoints) returnsAnswer(ctx context.Context, r ReturnsOrder) (gen.ReturnsOrder, error) {
	refs, err := e.returnsRefs(ctx, []ReturnsOrder{r})
	if err != nil {
		return gen.ReturnsOrder{}, err
	}
	return returnsAPI(r, refs), nil
}

// CreateReturnsOrder implements POST /v1/returns-orders.
func (e *Endpoints) CreateReturnsOrder(ctx context.Context, req *gen.ReturnsOrderCreate, _ gen.CreateReturnsOrderParams) (gen.CreateReturnsOrderRes, error) {
	in := ReturnsInput{BillingDocumentID: req.BillingDocumentID, Reason: req.Reason, ReasonText: apitypes.Ptr[string](req.ReasonText),
		Items: make([]ItemQuantity, len(req.Items))}
	for i, it := range req.Items {
		in.Items[i] = ItemQuantity{ItemNo: it.BillingItemNo, Quantity: it.Quantity.Value, Unit: it.Quantity.Unit}
	}
	r, err := e.MakeReturns(ctx, in)
	if err != nil {
		return nil, err
	}
	out, err := e.returnsAnswer(ctx, r)
	if err != nil {
		return nil, err
	}
	return &gen.ReturnsOrderHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Location: gen.NewOptString("/v1/returns-orders/" + r.ID.String()),
		Response: out}, nil
}

// GetReturnsOrder implements GET /v1/returns-orders/{id}.
func (e *Endpoints) GetReturnsOrder(ctx context.Context, params gen.GetReturnsOrderParams) (gen.GetReturnsOrderRes, error) {
	r, err := GetReturns(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	out, err := e.returnsAnswer(ctx, r)
	if err != nil {
		return nil, err
	}
	return &gen.GetReturnsOrderOKHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Response: out}, nil
}

// ConfirmReturnsOrder implements POST /v1/returns-orders/{id}:confirm.
func (e *Endpoints) ConfirmReturnsOrder(ctx context.Context, params gen.ConfirmReturnsOrderParams) (gen.ConfirmReturnsOrderRes, error) {
	r, err := e.ConfirmReturns(ctx, params.ID, etag.IfMatch(params.IfMatch))
	if err != nil {
		return nil, err
	}
	out, err := e.returnsAnswer(ctx, r)
	if err != nil {
		return nil, err
	}
	return &gen.ConfirmReturnsOrderOKHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Response: out}, nil
}

var returnsSort = pagination.Sort{Default: "returns_date", Fields: []pagination.Field{
	{Name: "returns_date", Column: "r.returns_date", Type: "date"}, {Name: "document_number", Column: "r.document_number", Type: "text"},
}}

// ListReturnsOrders implements GET /v1/returns-orders: the returns orders
// of the companies the caller may act in.
func (e *Endpoints) ListReturnsOrders(ctx context.Context, params gen.ListReturnsOrdersParams) (gen.ListReturnsOrdersRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, customer := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.CustomerID)
	doc, order := apitypes.Ptr[uuid.UUID](params.BillingDocumentID), apitypes.Ptr[uuid.UUID](params.SalesOrderID)
	scope := fmt.Sprintf("listReturnsOrders|%s|%s|%s|%s|%s|%s", le, customer, doc, order, params.Status.Value, params.DocumentNumber.Value)
	ks, err := e.Pages.Keyset(returnsSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("sales: page of returns orders: %w", err)
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, fmt.Errorf("sales: company access for returns orders: %w", err)
	}
	query, args := ks.SQL(returnsSelect+` WHERE ($1 OR r.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR r.legal_entity_id = $3)
		AND ($4::uuid IS NULL OR r.customer_id = $4) AND ($5::uuid IS NULL OR r.billing_document_id = $5)
		AND ($6::uuid IS NULL OR r.sales_order_id = $6) AND ($7 = '' OR r.status = $7) AND ($8 = '' OR r.document_number = $8)`,
		[]any{access.All, access.IDs, le, customer, doc, order, params.Status.Value, params.DocumentNumber.Value})
	rows, _ := tx.Query(ctx, query, args...)
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ReturnsOrder, error) { return scanReturns(row) })
	if err != nil {
		return nil, fmt.Errorf("sales: list returns orders: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, found, func(r ReturnsOrder) (string, uuid.UUID) {
		if ks.Field() == "document_number" {
			return r.DocumentNumber, r.ID
		}
		return r.ReturnsDate.Format(time.DateOnly), r.ID
	}, func(page []ReturnsOrder) (references, error) { return e.returnsRefs(ctx, page) }, returnsAPI)
	if err != nil {
		return nil, err
	}
	return &gen.ReturnsOrderPage{Items: items, NextPageToken: next}, nil
}
