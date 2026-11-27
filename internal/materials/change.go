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

// Change is a merge patch of a material: nil fields stay. Description is
// the default description; Descriptions replaces the further ones, Plants
// the material's plants in the companies the caller may act in, Units its
// alternative units; an empty list clears them.
type Change struct {
	Description  *string
	Descriptions *[]Description
	Group        *string
	Status       *string
	Plants       *[]PlantInput
	Units        *[]UnitInput
}

// view is what a change compares: the material's texts and status, its
// further descriptions by language, its plants in the caller's companies
// with their storage locations, and its units with their factors, each in
// a stated order.
type view struct {
	Description, Group, Status string
	Descriptions               map[string]string
	Plants                     map[uuid.UUID][]uuid.UUID
	Units                      map[string][2]int32
}

// viewOf is a material as the caller's change sees it.
func viewOf(m Material, access authz.Access) view {
	v := view{Description: m.Description, Group: m.Group, Status: m.Status, Descriptions: descriptionDetail(m.Descriptions),
		Plants: map[uuid.UUID][]uuid.UUID{}, Units: map[string][2]int32{}}
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
	if ch.Descriptions != nil {
		to.Descriptions = descriptionDetail(*ch.Descriptions)
		set("descriptions", !maps.Equal(to.Descriptions, v.Descriptions))
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
	all := map[string]any{"description": v.Description, "descriptions": v.Descriptions, "material_group": v.Group, "status": v.Status,
		"plants": slices.SortedFunc(maps.Keys(v.Plants), byBytes), "alternative_units": v.Units}
	out := map[string]any{}
	for _, f := range fields {
		out[f] = all[f]
	}
	return out
}

// Changed is what a change request did: the material after it, and the
// change it opened, or, for a block, applied.
type Changed struct {
	Material Material
	Change   MaterialChange
}

// Change requests a change of a material with the caller's version of it
// (ADR-0008, ADR-0082 §9): the row is locked, the version compared and
// the change checked as a creation is. A change that only blocks the
// material is applied at once, as a business partner's block is; any
// other opens a pending change, an unblock when it only sets the status
// active, applied when its approval request is approved. The number, base
// unit and type do not change (the contract has no field for them;
// material_fixed). A pending material, its create change undecided, takes
// no change (409 materials.invalid_status), nor one with a change pending
// (409 materials.change_pending).
func (e *Endpoints) Change(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, ch Change, reason string) (Changed, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Changed{}, err
	}
	m, err := load(ctx, tx, id, true)
	if err != nil {
		return Changed{}, err
	}
	if err := ifMatch(m.Version); err != nil {
		return Changed{}, err
	}
	if m.Status == Pending {
		return Changed{}, problem.Conflict("materials.invalid_status", "Material "+m.Number+" is pending until its create change is decided.")
	}
	req, fields, before, to, err := e.requested(ctx, tx, m, ch)
	if err != nil {
		return Changed{}, err
	}
	detail := map[string]any{"number": m.Number, "fields": fields, "before": before.detail(fields), "after": to.detail(fields)}
	if slices.Equal(fields, []string{"status"}) && to.Status == Blocked {
		return e.block(ctx, tx, p, m, req, reason, detail)
	}
	kind := KindUpdate
	if slices.Equal(fields, []string{"status"}) {
		kind = KindUnblock
	}
	c, err := insertChange(ctx, tx, p, kind, m.ID, req, reason, nil, false)
	if err != nil {
		return Changed{}, err
	}
	detail["material_change_id"], detail["material_change_number"] = c.ID, c.Number
	if err := recordChange(ctx, c, m, "materials.change.request", nil, detail); err != nil {
		return Changed{}, err
	}
	if c, err = e.open(ctx, tx, p, c, m.Number); err != nil {
		return Changed{}, err
	}
	after, err := load(ctx, tx, id, false)
	return Changed{Material: after, Change: c}, err
}

// requested checks a change of the material as a creation is checked and
// is what it asks for, the fields it changes, and the material before and
// after it as the caller sees them (422 materials.unknown_status,
// materials.nothing_to_change). Its descriptions are checked before they
// are compared, so a language given twice is refused as such.
func (e *Endpoints) requested(ctx context.Context, tx pgx.Tx, m Material, ch Change) (Requested, []string, view, view, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return Requested{}, nil, view{}, view{}, fmt.Errorf("materials: company access for material %s: %w", m.Number, err)
	}
	if ch.Status != nil && *ch.Status != Active && *ch.Status != Blocked {
		return Requested{}, nil, view{}, view{}, problem.Invalid("materials.unknown_status", "status",
			fmt.Sprintf("%q is not a status a change sets; it sets active or blocked.", *ch.Status))
	}
	if ch.Descriptions != nil {
		if err := checkDescriptions(m.DescriptionLanguage, *ch.Descriptions); err != nil {
			return Requested{}, nil, view{}, view{}, err
		}
	}
	before := viewOf(m, access)
	to, fields := ch.apply(before)
	if len(fields) == 0 {
		return Requested{}, nil, view{}, view{}, problem.Unprocessable("materials.nothing_to_change", "The change would leave the material as it is.")
	}
	if err := problem.RequireText("description", to.Description); err != nil {
		return Requested{}, nil, view{}, view{}, err
	}
	req := Requested{Description: ch.Description, Descriptions: ch.Descriptions, Group: ch.Group, Status: ch.Status}
	if ch.Units != nil {
		if err := checkUnits(ctx, tx, m.BaseUnit, *ch.Units); err != nil {
			return Requested{}, nil, view{}, view{}, err
		}
		req.Units = &[]RequestedUnit{}
		for _, u := range *ch.Units {
			*req.Units = append(*req.Units, RequestedUnit(u))
		}
	}
	if ch.Plants != nil {
		companies, err := e.checkPlants(ctx, *ch.Plants)
		if err != nil {
			return Requested{}, nil, view{}, view{}, err
		}
		req.Plants, req.Scope = requestedPlants(*ch.Plants, companies), &Scope{All: access.All, Companies: append([]uuid.UUID{}, access.IDs...)}
	}
	return req.only(fields), fields, before, to, nil
}

// only keeps what the change sets of the fields it changes: a field it
// gives as it is would, applied after another change, write the value of
// the request's time back (an update undoing a block in between).
func (r Requested) only(fields []string) Requested {
	if !slices.Contains(fields, "description") {
		r.Description = nil
	}
	if !slices.Contains(fields, "descriptions") {
		r.Descriptions = nil
	}
	if !slices.Contains(fields, "material_group") {
		r.Group = nil
	}
	if !slices.Contains(fields, "status") {
		r.Status = nil
	}
	if !slices.Contains(fields, "plants") {
		r.Plants, r.Scope = nil, nil
	}
	if !slices.Contains(fields, "alternative_units") {
		r.Units = nil
	}
	return r
}

// requestedPlants are the plants as a change keeps them, each with its
// company.
func requestedPlants(plants []PlantInput, companies map[uuid.UUID]uuid.UUID) *[]RequestedPlant {
	out := make([]RequestedPlant, len(plants))
	for i, p := range plants {
		out[i] = RequestedPlant{PlantID: p.PlantID, LegalEntityID: companies[p.PlantID], StorageLocations: append([]uuid.UUID{}, p.StorageLocations...)}
	}
	return &out
}

// block applies a change that only blocks the material, at once, with
// its applied change, both audited and published.
func (e *Endpoints) block(ctx context.Context, tx pgx.Tx, p reqctx.Principal, m Material, req Requested, reason string,
	detail map[string]any) (Changed, error) {
	if err := write(ctx, tx, p, m, req); err != nil {
		return Changed{}, err
	}
	c, err := insertChange(ctx, tx, p, KindBlock, m.ID, req, reason, nil, true)
	if err != nil {
		return Changed{}, err
	}
	after, err := load(ctx, tx, m.ID, false)
	if err != nil {
		return Changed{}, err
	}
	detail["material_change_id"], detail["material_change_number"] = c.ID, c.Number
	if err := e.record(ctx, after, change{action: "block", name: "blocked", detail: detail}, &m.Status); err != nil {
		return Changed{}, err
	}
	return Changed{Material: after, Change: c}, recordChange(ctx, c, after, "materials.change.apply", nil, nil)
}

// write writes what a change asks for on the material: its texts and
// status, then its further descriptions, its plants in the change's scope
// and its units, those it no longer names marked removed and those it
// names written.
func write(ctx context.Context, tx pgx.Tx, p reqctx.Principal, m Material, req Requested) error {
	description, group, status := m.Description, m.Group, m.Status
	for _, f := range []struct{ to, from *string }{{&description, req.Description}, {&group, req.Group}, {&status, req.Status}} {
		if f.from != nil {
			*f.to = *f.from
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE materials.material SET description = $2, material_group = $3, status = $4, version = version + 1, updated_by = $5
		WHERE id = $1`, m.ID, description, group, status, p.ID); err != nil {
		return fmt.Errorf("materials: change material %s: %w", m.Number, err)
	}
	if req.Descriptions != nil {
		if err := replaceDescriptions(ctx, tx, p, m, *req.Descriptions); err != nil {
			return err
		}
	}
	if req.Plants != nil {
		if err := replacePlants(ctx, tx, p, m, *req.Plants, *req.Scope); err != nil {
			return err
		}
	}
	if req.Units == nil {
		return nil
	}
	codes := []string{} // never nil: ANY of a NULL array matches nothing, and NOT of it keeps all
	units := make([]UnitInput, len(*req.Units))
	for i, u := range *req.Units {
		codes, units[i] = append(codes, u.Code), UnitInput(u)
	}
	if _, err := tx.Exec(ctx, `UPDATE materials.unit_conversion SET removed_at = now(), removed_by = $3
		WHERE material_id = $1 AND removed_at IS NULL AND NOT uom_code = ANY($2)`, m.ID, codes, p.ID); err != nil {
		return fmt.Errorf("materials: remove alternative units of material %s: %w", m.Number, err)
	}
	return writeUnits(ctx, tx, p, m.ID, units)
}

// replacePlants replaces the material's plants in the scope's companies:
// the storage locations no longer given marked removed, then the plants,
// then those given written. Plants of other companies stay.
func replacePlants(ctx context.Context, tx pgx.Tx, p reqctx.Principal, m Material, plants []RequestedPlant, scope Scope) error {
	kept, locations, companies := []uuid.UUID{}, []uuid.UUID{}, map[uuid.UUID]uuid.UUID{} // never nil, as codes in write
	given := make([]PlantInput, len(plants))
	for i, pl := range plants {
		kept, locations = append(kept, pl.PlantID), append(locations, pl.StorageLocations...)
		companies[pl.PlantID], given[i] = pl.LegalEntityID, PlantInput{PlantID: pl.PlantID, StorageLocations: pl.StorageLocations}
	}
	if _, err := tx.Exec(ctx, `UPDATE materials.storage_location_data s SET removed_at = now(), removed_by = $5
		FROM materials.plant_data d
		WHERE s.material_id = $1 AND s.removed_at IS NULL AND NOT s.storage_location_id = ANY($4)
			AND d.tenant_id = s.tenant_id AND d.material_id = s.material_id AND d.plant_id = s.plant_id AND ($2 OR d.legal_entity_id = ANY($3))`,
		m.ID, scope.All, scope.Companies, locations, p.ID); err != nil {
		return fmt.Errorf("materials: remove storage locations of material %s: %w", m.Number, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE materials.plant_data SET removed_at = now(), removed_by = $5
		WHERE material_id = $1 AND removed_at IS NULL AND ($2 OR legal_entity_id = ANY($3)) AND NOT plant_id = ANY($4)`,
		m.ID, scope.All, scope.Companies, kept, p.ID); err != nil {
		return fmt.Errorf("materials: remove plants of material %s: %w", m.Number, err)
	}
	return writePlants(ctx, tx, p, m.ID, given, companies)
}
