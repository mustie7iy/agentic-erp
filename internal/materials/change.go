package materials

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Change is a merge patch of a material: nil fields stay. Plants replaces
// the material's plants in the companies the caller may act in, Units its
// alternative units; an empty list clears them.
type Change struct {
	Description *string
	Group       *string
	Status      *string
	Plants      *[]PlantInput
	Units       *[]UnitInput
}

// view is what a change compares: the material's texts and status, its
// plants in the caller's companies with their storage locations, and its
// units with their factors, each in a stated order.
type view struct {
	Description, Group, Status string
	Plants                     map[uuid.UUID][]uuid.UUID
	Units                      map[string][2]int32
}

// viewOf is a material as the caller's change sees it.
func viewOf(m Material, access authz.Access) view {
	v := view{Description: m.Description, Group: m.Group, Status: m.Status, Plants: map[uuid.UUID][]uuid.UUID{}, Units: map[string][2]int32{}}
	for _, p := range m.Plants {
		if access.Allows(p.LegalEntityID) {
			v.Plants[p.PlantID] = p.StorageLocations
		}
	}
	for _, u := range m.Units {
		v.Units[u.Code] = [2]int32{u.Numerator, u.Denominator}
	}
	return v
}

// apply is the view after the change and the names of the fields that
// differ, in the contract's order.
func (ch Change) apply(v view) (view, []string) {
	to := v
	var fields []string
	set := func(name string, differs bool) {
		if differs {
			fields = append(fields, name)
		}
	}
	if ch.Description != nil {
		set("description", *ch.Description != v.Description)
		to.Description = *ch.Description
	}
	if ch.Group != nil {
		set("material_group", *ch.Group != v.Group)
		to.Group = *ch.Group
	}
	if ch.Status != nil {
		set("status", *ch.Status != v.Status)
		to.Status = *ch.Status
	}
	if ch.Plants != nil {
		to.Plants = map[uuid.UUID][]uuid.UUID{}
		for _, p := range *ch.Plants {
			to.Plants[p.PlantID] = slices.SortedFunc(slices.Values(p.StorageLocations), byBytes)
		}
		set("plants", !maps.EqualFunc(to.Plants, v.Plants, slices.Equal))
	}
	if ch.Units != nil {
		to.Units = map[string][2]int32{}
		for _, u := range *ch.Units {
			to.Units[u.Code] = [2]int32{u.Numerator, u.Denominator}
		}
		set("alternative_units", !maps.Equal(to.Units, v.Units))
	}
	return to, fields
}

// byBytes orders ids as PostgreSQL orders uuids, byte by byte, the order
// a material's storage locations are read in.
func byBytes(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) }

// detail is the view's fields as an audit record names them.
func (v view) detail(fields []string) map[string]any {
	all := map[string]any{"description": v.Description, "material_group": v.Group, "status": v.Status,
		"plants": slices.SortedFunc(maps.Keys(v.Plants), byBytes), "alternative_units": v.Units}
	out := map[string]any{}
	for _, f := range fields {
		out[f] = all[f]
	}
	return out
}

// Change changes a material at once, with the caller's version of it
// (ADR-0008): the row is locked, the version compared, the change checked
// as a creation is and written with an audit record naming the values
// before and after and MaterialChanged. The number, base unit and type do
// not change (the contract has no field for them; material_fixed).
func (e *Endpoints) Change(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, ch Change) (Material, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Material{}, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return Material{}, fmt.Errorf("materials: company access for material %s: %w", id, err)
	}
	m, err := load(ctx, tx, id, true)
	if err != nil {
		return Material{}, err
	}
	if err := ifMatch(m.Version); err != nil {
		return Material{}, err
	}
	if ch.Status != nil && *ch.Status != Active && *ch.Status != Blocked {
		return Material{}, problem.Invalid("materials.unknown_status", "status", fmt.Sprintf("%q is not a status a change sets; it sets active or blocked.", *ch.Status))
	}
	before := viewOf(m, access)
	to, fields := ch.apply(before)
	if len(fields) == 0 {
		return Material{}, problem.Unprocessable("materials.nothing_to_change", "The change would leave the material as it is.")
	}
	if err := e.write(ctx, tx, p, m, ch, to); err != nil {
		return Material{}, err
	}
	after, err := load(ctx, tx, id, false)
	if err != nil {
		return Material{}, err
	}
	return after, e.record(ctx, after, change{action: "change", name: "changed", detail: map[string]any{"number": after.Number, "fields": fields,
		"before": before.detail(fields), "after": to.detail(fields)}}, &m.Status)
}

// write checks the change's plants and units as a creation does and writes
// the change: the material's texts and status, then the plants, storage
// locations and units it no longer has marked removed and those given
// written.
func (e *Endpoints) write(ctx context.Context, tx pgx.Tx, p reqctx.Principal, m Material, ch Change, to view) error {
	var plants []PlantInput
	if ch.Plants != nil {
		plants = *ch.Plants
	}
	if err := problem.RequireText("description", to.Description); err != nil {
		return err
	}
	if ch.Units != nil {
		if err := checkUnits(ctx, tx, m.BaseUnit, *ch.Units); err != nil {
			return err
		}
	}
	companies, err := e.checkPlants(ctx, plants)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE materials.material SET description = $2, material_group = $3, status = $4, version = version + 1, updated_by = $5
		WHERE id = $1`, m.ID, to.Description, to.Group, to.Status, p.ID); err != nil {
		return fmt.Errorf("materials: change material %s: %w", m.Number, err)
	}
	if ch.Plants != nil {
		if err := e.replacePlants(ctx, tx, p, m, plants, companies, to); err != nil {
			return err
		}
	}
	if ch.Units == nil {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE materials.unit_conversion SET removed_at = now(), removed_by = $3
		WHERE material_id = $1 AND removed_at IS NULL AND NOT uom_code = ANY($2)`, m.ID, slices.AppendSeq([]string{}, maps.Keys(to.Units)), p.ID); err != nil {
		return fmt.Errorf("materials: remove alternative units of material %s: %w", m.Number, err)
	}
	return writeUnits(ctx, tx, p, m.ID, *ch.Units)
}

// replacePlants replaces the material's plants in the companies the caller
// may act in: the storage locations no longer given marked removed, then
// the plants, then those given written. Plants of other companies stay.
func (e *Endpoints) replacePlants(ctx context.Context, tx pgx.Tx, p reqctx.Principal, m Material, plants []PlantInput,
	companies map[uuid.UUID]uuid.UUID, to view) error {
	access, err := authz.Companies(ctx)
	if err != nil {
		return fmt.Errorf("materials: company access for the plants of material %s: %w", m.Number, err)
	}
	// Never nil: ANY of a NULL array matches nothing, and NOT of it keeps all.
	kept, locations := slices.AppendSeq([]uuid.UUID{}, maps.Keys(to.Plants)), []uuid.UUID{}
	for _, l := range to.Plants {
		locations = append(locations, l...)
	}
	if _, err := tx.Exec(ctx, `UPDATE materials.storage_location_data s SET removed_at = now(), removed_by = $5
		FROM materials.plant_data d
		WHERE s.material_id = $1 AND s.removed_at IS NULL AND NOT s.storage_location_id = ANY($4)
			AND d.tenant_id = s.tenant_id AND d.material_id = s.material_id AND d.plant_id = s.plant_id AND ($2 OR d.legal_entity_id = ANY($3))`,
		m.ID, access.All, access.IDs, locations, p.ID); err != nil {
		return fmt.Errorf("materials: remove storage locations of material %s: %w", m.Number, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE materials.plant_data SET removed_at = now(), removed_by = $5
		WHERE material_id = $1 AND removed_at IS NULL AND ($2 OR legal_entity_id = ANY($3)) AND NOT plant_id = ANY($4)`,
		m.ID, access.All, access.IDs, kept, p.ID); err != nil {
		return fmt.Errorf("materials: remove plants of material %s: %w", m.Number, err)
	}
	return writePlants(ctx, tx, p, m.ID, plants, companies)
}
