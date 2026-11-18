// Package materials is the material master context (ADR-0082; 02-architecture
// §2.2, 03-data-model §3.34): materials, tenant-wide master data, each with
// its number, description, base unit of measure, type, group and status,
// the plants it is extended to and its storage locations there, and its
// alternative units of measure with exact conversions (S1a).
//
// Plants and storage locations are tenancy's org units; a material's number
// is drawn from the configuration's material range of the company that
// creates it, for the fiscal year of that company's date today, and is
// unique in the tenant (ADR-0082 §6). The context reads tenancy, the
// configuration and the ledger's fiscal calendar through the ports below,
// which the composition root wires; inventory, procurement and sales will
// read materials through its published API (ADR-0082 §2-§3).
package materials

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Material statuses (ADR-0082 §7, §9): pending until its create change is
// approved.
const (
	Pending = "pending"
	Active  = "active"
	Blocked = "blocked"
)

// NumberRange is the document type materials draw their numbers from
// (configuration number_ranges, ADR-0082 §6).
const NumberRange = "material"

// types are the material types (ADR-0082 §4: SAP's ROH, HALB, FERT, HAWA,
// NLAG and DIEN, from memory). The enumeration is open: a later version
// may add to it.
var types = map[string]bool{"raw_material": true, "semi_finished": true, "finished_good": true, "trading_good": true, "non_stock": true,
	"service": true}

// Org unit kinds a material is extended to (tenancy.OrgUnitKinds).
const (
	kindPlant           = "plant"
	kindStorageLocation = "storage_location"
)

func init() {
	problem.RegisterConstraint("material_number_once", func() *problem.Problem {
		return problem.Conflict("materials.number_taken", "Another material has the number the company's material range issued: "+
			"the material ranges of two companies or years meet; give them prefixes or numbers that do not, through a change request.")
	})
}

// LegalEntities reads tenancy's legal entities and org units: a company's
// code, and a plant's or storage location's kind, company, parent, code,
// name and status.
type LegalEntities interface {
	Entity(ctx context.Context, id uuid.UUID) (tenancy.Entity, bool, error)
	OrgUnits(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]tenancy.Unit, error)
}

// Numbers issues document numbers (configuration context).
type Numbers interface {
	Next(ctx context.Context, legalEntity uuid.UUID, documentType string, fiscalYear int16) (string, error)
}

// Calendar maps a company's date to its fiscal year (ledger context).
type Calendar interface {
	FiscalYear(ctx context.Context, legalEntity uuid.UUID, date time.Time) (int16, error)
}

// Settings reads the active configuration: a company's time zone
// (configuration context).
type Settings interface {
	Document(ctx context.Context) (*config.Config, int32, error)
}

// Endpoints implements the materials part of the generated API.
type Endpoints struct {
	Pages         *pagination.Codec
	LegalEntities LegalEntities
	Numbers       Numbers
	Calendar      Calendar
	Settings      Settings
	Approvals     Opener // opens the approval requests of material changes (workflow context)
	// Now is the clock a material is numbered by, in its company's time
	// zone; nil: the wall clock.
	Now func() time.Time
}

// Plant is a plant a material is extended to, with the storage locations
// it is kept in there, by id.
type Plant struct {
	PlantID          uuid.UUID   `json:"plant_id"`
	LegalEntityID    uuid.UUID   `json:"legal_entity_id"`
	StorageLocations []uuid.UUID `json:"storage_location_ids"`
}

// Unit is an alternative unit of measure: one of it is Numerator /
// Denominator base units.
type Unit struct {
	Code        string `json:"unit"`
	Name        string `json:"name"`
	Numerator   int32  `json:"numerator"`
	Denominator int32  `json:"denominator"`
}

// Material is a row of materials.material with its plants by plant id,
// each plant's storage locations by id, and its alternative units by
// code; removed ones are left out.
type Material struct {
	ID            uuid.UUID
	Number        string
	LegalEntityID uuid.UUID
	Description   string
	BaseUnit      string
	BaseUnitName  string
	Type          string
	Group         string
	Status        string
	Plants        []Plant
	Units         []Unit
	// PendingChangeID and PendingChangeNumber are its pending change, nil
	// when none is pending (ADR-0082 §9).
	PendingChangeID     *uuid.UUID
	PendingChangeNumber *string
	Version             int32
	CreatedAt           time.Time
	CreatedBy           uuid.UUID
	UpdatedAt           time.Time
	UpdatedBy           uuid.UUID
}

// materialSelect reads materials with their plants, storage locations and
// alternative units, each in a stated order, and the one change pending
// (material_change_one_pending).
const materialSelect = `SELECT m.id, m.number, m.legal_entity_id, m.description, m.base_uom_code, b.name, m.material_type, m.material_group,
		m.status, m.version, m.created_at, m.created_by, m.updated_at, m.updated_by,
		coalesce((SELECT json_agg(json_build_object('plant_id', d.plant_id, 'legal_entity_id', d.legal_entity_id,
				'storage_location_ids', coalesce((SELECT json_agg(s.storage_location_id ORDER BY s.storage_location_id)
					FROM materials.storage_location_data s WHERE s.tenant_id = d.tenant_id AND s.material_id = d.material_id
						AND s.plant_id = d.plant_id AND s.removed_at IS NULL), '[]')) ORDER BY d.plant_id)
			FROM materials.plant_data d WHERE d.tenant_id = m.tenant_id AND d.material_id = m.id AND d.removed_at IS NULL), '[]'),
		coalesce((SELECT json_agg(json_build_object('unit', u.uom_code, 'name', r.name, 'numerator', u.numerator, 'denominator', u.denominator)
				ORDER BY u.uom_code)
			FROM materials.unit_conversion u JOIN ref.unit_of_measure r ON r.code = u.uom_code
			WHERE u.tenant_id = m.tenant_id AND u.material_id = m.id AND u.removed_at IS NULL), '[]'),
		(SELECT c.id FROM materials.material_change c WHERE c.tenant_id = m.tenant_id AND c.material_id = m.id AND c.status = 'pending') AS pending_change_id,
		(SELECT c.number FROM materials.material_change c WHERE c.tenant_id = m.tenant_id AND c.material_id = m.id AND c.status = 'pending') AS pending_change_number
	FROM materials.material m JOIN ref.unit_of_measure b ON b.code = m.base_uom_code`

func scanMaterial(row pgx.Row) (Material, error) {
	var m Material
	err := row.Scan(&m.ID, &m.Number, &m.LegalEntityID, &m.Description, &m.BaseUnit, &m.BaseUnitName, &m.Type, &m.Group, &m.Status, &m.Version,
		&m.CreatedAt, &m.CreatedBy, &m.UpdatedAt, &m.UpdatedBy, &m.Plants, &m.Units, &m.PendingChangeID, &m.PendingChangeNumber)
	return m, errs.Wrapf(err, "scan material")
}

// load reads a material; lock takes its row lock for a change, FOR NO KEY
// UPDATE as its plants' and units' foreign keys hold it FOR KEY SHARE
// (ADR-0008).
func load(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (Material, error) {
	q := materialSelect + ` WHERE m.id = $1`
	if lock {
		q += ` FOR NO KEY UPDATE OF m`
	}
	m, err := scanMaterial(tx.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Material{}, problem.NotFoundOf("material")
	}
	if err != nil {
		return Material{}, fmt.Errorf("materials: read material %s: %w", id, err)
	}
	return m, nil
}

// change is one change of a material as it is recorded: the last word of
// its audit action, what the event calls it, and the audit detail.
type change struct {
	action, name string
	detail       map[string]any
}

// record adds the audit record of a material's change and publishes
// MaterialChanged in the same transaction (ADR-0007, ADR-0011); before is
// its status before, nil when it had none. The event names the numbering
// company and every plant, whatever companies the caller may act in, by id
// and code; a consumer reads the rest from getMaterial.
func (e *Endpoints) record(ctx context.Context, m Material, ch change, before *string) error {
	codes, units, err := e.codes(ctx, m)
	if err != nil {
		return err
	}
	plants := make([]map[string]any, len(m.Plants))
	for i, p := range m.Plants {
		plants[i] = map[string]any{"plant_id": p.PlantID, "plant_code": units[p.PlantID].Code, "legal_entity_id": p.LegalEntityID,
			"legal_entity_code": codes[p.LegalEntityID], "storage_location_ids": p.StorageLocations}
	}
	alternatives := make([]map[string]any, len(m.Units))
	for i, u := range m.Units {
		alternatives[i] = map[string]any{"unit": u.Code, "numerator": u.Numerator, "denominator": u.Denominator}
	}
	payload := map[string]any{"material_id": m.ID, "number": m.Number, "legal_entity_id": m.LegalEntityID, "legal_entity_code": codes[m.LegalEntityID],
		"description": m.Description, "base_unit": m.BaseUnit, "material_type": m.Type, "material_group": m.Group, "status": m.Status,
		"status_before": before, "plants": plants, "alternative_units": alternatives, "change": ch.name, "version": m.Version}
	if err := audit.Record(ctx, "materials.material."+ch.action, "materials.material", m.ID, ch.detail,
		outbox.Event{Context: "materials", Name: "material_changed", AggregateType: "materials.material", AggregateID: m.ID, Payload: payload}); err != nil {
		return fmt.Errorf("materials: record %s of material %s: %w", ch.action, m.Number, err)
	}
	return nil
}

// codes reads the codes of a material's numbering company and of its
// plants and their companies, whatever the caller may act in.
func (e *Endpoints) codes(ctx context.Context, m Material) (map[uuid.UUID]string, map[uuid.UUID]tenancy.Unit, error) {
	codes, companies, ids := map[uuid.UUID]string{}, []uuid.UUID{m.LegalEntityID}, make([]uuid.UUID, len(m.Plants))
	for i, p := range m.Plants {
		ids[i], companies = p.PlantID, append(companies, p.LegalEntityID)
	}
	for _, id := range companies {
		if _, ok := codes[id]; ok {
			continue
		}
		le, _, err := e.LegalEntities.Entity(ctx, id) // a stored material's or plant's company exists
		if err != nil {
			return nil, nil, fmt.Errorf("materials: company %s: %w", id, err)
		}
		codes[id] = le.Code
	}
	units, err := e.LegalEntities.OrgUnits(ctx, ids)
	if err != nil {
		return nil, nil, fmt.Errorf("materials: plants of material %s: %w", m.Number, err)
	}
	return codes, units, nil
}
