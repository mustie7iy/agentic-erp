package materials

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Migration is the materials context's import entry point for the SAP
// migration run (integration context; ADR-0082 As built S2o). The run is
// the governed act, a reconciled load a person started, as it is for the
// centers and suppliers it creates (ADR-0050 §10), so a material it
// creates is active at once with its create change applied, naming the
// run in its reason, and a material an earlier run created is extended to
// more plants and storage locations by an update change applied at once:
// neither opens an approval request. Each is checked as a request is
// (description, type, known units, active plants and storage locations the
// caller may act in) and audited and published as the API's changes are.
// A migrated material lists no possible duplicates: the run creates many
// materials under its company's material range lock, and the duplicate
// check's description lock taken after it would deadlock with a request
// that takes the two the other way round (Create).
type Migration struct{ Endpoints *Endpoints }

// MigratedMaterial is a material as an SAP migration run creates it,
// numbered from the material range of the company LegalEntityID.
type MigratedMaterial struct {
	LegalEntityID uuid.UUID
	Description   string
	BaseUnit      string
	Type          string
	Group         string
	Plants        []PlantInput
	Units         []UnitInput
	Reason        string
}

// CreateMaterial creates a migrated material, active, with its applied
// create change, and answers its id.
func (m Migration) CreateMaterial(ctx context.Context, in MigratedMaterial) (uuid.UUID, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	e := m.Endpoints
	le, err := e.company(ctx, in.LegalEntityID)
	if err != nil {
		return uuid.Nil, err
	}
	input := Input{LegalEntityID: in.LegalEntityID, Description: in.Description, BaseUnit: in.BaseUnit, Type: in.Type, Group: in.Group,
		Plants: in.Plants, Units: in.Units, Reason: in.Reason}
	companies, err := e.says(ctx, tx, input)
	if err != nil {
		return uuid.Nil, err
	}
	mat, err := e.insert(ctx, tx, p, le, input, companies, Active)
	if err != nil {
		return uuid.Nil, err
	}
	c, err := insertChange(ctx, tx, p, KindCreate, mat.ID, requestedOf(mat, input, companies), in.Reason, nil, true)
	if err != nil {
		return uuid.Nil, err
	}
	if err := e.record(ctx, mat, change{action: "create", name: "created", detail: map[string]any{"number": mat.Number,
		"description": mat.Description, "base_unit": mat.BaseUnit, "material_type": mat.Type, "material_group": mat.Group, "status": mat.Status,
		"plants": plantIDs(mat.Plants), "alternative_units": unitDetail(mat.Units), "material_change_id": c.ID, "migrated": true}}, nil); err != nil {
		return uuid.Nil, err
	}
	return mat.ID, recordChange(ctx, c, mat, "materials.change.apply", nil, nil)
}

// LockMaterials locks the materials an SAP migration run extends, in id
// order, before it creates anything: an update request locks its material
// and then draws its change's number, which a migrated creation holds
// from its first material on, so the run must hold the materials first
// (ADR-0008). FOR NO KEY UPDATE, as load locks a material.
func (Migration) LockMaterials(ctx context.Context, ids []uuid.UUID) error {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `SELECT FROM materials.material WHERE id = ANY($1) ORDER BY id FOR NO KEY UPDATE`, ids)
	return errs.Wrapf(err, "materials: lock the materials a migration run extends")
}

// ExtendMaterial extends a material to the plants and storage locations
// given that it is not yet at, as an update change applied at once, and
// reports whether there were any. A plant or storage location it has
// stays; one it lost is given back.
func (m Migration) ExtendMaterial(ctx context.Context, id uuid.UUID, plants []PlantInput, reason string) (bool, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return false, err
	}
	mat, err := load(ctx, tx, id, true)
	if err != nil {
		return false, err
	}
	missing := missingPlants(mat, plants)
	if len(missing) == 0 {
		return false, nil
	}
	e := m.Endpoints
	companies, err := e.checkPlants(ctx, missing)
	if err != nil {
		return false, err
	}
	if err := writePlants(ctx, tx, p, id, missing, companies); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE materials.material SET version = version + 1, updated_by = $2 WHERE id = $1`, id, p.ID); err != nil {
		return false, fmt.Errorf("materials: extend material %s: %w", mat.Number, err)
	}
	after, err := load(ctx, tx, id, false)
	if err != nil {
		return false, err
	}
	c, err := insertChange(ctx, tx, p, KindUpdate, id, extended(after, companies), reason, nil, true)
	if err != nil {
		return false, err
	}
	if err := e.record(ctx, after, change{action: "extend", name: "changed", detail: map[string]any{"number": after.Number,
		"plants": plantIDs(after.Plants), "material_change_id": c.ID, "material_change_number": c.Number, "migrated": true}}, &mat.Status); err != nil {
		return false, err
	}
	return true, recordChange(ctx, c, after, "materials.change.apply", nil, nil)
}

// Materials reads materials by id, as the published API does
// (Materials.Materials), for the run's reconciliation.
func (Migration) Materials(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Material, error) {
	return Materials{}.Materials(ctx, ids)
}

// missingPlants are the plants given the material is not at, and the
// storage locations given of the plants it is at that it is not kept in,
// in the order given.
func missingPlants(m Material, plants []PlantInput) []PlantInput {
	var out []PlantInput
	for _, pl := range plants {
		have, ok := m.Plant(pl.PlantID)
		if !ok {
			out = append(out, pl)
			continue
		}
		locations := slices.DeleteFunc(slices.Clone(pl.StorageLocations), func(s uuid.UUID) bool { return slices.Contains(have.StorageLocations, s) })
		if len(locations) > 0 {
			out = append(out, PlantInput{PlantID: pl.PlantID, StorageLocations: locations})
		}
	}
	return out
}

// extended is an extension's payload: the material's plants, after it, in
// the companies of the plants it extended the material to, as an update
// of its plants names them with its scope.
func extended(m Material, companies map[uuid.UUID]uuid.UUID) Requested {
	in := map[uuid.UUID]bool{}
	for _, le := range companies {
		in[le] = true
	}
	var plants []PlantInput
	owners := map[uuid.UUID]uuid.UUID{}
	for _, pl := range m.Plants {
		if in[pl.LegalEntityID] {
			plants, owners[pl.PlantID] = append(plants, PlantInput{PlantID: pl.PlantID, StorageLocations: pl.StorageLocations}), pl.LegalEntityID
		}
	}
	scope := slices.SortedFunc(maps.Keys(in), byBytes)
	return Requested{Plants: requestedPlants(plants, owners), Scope: &Scope{Companies: scope}}
}
