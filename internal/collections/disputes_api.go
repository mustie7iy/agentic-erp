package collections

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
	"github.com/mustie7iy/agentic-erp/internal/platform/idref"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// personRef is a principal as a case names it: its assignee or who closed
// it, held by the identity context.
var personRef = idref.Kind{Name: "principal", Context: "identity"}

// caseRefs is what a page of cases is shown with: their companies, their
// customers' names and the names of the people they name.
type caseRefs struct {
	companies map[uuid.UUID]tenancy.Entity
	customers map[uuid.UUID]partner.Named
	people    map[uuid.UUID]string
}

// refs reads what the cases name by id, once each: their companies,
// customers and people, as their consumer reads them (REVIEW.md,
// API-consumer classes).
func (e *Endpoints) refs(ctx context.Context, cases []Case) (caseRefs, error) {
	out := caseRefs{companies: map[uuid.UUID]tenancy.Entity{}}
	var customers []uuid.UUID
	for _, c := range cases {
		customers = append(customers, c.CustomerID)
		if _, ok := out.companies[c.LegalEntityID]; ok {
			continue
		}
		le, _, err := e.LegalEntities.Entity(ctx, c.LegalEntityID) // a stored case's legal entity exists
		if err != nil {
			return caseRefs{}, fmt.Errorf("collections: company of dispute case %s: %w", c.ID, err)
		}
		out.companies[c.LegalEntityID] = le
	}
	var err error
	if out.customers, err = e.Partners.Names(ctx, customers); err != nil {
		return caseRefs{}, fmt.Errorf("collections: customers of %d dispute cases: %w", len(cases), err)
	}
	people := append(idref.Of(cases, func(c Case) *uuid.UUID { return c.Assignee }), idref.Of(cases, func(c Case) *uuid.UUID { return c.ClosedBy })...)
	if out.people, err = idref.Resolve(ctx, e.People.PersonNames, people, personRef); err != nil {
		return caseRefs{}, fmt.Errorf("collections: people of %d dispute cases: %w", len(cases), err)
	}
	return out, nil
}

// caseToAPI is a case as the API gives it: its company by code and name,
// its customer by number and name, its people by name, each amount with
// its currency.
func caseToAPI(c Case, r caseRefs) gen.DisputeCase {
	le := r.companies[c.LegalEntityID]
	out := gen.DisputeCase{ID: c.ID, Version: c.Version, CreatedAt: c.CreatedAt, CreatedBy: c.CreatedBy, UpdatedAt: c.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(c.UpdatedBy), LegalEntityID: c.LegalEntityID, CompanyCode: le.Code, CompanyName: le.Name, CustomerID: c.CustomerID,
		CustomerNumber: c.CustomerNumber, CustomerName: r.customers[c.CustomerID].Name, FiscalYear: int32(c.FiscalYear), JournalEntryID: c.EntryID,
		LineNo: c.LineNo, DocumentNumber: c.DocumentNumber, Reference: apitypes.OptString(c.Reference),
		DisputedAmount: apitypes.Money(c.Amount, c.Currency, c.Exponent), LocalDisputedAmount: apitypes.Money(c.AmountLC, c.CurrencyLC, c.ExponentLC),
		Reason: c.Reason, AssigneeID: apitypes.OptUUID(c.Assignee), Note: apitypes.OptString(c.Note), Status: c.Status,
		CloseReason: apitypes.OptString(c.CloseReason), ClosedAt: apitypes.OptDateTime(c.ClosedAt), ClosedBy: apitypes.OptUUID(c.ClosedBy)}
	if c.Assignee != nil {
		out.AssigneeName.SetTo(r.people[*c.Assignee])
	}
	if c.ClosedBy != nil {
		out.ClosedByName.SetTo(r.people[*c.ClosedBy])
	}
	return out
}

// shownCase is a case as the API gives it, with what it names.
func (e *Endpoints) shownCase(ctx context.Context, c Case) (gen.DisputeCase, error) {
	r, err := e.refs(ctx, []Case{c})
	if err != nil {
		return gen.DisputeCase{}, err
	}
	return caseToAPI(c, r), nil
}

// CreateDisputeCase implements POST /v1/dispute-cases.
func (e *Endpoints) CreateDisputeCase(ctx context.Context, req *gen.DisputeCaseCreate, _ gen.CreateDisputeCaseParams) (gen.CreateDisputeCaseRes, error) {
	c, err := e.OpenCase(ctx, CaseInput{LegalEntityID: req.LegalEntityID,
		Item:   ledger.ItemRef{FiscalYear: int16(req.FiscalYear), EntryID: req.JournalEntryID, LineNo: req.LineNo}, // the contract bounds the year to 1900-2100
		Amount: req.DisputedAmount.Amount, Currency: req.DisputedAmount.Currency, Reason: req.Reason,
		Assignee: apitypes.Ptr[uuid.UUID](req.AssigneeID), Note: apitypes.Ptr[string](req.Note)})
	if err != nil {
		return nil, err
	}
	out, err := e.shownCase(ctx, c)
	if err != nil {
		return nil, err
	}
	return &gen.DisputeCaseHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Location: gen.NewOptString("/v1/dispute-cases/" + c.ID.String()),
		Response: out}, nil
}

// GetDisputeCase implements GET /v1/dispute-cases/{id}.
func (e *Endpoints) GetDisputeCase(ctx context.Context, params gen.GetDisputeCaseParams) (gen.GetDisputeCaseRes, error) {
	c, err := GetCase(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	out, err := e.shownCase(ctx, c)
	if err != nil {
		return nil, err
	}
	return &gen.GetDisputeCaseOKHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Response: out}, nil
}

// UpdateDisputeCase implements PATCH /v1/dispute-cases/{id}.
func (e *Endpoints) UpdateDisputeCase(ctx context.Context, req *gen.DisputeCaseUpdate, params gen.UpdateDisputeCaseParams) (gen.UpdateDisputeCaseRes, error) {
	patch := CasePatch{AssigneeSet: req.AssigneeID.Set, NoteSet: req.Note.Set}
	if a := apitypes.OptNilUUID(req.AssigneeID); a != nil {
		patch.Assignee = *a
	}
	if n := apitypes.OptNilString(req.Note); n != nil {
		patch.Note = *n
	}
	c, err := e.UpdateCase(ctx, params.ID, etag.IfMatch(params.IfMatch), patch)
	if err != nil {
		return nil, err
	}
	out, err := e.shownCase(ctx, c)
	if err != nil {
		return nil, err
	}
	return &gen.UpdateDisputeCaseOKHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Response: out}, nil
}

// CloseDisputeCase implements POST /v1/dispute-cases/{id}:close.
func (e *Endpoints) CloseDisputeCase(ctx context.Context, req *gen.DisputeCaseClose, params gen.CloseDisputeCaseParams) (gen.CloseDisputeCaseRes, error) {
	c, err := e.CloseCase(ctx, params.ID, etag.IfMatch(params.IfMatch), req.Outcome, req.Reason)
	if err != nil {
		return nil, err
	}
	out, err := e.shownCase(ctx, c)
	if err != nil {
		return nil, err
	}
	return &gen.UpdateDisputeCaseOKHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Response: out}, nil
}

var caseSort = pagination.Sort{Default: "created_at", DefaultDesc: true, Fields: []pagination.Field{
	{Name: "created_at", Column: "d.created_at", Type: "timestamptz"},
}}

// ListDisputeCases implements GET /v1/dispute-cases: the cases of the
// companies the caller may act in, the latest first.
func (e *Endpoints) ListDisputeCases(ctx context.Context, params gen.ListDisputeCasesParams) (gen.ListDisputeCasesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, customer := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.CustomerID)
	assignee := apitypes.Ptr[uuid.UUID](params.AssigneeID)
	scope := fmt.Sprintf("listDisputeCases|%s|%s|%s|%s", le, customer, params.Status.Value, assignee)
	ks, err := e.Pages.Keyset(caseSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("collections: page of dispute cases: %w", err)
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, fmt.Errorf("collections: company access for dispute cases: %w", err)
	}
	q, args := ks.SQL(caseSelect+` WHERE ($1 OR d.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR d.legal_entity_id = $3)
		AND ($4::uuid IS NULL OR d.customer_id = $4) AND ($5 = '' OR d.status = $5) AND ($6::uuid IS NULL OR d.assignee_id = $6)`,
		[]any{access.All, access.IDs, le, customer, params.Status.Value, assignee})
	rows, _ := tx.Query(ctx, q, args...)
	cases, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Case, error) { return scanCase(r) })
	if err != nil {
		return nil, fmt.Errorf("collections: list dispute cases: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, cases, func(c Case) (string, uuid.UUID) { return c.CreatedAt.UTC().Format(time.RFC3339Nano), c.ID },
		func(page []Case) (caseRefs, error) { return e.refs(ctx, page) }, caseToAPI)
	if err != nil {
		return nil, err
	}
	return &gen.DisputeCasePage{Items: items, NextPageToken: next}, nil
}
