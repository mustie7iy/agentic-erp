package inventory

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// abcAPI is an analysis as the API gives it.
func abcAPI(a ABCAnalysis) *gen.CycleCountAbcAnalysis {
	amount := func(m money.Minor) gen.Money { return apitypes.Money(m, a.Currency, a.Exponent) }
	out := &gen.CycleCountAbcAnalysis{LegalEntityID: a.Plant.LegalEntityID, CompanyCode: a.materials.companies[a.Plant.LegalEntityID].Code,
		PlantID: a.Plant.ID, PlantCode: a.Plant.Code, PlantName: a.Plant.Name, From: a.From, To: a.To, ShareA: int32(a.Shares.A),
		ShareB: int32(a.Shares.B), TotalConsumption: amount(a.Total), ExcludedMaterials: a.ExcludedMaterials, ExcludedConsumption: amount(a.Excluded),
		Classes: make([]gen.CycleCountAbcClass, len(a.Classes)), Lines: make([]gen.CycleCountAbcLine, len(a.Lines)), Changes: a.Changes,
		PreviewToken: a.Token}
	for i, c := range a.Classes {
		out.Classes[i] = gen.CycleCountAbcClass{Class: c.Class, Materials: c.Materials, Consumption: amount(c.Consumption)}
		if c.Indicator != nil {
			out.Classes[i].IndicatorID, out.Classes[i].IndicatorCode = gen.NewOptUUID(c.Indicator.ID), gen.NewOptString(c.Indicator.Code)
			out.Classes[i].IntervalDays = gen.NewOptInt32(c.Indicator.IntervalDays)
		}
	}
	for i, l := range a.Lines {
		m := a.materials.materials[l.MaterialID]
		line := gen.CycleCountAbcLine{MaterialID: l.MaterialID, MaterialNumber: m.Number, MaterialDescription: m.Description, Consumption: amount(l.Consumption),
			Class: l.Class, IndicatorID: l.Indicator.ID, IndicatorCode: l.Indicator.Code, IntervalDays: l.Indicator.IntervalDays, Change: l.Change}
		if l.Before != nil {
			line.BeforeIndicatorID, line.BeforeIndicatorCode = gen.NewOptUUID(l.Before.IndicatorID), gen.NewOptString(l.Before.IndicatorCode)
			line.CycleCountMaterialID, line.CycleCountMaterialVersion = gen.NewOptUUID(l.Before.ID), gen.NewOptInt32(l.Before.Version)
		}
		out.Lines[i] = line
	}
	return out
}

// GetCycleCountAbcAnalysis implements GET /v1/cycle-count-abc-analysis.
func (e *Endpoints) GetCycleCountAbcAnalysis(ctx context.Context, params gen.GetCycleCountAbcAnalysisParams) (gen.GetCycleCountAbcAnalysisRes, error) {
	a, err := e.ABC(ctx, ABCInput{PlantID: params.PlantID, From: params.From, To: params.To})
	if err != nil {
		return nil, err
	}
	return abcAPI(a), nil
}

// applicationRefs are what applications name: their materials, plants
// and companies, and who applied them by name.
type applicationRefs struct {
	refs
	names map[uuid.UUID]string
}

// applicationRefs reads what the applications name.
func (e *Endpoints) applicationRefs(ctx context.Context, as []ABCApplication) (applicationRefs, error) {
	var ms, units, companies, people []uuid.UUID
	for _, a := range as {
		units, companies, people = append(units, a.PlantID), append(companies, a.LegalEntityID), append(people, a.CreatedBy)
		for _, c := range a.Changes {
			ms = append(ms, c.MaterialID)
		}
	}
	r, err := e.read(ctx, ms, units, nil, companies)
	if err != nil {
		return applicationRefs{}, err
	}
	names, err := e.People.PersonNames(ctx, people)
	if err != nil {
		return applicationRefs{}, fmt.Errorf("inventory: names of who applied %d ABC analyses: %w", len(as), err)
	}
	return applicationRefs{refs: r, names: names}, nil
}

// applicationAPI is an application as the API gives it.
func applicationAPI(a ABCApplication, r applicationRefs) gen.CycleCountAbcApplication {
	plant := r.units[a.PlantID]
	out := gen.CycleCountAbcApplication{ID: a.ID, Version: 1, LegalEntityID: a.LegalEntityID, CompanyCode: r.companies[a.LegalEntityID].Code,
		PlantID: a.PlantID, PlantCode: plant.Code, PlantName: plant.Name, From: a.From, To: a.To, ShareA: a.ShareA, ShareB: a.ShareB,
		TotalConsumption: apitypes.Money(a.Total, a.Currency, a.Exponent), PreviewToken: a.Token, Changes: make([]gen.CycleCountAbcChange, len(a.Changes)),
		CreatedAt: a.CreatedAt, CreatedBy: a.CreatedBy, CreatedByName: r.names[a.CreatedBy]}
	for i, c := range a.Changes {
		m := r.materials[c.MaterialID]
		change := ABCCreate
		if c.BeforeID != nil {
			change = ABCChange
		}
		out.Changes[i] = gen.CycleCountAbcChange{LineNo: c.LineNo, MaterialID: c.MaterialID, MaterialNumber: m.Number, MaterialDescription: m.Description,
			Consumption: apitypes.Money(c.Consumption, a.Currency, a.Exponent), Class: c.Class, IndicatorID: c.IndicatorID, IndicatorCode: c.IndicatorCode,
			Change: change, BeforeIndicatorID: apitypes.OptUUID(c.BeforeID), BeforeIndicatorCode: apitypes.OptString(c.BeforeCode),
			CycleCountMaterialID: c.CycleMaterialID}
	}
	return out
}

// oneApplication is an application as the API gives it.
func (e *Endpoints) oneApplication(ctx context.Context, a ABCApplication, err error) (gen.CycleCountAbcApplication, error) {
	if err != nil {
		return gen.CycleCountAbcApplication{}, err
	}
	r, err := e.applicationRefs(ctx, []ABCApplication{a})
	if err != nil {
		return gen.CycleCountAbcApplication{}, err
	}
	return applicationAPI(a, r), nil
}

// CreateCycleCountAbcApplication implements POST /v1/cycle-count-abc-applications.
func (e *Endpoints) CreateCycleCountAbcApplication(ctx context.Context, req *gen.CycleCountAbcApply,
	_ gen.CreateCycleCountAbcApplicationParams) (gen.CreateCycleCountAbcApplicationRes, error) {
	a, err := e.ApplyABC(ctx, ABCInput{PlantID: req.PlantID, From: req.From, To: req.To}, req.PreviewToken)
	out, err := e.oneApplication(ctx, a, err)
	if err != nil {
		return nil, err
	}
	return &gen.CycleCountAbcApplicationHeaders{ETag: gen.NewOptString(etag.Format(1)), Location: gen.NewOptString("/v1/cycle-count-abc-applications/" +
		a.ID.String()), Response: out}, nil
}

// GetCycleCountAbcApplication implements GET /v1/cycle-count-abc-applications/{id}.
func (e *Endpoints) GetCycleCountAbcApplication(ctx context.Context, params gen.GetCycleCountAbcApplicationParams) (gen.GetCycleCountAbcApplicationRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	a, err := loadApplication(ctx, tx, params.ID)
	out, err := e.oneApplication(ctx, a, err)
	if err != nil {
		return nil, err
	}
	return &gen.GetCycleCountAbcApplicationOKHeaders{ETag: gen.NewOptString(etag.Format(1)), Response: out}, nil
}

var applicationSort = pagination.Sort{Default: "created_at", DefaultDesc: true,
	Fields: []pagination.Field{{Name: "created_at", Column: "created_at", Type: "timestamptz"}}}

// ListCycleCountAbcApplications implements GET /v1/cycle-count-abc-applications:
// the applications at the plants of the companies the caller may act in,
// the latest first, filtered by plant.
func (e *Endpoints) ListCycleCountAbcApplications(ctx context.Context, params gen.ListCycleCountAbcApplicationsParams) (gen.ListCycleCountAbcApplicationsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	plant := apitypes.Ptr[uuid.UUID](params.PlantID)
	ks, err := e.Pages.Keyset(applicationSort, fmt.Sprintf("listCycleCountAbcApplications|%s", plant),
		apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("inventory: page of ABC analyses applied: %w", err)
	}
	a, err := access(ctx)
	if err != nil {
		return nil, err
	}
	query, args := ks.SQL(applicationSelect+` WHERE ($1 OR legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR plant_id = $3)`, []any{a.All, a.IDs, plant})
	rows, _ := tx.Query(ctx, query, args...)
	as, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ABCApplication, error) { return scanApplication(r) })
	if err != nil {
		return nil, fmt.Errorf("inventory: list ABC analyses applied: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, as, func(x ABCApplication) (string, uuid.UUID) { return pagination.Instant(x.CreatedAt), x.ID },
		func(page []ABCApplication) (applicationRefs, error) {
			if err := withChanges(ctx, tx, page); err != nil {
				return applicationRefs{}, err
			}
			return e.applicationRefs(ctx, page)
		}, applicationAPI)
	if err != nil {
		return nil, err
	}
	return &gen.CycleCountAbcApplicationPage{Items: items, NextPageToken: next}, nil
}
