package inventory

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// valuationAPI is a valuation as the API gives it, naming its material,
// plant and company.
func valuationAPI(v Valuation, r refs) gen.MaterialValuation {
	m, plant := r.materials[v.MaterialID], r.units[v.PlantID]
	return gen.MaterialValuation{ID: v.ID, Version: v.Version, CreatedAt: v.CreatedAt, CreatedBy: v.CreatedBy, UpdatedAt: v.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(v.UpdatedBy), MaterialID: v.MaterialID, MaterialNumber: m.Number, MaterialDescription: m.Description,
		PlantID: v.PlantID, PlantCode: plant.Code, PlantName: plant.Name, LegalEntityID: v.LegalEntityID, CompanyCode: r.companies[v.LegalEntityID].Code,
		ValuationClass: v.ValuationClass, PriceControl: v.PriceControl, Price: apitypes.Money(v.Price, v.Currency, v.Exponent), PriceUnit: v.PriceUnit,
		Quantity: gen.Quantity{Value: v.Quantity, Unit: m.BaseUnit}, Value: apitypes.Money(v.Value, v.Currency, v.Exponent)}
}

// valuationRefs reads what valuations name.
func (e *Endpoints) valuationRefs(ctx context.Context, vs []Valuation) (refs, error) {
	var ms, units, companies []uuid.UUID
	for _, v := range vs {
		ms, units, companies = append(ms, v.MaterialID), append(units, v.PlantID), append(companies, v.LegalEntityID)
	}
	return e.read(ctx, ms, units, nil, companies)
}

// oneValuation is a valuation as the API gives it, with its ETag.
func (e *Endpoints) oneValuation(ctx context.Context, v Valuation, err error) (gen.MaterialValuation, gen.OptString, error) {
	if err != nil {
		return gen.MaterialValuation{}, gen.OptString{}, err
	}
	r, err := e.valuationRefs(ctx, []Valuation{v})
	if err != nil {
		return gen.MaterialValuation{}, gen.OptString{}, err
	}
	return valuationAPI(v, r), gen.NewOptString(etag.Format(v.Version)), nil
}

// CreateMaterialValuation implements POST /v1/material-valuations.
func (e *Endpoints) CreateMaterialValuation(ctx context.Context, req *gen.MaterialValuationCreate, _ gen.CreateMaterialValuationParams) (gen.CreateMaterialValuationRes, error) {
	v, err := e.CreateValuation(ctx, ValuationInput{MaterialID: req.MaterialID, PlantID: req.PlantID, ValuationClass: req.ValuationClass,
		PriceControl: req.PriceControl, Price: req.Price.Amount, Currency: req.Price.Currency, PriceUnit: req.PriceUnit.Or(1)})
	out, tag, err := e.oneValuation(ctx, v, err)
	if err != nil {
		return nil, err
	}
	return &gen.MaterialValuationHeaders{ETag: tag, Location: gen.NewOptString("/v1/material-valuations/" + v.ID.String()), Response: out}, nil
}

// GetMaterialValuation implements GET /v1/material-valuations/{id}.
func (e *Endpoints) GetMaterialValuation(ctx context.Context, params gen.GetMaterialValuationParams) (gen.GetMaterialValuationRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	v, err := loadValuation(ctx, tx, params.ID, false)
	out, tag, err := e.oneValuation(ctx, v, err)
	if err != nil {
		return nil, err
	}
	return &gen.GetMaterialValuationOKHeaders{ETag: tag, Response: out}, nil
}

// UpdateMaterialValuation implements PATCH /v1/material-valuations/{id}.
func (e *Endpoints) UpdateMaterialValuation(ctx context.Context, req *gen.MaterialValuationUpdate, params gen.UpdateMaterialValuationParams) (gen.UpdateMaterialValuationRes, error) {
	v, err := e.ChangeValuationClass(ctx, params.ID, etag.IfMatch(params.IfMatch), req.ValuationClass)
	out, tag, err := e.oneValuation(ctx, v, err)
	if err != nil {
		return nil, err
	}
	return &gen.GetMaterialValuationOKHeaders{ETag: tag, Response: out}, nil
}

// ChangeMaterialPrice implements POST /v1/material-valuations/{id}:changePrice.
func (e *Endpoints) ChangeMaterialPrice(ctx context.Context, req *gen.MaterialPriceChange, params gen.ChangeMaterialPriceParams) (gen.ChangeMaterialPriceRes, error) {
	v, err := e.ChangePrice(ctx, params.ID, etag.IfMatch(params.IfMatch), req.Price.Amount, req.Price.Currency)
	out, tag, err := e.oneValuation(ctx, v, err)
	if err != nil {
		return nil, err
	}
	return &gen.ChangeMaterialPriceOKHeaders{ETag: tag, Response: out}, nil
}

var valuationSort = pagination.Sort{Default: "created_at", Fields: []pagination.Field{{Name: "created_at", Column: "created_at", Type: "timestamptz"}}}

// ListMaterialValuations implements GET /v1/material-valuations: the
// valuations in the plants of the companies the caller may act in, in
// the order they were created, filtered by material and plant.
func (e *Endpoints) ListMaterialValuations(ctx context.Context, params gen.ListMaterialValuationsParams) (gen.ListMaterialValuationsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	material, plant := apitypes.Ptr[uuid.UUID](params.MaterialID), apitypes.Ptr[uuid.UUID](params.PlantID)
	ks, err := e.Pages.Keyset(valuationSort, fmt.Sprintf("listMaterialValuations|%s|%s", material, plant),
		apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("inventory: page of valuations: %w", err)
	}
	a, err := access(ctx)
	if err != nil {
		return nil, err
	}
	// The valuations' columns have names of their own, so the page's
	// condition and order name them.
	query, args := ks.SQL(`SELECT * FROM (`+valuationSelect+` WHERE ($1 OR v.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR v.material_id = $3)
		AND ($4::uuid IS NULL OR v.plant_id = $4)) x WHERE true`, []any{a.All, a.IDs, material, plant})
	rows, _ := tx.Query(ctx, query, args...)
	vs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Valuation, error) { return scanValuation(r) })
	if err != nil {
		return nil, fmt.Errorf("inventory: list valuations: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, vs, func(v Valuation) (string, uuid.UUID) { return pagination.Instant(v.CreatedAt), v.ID },
		func(page []Valuation) (refs, error) { return e.valuationRefs(ctx, page) }, valuationAPI)
	if err != nil {
		return nil, err
	}
	return &gen.MaterialValuationPage{Items: items, NextPageToken: next}, nil
}
