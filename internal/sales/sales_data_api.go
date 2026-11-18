package sales

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// salesDataShown is what customers' sales data name beyond their ids:
// their companies and customers, and the active configuration's price
// groups, whose names they show.
type salesDataShown struct {
	refs   references
	groups config.Pricing
}

// salesDataShow reads what the sales data name, once for all of them.
func (e *Endpoints) salesDataShow(ctx context.Context, data []SalesData) (salesDataShown, error) {
	docs := make([]named, len(data))
	for i, d := range data {
		docs[i] = named{legalEntity: d.LegalEntityID, customer: d.CustomerID}
	}
	refs, err := e.references(ctx, docs)
	if err != nil {
		return salesDataShown{}, err
	}
	cfg, _, err := e.Settings.Document(ctx)
	if err != nil {
		return salesDataShown{}, fmt.Errorf("sales: price groups of customer sales data: %w", err)
	}
	return salesDataShown{refs: refs, groups: cfg.Pricing}, nil
}

// salesDataAPI is a customer's sales data as the API gives it, with what
// it names by id and its price group's name.
func salesDataAPI(d SalesData, s salesDataShown) gen.CustomerSalesData {
	le, customer := s.refs.companies[d.LegalEntityID], s.refs.partners[d.CustomerID]
	out := gen.CustomerSalesData{ID: d.ID, Version: d.Version, CreatedAt: d.CreatedAt, CreatedBy: d.CreatedBy, UpdatedAt: d.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(d.UpdatedBy), LegalEntityID: d.LegalEntityID, CompanyCode: le.Code, CompanyName: le.Name, CustomerID: d.CustomerID,
		CustomerNumber: customer.Number, CustomerName: customer.Name, PriceGroup: apitypes.OptString(d.PriceGroup)}
	if d.PriceGroup != nil {
		g, _ := s.groups.PriceGroup(*d.PriceGroup) // a group is never removed once activated: a stored one is configured
		out.PriceGroupName = gen.NewOptString(g.Name)
	}
	return out
}

// salesDataAnswer is a customer's sales data as an operation answers it.
func (e *Endpoints) salesDataAnswer(ctx context.Context, d SalesData) (gen.CustomerSalesData, error) {
	s, err := e.salesDataShow(ctx, []SalesData{d})
	if err != nil {
		return gen.CustomerSalesData{}, err
	}
	return salesDataAPI(d, s), nil
}

// CreateCustomerSalesData implements POST /v1/customer-sales-data.
func (e *Endpoints) CreateCustomerSalesData(ctx context.Context, req *gen.CustomerSalesDataCreate, _ gen.CreateCustomerSalesDataParams) (gen.CreateCustomerSalesDataRes, error) {
	d, err := e.SetUpSalesData(ctx, req.LegalEntityID, req.CustomerID, apitypes.Ptr[string](req.PriceGroup))
	if err != nil {
		return nil, err
	}
	out, err := e.salesDataAnswer(ctx, d)
	if err != nil {
		return nil, err
	}
	return &gen.CustomerSalesDataHeaders{ETag: gen.NewOptString(etag.Format(d.Version)),
		Location: gen.NewOptString("/v1/customer-sales-data/" + d.ID.String()), Response: out}, nil
}

// GetCustomerSalesData implements GET /v1/customer-sales-data/{id}.
func (e *Endpoints) GetCustomerSalesData(ctx context.Context, params gen.GetCustomerSalesDataParams) (gen.GetCustomerSalesDataRes, error) {
	d, err := GetSalesData(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	out, err := e.salesDataAnswer(ctx, d)
	if err != nil {
		return nil, err
	}
	return &gen.GetCustomerSalesDataOKHeaders{ETag: gen.NewOptString(etag.Format(d.Version)), Response: out}, nil
}

// UpdateCustomerSalesData implements PATCH /v1/customer-sales-data/{id}.
func (e *Endpoints) UpdateCustomerSalesData(ctx context.Context, req *gen.CustomerSalesDataUpdate, params gen.UpdateCustomerSalesDataParams) (gen.UpdateCustomerSalesDataRes, error) {
	var group *string
	if g, ok := req.PriceGroup.Get(); ok {
		group = &g
	}
	d, err := e.ChangeSalesData(ctx, params.ID, etag.IfMatch(params.IfMatch), group)
	if err != nil {
		return nil, err
	}
	out, err := e.salesDataAnswer(ctx, d)
	if err != nil {
		return nil, err
	}
	return &gen.UpdateCustomerSalesDataOKHeaders{ETag: gen.NewOptString(etag.Format(d.Version)), Response: out}, nil
}

var salesDataSort = pagination.Sort{Default: "customer_number", Fields: []pagination.Field{
	{Name: "customer_number", Column: "d.customer_number", Type: "text"},
}}

// ListCustomerSalesData implements GET /v1/customer-sales-data: the
// sales data of the companies the caller may act in, by customer number.
func (e *Endpoints) ListCustomerSalesData(ctx context.Context, params gen.ListCustomerSalesDataParams) (gen.ListCustomerSalesDataRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, customer := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.CustomerID)
	scope := fmt.Sprintf("listCustomerSalesData|%s|%s|%s", le, customer, params.PriceGroup.Value)
	ks, err := e.Pages.Keyset(salesDataSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("sales: page of customer sales data: %w", err)
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, fmt.Errorf("sales: company access for customer sales data: %w", err)
	}
	q, args := ks.SQL(salesDataSelect+` WHERE ($1 OR d.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR d.legal_entity_id = $3)
		AND ($4::uuid IS NULL OR d.customer_id = $4) AND ($5 = '' OR d.price_group = $5)`,
		[]any{access.All, access.IDs, le, customer, params.PriceGroup.Value})
	rows, _ := tx.Query(ctx, q, args...)
	data, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (SalesData, error) { return scanSalesData(r) })
	if err != nil {
		return nil, fmt.Errorf("sales: list customer sales data: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, data, func(d SalesData) (string, uuid.UUID) { return d.CustomerNumber, d.ID },
		func(page []SalesData) (salesDataShown, error) { return e.salesDataShow(ctx, page) }, salesDataAPI)
	if err != nil {
		return nil, err
	}
	return &gen.CustomerSalesDataPage{Items: items, NextPageToken: next}, nil
}
