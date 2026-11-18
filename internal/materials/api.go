package materials

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// references is what materials name by id, as their consumer reads it
// (REVIEW.md, API-consumer classes): their companies by code and their
// plants and storage locations by code and name; access is the companies
// whose plants the caller sees.
type references struct {
	companies map[uuid.UUID]tenancy.Entity
	units     map[uuid.UUID]tenancy.Unit
	access    authz.Access
}

// references reads the companies the materials and their visible plants
// name, once each, and the plants and storage locations in one read.
func (e *Endpoints) references(ctx context.Context, ms []Material) (references, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return references{}, fmt.Errorf("materials: company access: %w", err)
	}
	companies, ids := []uuid.UUID{}, []uuid.UUID{}
	for _, m := range ms {
		companies = append(companies, m.LegalEntityID)
		for _, p := range m.Plants {
			if access.Allows(p.LegalEntityID) {
				companies, ids = append(companies, p.LegalEntityID), append(append(ids, p.PlantID), p.StorageLocations...)
			}
		}
	}
	refs := references{access: access}
	if refs.companies, refs.units, err = e.lookup(ctx, companies, ids); err != nil {
		return references{}, err
	}
	return refs, nil
}

// materialAPI is a material as the API gives it: its plants in the
// companies the caller may act in, by plant code, each with its storage
// locations by code.
func materialAPI(m Material, refs references) gen.Material {
	out := gen.Material{ID: m.ID, Version: m.Version, CreatedAt: m.CreatedAt, CreatedBy: m.CreatedBy, UpdatedAt: m.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(m.UpdatedBy), Number: m.Number, LegalEntityID: m.LegalEntityID, CompanyCode: refs.companies[m.LegalEntityID].Code,
		Description: m.Description, BaseUnit: m.BaseUnit, BaseUnitName: m.BaseUnitName, MaterialType: m.Type, MaterialGroup: m.Group, Status: m.Status,
		Plants: []gen.MaterialPlant{}, AlternativeUnits: make([]gen.MaterialUnit, len(m.Units)), PendingChangeID: apitypes.OptUUID(m.PendingChangeID),
		PendingChangeNumber: apitypes.OptString(m.PendingChangeNumber)}
	for _, p := range m.Plants {
		if !refs.access.Allows(p.LegalEntityID) {
			continue
		}
		plant := refs.units[p.PlantID]
		row := gen.MaterialPlant{PlantID: p.PlantID, PlantCode: plant.Code, PlantName: plant.Name, LegalEntityID: p.LegalEntityID,
			CompanyCode: refs.companies[p.LegalEntityID].Code, StorageLocations: make([]gen.MaterialStorageLocation, len(p.StorageLocations))}
		for i, s := range p.StorageLocations {
			l := refs.units[s]
			row.StorageLocations[i] = gen.MaterialStorageLocation{StorageLocationID: s, Code: l.Code, Name: l.Name}
		}
		slices.SortFunc(row.StorageLocations, func(a, b gen.MaterialStorageLocation) int { return cmp.Compare(a.Code, b.Code) })
		out.Plants = append(out.Plants, row)
	}
	slices.SortFunc(out.Plants, func(a, b gen.MaterialPlant) int {
		return cmp.Or(cmp.Compare(a.PlantCode, b.PlantCode), cmp.Compare(a.CompanyCode, b.CompanyCode))
	})
	for i, u := range m.Units {
		out.AlternativeUnits[i] = gen.MaterialUnit{Unit: u.Code, UnitName: u.Name, Numerator: u.Numerator, Denominator: u.Denominator}
	}
	return out
}

// one is a single material as the API gives it, with its ETag.
func (e *Endpoints) one(ctx context.Context, m Material, err error) (gen.Material, gen.OptString, error) {
	if err != nil {
		return gen.Material{}, gen.OptString{}, err
	}
	refs, err := e.references(ctx, []Material{m})
	if err != nil {
		return gen.Material{}, gen.OptString{}, err
	}
	return materialAPI(m, refs), gen.NewOptString(etag.Format(m.Version)), nil
}

// inputs are a request's plants and units as the context takes them.
func inputs(plants []gen.MaterialPlantInput, units []gen.MaterialUnitInput) ([]PlantInput, []UnitInput) {
	ps, us := make([]PlantInput, len(plants)), make([]UnitInput, len(units))
	for i, p := range plants {
		ps[i] = PlantInput{PlantID: p.PlantID, StorageLocations: p.StorageLocationIds}
	}
	for i, u := range units {
		us[i] = UnitInput{Code: u.Unit, Numerator: u.Numerator, Denominator: u.Denominator}
	}
	return ps, us
}

// CreateMaterial implements POST /v1/materials.
func (e *Endpoints) CreateMaterial(ctx context.Context, req *gen.MaterialCreate, _ gen.CreateMaterialParams) (gen.CreateMaterialRes, error) {
	plants, units := inputs(req.Plants, req.AlternativeUnits)
	m, err := e.Create(ctx, Input{LegalEntityID: req.LegalEntityID, Description: req.Description, BaseUnit: req.BaseUnit, Type: req.MaterialType,
		Group: req.MaterialGroup, Plants: plants, Units: units, Reason: req.Reason.Value, ConfirmDuplicate: req.ConfirmDuplicate.Value})
	out, tag, err := e.one(ctx, m, err)
	if err != nil {
		return nil, err
	}
	return &gen.MaterialHeaders{ETag: tag, Location: gen.NewOptString("/v1/materials/" + m.ID.String()), Response: out}, nil
}

// GetMaterial implements GET /v1/materials/{id}.
func (e *Endpoints) GetMaterial(ctx context.Context, params gen.GetMaterialParams) (gen.GetMaterialRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	m, err := load(ctx, tx, params.ID, false)
	out, tag, err := e.one(ctx, m, err)
	if err != nil {
		return nil, err
	}
	return &gen.GetMaterialOKHeaders{ETag: tag, Response: out}, nil
}

// UpdateMaterial implements PATCH /v1/materials/{id}.
func (e *Endpoints) UpdateMaterial(ctx context.Context, req *gen.MaterialUpdate, params gen.UpdateMaterialParams) (gen.UpdateMaterialRes, error) {
	ch := Change{Description: apitypes.Ptr[string](req.Description), Group: apitypes.Ptr[string](req.MaterialGroup),
		Status: apitypes.Ptr[string](req.Status)}
	plants, units := inputs(req.Plants, req.AlternativeUnits)
	if req.Plants != nil { // absent from the patch: nil; given, even empty: a list
		ch.Plants = &plants
	}
	if req.AlternativeUnits != nil {
		ch.Units = &units
	}
	done, err := e.Change(ctx, params.ID, etag.IfMatch(params.IfMatch), ch, req.Reason.Value)
	out, tag, err := e.one(ctx, done.Material, err)
	if err != nil {
		return nil, err
	}
	return &gen.UpdateMaterialOKHeaders{ETag: tag, Response: out}, nil
}

var materialSort = pagination.Sort{Default: "number", Fields: []pagination.Field{{Name: "number", Column: "m.number", Type: "text"}}}

// ListMaterials implements GET /v1/materials: the tenant's materials by
// number, filtered by number, plant (one it has, not removed), status,
// type and group.
func (e *Endpoints) ListMaterials(ctx context.Context, params gen.ListMaterialsParams) (gen.ListMaterialsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	plant := apitypes.Ptr[uuid.UUID](params.PlantID)
	scope := fmt.Sprintf("listMaterials|%s|%s|%s|%s|%s", params.Number.Value, plant, params.Status.Value, params.MaterialType.Value,
		params.MaterialGroup.Value)
	ks, err := e.Pages.Keyset(materialSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("materials: page of materials: %w", err)
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, fmt.Errorf("materials: company access for materials: %w", err)
	}
	// A plant of a company the caller may not act in matches none, as
	// materialAPI does not show it.
	query, args := ks.SQL(materialSelect+` WHERE ($1 = '' OR m.number = $1) AND ($2::uuid IS NULL OR EXISTS (SELECT FROM materials.plant_data d
			WHERE d.tenant_id = m.tenant_id AND d.material_id = m.id AND d.plant_id = $2 AND d.removed_at IS NULL AND ($6 OR d.legal_entity_id = ANY($7))))
		AND ($3 = '' OR m.status = $3) AND ($4 = '' OR m.material_type = $4) AND ($5 = '' OR m.material_group = $5)`,
		[]any{params.Number.Value, plant, params.Status.Value, params.MaterialType.Value, params.MaterialGroup.Value, access.All, access.IDs})
	rows, _ := tx.Query(ctx, query, args...)
	ms, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Material, error) { return scanMaterial(r) })
	if err != nil {
		return nil, fmt.Errorf("materials: list materials: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, ms, func(m Material) (string, uuid.UUID) { return m.Number, m.ID },
		func(page []Material) (references, error) { return e.references(ctx, page) }, materialAPI)
	if err != nil {
		return nil, err
	}
	return &gen.MaterialPage{Items: items, NextPageToken: next}, nil
}
