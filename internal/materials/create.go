package materials

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// PlantInput is a plant a request extends a material to, with the storage
// locations it is kept in there.
type PlantInput struct {
	PlantID          uuid.UUID
	StorageLocations []uuid.UUID
}

// UnitInput is an alternative unit a request gives a material.
type UnitInput struct {
	Code                   string
	Numerator, Denominator int32
}

// Input is a material as a request creates it (ADR-0082 §4).
type Input struct {
	LegalEntityID uuid.UUID
	Description   string
	BaseUnit      string
	Type          string
	Group         string
	Plants        []PlantInput
	Units         []UnitInput
	// BatchManaged manages the material in batches (As built S2f).
	BatchManaged bool
	// Reason is the creator's, kept on the create change.
	Reason string
	// ConfirmDuplicate creates the material though others have its
	// description (ADR-0082 §9).
	ConfirmDuplicate bool
}

// company is the company whose range numbers a material, one the caller
// may act in (422 request.reference_not_found naming legal_entity_id).
func (e *Endpoints) company(ctx context.Context, id uuid.UUID) (tenancy.Entity, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return tenancy.Entity{}, fmt.Errorf("materials: company access for legal entity %s: %w", id, err)
	}
	le, ok, err := e.LegalEntities.Entity(ctx, id)
	if err != nil {
		return tenancy.Entity{}, fmt.Errorf("materials: legal entity %s: %w", id, err)
	}
	if !ok || !access.Allows(id) {
		return tenancy.Entity{}, problem.ReferenceNotFound("legal_entity_id", "legal entity", "no legal entity with this id that you can access")
	}
	return le, nil
}

// number draws a material's number from the company's material range of
// the fiscal year of the company's date today, in its time zone
// (ADR-0082 §6).
func (e *Endpoints) number(ctx context.Context, le tenancy.Entity) (string, error) {
	day, err := config.DocumentDay(ctx, e.Settings, le.Code, clock.Now(e.Now))
	if err != nil {
		return "", fmt.Errorf("materials: today in company %s: %w", le.Code, err)
	}
	year, err := e.Calendar.FiscalYear(ctx, le.ID, day.Date)
	if err != nil {
		return "", fmt.Errorf("materials: fiscal year of %s in company %s: %w", day.Date.Format(time.DateOnly), le.Code, err)
	}
	n, err := e.Numbers.Next(ctx, le.ID, NumberRange, year)
	if err != nil {
		return "", fmt.Errorf("materials: number a material in company %s: %w", le.Code, err)
	}
	return n, nil
}

// knownUnits refuses a unit of measure that ref.unit_of_measure does not
// have (422 materials.unknown_unit naming its field). fields name each
// code's field, in the request's order.
func knownUnits(ctx context.Context, tx pgx.Tx, codes, fields []string) error {
	rows, _ := tx.Query(ctx, `SELECT code FROM ref.unit_of_measure WHERE code = ANY($1)`, codes)
	known, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("materials: read %d units of measure: %w", len(codes), err)
	}
	for i, c := range codes {
		if !slices.Contains(known, c) {
			return problem.Invalid("materials.unknown_unit", fields[i], c+" is not a unit of measure.")
		}
	}
	return nil
}

// checkUnits checks a material's alternative units: known units other
// than the base unit, each once. Their factors' range, 1 to 1,000,000, is
// the contract's and the database's.
func checkUnits(ctx context.Context, tx pgx.Tx, base string, units []UnitInput) error {
	codes, fields := []string{base}, []string{"base_unit"}
	seen := map[string]bool{}
	for i, u := range units {
		field := fmt.Sprintf("alternative_units[%d].unit", i)
		switch {
		case u.Code == base:
			return problem.Invalid("materials.base_unit_conversion", field, u.Code+" is the base unit, which converts to itself; give only other units.")
		case seen[u.Code]:
			return problem.Invalid("materials.duplicate_unit", field, u.Code+" is given twice.")
		}
		seen[u.Code] = true
		codes, fields = append(codes, u.Code), append(fields, field)
	}
	return knownUnits(ctx, tx, codes, fields)
}

// checkPlants checks the plants a request extends a material to: each an
// active plant of a company the caller may act in, given once, and each
// of its storage locations an active storage location under it, given
// once. It answers each plant's company, by plant.
func (e *Endpoints) checkPlants(ctx context.Context, plants []PlantInput) (map[uuid.UUID]uuid.UUID, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, fmt.Errorf("materials: company access for plants: %w", err)
	}
	var ids []uuid.UUID
	for _, p := range plants {
		ids = append(append(ids, p.PlantID), p.StorageLocations...)
	}
	units, err := e.LegalEntities.OrgUnits(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("materials: %d plants and storage locations: %w", len(ids), err)
	}
	companies, seen := map[uuid.UUID]uuid.UUID{}, map[uuid.UUID]bool{}
	for i, p := range plants {
		field := fmt.Sprintf("plants[%d].plant_id", i)
		u, ok := units[p.PlantID]
		switch {
		case !ok || u.Kind != kindPlant || u.Status != tenancy.Active || !access.Allows(u.LegalEntityID):
			return nil, problem.Invalid("materials.plant_invalid", field, "No active plant with this id in a company you can access.")
		case companies[p.PlantID] != uuid.Nil:
			return nil, problem.Invalid("materials.duplicate_plant", field, "Plant "+u.Code+" is given twice.")
		}
		companies[p.PlantID] = u.LegalEntityID
		for j, s := range p.StorageLocations {
			field := fmt.Sprintf("plants[%d].storage_location_ids[%d]", i, j)
			l, ok := units[s]
			switch {
			case !ok || l.Kind != kindStorageLocation || l.Status != tenancy.Active || l.ParentID == nil || *l.ParentID != p.PlantID:
				return nil, problem.Invalid("materials.storage_location_invalid", field, "No active storage location with this id under plant "+u.Code+".")
			case seen[s]:
				return nil, problem.Invalid("materials.duplicate_storage_location", field, "Storage location "+l.Code+" is given twice.")
			}
			seen[s] = true
		}
	}
	return companies, nil
}

// says checks what a request says of a material before anything is
// written: a known type, then its units and plants. The texts' lengths and
// the group's form are the contract's and the database's.
func (e *Endpoints) says(ctx context.Context, tx pgx.Tx, in Input) (map[uuid.UUID]uuid.UUID, error) {
	if err := problem.RequireText("description", in.Description); err != nil {
		return nil, err
	}
	if !types[in.Type] {
		return nil, problem.Invalid("materials.unknown_material_type", "material_type", fmt.Sprintf("%q is not a material type; the types are "+
			"raw_material, semi_finished, finished_good, trading_good, non_stock and service.", in.Type))
	}
	if in.BatchManaged && !stocked[in.Type] {
		return nil, problem.Invalid("materials.batch_managed_not_stocked", "batch_managed", "A material of type "+in.Type+
			" is kept in no stock, so it has no batches.")
	}
	if err := checkUnits(ctx, tx, in.BaseUnit, in.Units); err != nil {
		return nil, err
	}
	return e.checkPlants(ctx, in.Plants)
}

// Create creates a material (ADR-0082 §4-§6, §9): the request checked,
// the materials of its description found and refused unless confirmed, the
// material numbered from its company's material range and written pending
// with its plants, storage locations and alternative units, and its create
// change opened with the possible duplicates for its approval; the audit
// records, MaterialChanged and MaterialChangeChanged in the request's
// transaction.
func (e *Endpoints) Create(ctx context.Context, in Input) (Material, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Material{}, err
	}
	le, err := e.company(ctx, in.LegalEntityID)
	if err != nil {
		return Material{}, err
	}
	companies, err := e.says(ctx, tx, in)
	if err != nil {
		return Material{}, err
	}
	dups, err := duplicates(ctx, tx, in.Description)
	if err != nil {
		return Material{}, err
	}
	if err := confirmed(dups, in.ConfirmDuplicate); err != nil {
		return Material{}, err
	}
	number, err := e.number(ctx, le)
	if err != nil {
		return Material{}, err
	}
	m, err := e.insert(ctx, tx, p, le, in, companies, number, Pending)
	if err != nil {
		return Material{}, err
	}
	c, err := insertChange(ctx, tx, p, KindCreate, m.ID, requestedOf(m, in, companies), in.Reason, dups, false)
	if err != nil {
		return Material{}, err
	}
	if err := e.record(ctx, m, change{action: "create", name: "created", detail: map[string]any{"number": m.Number, "description": m.Description,
		"base_unit": m.BaseUnit, "material_type": m.Type, "material_group": m.Group, "status": m.Status, "batch_managed": m.BatchManaged, "plants": plantIDs(m.Plants),
		"alternative_units": unitDetail(m.Units), "material_change_id": c.ID, "possible_duplicates": len(dups)}}, nil); err != nil {
		return Material{}, err
	}
	if err := recordChange(ctx, c, m, "materials.change.request", nil, nil); err != nil {
		return Material{}, err
	}
	if _, err := e.open(ctx, tx, p, c, m.Number); err != nil {
		return Material{}, err
	}
	return load(ctx, tx, m.ID, false)
}

// insert writes the material with its number in the status, pending for
// a request and active for a migration (Migration), with its plants,
// storage locations and units.
func (e *Endpoints) insert(ctx context.Context, tx pgx.Tx, p reqctx.Principal, le tenancy.Entity, in Input, companies map[uuid.UUID]uuid.UUID,
	number, status string) (Material, error) {
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO materials.material (tenant_id, id, number, legal_entity_id, description, base_uom_code, material_type,
			material_group, status, batch_managed, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $11)`,
		p.TenantID, id, number, le.ID, in.Description, in.BaseUnit, in.Type, in.Group, status, in.BatchManaged, p.ID); err != nil {
		return Material{}, fmt.Errorf("materials: insert material %s: %w", number, err)
	}
	if err := writePlants(ctx, tx, p, id, in.Plants, companies); err != nil {
		return Material{}, err
	}
	if err := writeUnits(ctx, tx, p, id, in.Units); err != nil {
		return Material{}, err
	}
	return load(ctx, tx, id, false)
}

// requestedOf is a create change's payload: the material as requested.
func requestedOf(m Material, in Input, companies map[uuid.UUID]uuid.UUID) Requested {
	units := make([]RequestedUnit, len(in.Units))
	for i, u := range in.Units {
		units[i] = RequestedUnit(u)
	}
	return Requested{Description: &m.Description, BaseUnit: &m.BaseUnit, Type: &m.Type, Group: &m.Group, Plants: requestedPlants(in.Plants, companies),
		Units: &units, BatchManaged: &m.BatchManaged}
}

// writePlants writes the plants a material is extended to and their
// storage locations, giving back a plant or storage location removed
// earlier rather than writing it twice.
func writePlants(ctx context.Context, tx pgx.Tx, p reqctx.Principal, material uuid.UUID, plants []PlantInput, companies map[uuid.UUID]uuid.UUID) error {
	for _, pl := range plants {
		if _, err := tx.Exec(ctx, `INSERT INTO materials.plant_data (tenant_id, material_id, plant_id, legal_entity_id, created_by)
			VALUES ($1, $2, $3, $4, $5) ON CONFLICT (tenant_id, material_id, plant_id) DO UPDATE SET removed_at = NULL, removed_by = NULL`,
			p.TenantID, material, pl.PlantID, companies[pl.PlantID], p.ID); err != nil {
			return fmt.Errorf("materials: extend material %s to plant %s: %w", material, pl.PlantID, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO materials.storage_location_data (tenant_id, material_id, plant_id, storage_location_id, created_by)
			SELECT $1, $2, $3, s, $4 FROM unnest($5::uuid[]) AS s
			ON CONFLICT (tenant_id, material_id, storage_location_id) DO UPDATE SET removed_at = NULL, removed_by = NULL`,
			p.TenantID, material, pl.PlantID, p.ID, pl.StorageLocations); err != nil {
			return fmt.Errorf("materials: storage locations of material %s at plant %s: %w", material, pl.PlantID, err)
		}
	}
	return nil
}

// writeUnits writes a material's alternative units, setting the factors
// of one it has, or had and lost, rather than writing it twice.
func writeUnits(ctx context.Context, tx pgx.Tx, p reqctx.Principal, material uuid.UUID, units []UnitInput) error {
	codes, nums, dens := make([]string, len(units)), make([]int32, len(units)), make([]int32, len(units))
	for i, u := range units {
		codes[i], nums[i], dens[i] = u.Code, u.Numerator, u.Denominator
	}
	if _, err := tx.Exec(ctx, `INSERT INTO materials.unit_conversion (tenant_id, material_id, uom_code, numerator, denominator, created_by)
		SELECT $1, $2, x.code, x.num, x.den, $3 FROM unnest($4::text[], $5::int[], $6::int[]) AS x (code, num, den)
		ON CONFLICT (tenant_id, material_id, uom_code) DO UPDATE SET numerator = EXCLUDED.numerator, denominator = EXCLUDED.denominator,
			removed_at = NULL, removed_by = NULL`, p.TenantID, material, p.ID, codes, nums, dens); err != nil {
		return fmt.Errorf("materials: alternative units of material %s: %w", material, err)
	}
	return nil
}

// plantIDs are the plants' ids, as the audit names them.
func plantIDs(plants []Plant) []uuid.UUID {
	out := make([]uuid.UUID, len(plants))
	for i, p := range plants {
		out[i] = p.PlantID
	}
	return out
}

// unitDetail is the units as the audit names them: each unit with its
// factors.
func unitDetail(units []Unit) []string {
	out := make([]string, len(units))
	for i, u := range units {
		out[i] = fmt.Sprintf("%s %d/%d", u.Code, u.Numerator, u.Denominator)
	}
	return out
}
