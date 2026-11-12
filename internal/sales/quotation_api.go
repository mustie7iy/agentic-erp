package sales

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// references are what sales documents name by id, as their consumer
// reads them (REVIEW.md, API-consumer classes): the companies, the
// customers and the items' accounts.
type references struct {
	companies map[uuid.UUID]tenancy.Entity
	partners  map[uuid.UUID]partner.Named
	accounts  map[uuid.UUID]ledger.AccountRef
}

// named is what one document names by id: its company, its customer and
// its items.
type named struct {
	legalEntity, customer uuid.UUID
	items                 []Item
}

// references reads what the documents name by id, once for all of them.
func (e *Endpoints) references(ctx context.Context, docs []named) (references, error) {
	refs := references{companies: map[uuid.UUID]tenancy.Entity{}}
	var partners, accounts []uuid.UUID
	for _, d := range docs {
		if d.customer != uuid.Nil { // a general price names none
			partners = append(partners, d.customer)
		}
		for _, it := range d.items {
			accounts = append(accounts, it.AccountID)
		}
		if _, ok := refs.companies[d.legalEntity]; ok {
			continue
		}
		le, _, err := e.LegalEntities.Entity(ctx, d.legalEntity) // a stored document's legal entity exists
		if err != nil {
			return references{}, fmt.Errorf("sales: company %s of a document: %w", d.legalEntity, err)
		}
		refs.companies[d.legalEntity] = le
	}
	var err error
	if refs.partners, err = e.Partners.Names(ctx, partners); err != nil {
		return references{}, fmt.Errorf("sales: customers of %d documents: %w", len(docs), err)
	}
	if refs.accounts, err = e.References.AccountNames(ctx, accounts); err != nil {
		return references{}, fmt.Errorf("sales: accounts of %d documents: %w", len(docs), err)
	}
	return refs, nil
}

// quotationRefs reads what the quotations name by id.
func (e *Endpoints) quotationRefs(ctx context.Context, quotations []Quotation) (references, error) {
	docs := make([]named, len(quotations))
	for i, q := range quotations {
		docs[i] = named{legalEntity: q.LegalEntityID, customer: q.CustomerID, items: q.Items}
	}
	return e.references(ctx, docs)
}

// itemInputs are the items of a request. A price unit the request did
// not give stays 0, so that pricing can tell it from one sent (T80d).
func itemInputs(items []gen.SalesItemCreate) []ItemInput {
	out := make([]ItemInput, len(items))
	for i, it := range items {
		out[i] = ItemInput{ItemKey: apitypes.Ptr[string](it.ItemKey), Description: it.Description, Quantity: it.Quantity.Value, Unit: it.Quantity.Unit,
			PriceUnit: it.PriceUnit.Or(0), AccountID: it.AccountID, TaxCode: apitypes.Ptr[string](it.TaxCode)}
		if p, ok := it.UnitPrice.Get(); ok {
			out[i].UnitPrice = &Amount{Value: p.Amount, Currency: p.Currency}
		}
	}
	return out
}

// conditionAPI is a condition applied to an item as the API gives it, its
// amounts in the document's currency.
func conditionAPI(c Condition, currency string, exp int) gen.SalesCondition {
	out := gen.SalesCondition{ConditionType: c.Type, Name: c.Name, Kind: c.Kind, PriceUnit: apitypes.OptInt32(c.PriceUnit),
		Percent: apitypes.OptString(c.Percent), Amount: apitypes.Money(c.Amount, currency, exp)}
	if c.UnitPrice != nil {
		out.UnitPrice = gen.NewOptMoney(apitypes.Money(*c.UnitPrice, currency, exp))
	}
	return out
}

// itemAPI is an item as the API gives it, in the document's currency,
// with its account's number and name and the conditions applied to it.
func itemAPI(it Item, currency string, exp int, refs references) gen.SalesItem {
	account := refs.accounts[it.AccountID]
	conditions := make([]gen.SalesCondition, len(it.Conditions))
	for i, c := range it.Conditions {
		conditions[i] = conditionAPI(c, currency, exp)
	}
	return gen.SalesItem{ItemNo: it.ItemNo, ItemKey: apitypes.OptString(it.ItemKey), Description: it.Description,
		Quantity: gen.Quantity{Value: it.Quantity, Unit: it.Unit}, UnitPrice: apitypes.Money(it.UnitPrice, currency, exp), PriceUnit: it.PriceUnit,
		PriceSource: it.PriceSource, Conditions: conditions, NetAmount: apitypes.Money(it.Net, currency, exp), AccountID: it.AccountID, AccountNumber: account.Number, AccountName: account.Name,
		TaxCode: apitypes.OptString(it.TaxCode)}
}

// quotationAPI is a quotation as the API gives it, with what it names by
// id.
func quotationAPI(q Quotation, refs references) gen.Quotation {
	le, customer := refs.companies[q.LegalEntityID], refs.partners[q.CustomerID]
	out := gen.Quotation{ID: q.ID, Version: q.Version, CreatedAt: q.CreatedAt, CreatedBy: q.CreatedBy, UpdatedAt: q.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(q.UpdatedBy), DocumentNumber: q.DocumentNumber, LegalEntityID: q.LegalEntityID, CompanyCode: le.Code,
		CompanyName: le.Name, CustomerID: q.CustomerID, CustomerNumber: customer.Number, CustomerName: customer.Name,
		QuotationDate: q.QuotationDate, ValidUntil: q.ValidUntil, Currency: q.Currency, NetAmount: apitypes.Money(q.Net, q.Currency, q.Exponent),
		Status: q.Status, Items: make([]gen.SalesItem, len(q.Items))}
	for i, it := range q.Items {
		out.Items[i] = itemAPI(it, q.Currency, q.Exponent, refs)
	}
	return out
}

// answered is a quotation as an action answers it.
func (e *Endpoints) answered(ctx context.Context, q Quotation) (gen.Quotation, error) {
	refs, err := e.quotationRefs(ctx, []Quotation{q})
	if err != nil {
		return gen.Quotation{}, err
	}
	return quotationAPI(q, refs), nil
}

// CreateQuotation implements POST /v1/quotations.
func (e *Endpoints) CreateQuotation(ctx context.Context, req *gen.QuotationCreate, _ gen.CreateQuotationParams) (gen.CreateQuotationRes, error) {
	q, err := e.MakeQuotation(ctx, QuotationInput{LegalEntityID: req.LegalEntityID, CustomerID: req.CustomerID, ValidUntil: req.ValidUntil,
		Currency: req.Currency, Items: itemInputs(req.Items)})
	if err != nil {
		return nil, err
	}
	out, err := e.answered(ctx, q)
	if err != nil {
		return nil, err
	}
	return &gen.QuotationHeaders{ETag: gen.NewOptString(etag.Format(q.Version)), Location: gen.NewOptString("/v1/quotations/" + q.ID.String()),
		Response: out}, nil
}

// GetQuotation implements GET /v1/quotations/{id}.
func (e *Endpoints) GetQuotation(ctx context.Context, params gen.GetQuotationParams) (gen.GetQuotationRes, error) {
	q, err := GetQuotation(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	out, err := e.answered(ctx, q)
	if err != nil {
		return nil, err
	}
	return &gen.GetQuotationOKHeaders{ETag: gen.NewOptString(etag.Format(q.Version)), Response: out}, nil
}

// AcceptQuotation implements POST /v1/quotations/{id}:accept.
func (e *Endpoints) AcceptQuotation(ctx context.Context, params gen.AcceptQuotationParams) (gen.AcceptQuotationRes, error) {
	q, err := e.MarkAccepted(ctx, params.ID, etag.IfMatch(params.IfMatch))
	if err != nil {
		return nil, err
	}
	out, err := e.answered(ctx, q)
	if err != nil {
		return nil, err
	}
	return &gen.AcceptQuotationOKHeaders{ETag: gen.NewOptString(etag.Format(q.Version)), Response: out}, nil
}

// DeclineQuotation implements POST /v1/quotations/{id}:decline. It
// answers as accepting does: the generated response is the one type.
func (e *Endpoints) DeclineQuotation(ctx context.Context, params gen.DeclineQuotationParams) (gen.DeclineQuotationRes, error) {
	q, err := e.MarkRejected(ctx, params.ID, etag.IfMatch(params.IfMatch))
	if err != nil {
		return nil, err
	}
	out, err := e.answered(ctx, q)
	if err != nil {
		return nil, err
	}
	return &gen.AcceptQuotationOKHeaders{ETag: gen.NewOptString(etag.Format(q.Version)), Response: out}, nil
}

var quotationSort = pagination.Sort{Default: "quotation_date", Fields: []pagination.Field{
	{Name: "quotation_date", Column: "q.quotation_date", Type: "date"}, {Name: "document_number", Column: "q.document_number", Type: "text"},
}}

// ListQuotations implements GET /v1/quotations: the quotations of the
// companies the caller may act in.
func (e *Endpoints) ListQuotations(ctx context.Context, params gen.ListQuotationsParams) (gen.ListQuotationsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, customer := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.CustomerID)
	scope := fmt.Sprintf("listQuotations|%s|%s|%s|%s", le, customer, params.Status.Value, params.DocumentNumber.Value)
	ks, err := e.Pages.Keyset(quotationSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("sales: page of quotations: %w", err)
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, fmt.Errorf("sales: company access for quotations: %w", err)
	}
	query, args := ks.SQL(quotationSelect+` WHERE ($1 OR q.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR q.legal_entity_id = $3)
		AND ($4::uuid IS NULL OR q.customer_id = $4) AND ($5 = '' OR q.status = $5) AND ($6 = '' OR q.document_number = $6)`,
		[]any{access.All, access.IDs, le, customer, params.Status.Value, params.DocumentNumber.Value})
	rows, _ := tx.Query(ctx, query, args...)
	quotations, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Quotation, error) { return scanQuotation(r) })
	if err != nil {
		return nil, fmt.Errorf("sales: list quotations: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, quotations, func(q Quotation) (string, uuid.UUID) {
		if ks.Field() == "document_number" {
			return q.DocumentNumber, q.ID
		}
		return q.QuotationDate.Format(time.DateOnly), q.ID
	}, func(page []Quotation) (references, error) { return e.quotationRefs(ctx, page) }, quotationAPI)
	if err != nil {
		return nil, err
	}
	return &gen.QuotationPage{Items: items, NextPageToken: next}, nil
}
