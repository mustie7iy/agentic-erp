package credit

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// shown is what limits are shown with: their companies' codes and names
// and their customers' names, what a limit names by id, as its consumer
// reads it (REVIEW.md, API-consumer classes).
type shown struct {
	companies map[uuid.UUID]tenancy.Entity
	customers map[uuid.UUID]partner.Named
}

// shown reads what the limits are shown with: each company once, the
// customers in one call.
func (e *Endpoints) shown(ctx context.Context, limits []Limit) (shown, error) {
	s := shown{companies: map[uuid.UUID]tenancy.Entity{}}
	customers := make([]uuid.UUID, len(limits))
	for i, l := range limits {
		customers[i] = l.CustomerID
		if _, ok := s.companies[l.LegalEntityID]; ok {
			continue
		}
		le, _, err := e.LegalEntities.Entity(ctx, l.LegalEntityID) // a stored limit's legal entity exists
		if err != nil {
			return shown{}, fmt.Errorf("credit: company of credit limit %s: %w", l.ID, err)
		}
		s.companies[l.LegalEntityID] = le
	}
	var err error
	if s.customers, err = e.Partners.Names(ctx, customers); err != nil {
		return shown{}, fmt.Errorf("credit: customers of %d credit limits: %w", len(limits), err)
	}
	return s, nil
}

// limitToAPI is a limit as the API gives it, with its company's code and
// name and its customer's number and name.
func limitToAPI(l Limit, s shown) gen.CustomerCreditLimit {
	le := s.companies[l.LegalEntityID]
	return gen.CustomerCreditLimit{ID: l.ID, Version: l.Version, CreatedAt: l.CreatedAt, CreatedBy: l.CreatedBy, UpdatedAt: l.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(l.UpdatedBy), LegalEntityID: l.LegalEntityID, CompanyCode: le.Code, CompanyName: le.Name,
		CustomerID: l.CustomerID, CustomerNumber: l.CustomerNumber, CustomerName: s.customers[l.CustomerID].Name,
		Limit: apitypes.Money(l.Amount, l.Currency, l.Exponent)}
}

// one is a single limit as the API gives it.
func (e *Endpoints) one(ctx context.Context, l Limit) (gen.CustomerCreditLimit, error) {
	s, err := e.shown(ctx, []Limit{l})
	if err != nil {
		return gen.CustomerCreditLimit{}, err
	}
	return limitToAPI(l, s), nil
}

// CreateCreditLimit implements POST /v1/credit-limits.
func (e *Endpoints) CreateCreditLimit(ctx context.Context, req *gen.CustomerCreditLimitCreate, _ gen.CreateCreditLimitParams) (gen.CreateCreditLimitRes, error) {
	l, err := e.SetLimit(ctx, LimitInput{LegalEntityID: req.LegalEntityID, CustomerID: req.CustomerID, Amount: req.Limit.Amount, Currency: req.Limit.Currency})
	if err != nil {
		return nil, err
	}
	out, err := e.one(ctx, l)
	if err != nil {
		return nil, err
	}
	return &gen.CustomerCreditLimitHeaders{ETag: gen.NewOptString(etag.Format(l.Version)), Location: gen.NewOptString("/v1/credit-limits/" + l.ID.String()),
		Response: out}, nil
}

// UpdateCreditLimit implements PATCH /v1/credit-limits/{id}.
func (e *Endpoints) UpdateCreditLimit(ctx context.Context, req *gen.CustomerCreditLimitUpdate, params gen.UpdateCreditLimitParams) (gen.UpdateCreditLimitRes, error) {
	l, err := e.ChangeLimit(ctx, params.ID, etag.IfMatch(params.IfMatch), req.Limit.Amount, req.Limit.Currency)
	if err != nil {
		return nil, err
	}
	out, err := e.one(ctx, l)
	if err != nil {
		return nil, err
	}
	return &gen.UpdateCreditLimitOKHeaders{ETag: gen.NewOptString(etag.Format(l.Version)), Response: out}, nil
}

// GetCreditLimit implements GET /v1/credit-limits/{id}.
func (e *Endpoints) GetCreditLimit(ctx context.Context, params gen.GetCreditLimitParams) (gen.GetCreditLimitRes, error) {
	l, err := GetLimit(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	out, err := e.one(ctx, l)
	if err != nil {
		return nil, err
	}
	return &gen.GetCreditLimitOKHeaders{ETag: gen.NewOptString(etag.Format(l.Version)), Response: out}, nil
}

var limitSort = pagination.Sort{Default: "customer_number", Fields: []pagination.Field{
	{Name: "customer_number", Column: "l.customer_number", Type: "text"},
}}

// ListCreditLimits implements GET /v1/credit-limits: the limits of the
// companies the caller may act in, by customer number.
func (e *Endpoints) ListCreditLimits(ctx context.Context, params gen.ListCreditLimitsParams) (gen.ListCreditLimitsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, customer := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.CustomerID)
	scope := fmt.Sprintf("listCreditLimits|%s|%s", le, customer)
	ks, err := e.Pages.Keyset(limitSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("credit: page of credit limits: %w", err)
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, fmt.Errorf("credit: company access for credit limits: %w", err)
	}
	q, args := ks.SQL(limitSelect+` WHERE ($1 OR l.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR l.legal_entity_id = $3)
		AND ($4::uuid IS NULL OR l.customer_id = $4)`, []any{access.All, access.IDs, le, customer})
	rows, _ := tx.Query(ctx, q, args...)
	limits, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Limit, error) { return scanLimit(r) })
	if err != nil {
		return nil, fmt.Errorf("credit: list credit limits: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, limits, func(l Limit) (string, uuid.UUID) { return l.CustomerNumber, l.ID },
		func(page []Limit) (shown, error) { return e.shown(ctx, page) }, limitToAPI)
	if err != nil {
		return nil, err
	}
	return &gen.CustomerCreditLimitPage{Items: items, NextPageToken: next}, nil
}

// GetCreditExposure implements GET /v1/credit-exposure: what a customer
// owes a company, its limit there and whether an amount is within it.
func (e *Endpoints) GetCreditExposure(ctx context.Context, params gen.GetCreditExposureParams) (gen.GetCreditExposureRes, error) {
	x, err := e.ExposureOf(ctx, params.LegalEntityID, params.CustomerID, apitypes.Ptr[string](params.Amount))
	if err != nil {
		return nil, err
	}
	amount := func(m money.Minor) gen.Money { return apitypes.Money(m, x.Company.LocalCurrency, x.Exponent) }
	out := &gen.CreditExposure{LegalEntityID: x.Company.ID, CompanyCode: x.Company.Code, CompanyName: x.Company.Name, CustomerID: params.CustomerID,
		CustomerNumber: x.Customer.Number, CustomerName: x.Customer.Name, Currency: x.Company.LocalCurrency, OpenItemCount: x.Items,
		Exposure: amount(x.Owed), HasLimit: x.Limit != nil, IsWithinLimit: x.Within}
	if x.Limit != nil {
		out.CreditLimitID.SetTo(x.Limit.ID)
		out.Limit.SetTo(amount(x.Limit.Amount))
		out.Available.SetTo(amount(x.Available))
	}
	if x.Checked != nil {
		out.Amount.SetTo(amount(*x.Checked))
	}
	return out, nil
}
