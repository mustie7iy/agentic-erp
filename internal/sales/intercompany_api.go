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

// intercompanyRefs reads what the billings name by id: both companies,
// the customer, the items' accounts and the entries by number.
func (e *Endpoints) intercompanyRefs(ctx context.Context, docs []IntercompanyBilling) (references, error) {
	out := make([]named, 0, 2*len(docs))
	entries := make([]uuid.UUID, len(docs))
	for i, b := range docs {
		items := make([]Item, len(b.Items))
		for j, it := range b.Items {
			items[j] = it.Item
		}
		out = append(out, named{legalEntity: b.LegalEntityID, customer: b.CustomerID, items: items}, named{legalEntity: b.ReceivingID})
		entries[i] = b.EntryID
	}
	refs, err := e.references(ctx, out)
	if err != nil {
		return references{}, err
	}
	if refs.entries, err = e.Entries.Numbers(ctx, entries); err != nil {
		return references{}, fmt.Errorf("sales: journal entries of %d intercompany billings: %w", len(docs), err)
	}
	return refs, nil
}

// intercompanyAPI is an intercompany billing as the API gives it, with
// what it names by id.
func intercompanyAPI(b IntercompanyBilling, refs references) gen.IntercompanyBilling {
	amount := func(m money.Minor) gen.Money { return apitypes.Money(m, b.Currency, b.Exponent) }
	customer := refs.partners[b.CustomerID]
	out := gen.IntercompanyBilling{ID: b.ID, DocumentNumber: b.DocumentNumber, LegalEntityID: b.LegalEntityID, CompanyCode: refs.companies[b.LegalEntityID].Code,
		CustomerID: b.CustomerID, CustomerNumber: customer.Number, CustomerName: customer.Name, ReceivingLegalEntityID: b.ReceivingID,
		ReceivingCompanyCode: refs.companies[b.ReceivingID].Code, PurchaseOrderID: b.OrderID, PurchaseOrderNumber: b.OrderNumber,
		IssueDocumentID: b.IssueDocumentID, IssueDocumentNumber: b.IssueDocumentNumber, BillingDate: b.BillingDate, PostingDate: b.PostingDate,
		Currency: b.Currency, NetAmount: amount(b.Net), TaxAmount: amount(b.Tax), GrossAmount: amount(b.Gross()),
		CustomerInvoiceID: b.InvoiceID, CustomerInvoiceNumber: b.InvoiceNumber, JournalEntryID: b.EntryID, JournalEntryYear: int32(b.EntryYear),
		JournalEntryNumber: refs.entries[b.EntryID], Items: make([]gen.IntercompanyBillingItem, len(b.Items)), CreatedAt: b.CreatedAt, CreatedBy: b.CreatedBy}
	for i, it := range b.Items {
		account := refs.accounts[it.AccountID]
		out.Items[i] = gen.IntercompanyBillingItem{ItemNo: it.ItemNo, PoItemNo: it.POItemNo, MaterialID: *it.MaterialID, Description: it.Description,
			Quantity: gen.Quantity{Value: it.Quantity, Unit: it.Unit}, UnitPrice: amount(it.UnitPrice), PriceUnit: it.PriceUnit,
			NetAmount: amount(it.Net), TaxAmount: amount(it.Tax), AccountID: it.AccountID, AccountNumber: account.Number,
			AccountName: account.Name, TaxCode: apitypes.OptString(it.TaxCode), TaxJurisdiction: apitypes.OptString(it.TaxJurisdiction)}
	}
	return out
}

// intercompanyAnswer is a billing as an operation answers it, with its
// ETag: the representation's, as it never changes.
func (e *Endpoints) intercompanyAnswer(ctx context.Context, b IntercompanyBilling) (gen.IntercompanyBilling, gen.OptString, error) {
	refs, err := e.intercompanyRefs(ctx, []IntercompanyBilling{b})
	if err != nil {
		return gen.IntercompanyBilling{}, gen.OptString{}, err
	}
	out := intercompanyAPI(b, refs)
	return out, gen.NewOptString(etag.Of(&out)), nil
}

// CreateIntercompanyBilling implements POST /v1/intercompany-billings.
func (e *Endpoints) CreateIntercompanyBilling(ctx context.Context, req *gen.IntercompanyBillingCreate, _ gen.CreateIntercompanyBillingParams) (
	gen.CreateIntercompanyBillingRes, error) {
	in := IntercompanyInput{OrderID: req.PurchaseOrderID, CustomerID: req.CustomerID, BillingDate: apitypes.Ptr[time.Time](req.BillingDate),
		PostingDate: apitypes.Ptr[time.Time](req.PostingDate), Items: make([]IntercompanyItemInput, len(req.Items))}
	for i, it := range req.Items {
		in.Items[i] = IntercompanyItemInput{POItemNo: it.PoItemNo, AccountID: it.AccountID, TaxCode: apitypes.Ptr[string](it.TaxCode),
			TaxJurisdiction: apitypes.Ptr[string](it.TaxJurisdiction)}
	}
	b, err := e.BillIntercompany(ctx, in)
	if err != nil {
		return nil, err
	}
	out, tag, err := e.intercompanyAnswer(ctx, b)
	if err != nil {
		return nil, err
	}
	return &gen.IntercompanyBillingHeaders{ETag: tag, Location: gen.NewOptString("/v1/intercompany-billings/" + b.ID.String()), Response: out}, nil
}

// GetIntercompanyBilling implements GET /v1/intercompany-billings/{id}.
func (e *Endpoints) GetIntercompanyBilling(ctx context.Context, params gen.GetIntercompanyBillingParams) (gen.GetIntercompanyBillingRes, error) {
	b, err := GetIntercompany(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	out, tag, err := e.intercompanyAnswer(ctx, b)
	if err != nil {
		return nil, err
	}
	return &gen.GetIntercompanyBillingOKHeaders{ETag: tag, Response: out}, nil
}

var intercompanySort = pagination.Sort{Default: "billing_date", Fields: []pagination.Field{
	{Name: "billing_date", Column: "b.billing_date", Type: "date"}, {Name: "document_number", Column: "b.document_number", Type: "text"},
}}

// ListIntercompanyBillings implements GET /v1/intercompany-billings: the
// billings of or to the companies the caller may act in.
func (e *Endpoints) ListIntercompanyBillings(ctx context.Context, params gen.ListIntercompanyBillingsParams) (gen.ListIntercompanyBillingsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, receiving, order := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.ReceivingLegalEntityID),
		apitypes.Ptr[uuid.UUID](params.PurchaseOrderID)
	scope := fmt.Sprintf("listIntercompanyBillings|%s|%s|%s", le, receiving, order)
	ks, err := e.Pages.Keyset(intercompanySort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("sales: page of intercompany billings: %w", err)
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, fmt.Errorf("sales: company access for intercompany billings: %w", err)
	}
	query, args := ks.SQL(intercompanySelect+` WHERE ($1 OR b.legal_entity_id = ANY($2) OR b.receiving_legal_entity_id = ANY($2))
		AND ($3::uuid IS NULL OR b.legal_entity_id = $3) AND ($4::uuid IS NULL OR b.receiving_legal_entity_id = $4)
		AND ($5::uuid IS NULL OR b.purchase_order_id = $5)`, []any{access.All, access.IDs, le, receiving, order})
	rows, _ := tx.Query(ctx, query, args...)
	docs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (IntercompanyBilling, error) { return scanIntercompany(r) })
	if err != nil {
		return nil, fmt.Errorf("sales: list intercompany billings: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, docs, func(b IntercompanyBilling) (string, uuid.UUID) {
		if ks.Field() == "document_number" {
			return b.DocumentNumber, b.ID
		}
		return b.BillingDate.Format(time.DateOnly), b.ID
	}, func(page []IntercompanyBilling) (references, error) { return e.intercompanyRefs(ctx, page) }, intercompanyAPI)
	if err != nil {
		return nil, err
	}
	return &gen.IntercompanyBillingPage{Items: items, NextPageToken: next}, nil
}
