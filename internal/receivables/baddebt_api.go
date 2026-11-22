package receivables

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// reliefReferences are what reliefs name by id, as their consumer reads
// them (REVIEW.md, API-consumer classes): their companies and customers.
type reliefReferences struct {
	companies map[uuid.UUID]tenancy.Entity
	partners  map[uuid.UUID]partner.Named
}

// reliefReferences reads what the reliefs name by id, once for all of
// them.
func (e *Endpoints) reliefReferences(ctx context.Context, reliefs []Relief) (reliefReferences, error) {
	refs := reliefReferences{companies: map[uuid.UUID]tenancy.Entity{}}
	customers := make([]uuid.UUID, len(reliefs))
	for i, r := range reliefs {
		customers[i] = r.CustomerID
		if _, ok := refs.companies[r.LegalEntityID]; ok {
			continue
		}
		le, _, err := e.LegalEntities.Entity(ctx, r.LegalEntityID) // a stored relief's legal entity exists
		if err != nil {
			return reliefReferences{}, fmt.Errorf("receivables: company of bad debt relief %s: %w", r.ID, err)
		}
		refs.companies[r.LegalEntityID] = le
	}
	var err error
	if refs.partners, err = e.Partners.Names(ctx, customers); err != nil {
		return reliefReferences{}, fmt.Errorf("receivables: customers of %d bad debt reliefs: %w", len(reliefs), err)
	}
	return refs, nil
}

// reliefToAPI is a relief as the API gives it, with what it names by id.
func reliefToAPI(r Relief, refs reliefReferences) gen.BadDebtRelief {
	le, customer := refs.companies[r.LegalEntityID], refs.partners[r.CustomerID]
	out := gen.BadDebtRelief{ID: r.ID, LegalEntityID: r.LegalEntityID, CompanyCode: le.Code, CompanyName: le.Name, CustomerInvoiceID: r.InvoiceID,
		CustomerInvoiceNumber: r.InvoiceNumber, CustomerID: r.CustomerID, CustomerNumber: customer.Number, CustomerName: customer.Name,
		DisputeCaseID: r.DisputeCaseID, WriteOffJournalEntryID: r.WriteOffID, WriteOffJournalEntryYear: int32(r.WriteOffYear),
		WriteOffJournalEntryNumber: r.WriteOffNumber, Currency: r.Currency, WrittenOffAmount: apitypes.Money(r.WrittenOff, r.Currency, r.Exponent),
		TaxAmount: apitypes.Money(r.Tax, r.Currency, r.Exponent), LocalWrittenOffAmount: apitypes.Money(r.WrittenOffLC, r.LocalCurrency, r.LocalExponent),
		LocalTaxAmount: apitypes.Money(r.TaxLC, r.LocalCurrency, r.LocalExponent), TaxCode: r.TaxCode, PostingDate: r.PostingDate,
		JournalEntryID: r.EntryID, JournalEntryYear: int32(r.EntryYear), JournalEntryNumber: r.EntryNumber, CreatedAt: r.CreatedAt, CreatedBy: r.CreatedBy}
	if r.CustomerNotifiedOn != nil {
		out.CustomerNotifiedOn.SetTo(*r.CustomerNotifiedOn)
	}
	return out
}

// reliefAnswer is the relief as the API gives it.
func (e *Endpoints) reliefAnswer(ctx context.Context, r Relief) (gen.BadDebtRelief, error) {
	refs, err := e.reliefReferences(ctx, []Relief{r})
	if err != nil {
		return gen.BadDebtRelief{}, err
	}
	return reliefToAPI(r, refs), nil
}

// CreateBadDebtRelief implements POST /v1/bad-debt-reliefs.
func (e *Endpoints) CreateBadDebtRelief(ctx context.Context, req *gen.BadDebtReliefCreate, _ gen.CreateBadDebtReliefParams) (gen.CreateBadDebtReliefRes, error) {
	r, err := e.ClaimBadDebtRelief(ctx, ReliefInput{InvoiceID: req.CustomerInvoiceID, WriteOffEntryID: req.WriteOffJournalEntryID, TaxCode: req.TaxCode,
		CustomerNotifiedOn: apitypes.Ptr[time.Time](req.CustomerNotifiedOn), PostingDate: apitypes.Ptr[time.Time](req.PostingDate)})
	if err != nil {
		return nil, err
	}
	out, err := e.reliefAnswer(ctx, r)
	if err != nil {
		return nil, err
	}
	return &gen.BadDebtReliefHeaders{Location: gen.NewOptString("/v1/bad-debt-reliefs/" + r.ID.String()), Response: out}, nil
}

// GetBadDebtRelief implements GET /v1/bad-debt-reliefs/{id}.
func (e *Endpoints) GetBadDebtRelief(ctx context.Context, params gen.GetBadDebtReliefParams) (gen.GetBadDebtReliefRes, error) {
	r, err := GetRelief(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	out, err := e.reliefAnswer(ctx, r)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

var reliefSort = pagination.Sort{Default: "posting_date", Fields: []pagination.Field{
	{Name: "posting_date", Column: "r.posting_date", Type: "date"},
}}

// ListBadDebtReliefs implements GET /v1/bad-debt-reliefs: the reliefs of
// the companies the caller may act in.
func (e *Endpoints) ListBadDebtReliefs(ctx context.Context, params gen.ListBadDebtReliefsParams) (gen.ListBadDebtReliefsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, invoice := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.CustomerInvoiceID)
	from, to := apitypes.Ptr[time.Time](params.PostingDateFrom), apitypes.Ptr[time.Time](params.PostingDateTo)
	scope := fmt.Sprintf("listBadDebtReliefs|%s|%s|%v|%v", le, invoice, from, to)
	ks, err := e.Pages.Keyset(reliefSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("receivables: page of bad debt reliefs: %w", err)
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, fmt.Errorf("receivables: company access for bad debt reliefs: %w", err)
	}
	q, args := ks.SQL(reliefSelect+` WHERE ($1 OR r.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR r.legal_entity_id = $3)
		AND ($4::uuid IS NULL OR r.customer_invoice_id = $4) AND ($5::date IS NULL OR r.posting_date >= $5)
		AND ($6::date IS NULL OR r.posting_date <= $6)`, []any{access.All, access.IDs, le, invoice, from, to})
	rows, _ := tx.Query(ctx, q, args...)
	reliefs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Relief, error) { return scanRelief(r) })
	if err != nil {
		return nil, fmt.Errorf("receivables: list bad debt reliefs: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, reliefs, func(r Relief) (string, uuid.UUID) { return r.PostingDate.Format(time.DateOnly), r.ID },
		func(page []Relief) (reliefReferences, error) { return e.reliefReferences(ctx, page) }, reliefToAPI)
	if err != nil {
		return nil, err
	}
	return &gen.BadDebtReliefPage{Items: items, NextPageToken: next}, nil
}
