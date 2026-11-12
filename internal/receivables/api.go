package receivables

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// references are what invoices name by id, as their consumer reads them
// (REVIEW.md, API-consumer classes): the companies, the customers, the
// items' accounts and centers, and a posted invoice's journal entry by
// its document number. The invoice a credit memo credits is named with
// the memo (Invoice.ReferenceInvoiceNumber).
type references struct {
	companies map[uuid.UUID]tenancy.Entity
	partners  map[uuid.UUID]partner.Named
	accounts  map[uuid.UUID]ledger.AccountRef
	centers   map[uuid.UUID]ledger.CenterName
	entries   map[uuid.UUID]string
}

// references reads what the invoices name by id, once for all of them.
func (e *Endpoints) references(ctx context.Context, invoices []Invoice) (references, error) {
	refs := references{companies: map[uuid.UUID]tenancy.Entity{}}
	var partners, accounts, centers, entries []uuid.UUID
	for _, v := range invoices {
		partners = append(partners, v.CustomerID)
		if v.EntryID != nil {
			entries = append(entries, *v.EntryID)
		}
		for _, it := range v.Items {
			accounts = append(accounts, it.AccountID)
			for _, c := range []*uuid.UUID{it.CostCenterID, it.ProfitCenterID} {
				if c != nil {
					centers = append(centers, *c)
				}
			}
		}
		if _, ok := refs.companies[v.LegalEntityID]; ok {
			continue
		}
		le, _, err := e.LegalEntities.Entity(ctx, v.LegalEntityID) // a stored invoice's legal entity exists
		if err != nil {
			return references{}, fmt.Errorf("receivables: company of customer invoice %s: %w", v.ID, err)
		}
		refs.companies[v.LegalEntityID] = le
	}
	var err error
	if refs.partners, err = e.Partners.Names(ctx, partners); err != nil {
		return references{}, fmt.Errorf("receivables: customers of %d invoices: %w", len(invoices), err)
	}
	if refs.accounts, err = e.References.AccountNames(ctx, accounts); err != nil {
		return references{}, fmt.Errorf("receivables: accounts of %d invoices: %w", len(invoices), err)
	}
	if refs.centers, err = e.References.CenterNames(ctx, centers); err != nil {
		return references{}, fmt.Errorf("receivables: centers of %d invoices: %w", len(invoices), err)
	}
	if refs.entries, err = e.Entries.Numbers(ctx, entries); err != nil {
		return references{}, fmt.Errorf("receivables: journal entries of %d invoices: %w", len(invoices), err)
	}
	return refs, nil
}

// toAPI is an invoice as the API gives it, with what it names by id.
func toAPI(v Invoice, refs references) gen.CustomerInvoice {
	m := func(a money.Minor) gen.Money { return apitypes.Money(a, v.Currency, v.Exponent) }
	le, customer := refs.companies[v.LegalEntityID], refs.partners[v.CustomerID]
	out := gen.CustomerInvoice{ID: v.ID, Version: v.Version, CreatedAt: v.CreatedAt, CreatedBy: v.CreatedBy, UpdatedAt: v.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(v.UpdatedBy), DocumentNumber: v.DocumentNumber, LegalEntityID: v.LegalEntityID, CompanyCode: le.Code,
		CompanyName: le.Name, CustomerID: v.CustomerID, CustomerNumber: customer.Number, CustomerName: customer.Name, Kind: v.Kind,
		ReferenceInvoiceID:     apitypes.OptUUID(v.ReferenceInvoiceID),
		ReferenceInvoiceNumber: apitypes.OptString(v.ReferenceInvoiceNumber), InvoiceDate: v.InvoiceDate, PostingDate: apitypes.OptDate(v.PostingDate), Currency: v.Currency, NetAmount: m(v.Net),
		TaxAmount: m(v.Tax), GrossAmount: m(v.Gross), Status: v.Status, PaymentTermCode: apitypes.OptString(v.PaymentTermCode),
		NetDueDate: apitypes.OptDate(v.NetDueDate), JournalEntryID: apitypes.OptUUID(v.EntryID),
		Items: make([]gen.CustomerInvoiceItem, len(v.Items))}
	if v.EntryID != nil { // posted: its entry's year and number are known with it
		out.JournalEntryYear.SetTo(int32(*v.EntryYear))
		out.JournalEntryNumber.SetTo(refs.entries[*v.EntryID])
	}
	for i, it := range v.Items {
		account := refs.accounts[it.AccountID]
		item := gen.CustomerInvoiceItem{ItemNo: it.ItemNo, Description: apitypes.OptString(it.Description), AccountID: it.AccountID,
			AccountNumber: account.Number, AccountName: account.Name, NetAmount: m(it.Net), TaxCode: apitypes.OptString(it.TaxCode),
			TaxAmount: m(it.Tax), GrossAmount: m(it.Net + it.Tax), // within the invoice's gross, which was checked
			CostCenterID: apitypes.OptUUID(it.CostCenterID), ProfitCenterID: apitypes.OptUUID(it.ProfitCenterID)}
		if it.CostCenterID != nil {
			item.CostCenterCode = apitypes.OptText(refs.centers[*it.CostCenterID].Code)
		}
		if it.ProfitCenterID != nil {
			item.ProfitCenterCode = apitypes.OptText(refs.centers[*it.ProfitCenterID].Code)
		}
		out.Items[i] = item
	}
	return out
}

// CreateCustomerInvoice implements POST /v1/customer-invoices.
func (e *Endpoints) CreateCustomerInvoice(ctx context.Context, req *gen.CustomerInvoiceCreate, _ gen.CreateCustomerInvoiceParams) (gen.CreateCustomerInvoiceRes, error) {
	in := Input{LegalEntityID: req.LegalEntityID, CustomerID: req.CustomerID, Kind: req.Kind.Value,
		ReferenceInvoiceID: apitypes.Ptr[uuid.UUID](req.ReferenceInvoiceID), InvoiceDate: req.InvoiceDate,
		PostingDate: apitypes.Ptr[time.Time](req.PostingDate), Currency: req.Currency, PaymentTermCode: apitypes.Ptr[string](req.PaymentTermCode),
		Items: make([]ItemInput, len(req.Items))}
	for i, it := range req.Items {
		in.Items[i] = ItemInput{Description: apitypes.Ptr[string](it.Description), AccountID: it.AccountID,
			Net: Amount{Value: it.NetAmount.Amount, Currency: it.NetAmount.Currency}, TaxCode: apitypes.Ptr[string](it.TaxCode),
			CostCenterID: apitypes.Ptr[uuid.UUID](it.CostCenterID), ProfitCenterID: apitypes.Ptr[uuid.UUID](it.ProfitCenterID)}
	}
	v, err := e.CreateInvoice(ctx, in)
	if err != nil {
		return nil, err
	}
	refs, err := e.references(ctx, []Invoice{v})
	if err != nil {
		return nil, err
	}
	return &gen.CustomerInvoiceHeaders{ETag: gen.NewOptString(etag.Format(v.Version)), Location: gen.NewOptString("/v1/customer-invoices/" + v.ID.String()),
		Response: toAPI(v, refs)}, nil
}

// PostCustomerInvoice implements POST /v1/customer-invoices/{id}:post.
func (e *Endpoints) PostCustomerInvoice(ctx context.Context, req gen.OptCustomerInvoicePost, params gen.PostCustomerInvoiceParams) (gen.PostCustomerInvoiceRes, error) {
	v, err := e.PostInvoice(ctx, params.ID, etag.IfMatch(params.IfMatch), apitypes.Ptr[time.Time](req.Value.PostingDate))
	if err != nil {
		return nil, err
	}
	refs, err := e.references(ctx, []Invoice{v})
	if err != nil {
		return nil, err
	}
	return &gen.PostCustomerInvoiceOKHeaders{ETag: gen.NewOptString(etag.Format(v.Version)), Response: toAPI(v, refs)}, nil
}

// GetCustomerInvoice implements GET /v1/customer-invoices/{id}.
func (e *Endpoints) GetCustomerInvoice(ctx context.Context, params gen.GetCustomerInvoiceParams) (gen.GetCustomerInvoiceRes, error) {
	v, err := GetInvoice(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	refs, err := e.references(ctx, []Invoice{v})
	if err != nil {
		return nil, err
	}
	return &gen.GetCustomerInvoiceOKHeaders{ETag: gen.NewOptString(etag.Format(v.Version)), Response: toAPI(v, refs)}, nil
}

var invoiceSort = pagination.Sort{Default: "invoice_date", Fields: []pagination.Field{
	{Name: "invoice_date", Column: "i.invoice_date", Type: "date"}, {Name: "document_number", Column: "i.document_number", Type: "text"},
}}

// ListCustomerInvoices implements GET /v1/customer-invoices: the invoices
// of the companies the caller may act in.
func (e *Endpoints) ListCustomerInvoices(ctx context.Context, params gen.ListCustomerInvoicesParams) (gen.ListCustomerInvoicesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, customer := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.CustomerID)
	from, to := apitypes.Ptr[time.Time](params.InvoiceDateFrom), apitypes.Ptr[time.Time](params.InvoiceDateTo)
	reference := apitypes.Ptr[uuid.UUID](params.ReferenceInvoiceID)
	scope := fmt.Sprintf("listCustomerInvoices|%s|%s|%s|%s|%v|%v|%s|%s", le, customer, params.Status.Value, params.DocumentNumber.Value, from, to,
		params.Kind.Value, reference)
	ks, err := e.Pages.Keyset(invoiceSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("receivables: page of customer invoices: %w", err)
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, fmt.Errorf("receivables: company access for customer invoices: %w", err)
	}
	q, args := ks.SQL(invoiceSelect+` WHERE ($1 OR i.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR i.legal_entity_id = $3)
		AND ($4::uuid IS NULL OR i.customer_id = $4) AND ($5 = '' OR i.status = $5) AND ($6 = '' OR i.document_number = $6)
		AND ($7::date IS NULL OR i.invoice_date >= $7) AND ($8::date IS NULL OR i.invoice_date <= $8)
		AND ($9 = '' OR i.kind = $9) AND ($10::uuid IS NULL OR i.reference_invoice_id = $10)`,
		[]any{access.All, access.IDs, le, customer, params.Status.Value, params.DocumentNumber.Value, from, to, params.Kind.Value, reference})
	rows, _ := tx.Query(ctx, q, args...)
	invoices, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Invoice, error) { return scanInvoice(r) })
	if err != nil {
		return nil, fmt.Errorf("receivables: list customer invoices: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, invoices, func(v Invoice) (string, uuid.UUID) {
		if ks.Field() == "document_number" {
			return v.DocumentNumber, v.ID
		}
		return v.InvoiceDate.Format(time.DateOnly), v.ID
	}, func(page []Invoice) (references, error) { return e.references(ctx, page) }, toAPI)
	if err != nil {
		return nil, err
	}
	return &gen.CustomerInvoicePage{Items: items, NextPageToken: next}, nil
}
