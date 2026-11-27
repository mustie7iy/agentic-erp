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
// whose plants the caller sees; unitNames are the units of measure's
// names by code, which the measures name (S1a3).
type references struct {
	companies map[uuid.UUID]tenancy.Entity
	units     map[uuid.UUID]tenancy.Unit
	access    authz.Access
	unitNames map[string]string
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
	if refs.unitNames, err = unitNames(ctx); err != nil {
		return references{}, err
	}
	return refs, nil
}

// unitNames are the units of measure's names by code: the reference
// table is a handful of rows, read whole once per response.
func unitNames(ctx context.Context) (map[string]string, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT code, name FROM ref.unit_of_measure`)
	out := map[string]string{}
	var code, name string
	if _, err := pgx.ForEachRow(rows, []any{&code, &name}, func() error {
		out[code] = name
		return nil
	}); err != nil {
		return nil, fmt.Errorf("materials: read the units of measure's names: %w", err)
	}
	return out, nil
}

// measuresAPI are a unit's measures as the API gives them, each unit with
// its name.
func measuresAPI(m Measures, names map[string]string) gen.MaterialUnitMeasures {
	opt := apitypes.OptText
	return gen.MaterialUnitMeasures{Gtin: opt(m.GTIN), GrossWeight: opt(m.GrossWeight), NetWeight: opt(m.NetWeight), WeightUnit: opt(m.WeightUnit),
		WeightUnitName: opt(names[m.WeightUnit]), Volume: opt(m.Volume), VolumeUnit: opt(m.VolumeUnit), VolumeUnitName: opt(names[m.VolumeUnit]),
		Length: opt(m.Length), Width: opt(m.Width), Height: opt(m.Height), DimensionUnit: opt(m.DimensionUnit),
		DimensionUnitName: opt(names[m.DimensionUnit])}
}

// measuresIn are a request's measures as the context takes them.
func measuresIn(m gen.MaterialMeasures) Measures {
	return Measures{GTIN: m.Gtin.Value, GrossWeight: m.GrossWeight.Value, NetWeight: m.NetWeight.Value, WeightUnit: m.WeightUnit.Value,
		Volume: m.Volume.Value, VolumeUnit: m.VolumeUnit.Value, Length: m.Length.Value, Width: m.Width.Value, Height: m.Height.Value,
		DimensionUnit: m.DimensionUnit.Value}
}

// measuresOut are measures as a request gives them back (a change's
// requested ones), absent when the unit has none.
func measuresOut(m Measures) gen.OptMaterialMeasures {
	if m.IsZero() {
		return gen.OptMaterialMeasures{}
	}
	in := apitypes.OptText
	return gen.NewOptMaterialMeasures(gen.MaterialMeasures{Gtin: in(m.GTIN), GrossWeight: in(m.GrossWeight), NetWeight: in(m.NetWeight),
		WeightUnit: in(m.WeightUnit), Volume: in(m.Volume), VolumeUnit: in(m.VolumeUnit), Length: in(m.Length), Width: in(m.Width), Height: in(m.Height),
		DimensionUnit: in(m.DimensionUnit)})
}

// materialAPI is a material as the API gives it: its description in the
// language asked for, else its default one (S1a2), with all of them; its
// plants in the companies the caller may act in, by plant code, each with
// its storage locations by code.
func materialAPI(m Material, refs references, language string) gen.Material {
	described := m.DescriptionIn(language)
	out := gen.Material{ID: m.ID, Version: m.Version, CreatedAt: m.CreatedAt, CreatedBy: m.CreatedBy, UpdatedAt: m.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(m.UpdatedBy), Number: m.Number, LegalEntityID: m.LegalEntityID, CompanyCode: refs.companies[m.LegalEntityID].Code,
		Description: described.Text, DescriptionLanguage: described.Language, DefaultLanguage: m.DescriptionLanguage,
		Descriptions: descriptionsAPI(m.AllDescriptions()), BaseUnit: m.BaseUnit, BaseUnitName: m.BaseUnitName,
		BaseUnitMeasures: measuresAPI(m.BaseMeasures, refs.unitNames), MaterialType: m.Type, MaterialGroup: m.Group, Status: m.Status,
		BatchManaged: m.BatchManaged, SerialManaged: m.SerialManaged,
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
		out.AlternativeUnits[i] = gen.MaterialUnit{Unit: u.Code, UnitName: u.Name, Numerator: u.Numerator, Denominator: u.Denominator,
			Measures: measuresAPI(u.Measures, refs.unitNames)}
	}
	return out
}

// descriptionsAPI are descriptions as the API gives and takes them.
func descriptionsAPI(ds []Description) []gen.MaterialDescription {
	out := make([]gen.MaterialDescription, len(ds))
	for i, d := range ds {
		out[i] = gen.MaterialDescription{Language: d.Language, Description: d.Text}
	}
	return out
}

// one is a single material as the API gives it, in the language asked
// for, with its ETag.
func (e *Endpoints) one(ctx context.Context, m Material, err error, language string) (gen.Material, gen.OptString, error) {
	if err != nil {
		return gen.Material{}, gen.OptString{}, err
	}
	refs, err := e.references(ctx, []Material{m})
	if err != nil {
		return gen.Material{}, gen.OptString{}, err
	}
	return materialAPI(m, refs, language), gen.NewOptString(etag.Format(m.Version)), nil
}

// descriptionsIn are a request's further descriptions as the context
// takes them.
func descriptionsIn(ds []gen.MaterialDescription) []Description {
	out := make([]Description, len(ds))
	for i, d := range ds {
		out[i] = Description{Language: d.Language, Text: d.Description}
	}
	return out
}

// inputs are a request's plants and units as the context takes them.
func inputs(plants []gen.MaterialPlantInput, units []gen.MaterialUnitInput) ([]PlantInput, []UnitInput) {
	ps, us := make([]PlantInput, len(plants)), make([]UnitInput, len(units))
	for i, p := range plants {
		ps[i] = PlantInput{PlantID: p.PlantID, StorageLocations: p.StorageLocationIds}
	}
	for i, u := range units {
		us[i] = UnitInput{Code: u.Unit, Numerator: u.Numerator, Denominator: u.Denominator, Measures: measuresIn(u.Measures.Value)}
	}
	return ps, us
}

// CreateMaterial implements POST /v1/materials.
func (e *Endpoints) CreateMaterial(ctx context.Context, req *gen.MaterialCreate, _ gen.CreateMaterialParams) (gen.CreateMaterialRes, error) {
	plants, units := inputs(req.Plants, req.AlternativeUnits)
	m, err := e.Create(ctx, Input{LegalEntityID: req.LegalEntityID, Description: req.Description,
		DescriptionLanguage: req.DescriptionLanguage.Or(DefaultLanguage), Descriptions: descriptionsIn(req.Descriptions), BaseUnit: req.BaseUnit,
		BaseMeasures: measuresIn(req.BaseUnitMeasures.Value),
		Type:         req.MaterialType,
		Group:        req.MaterialGroup, Plants: plants, Units: units, Reason: req.Reason.Value, ConfirmDuplicate: req.ConfirmDuplicate.Value,
		BatchManaged: req.BatchManaged.Value, SerialManaged: req.SerialManaged.Value})
	out, tag, err := e.one(ctx, m, err, "")
	if err != nil {
		return nil, err
	}
	return &gen.MaterialHeaders{ETag: tag, Location: gen.NewOptString("/v1/materials/" + m.ID.String()), Response: out}, nil
}

// GetMaterial implements GET /v1/materials/{id}, its description in the
// language asked for.
func (e *Endpoints) GetMaterial(ctx context.Context, params gen.GetMaterialParams) (gen.GetMaterialRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	m, err := load(ctx, tx, params.ID, false)
	out, tag, err := e.one(ctx, m, err, params.Language.Value)
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
	if req.Descriptions != nil {
		ds := descriptionsIn(req.Descriptions)
		ch.Descriptions = &ds
	}
	if req.BaseUnitMeasures.Set { // given, even empty: the base unit's measures
		base := measuresIn(req.BaseUnitMeasures.Value)
		ch.BaseMeasures = &base
	}
	done, err := e.Change(ctx, params.ID, etag.IfMatch(params.IfMatch), ch, req.Reason.Value)
	out, tag, err := e.one(ctx, done.Material, err, "")
	if err != nil {
		return nil, err
	}
	return &gen.UpdateMaterialOKHeaders{ETag: tag, Response: out}, nil
}

var materialSort = pagination.Sort{Default: "number", Fields: []pagination.Field{{Name: "number", Column: "m.number", Type: "text"}}}

// ListMaterials implements GET /v1/materials: the tenant's materials by
// number, filtered by number, plant (one it has, not removed), status,
// type, group and a unit's GTIN (gtin.Key's comparison, which
// unit_measure_gtin_once serves), each described in the language asked
// for.
func (e *Endpoints) ListMaterials(ctx context.Context, params gen.ListMaterialsParams) (gen.ListMaterialsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	plant := apitypes.Ptr[uuid.UUID](params.PlantID)
	scope := fmt.Sprintf("listMaterials|%s|%s|%s|%s|%s|%s", params.Number.Value, plant, params.Status.Value, params.MaterialType.Value,
		params.MaterialGroup.Value, params.Gtin.Value)
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
		AND ($3 = '' OR m.status = $3) AND ($4 = '' OR m.material_type = $4) AND ($5 = '' OR m.material_group = $5)
		AND ($8 = '' OR EXISTS (SELECT FROM materials.unit_measure x WHERE x.tenant_id = m.tenant_id AND x.material_id = m.id AND x.gtin IS NOT NULL
			AND x.removed_at IS NULL AND lpad(x.gtin, 14, '0') = lpad($8, 14, '0')))`,
		[]any{params.Number.Value, plant, params.Status.Value, params.MaterialType.Value, params.MaterialGroup.Value, access.All, access.IDs,
			params.Gtin.Value})
	rows, _ := tx.Query(ctx, query, args...)
	ms, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Material, error) { return scanMaterial(r) })
	if err != nil {
		return nil, fmt.Errorf("materials: list materials: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, ms, func(m Material) (string, uuid.UUID) { return m.Number, m.ID },
		func(page []Material) (references, error) { return e.references(ctx, page) },
		func(m Material, refs references) gen.Material { return materialAPI(m, refs, params.Language.Value) })
	if err != nil {
		return nil, err
	}
	return &gen.MaterialPage{Items: items, NextPageToken: next}, nil
}
