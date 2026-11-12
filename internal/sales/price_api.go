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

// priceVersion is the version of every price record: it is written once
// and never changed.
const priceVersion = 1

// priceRefs reads what the price records name by id: their companies
// and, for a customer's record, the customer.
func (e *Endpoints) priceRefs(ctx context.Context, prices []Price) (references, error) {
	docs := make([]named, len(prices))
	for i, p := range prices {
		docs[i] = named{legalEntity: p.LegalEntityID}
		if p.CustomerID != nil {
			docs[i].customer = *p.CustomerID
		}
	}
	return e.references(ctx, docs)
}

// priceAPI is a price record as the API gives it, with what it names by
// id.
func priceAPI(p Price, refs references) gen.SalesPrice {
	le := refs.companies[p.LegalEntityID]
	out := gen.SalesPrice{ID: p.ID, Version: priceVersion, CreatedAt: p.CreatedAt, CreatedBy: p.CreatedBy, UpdatedAt: p.CreatedAt,
		UpdatedBy: gen.NewOptUUID(p.CreatedBy), LegalEntityID: p.LegalEntityID, CompanyCode: le.Code, CompanyName: le.Name, ItemKey: p.ItemKey,
		CustomerID: apitypes.OptUUID(p.CustomerID), Kind: p.Kind(), ConditionType: apitypes.OptString(p.ConditionType),
		Unit: apitypes.OptString(p.Unit), PriceUnit: apitypes.OptInt32(p.PriceUnit), Percent: apitypes.OptString(p.Percent),
		ValidFrom: p.ValidFrom, ValidTo: apitypes.OptDate(p.ValidTo)}
	if p.UnitPrice != nil { // a price: its currency came with it
		out.UnitPrice = gen.NewOptMoney(apitypes.Money(*p.UnitPrice, *p.Currency, *p.Exponent))
	}
	if p.CustomerID != nil {
		customer := refs.partners[*p.CustomerID]
		out.CustomerNumber, out.CustomerName = gen.NewOptString(customer.Number), gen.NewOptString(customer.Name)
	}
	return out
}

// priceAnswer is a price record as an operation answers it.
func (e *Endpoints) priceAnswer(ctx context.Context, p Price) (gen.SalesPrice, error) {
	refs, err := e.priceRefs(ctx, []Price{p})
	if err != nil {
		return gen.SalesPrice{}, err
	}
	return priceAPI(p, refs), nil
}

// CreateSalesPrice implements POST /v1/sales-prices.
func (e *Endpoints) CreateSalesPrice(ctx context.Context, req *gen.SalesPriceCreate, _ gen.CreateSalesPriceParams) (gen.CreateSalesPriceRes, error) {
	in := PriceInput{LegalEntityID: req.LegalEntityID, ItemKey: req.ItemKey, CustomerID: apitypes.Ptr[uuid.UUID](req.CustomerID),
		ConditionType: apitypes.Ptr[string](req.ConditionType), Unit: apitypes.Ptr[string](req.Unit), PriceUnit: apitypes.Ptr[int32](req.PriceUnit),
		Percent: apitypes.Ptr[string](req.Percent), ValidFrom: req.ValidFrom, ValidTo: apitypes.Ptr[time.Time](req.ValidTo)}
	if price, ok := req.UnitPrice.Get(); ok {
		in.UnitPrice = &Amount{Value: price.Amount, Currency: price.Currency}
	}
	p, err := e.RecordPrice(ctx, in)
	if err != nil {
		return nil, err
	}
	out, err := e.priceAnswer(ctx, p)
	if err != nil {
		return nil, err
	}
	return &gen.SalesPriceHeaders{ETag: gen.NewOptString(etag.Format(priceVersion)), Location: gen.NewOptString("/v1/sales-prices/" + p.ID.String()),
		Response: out}, nil
}

// GetSalesPrice implements GET /v1/sales-prices/{id}.
func (e *Endpoints) GetSalesPrice(ctx context.Context, params gen.GetSalesPriceParams) (gen.GetSalesPriceRes, error) {
	p, err := GetPrice(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	out, err := e.priceAnswer(ctx, p)
	if err != nil {
		return nil, err
	}
	return &gen.GetSalesPriceOKHeaders{ETag: gen.NewOptString(etag.Format(priceVersion)), Response: out}, nil
}

var priceSort = pagination.Sort{Default: "item_key", Fields: []pagination.Field{
	{Name: "item_key", Column: "p.item_key", Type: "text"}, {Name: "valid_from", Column: "p.valid_from", Type: "date"},
}}

// ListSalesPrices implements GET /v1/sales-prices: the price records of
// the companies the caller may act in.
func (e *Endpoints) ListSalesPrices(ctx context.Context, params gen.ListSalesPricesParams) (gen.ListSalesPricesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, customer, validOn := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.CustomerID), apitypes.Ptr[time.Time](params.ValidOn)
	on := ""
	if validOn != nil {
		on = validOn.Format(time.DateOnly)
	}
	scope := fmt.Sprintf("listSalesPrices|%s|%q|%s|%s|%s", le, params.ItemKey.Value, customer, on, params.ConditionType.Value)
	ks, err := e.Pages.Keyset(priceSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("sales: page of prices: %w", err)
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, fmt.Errorf("sales: company access for prices: %w", err)
	}
	query, args := ks.SQL(priceSelect+` WHERE ($1 OR p.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR p.legal_entity_id = $3)
		AND ($4 = '' OR p.item_key = $4) AND ($5::uuid IS NULL OR p.customer_id = $5)
		AND ($6::date IS NULL OR (p.valid_from <= $6 AND (p.valid_to IS NULL OR p.valid_to >= $6)))
		AND ($7 = '' OR p.condition_type = $7)`,
		[]any{access.All, access.IDs, le, params.ItemKey.Value, customer, validOn, params.ConditionType.Value})
	rows, _ := tx.Query(ctx, query, args...)
	prices, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Price, error) { return scanPrice(r) })
	if err != nil {
		return nil, fmt.Errorf("sales: list prices: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, prices, func(p Price) (string, uuid.UUID) {
		if ks.Field() == "valid_from" {
			return p.ValidFrom.Format(time.DateOnly), p.ID
		}
		return p.ItemKey, p.ID
	}, func(page []Price) (references, error) { return e.priceRefs(ctx, page) }, priceAPI)
	if err != nil {
		return nil, err
	}
	return &gen.SalesPricePage{Items: items, NextPageToken: next}, nil
}
