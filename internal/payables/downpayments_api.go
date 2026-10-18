package payables

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

func requestToAPI(r DownPaymentRequest) gen.DownPaymentRequest {
	return gen.DownPaymentRequest{ID: r.ID, DocumentNumber: r.DocumentNumber, Version: r.Version, CreatedAt: r.CreatedAt, CreatedBy: r.CreatedBy, UpdatedAt: r.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(r.UpdatedBy), LegalEntityID: r.LegalEntityID, SupplierID: r.SupplierID, PurchaseOrderID: apitypes.OptUUID(r.PurchaseOrderID),
		Amount: apitypes.Money(r.Amount, r.Currency, r.Exponent), TaxCode: apitypes.OptString(r.TaxCode),
		TaxAmount: apitypes.Money(r.TaxAmount, r.Currency, r.Exponent), DueDate: r.DueDate, Reference: apitypes.OptString(r.Reference),
		Status: r.Status, CancelReason: apitypes.OptString(r.CancelReason)}
}

// CreateDownPaymentRequest implements POST /v1/down-payment-requests.
func (e *Endpoints) CreateDownPaymentRequest(ctx context.Context, req *gen.DownPaymentRequestCreate, _ gen.CreateDownPaymentRequestParams) (gen.CreateDownPaymentRequestRes, error) {
	r, err := e.RequestDownPayment(ctx, DownPaymentInput{LegalEntityID: req.LegalEntityID, SupplierID: req.SupplierID,
		PurchaseOrderID: apitypes.Ptr[uuid.UUID](req.PurchaseOrderID), Amount: Amount{req.Amount.Amount, req.Amount.Currency},
		TaxCode: apitypes.Ptr[string](req.TaxCode), DueDate: req.DueDate, Reference: apitypes.Ptr[string](req.Reference)})
	if err != nil {
		return nil, err
	}
	return &gen.DownPaymentRequestHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Location: gen.NewOptString("/v1/down-payment-requests/" + r.ID.String()),
		Response: requestToAPI(r)}, nil
}

// GetDownPaymentRequest implements GET /v1/down-payment-requests/{id}.
func (e *Endpoints) GetDownPaymentRequest(ctx context.Context, params gen.GetDownPaymentRequestParams) (gen.GetDownPaymentRequestRes, error) {
	r, err := GetDownPaymentRequest(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	return &gen.GetDownPaymentRequestOKHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Response: requestToAPI(r)}, nil
}

// CancelDownPaymentRequest implements POST /v1/down-payment-requests/{id}:cancel.
func (e *Endpoints) CancelDownPaymentRequest(ctx context.Context, req *gen.ReasonInput, params gen.CancelDownPaymentRequestParams) (gen.CancelDownPaymentRequestRes, error) {
	r, err := CancelDownPaymentRequest(ctx, params.ID, etag.IfMatch(params.IfMatch), req.Reason)
	if err != nil {
		return nil, err
	}
	return &gen.CancelDownPaymentRequestOKHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Response: requestToAPI(r)}, nil
}

var requestSort = pagination.Sort{Default: "due_date", Fields: []pagination.Field{{Name: "due_date", Column: "r.due_date", Type: "date"}}}

// ListDownPaymentRequests implements GET /v1/down-payment-requests.
func (e *Endpoints) ListDownPaymentRequests(ctx context.Context, params gen.ListDownPaymentRequestsParams) (gen.ListDownPaymentRequestsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, sup, po := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.SupplierID), apitypes.Ptr[uuid.UUID](params.PurchaseOrderID)
	scope := fmt.Sprintf("listDownPaymentRequests|%s|%s|%s|%s", le, sup, params.Status.Value, po)
	ks, err := e.Pages.Keyset(requestSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(requestSelect+` WHERE ($1 OR r.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR r.legal_entity_id = $3)
		AND ($4::uuid IS NULL OR r.supplier_id = $4) AND ($5 = '' OR r.status = $5) AND ($6::uuid IS NULL OR r.purchase_order_id = $6)`,
		[]any{access.All, access.IDs, le, sup, params.Status.Value, po})
	rows, _ := tx.Query(ctx, q, args...)
	requests, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (DownPaymentRequest, error) { return scanRequest(r) })
	if err != nil {
		return nil, fmt.Errorf("payables: list down payment requests: %w", err)
	}
	requests, next := pagination.Page(ks, requests, func(r DownPaymentRequest) (string, uuid.UUID) { return r.DueDate.Format(time.DateOnly), r.ID })
	page := gen.DownPaymentRequestPage{Items: make([]gen.DownPaymentRequest, len(requests))}
	for i, r := range requests {
		page.Items[i] = requestToAPI(r)
	}
	if next != "" {
		page.NextPageToken.SetTo(next)
	}
	return &gen.DownPaymentRequestPageHeaders{Response: page}, nil
}
