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

// indicatorAPI is an indicator as the API gives it.
func indicatorAPI(i Indicator, r refs) gen.CycleCountIndicator {
	plant := r.units[i.PlantID]
	return gen.CycleCountIndicator{ID: i.ID, Version: i.Version, CreatedAt: i.CreatedAt, CreatedBy: i.CreatedBy, UpdatedAt: i.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(i.UpdatedBy), PlantID: i.PlantID, PlantCode: plant.Code, PlantName: plant.Name, LegalEntityID: i.LegalEntityID,
		CompanyCode: r.companies[i.LegalEntityID].Code, Code: i.Code, IntervalDays: i.IntervalDays}
}

// indicatorRefs reads what indicators name.
func (e *Endpoints) indicatorRefs(ctx context.Context, is []Indicator) (refs, error) {
	var units, companies []uuid.UUID
	for _, i := range is {
		units, companies = append(units, i.PlantID), append(companies, i.LegalEntityID)
	}
	return e.read(ctx, nil, units, nil, companies)
}

// oneIndicator is an indicator as the API gives it, with its ETag.
func (e *Endpoints) oneIndicator(ctx context.Context, i Indicator, err error) (gen.CycleCountIndicator, gen.OptString, error) {
	if err != nil {
		return gen.CycleCountIndicator{}, gen.OptString{}, err
	}
	r, err := e.indicatorRefs(ctx, []Indicator{i})
	if err != nil {
		return gen.CycleCountIndicator{}, gen.OptString{}, err
	}
	return indicatorAPI(i, r), gen.NewOptString(etag.Format(i.Version)), nil
}

// CreateCycleCountIndicator implements POST /v1/cycle-count-indicators.
func (e *Endpoints) CreateCycleCountIndicator(ctx context.Context, req *gen.CycleCountIndicatorCreate,
	_ gen.CreateCycleCountIndicatorParams) (gen.CreateCycleCountIndicatorRes, error) {
	i, err := e.CreateIndicator(ctx, IndicatorInput{PlantID: req.PlantID, Code: req.Code, IntervalDays: req.IntervalDays})
	out, tag, err := e.oneIndicator(ctx, i, err)
	if err != nil {
		return nil, err
	}
	return &gen.CycleCountIndicatorHeaders{ETag: tag, Location: gen.NewOptString("/v1/cycle-count-indicators/" + i.ID.String()), Response: out}, nil
}

// GetCycleCountIndicator implements GET /v1/cycle-count-indicators/{id}.
func (e *Endpoints) GetCycleCountIndicator(ctx context.Context, params gen.GetCycleCountIndicatorParams) (gen.GetCycleCountIndicatorRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	i, err := loadIndicator(ctx, tx, params.ID, false)
	out, tag, err := e.oneIndicator(ctx, i, err)
	if err != nil {
		return nil, err
	}
	return &gen.GetCycleCountIndicatorOKHeaders{ETag: tag, Response: out}, nil
}

// UpdateCycleCountIndicator implements PATCH /v1/cycle-count-indicators/{id}.
func (e *Endpoints) UpdateCycleCountIndicator(ctx context.Context, req *gen.CycleCountIndicatorUpdate,
	params gen.UpdateCycleCountIndicatorParams) (gen.UpdateCycleCountIndicatorRes, error) {
	// The contract's minProperties sets the one property it has.
	i, err := e.ChangeIndicator(ctx, params.ID, etag.IfMatch(params.IfMatch), req.IntervalDays.Value)
	out, tag, err := e.oneIndicator(ctx, i, err)
	if err != nil {
		return nil, err
	}
	return &gen.GetCycleCountIndicatorOKHeaders{ETag: tag, Response: out}, nil
}

var cycleSort = pagination.Sort{Default: "created_at", Fields: []pagination.Field{{Name: "created_at", Column: "created_at", Type: "timestamptz"}}}

// ListCycleCountIndicators implements GET /v1/cycle-count-indicators: the
// indicators of the plants of the companies the caller may act in, in the
// order they were created, filtered by plant and code.
func (e *Endpoints) ListCycleCountIndicators(ctx context.Context, params gen.ListCycleCountIndicatorsParams) (gen.ListCycleCountIndicatorsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	plant := apitypes.Ptr[uuid.UUID](params.PlantID)
	ks, err := e.Pages.Keyset(cycleSort, fmt.Sprintf("listCycleCountIndicators|%s|%s", plant, params.Code.Value),
		apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("inventory: page of cycle count indicators: %w", err)
	}
	a, err := access(ctx)
	if err != nil {
		return nil, err
	}
	query, args := ks.SQL(indicatorSelect+` WHERE ($1 OR legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR plant_id = $3) AND ($4 = '' OR code = $4)`,
		[]any{a.All, a.IDs, plant, params.Code.Value})
	rows, _ := tx.Query(ctx, query, args...)
	is, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Indicator, error) { return scanIndicator(r) })
	if err != nil {
		return nil, fmt.Errorf("inventory: list cycle count indicators: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, is, func(i Indicator) (string, uuid.UUID) { return pagination.Instant(i.CreatedAt), i.ID },
		func(page []Indicator) (refs, error) { return e.indicatorRefs(ctx, page) }, indicatorAPI)
	if err != nil {
		return nil, err
	}
	return &gen.CycleCountIndicatorPage{Items: items, NextPageToken: next}, nil
}

// cycleMaterialAPI is a material's indicator as the API gives it.
func cycleMaterialAPI(c CycleMaterial, r refs) gen.CycleCountMaterial {
	plant, m := r.units[c.PlantID], r.materials[c.MaterialID]
	return gen.CycleCountMaterial{ID: c.ID, Version: c.Version, CreatedAt: c.CreatedAt, CreatedBy: c.CreatedBy, UpdatedAt: c.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(c.UpdatedBy), PlantID: c.PlantID, PlantCode: plant.Code, PlantName: plant.Name, LegalEntityID: c.LegalEntityID,
		CompanyCode: r.companies[c.LegalEntityID].Code, MaterialID: c.MaterialID, MaterialNumber: m.Number, MaterialDescription: m.Description,
		IndicatorID: c.IndicatorID, IndicatorCode: c.IndicatorCode, IntervalDays: c.IntervalDays}
}

// cycleMaterialRefs reads what materials' indicators name.
func (e *Endpoints) cycleMaterialRefs(ctx context.Context, cs []CycleMaterial) (refs, error) {
	var ms, units, companies []uuid.UUID
	for _, c := range cs {
		ms, units, companies = append(ms, c.MaterialID), append(units, c.PlantID), append(companies, c.LegalEntityID)
	}
	return e.read(ctx, ms, units, nil, companies)
}

// oneCycleMaterial is a material's indicator as the API gives it, with
// its ETag.
func (e *Endpoints) oneCycleMaterial(ctx context.Context, c CycleMaterial, err error) (gen.CycleCountMaterial, gen.OptString, error) {
	if err != nil {
		return gen.CycleCountMaterial{}, gen.OptString{}, err
	}
	r, err := e.cycleMaterialRefs(ctx, []CycleMaterial{c})
	if err != nil {
		return gen.CycleCountMaterial{}, gen.OptString{}, err
	}
	return cycleMaterialAPI(c, r), gen.NewOptString(etag.Format(c.Version)), nil
}

// CreateCycleCountMaterial implements POST /v1/cycle-count-materials.
func (e *Endpoints) CreateCycleCountMaterial(ctx context.Context, req *gen.CycleCountMaterialCreate,
	_ gen.CreateCycleCountMaterialParams) (gen.CreateCycleCountMaterialRes, error) {
	c, err := e.CreateCycleMaterial(ctx, CycleMaterialInput{MaterialID: req.MaterialID, IndicatorID: req.IndicatorID})
	out, tag, err := e.oneCycleMaterial(ctx, c, err)
	if err != nil {
		return nil, err
	}
	return &gen.CycleCountMaterialHeaders{ETag: tag, Location: gen.NewOptString("/v1/cycle-count-materials/" + c.ID.String()), Response: out}, nil
}

// GetCycleCountMaterial implements GET /v1/cycle-count-materials/{id}.
func (e *Endpoints) GetCycleCountMaterial(ctx context.Context, params gen.GetCycleCountMaterialParams) (gen.GetCycleCountMaterialRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	c, err := loadCycleMaterial(ctx, tx, params.ID, false)
	out, tag, err := e.oneCycleMaterial(ctx, c, err)
	if err != nil {
		return nil, err
	}
	return &gen.GetCycleCountMaterialOKHeaders{ETag: tag, Response: out}, nil
}

// UpdateCycleCountMaterial implements PATCH /v1/cycle-count-materials/{id}.
func (e *Endpoints) UpdateCycleCountMaterial(ctx context.Context, req *gen.CycleCountMaterialUpdate,
	params gen.UpdateCycleCountMaterialParams) (gen.UpdateCycleCountMaterialRes, error) {
	// The contract's minProperties sets the one property it has.
	c, err := e.ChangeCycleMaterial(ctx, params.ID, etag.IfMatch(params.IfMatch), req.IndicatorID.Value)
	out, tag, err := e.oneCycleMaterial(ctx, c, err)
	if err != nil {
		return nil, err
	}
	return &gen.GetCycleCountMaterialOKHeaders{ETag: tag, Response: out}, nil
}

// ListCycleCountMaterials implements GET /v1/cycle-count-materials: the
// materials' indicators at the plants of the companies the caller may act
// in, in the order they were created, filtered by plant, material and
// indicator.
func (e *Endpoints) ListCycleCountMaterials(ctx context.Context, params gen.ListCycleCountMaterialsParams) (gen.ListCycleCountMaterialsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	plant, material, indicator := apitypes.Ptr[uuid.UUID](params.PlantID), apitypes.Ptr[uuid.UUID](params.MaterialID), apitypes.Ptr[uuid.UUID](params.IndicatorID)
	ks, err := e.Pages.Keyset(cycleSort, fmt.Sprintf("listCycleCountMaterials|%s|%s|%s", plant, material, indicator),
		apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("inventory: page of material cycle count indicators: %w", err)
	}
	a, err := access(ctx)
	if err != nil {
		return nil, err
	}
	query, args := ks.SQL(`SELECT * FROM (`+cycleMaterialSelect+` WHERE ($1 OR m.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR m.plant_id = $3)
		AND ($4::uuid IS NULL OR m.material_id = $4) AND ($5::uuid IS NULL OR m.indicator_id = $5)) m WHERE true`,
		[]any{a.All, a.IDs, plant, material, indicator})
	rows, _ := tx.Query(ctx, query, args...)
	cs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (CycleMaterial, error) { return scanCycleMaterial(r) })
	if err != nil {
		return nil, fmt.Errorf("inventory: list material cycle count indicators: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, cs, func(c CycleMaterial) (string, uuid.UUID) { return pagination.Instant(c.CreatedAt), c.ID },
		func(page []CycleMaterial) (refs, error) { return e.cycleMaterialRefs(ctx, page) }, cycleMaterialAPI)
	if err != nil {
		return nil, err
	}
	return &gen.CycleCountMaterialPage{Items: items, NextPageToken: next}, nil
}

// dueAPI is a count due as the API gives it.
func dueAPI(d Due, r refs) gen.CycleCountDue {
	plant, loc, m := r.units[d.PlantID], r.units[d.StorageLocationID], r.materials[d.MaterialID]
	return gen.CycleCountDue{LegalEntityID: d.LegalEntityID, CompanyCode: r.companies[d.LegalEntityID].Code, PlantID: d.PlantID, PlantCode: plant.Code,
		StorageLocationID: d.StorageLocationID, StorageLocationCode: loc.Code, StorageLocationName: loc.Name, MaterialID: d.MaterialID,
		MaterialNumber: m.Number, MaterialDescription: m.Description, IndicatorID: d.IndicatorID, IndicatorCode: d.IndicatorCode,
		IntervalDays: d.IntervalDays, LastCountID: apitypes.OptUUID(d.LastCountID), LastCountNumber: apitypes.OptString(d.LastCountNumber),
		LastCountedOn: apitypes.OptDate(d.LastCountedOn), DueOn: apitypes.OptDate(d.DueOn)}
}

// dueRefs reads what counts due name.
func (e *Endpoints) dueRefs(ctx context.Context, ds []Due) (refs, error) {
	var ms, units, companies []uuid.UUID
	for _, d := range ds {
		ms, units, companies = append(ms, d.MaterialID), append(units, d.PlantID, d.StorageLocationID), append(companies, d.LegalEntityID)
	}
	return e.read(ctx, ms, units, nil, companies)
}

// dueSort pages the counts due by the day they fell due, never counted
// first (dueSelect's due_key).
var dueSort = pagination.Sort{Default: "due_on", Fields: []pagination.Field{{Name: "due_on", Column: "due_key", Type: "date"}}}

// ListCycleCountsDue implements GET /v1/cycle-counts-due: the counts due
// at an active plant of a company the caller may act in on its company's
// date today, filtered by storage location and indicator.
func (e *Endpoints) ListCycleCountsDue(ctx context.Context, params gen.ListCycleCountsDueParams) (gen.ListCycleCountsDueRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	loc, indicator := apitypes.Ptr[uuid.UUID](params.StorageLocationID), apitypes.Ptr[uuid.UUID](params.IndicatorID)
	ks, err := e.Pages.Keyset(dueSort, fmt.Sprintf("listCycleCountsDue|%s|%s|%s", params.PlantID, loc, indicator),
		apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("inventory: page of counts due: %w", err)
	}
	r, err := e.read(ctx, nil, []uuid.UUID{params.PlantID}, nil, nil)
	if err != nil {
		return nil, err
	}
	plant, err := r.plant(params.PlantID)
	if err != nil {
		return nil, err
	}
	c, err := e.companyOf(ctx, plant.LegalEntityID)
	if err != nil {
		return nil, err
	}
	query, args := ks.SQL(dueSelect, []any{plant.ID, loc, indicator, c.day.Date})
	rows, _ := tx.Query(ctx, query, args...)
	ds, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Due, error) { return scanDue(r) })
	if err != nil {
		return nil, fmt.Errorf("inventory: counts due at plant %s: %w", plant.Code, err)
	}
	items, next, err := apitypes.PageWith(ks, ds, func(d Due) (string, uuid.UUID) { return d.dueKey(), d.StockID },
		func(page []Due) (refs, error) { return e.dueRefs(ctx, page) }, dueAPI)
	if err != nil {
		return nil, err
	}
	return &gen.CycleCountDuePage{Items: items, NextPageToken: next}, nil
}
