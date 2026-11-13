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

// billingRefs reads what the billing documents name by id: what every
// sales document names and, for the posted ones, their journal entries
// by number.
func (e *Endpoints) billingRefs(ctx context.Context, docs []BillingDocument) (references, error) {
	out := make([]named, len(docs))
	var entries []uuid.UUID
	for i, b := range docs {
		items := make([]Item, len(b.Items))
		for j, it := range b.Items {
			items[j] = it.Item
		}
		out[i] = named{legalEntity: b.LegalEntityID, customer: b.CustomerID, items: items}
		if b.EntryID != nil {
			entries = append(entries, *b.EntryID)
		}
	}
	refs, err := e.references(ctx, out)
	if err != nil {
		return references{}, err
	}
	if refs.entries, err = e.Entries.Numbers(ctx, entries); err != nil {
		return references{}, fmt.Errorf("sales: journal entries of %d billing documents: %w", len(docs), err)
	}
	return refs, nil
}

// billingAPI is a billing document as the API gives it, with what it
// names by id.
func billingAPI(b BillingDocument, refs references) gen.BillingDocument {
	le, customer := refs.companies[b.LegalEntityID], refs.partners[b.CustomerID]
	out := gen.BillingDocument{ID: b.ID, Version: b.Version, CreatedAt: b.CreatedAt, CreatedBy: b.CreatedBy, UpdatedAt: b.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(b.UpdatedBy), DocumentNumber: b.DocumentNumber, LegalEntityID: b.LegalEntityID, CompanyCode: le.Code,
		CompanyName: le.Name, CustomerID: b.CustomerID, CustomerNumber: customer.Number, CustomerName: customer.Name,
		SalesOrderID: b.OrderID, SalesOrderNumber: b.OrderNumber, PaymentTermCode: b.PaymentTermCode, BillingDate: b.BillingDate,
		Currency: b.Currency, NetAmount: apitypes.Money(b.Net, b.Currency, b.Exponent), Status: b.Status,
		PostingDate: apitypes.OptDate(b.PostingDate), CustomerInvoiceID: apitypes.OptUUID(b.InvoiceID),
		CustomerInvoiceNumber: apitypes.OptString(b.InvoiceNumber), JournalEntryID: apitypes.OptUUID(b.EntryID),
		Items: make([]gen.BillingDocumentItem, len(b.Items))}
	if b.EntryID != nil { // posted: its invoice's entry's year and number are known with it
		out.JournalEntryYear.SetTo(int32(*b.EntryYear))
		out.JournalEntryNumber.SetTo(refs.entries[*b.EntryID])
	}
	for i, it := range b.Items {
		base := itemAPI(it.Item, b.Currency, b.Exponent, refs)
		out.Items[i] = gen.BillingDocumentItem{ItemNo: base.ItemNo, ItemKey: base.ItemKey, Description: base.Description, Quantity: base.Quantity,
			UnitPrice: base.UnitPrice, PriceUnit: base.PriceUnit, PriceSource: base.PriceSource, Conditions: base.Conditions, NetAmount: base.NetAmount, AccountID: base.AccountID,
			AccountNumber: base.AccountNumber, AccountName: base.AccountName, TaxCode: base.TaxCode, OrderItemNo: it.OrderItemNo,
			CreditedAmount: apitypes.Money(it.Credited, b.Currency, b.Exponent)}
	}
	return out
}

// billingAnswer is a billing document as an operation answers it.
func (e *Endpoints) billingAnswer(ctx context.Context, b BillingDocument) (gen.BillingDocument, error) {
	refs, err := e.billingRefs(ctx, []BillingDocument{b})
	if err != nil {
		return gen.BillingDocument{}, err
	}
	return billingAPI(b, refs), nil
}

// CreateBillingDocument implements POST /v1/billing-documents.
func (e *Endpoints) CreateBillingDocument(ctx context.Context, req *gen.BillingDocumentCreate, _ gen.CreateBillingDocumentParams) (gen.CreateBillingDocumentRes, error) {
	b, err := e.CreateBilling(ctx, BillingInput{OrderID: req.SalesOrderID, Date: apitypes.Ptr[time.Time](req.BillingDate)})
	if err != nil {
		return nil, err
	}
	out, err := e.billingAnswer(ctx, b)
	if err != nil {
		return nil, err
	}
	return &gen.BillingDocumentHeaders{ETag: gen.NewOptString(etag.Format(b.Version)), Location: gen.NewOptString("/v1/billing-documents/" + b.ID.String()),
		Response: out}, nil
}

// PostBillingDocument implements POST /v1/billing-documents/{id}:post.
func (e *Endpoints) PostBillingDocument(ctx context.Context, req gen.OptBillingDocumentPost, params gen.PostBillingDocumentParams) (gen.PostBillingDocumentRes, error) {
	b, err := e.PostBilling(ctx, params.ID, etag.IfMatch(params.IfMatch), apitypes.Ptr[time.Time](req.Value.PostingDate))
	if err != nil {
		return nil, err
	}
	out, err := e.billingAnswer(ctx, b)
	if err != nil {
		return nil, err
	}
	return &gen.PostBillingDocumentOKHeaders{ETag: gen.NewOptString(etag.Format(b.Version)), Response: out}, nil
}

// GetBillingDocument implements GET /v1/billing-documents/{id}.
func (e *Endpoints) GetBillingDocument(ctx context.Context, params gen.GetBillingDocumentParams) (gen.GetBillingDocumentRes, error) {
	b, err := GetBilling(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	out, err := e.billingAnswer(ctx, b)
	if err != nil {
		return nil, err
	}
	return &gen.GetBillingDocumentOKHeaders{ETag: gen.NewOptString(etag.Format(b.Version)), Response: out}, nil
}

var billingSort = pagination.Sort{Default: "billing_date", Fields: []pagination.Field{
	{Name: "billing_date", Column: "b.billing_date", Type: "date"}, {Name: "document_number", Column: "b.document_number", Type: "text"},
}}

// ListBillingDocuments implements GET /v1/billing-documents: the billing
// documents of the companies the caller may act in.
func (e *Endpoints) ListBillingDocuments(ctx context.Context, params gen.ListBillingDocumentsParams) (gen.ListBillingDocumentsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, customer, order := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.CustomerID), apitypes.Ptr[uuid.UUID](params.SalesOrderID)
	scope := fmt.Sprintf("listBillingDocuments|%s|%s|%s|%s|%s", le, customer, order, params.Status.Value, params.DocumentNumber.Value)
	ks, err := e.Pages.Keyset(billingSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("sales: page of billing documents: %w", err)
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, fmt.Errorf("sales: company access for billing documents: %w", err)
	}
	query, args := ks.SQL(billingSelect+` WHERE ($1 OR b.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR b.legal_entity_id = $3)
		AND ($4::uuid IS NULL OR b.customer_id = $4) AND ($5::uuid IS NULL OR b.sales_order_id = $5) AND ($6 = '' OR b.status = $6)
		AND ($7 = '' OR b.document_number = $7)`,
		[]any{access.All, access.IDs, le, customer, order, params.Status.Value, params.DocumentNumber.Value})
	rows, _ := tx.Query(ctx, query, args...)
	docs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (BillingDocument, error) { return scanBilling(r) })
	if err != nil {
		return nil, fmt.Errorf("sales: list billing documents: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, docs, func(b BillingDocument) (string, uuid.UUID) {
		if ks.Field() == "document_number" {
			return b.DocumentNumber, b.ID
		}
		return b.BillingDate.Format(time.DateOnly), b.ID
	}, func(page []BillingDocument) (references, error) { return e.billingRefs(ctx, page) }, billingAPI)
	if err != nil {
		return nil, err
	}
	return &gen.BillingDocumentPage{Items: items, NextPageToken: next}, nil
}
