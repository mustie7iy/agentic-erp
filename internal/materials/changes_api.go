package materials

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
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// changeRefs is what material changes name by id, as their consumer reads
// it: their materials by number and description and the plants they ask
// for by code.
type changeRefs struct {
	materials map[uuid.UUID]Material
	plants    map[uuid.UUID]tenancy.Unit
}

// changeReferences reads the changes' materials and plants, in one read
// each.
func (e *Endpoints) changeReferences(ctx context.Context, tx pgx.Tx, cs []MaterialChange) (changeRefs, error) {
	ids, plants := make([]uuid.UUID, len(cs)), []uuid.UUID{}
	for i, c := range cs {
		ids[i] = c.MaterialID
		if c.Requested.Plants != nil {
			for _, p := range *c.Requested.Plants {
				plants = append(plants, p.PlantID)
			}
		}
	}
	rows, _ := tx.Query(ctx, materialSelect+` WHERE m.id = ANY($1)`, ids)
	refs := changeRefs{materials: map[uuid.UUID]Material{}}
	if _, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Material, error) {
		m, err := scanMaterial(r)
		refs.materials[m.ID] = m
		return m, err
	}); err != nil {
		return changeRefs{}, fmt.Errorf("materials: the materials of %d changes: %w", len(cs), err)
	}
	var err error
	if refs.plants, err = e.LegalEntities.OrgUnits(ctx, plants); err != nil {
		return changeRefs{}, fmt.Errorf("materials: the plants of %d changes: %w", len(cs), err)
	}
	return refs, nil
}

// changeAPI is a material change as the API gives it.
func changeAPI(c MaterialChange, refs changeRefs) gen.MaterialChange {
	m, r := refs.materials[c.MaterialID], c.Requested
	req := gen.MaterialChangeRequested{Description: apitypes.OptString(r.Description), BaseUnit: apitypes.OptString(r.BaseUnit),
		MaterialType: apitypes.OptString(r.Type), MaterialGroup: apitypes.OptString(r.Group), Status: apitypes.OptString(r.Status)}
	if r.Plants != nil {
		req.Plants = make([]gen.MaterialChangePlant, len(*r.Plants))
		for i, p := range *r.Plants {
			req.Plants[i] = gen.MaterialChangePlant{PlantID: p.PlantID, PlantCode: refs.plants[p.PlantID].Code, LegalEntityID: p.LegalEntityID,
				StorageLocationIds: p.StorageLocations}
		}
	}
	if r.Units != nil {
		req.AlternativeUnits = make([]gen.MaterialUnitInput, len(*r.Units))
		for i, u := range *r.Units {
			req.AlternativeUnits[i] = gen.MaterialUnitInput{Unit: u.Code, Numerator: u.Numerator, Denominator: u.Denominator}
		}
	}
	dups := make([]gen.MaterialDuplicate, len(c.PossibleDuplicates))
	for i, d := range c.PossibleDuplicates {
		dups[i] = gen.MaterialDuplicate{MaterialID: d.MaterialID, Number: d.Number, Description: d.Description}
	}
	out := gen.MaterialChange{ID: c.ID, Version: c.Version, CreatedAt: c.CreatedAt, CreatedBy: c.CreatedBy, UpdatedAt: c.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(c.UpdatedBy), Number: c.Number, Kind: c.Kind, MaterialID: c.MaterialID, MaterialNumber: m.Number,
		MaterialDescription: m.Description, Status: c.Status, Requested: req, Reason: c.Reason, PossibleDuplicates: dups,
		WorkflowRequestID: apitypes.OptUUID(c.WorkflowRequestID), RequestedBy: c.RequestedBy}
	if c.AppliedAt != nil {
		out.AppliedAt = gen.NewOptDateTime(*c.AppliedAt)
	}
	return out
}

// GetMaterialChange implements GET /v1/material-changes/{id}.
func (e *Endpoints) GetMaterialChange(ctx context.Context, params gen.GetMaterialChangeParams) (gen.GetMaterialChangeRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	c, err := getChange(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	refs, err := e.changeReferences(ctx, tx, []MaterialChange{c})
	if err != nil {
		return nil, err
	}
	return &gen.MaterialChangeHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Response: changeAPI(c, refs)}, nil
}

var changeSort = pagination.Sort{Default: "created_at", DefaultDesc: true, Fields: []pagination.Field{{Name: "created_at", Column: "created_at", Type: "timestamptz"}}}

// ListMaterialChanges implements GET /v1/material-changes: the tenant's
// material changes, newest first, filtered by material, kind and status.
func (e *Endpoints) ListMaterialChanges(ctx context.Context, params gen.ListMaterialChangesParams) (gen.ListMaterialChangesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	material := apitypes.Ptr[uuid.UUID](params.MaterialID)
	scope := fmt.Sprintf("listMaterialChanges|%s|%s|%s", material, params.Kind.Value, params.Status.Value)
	ks, err := e.Pages.Keyset(changeSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("materials: page of material changes: %w", err)
	}
	query, args := ks.SQL(`SELECT `+changeColumns+` FROM materials.material_change WHERE ($1::uuid IS NULL OR material_id = $1)
		AND ($2 = '' OR kind = $2) AND ($3 = '' OR status = $3)`, []any{material, params.Kind.Value, params.Status.Value})
	rows, _ := tx.Query(ctx, query, args...)
	cs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (MaterialChange, error) { return scanChange(r) })
	if err != nil {
		return nil, fmt.Errorf("materials: list material changes: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, cs, func(c MaterialChange) (string, uuid.UUID) { return pagination.Instant(c.CreatedAt), c.ID },
		func(page []MaterialChange) (changeRefs, error) { return e.changeReferences(ctx, tx, page) }, changeAPI)
	if err != nil {
		return nil, err
	}
	return &gen.MaterialChangePage{Items: items, NextPageToken: next}, nil
}
