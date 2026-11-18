package collections

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

// contactRefs reads what the contacts name by id, once each: their
// companies, customers and the collectors who recorded them.
func (e *Endpoints) contactRefs(ctx context.Context, contacts []Contact) (caseRefs, error) {
	les, customers, people := make([]uuid.UUID, len(contacts)), make([]uuid.UUID, len(contacts)), make([]uuid.UUID, len(contacts))
	for i, c := range contacts {
		les[i], customers[i], people[i] = c.LegalEntityID, c.CustomerID, c.CreatedBy
	}
	return e.named(ctx, "customer contacts", les, customers, people)
}

// contactToAPI is a contact as the API gives it: its company by code and
// name, its customer by number and name, its collector by name.
func contactToAPI(c Contact, r caseRefs) gen.CustomerContact {
	le := r.companies[c.LegalEntityID]
	return gen.CustomerContact{ID: c.ID, Version: c.Version, CreatedAt: c.CreatedAt, CreatedBy: c.CreatedBy, UpdatedAt: c.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(c.UpdatedBy), LegalEntityID: c.LegalEntityID, CompanyCode: le.Code, CompanyName: le.Name, CustomerID: c.CustomerID,
		CustomerNumber: c.CustomerNumber, CustomerName: r.customers[c.CustomerID].Name, ContactedAt: c.ContactedAt,
		ContactName: apitypes.OptString(c.ContactName), Outcome: c.Outcome, Note: apitypes.OptString(c.Note),
		ResubmissionDate: apitypes.OptDate(c.Resubmission), ContactedByName: r.people[c.CreatedBy]}
}

// shownContact is a contact as the API gives it, with what it names.
func (e *Endpoints) shownContact(ctx context.Context, c Contact) (gen.CustomerContact, error) {
	r, err := e.contactRefs(ctx, []Contact{c})
	if err != nil {
		return gen.CustomerContact{}, err
	}
	return contactToAPI(c, r), nil
}

// CreateCustomerContact implements POST /v1/customer-contacts.
func (e *Endpoints) CreateCustomerContact(ctx context.Context, req *gen.CustomerContactCreate, _ gen.CreateCustomerContactParams) (gen.CreateCustomerContactRes, error) {
	c, err := e.RecordContact(ctx, ContactInput{LegalEntityID: req.LegalEntityID, CustomerID: req.CustomerID, ContactedAt: req.ContactedAt,
		ContactName: apitypes.Ptr[string](req.ContactName), Outcome: req.Outcome, Note: apitypes.Ptr[string](req.Note),
		Resubmission: apitypes.Ptr[time.Time](req.ResubmissionDate)})
	if err != nil {
		return nil, err
	}
	out, err := e.shownContact(ctx, c)
	if err != nil {
		return nil, err
	}
	return &gen.CustomerContactHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Location: gen.NewOptString("/v1/customer-contacts/" + c.ID.String()),
		Response: out}, nil
}

// GetCustomerContact implements GET /v1/customer-contacts/{id}.
func (e *Endpoints) GetCustomerContact(ctx context.Context, params gen.GetCustomerContactParams) (gen.GetCustomerContactRes, error) {
	c, err := GetContact(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	out, err := e.shownContact(ctx, c)
	if err != nil {
		return nil, err
	}
	return &gen.GetCustomerContactOKHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Response: out}, nil
}

var contactSort = pagination.Sort{Default: "contacted_at", DefaultDesc: true, Fields: []pagination.Field{
	{Name: "contacted_at", Column: "c.contacted_at", Type: "timestamptz"},
}}

// ListCustomerContacts implements GET /v1/customer-contacts: the contacts
// of the companies the caller may act in, the latest first.
func (e *Endpoints) ListCustomerContacts(ctx context.Context, params gen.ListCustomerContactsParams) (gen.ListCustomerContactsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, customer := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.CustomerID)
	ks, err := e.Pages.Keyset(contactSort, fmt.Sprintf("listCustomerContacts|%s|%s", le, customer), apitypes.Page(params.OrderBy, params.PageSize,
		params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("collections: page of customer contacts: %w", err)
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, fmt.Errorf("collections: company access for customer contacts: %w", err)
	}
	q, args := ks.SQL(contactSelect+` WHERE ($1 OR c.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR c.legal_entity_id = $3)
		AND ($4::uuid IS NULL OR c.customer_id = $4)`, []any{access.All, access.IDs, le, customer})
	rows, _ := tx.Query(ctx, q, args...)
	contacts, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Contact, error) { return scanContact(r) })
	if err != nil {
		return nil, fmt.Errorf("collections: list customer contacts: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, contacts, func(c Contact) (string, uuid.UUID) { return pagination.Instant(c.ContactedAt), c.ID },
		func(page []Contact) (caseRefs, error) { return e.contactRefs(ctx, page) }, contactToAPI)
	if err != nil {
		return nil, err
	}
	return &gen.CustomerContactPage{Items: items, NextPageToken: next}, nil
}
