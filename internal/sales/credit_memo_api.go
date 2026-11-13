package sales

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// creditRefs reads what the credit memo requests name by id: their
// companies, their customers and their items' accounts.
func (e *Endpoints) creditRefs(ctx context.Context, requests []CreditMemoRequest) (references, error) {
	docs := make([]named, len(requests))
	for i, r := range requests {
		items := make([]Item, len(r.Items))
		for j, it := range r.Items {
			items[j] = Item{AccountID: it.AccountID}
		}
		docs[i] = named{legalEntity: r.LegalEntityID, customer: r.CustomerID, items: items}
	}
	return e.references(ctx, docs)
}

// creditAPI is a credit memo request as the API gives it, with what it
// names by id.
func creditAPI(r CreditMemoRequest, refs references) gen.CreditMemoRequest {
	le, customer := refs.companies[r.LegalEntityID], refs.partners[r.CustomerID]
	amount := func(m money.Minor) gen.Money { return apitypes.Money(m, r.Currency, r.Exponent) }
	out := gen.CreditMemoRequest{ID: r.ID, Version: r.Version, CreatedAt: r.CreatedAt, CreatedBy: r.CreatedBy, UpdatedAt: r.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(r.UpdatedBy), DocumentNumber: r.DocumentNumber, LegalEntityID: r.LegalEntityID, CompanyCode: le.Code,
		CompanyName: le.Name, CustomerID: r.CustomerID, CustomerNumber: customer.Number, CustomerName: customer.Name,
		BillingDocumentID: r.BillingDocumentID, BillingDocumentNumber: r.BillingDocumentNumber, SalesOrderID: r.OrderID,
		SalesOrderNumber: r.OrderNumber, ReturnsOrderID: apitypes.OptUUID(r.ReturnsOrderID), ReturnsOrderNumber: apitypes.OptString(r.ReturnsOrderNumber),
		RequestDate: r.RequestDate, Reason: apitypes.OptString(r.Reason), Status: r.Status, RejectionReason: apitypes.OptString(r.RejectionReason),
		Currency: r.Currency, NetAmount: amount(r.Net), Items: make([]gen.CreditMemoRequestItem, len(r.Items))}
	for i, it := range r.Items {
		account := refs.accounts[it.AccountID]
		item := gen.CreditMemoRequestItem{ItemNo: it.ItemNo, BillingItemNo: it.BillingItemNo, OrderItemNo: it.OrderItemNo,
			ItemKey: apitypes.OptString(it.ItemKey), Description: it.Description, BilledQuantity: gen.Quantity{Value: it.BilledQuantity, Unit: it.Unit},
			BilledAmount: amount(it.Billed), NetAmount: amount(it.Net), AccountID: it.AccountID, AccountNumber: account.Number,
			AccountName: account.Name, TaxCode: apitypes.OptString(it.TaxCode)}
		if it.Quantity != nil {
			item.Quantity.SetTo(gen.Quantity{Value: *it.Quantity, Unit: it.Unit})
		}
		out.Items[i] = item
	}
	return out
}

// creditAnswer is a credit memo request as an operation answers it.
func (e *Endpoints) creditAnswer(ctx context.Context, r CreditMemoRequest) (gen.CreditMemoRequest, error) {
	refs, err := e.creditRefs(ctx, []CreditMemoRequest{r})
	if err != nil {
		return gen.CreditMemoRequest{}, err
	}
	return creditAPI(r, refs), nil
}

// CreateCreditMemoRequest implements POST /v1/credit-memo-requests.
func (e *Endpoints) CreateCreditMemoRequest(ctx context.Context, req *gen.CreditMemoRequestCreate, _ gen.CreateCreditMemoRequestParams) (gen.CreateCreditMemoRequestRes, error) {
	in := CreditInput{BillingDocumentID: req.BillingDocumentID, ReturnsOrderID: apitypes.Ptr[uuid.UUID](req.ReturnsOrderID),
		Reason: apitypes.Ptr[string](req.Reason), Items: make([]CreditAmount, len(req.Items))}
	for i, it := range req.Items {
		in.Items[i] = CreditAmount{BillingItemNo: it.BillingItemNo, Value: it.NetAmount.Amount, Currency: it.NetAmount.Currency}
	}
	r, err := e.RequestCredit(ctx, in)
	if err != nil {
		return nil, err
	}
	out, err := e.creditAnswer(ctx, r)
	if err != nil {
		return nil, err
	}
	return &gen.CreditMemoRequestHeaders{ETag: gen.NewOptString(etag.Format(r.Version)),
		Location: gen.NewOptString("/v1/credit-memo-requests/" + r.ID.String()), Response: out}, nil
}

// GetCreditMemoRequest implements GET /v1/credit-memo-requests/{id}.
func (e *Endpoints) GetCreditMemoRequest(ctx context.Context, params gen.GetCreditMemoRequestParams) (gen.GetCreditMemoRequestRes, error) {
	r, err := GetCreditMemoRequest(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	out, err := e.creditAnswer(ctx, r)
	if err != nil {
		return nil, err
	}
	return &gen.GetCreditMemoRequestOKHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Response: out}, nil
}

// ReleaseCreditMemoRequest implements POST
// /v1/credit-memo-requests/{id}:release, the approval.
func (e *Endpoints) ReleaseCreditMemoRequest(ctx context.Context, params gen.ReleaseCreditMemoRequestParams) (gen.ReleaseCreditMemoRequestRes, error) {
	r, err := e.ApproveCredit(ctx, params.ID, etag.IfMatch(params.IfMatch))
	if err != nil {
		return nil, err
	}
	out, err := e.creditAnswer(ctx, r)
	if err != nil {
		return nil, err
	}
	return &gen.DeclineCreditMemoRequestOKHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Response: out}, nil
}

// DeclineCreditMemoRequest implements POST
// /v1/credit-memo-requests/{id}:decline, the rejection.
func (e *Endpoints) DeclineCreditMemoRequest(ctx context.Context, req *gen.CreditMemoRequestReject, params gen.DeclineCreditMemoRequestParams) (gen.DeclineCreditMemoRequestRes, error) {
	r, err := e.RejectCredit(ctx, params.ID, etag.IfMatch(params.IfMatch), req.Reason)
	if err != nil {
		return nil, err
	}
	out, err := e.creditAnswer(ctx, r)
	if err != nil {
		return nil, err
	}
	return &gen.DeclineCreditMemoRequestOKHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Response: out}, nil
}

var creditSort = pagination.Sort{Default: "request_date", Fields: []pagination.Field{
	{Name: "request_date", Column: "r.request_date", Type: "date"}, {Name: "document_number", Column: "r.document_number", Type: "text"},
}}

// ListCreditMemoRequests implements GET /v1/credit-memo-requests: the
// requests of the companies the caller may act in.
func (e *Endpoints) ListCreditMemoRequests(ctx context.Context, params gen.ListCreditMemoRequestsParams) (gen.ListCreditMemoRequestsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, customer, doc := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.CustomerID), apitypes.Ptr[uuid.UUID](params.BillingDocumentID)
	order, returns := apitypes.Ptr[uuid.UUID](params.SalesOrderID), apitypes.Ptr[uuid.UUID](params.ReturnsOrderID)
	scope := fmt.Sprintf("listCreditMemoRequests|%s|%s|%s|%s|%s|%s|%s", le, customer, doc, order, returns, params.Status.Value, params.DocumentNumber.Value)
	ks, err := e.Pages.Keyset(creditSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("sales: page of credit memo requests: %w", err)
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, fmt.Errorf("sales: company access for credit memo requests: %w", err)
	}
	query, args := ks.SQL(creditSelect+` WHERE ($1 OR r.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR r.legal_entity_id = $3)
		AND ($4::uuid IS NULL OR r.customer_id = $4) AND ($5::uuid IS NULL OR r.billing_document_id = $5)
		AND ($6::uuid IS NULL OR r.sales_order_id = $6) AND ($7::uuid IS NULL OR r.returns_order_id = $7)
		AND ($8 = '' OR r.status = $8) AND ($9 = '' OR r.document_number = $9)`,
		[]any{access.All, access.IDs, le, customer, doc, order, returns, params.Status.Value, params.DocumentNumber.Value})
	rows, _ := tx.Query(ctx, query, args...)
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (CreditMemoRequest, error) { return scanCredit(row) })
	if err != nil {
		return nil, fmt.Errorf("sales: list credit memo requests: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, found, func(r CreditMemoRequest) (string, uuid.UUID) {
		if ks.Field() == "document_number" {
			return r.DocumentNumber, r.ID
		}
		return r.RequestDate.Format(time.DateOnly), r.ID
	}, func(page []CreditMemoRequest) (references, error) { return e.creditRefs(ctx, page) }, creditAPI)
	if err != nil {
		return nil, err
	}
	return &gen.CreditMemoRequestPage{Items: items, NextPageToken: next}, nil
}
