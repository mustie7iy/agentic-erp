package inventory

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Cycle counting (S2c2; ADR-0082 As built S2c2): a plant's indicators
// (SAP T159C, from memory), each a code with the interval in days between
// counts of a material that carries it, and a material's indicator at a
// plant (SAP MARC-ABCIN, from memory), maintained by an inventory
// controller at their versions. A count of the material at a storage
// location of the plant falls due once the interval has passed since its
// last posted count there (Due, read by GET /v1/cycle-counts-due); the
// count itself is the normal count document (CreateCount). A material's
// indicator is taken off at its version and kept as removed (S2c3;
// ADR-0082 As built S2c3), and the ABC analysis sets materials'
// indicators from their consumption value (abc.go).

func init() {
	problem.RegisterConstraint("cycle_count_indicator_code_once", func() *problem.Problem {
		return problem.Conflict("inventory.cycle_count_indicator_exists", "The plant has a cycle counting indicator with this code.")
	})
	problem.RegisterConstraint("cycle_count_material_once", func() *problem.Problem {
		return problem.Conflict("inventory.cycle_count_material_exists", "The material has a cycle counting indicator at this plant; change it "+
			"instead.")
	})
}

// The statuses of a material's indicator (S2c3): counted by it, or taken
// off and kept so.
const (
	CycleActive  = "active"
	CycleRemoved = "removed"
)

// Indicator is a plant's cycle counting indicator.
type Indicator struct {
	ID, LegalEntityID, PlantID uuid.UUID
	Code                       string
	IntervalDays               int32
	Version                    int32
	CreatedAt, UpdatedAt       time.Time
	CreatedBy, UpdatedBy       uuid.UUID
}

const indicatorSelect = `SELECT id, legal_entity_id, plant_id, code, interval_days, version, created_at, updated_at, created_by, updated_by
	FROM inventory.cycle_count_indicator`

func scanIndicator(row pgx.Row) (Indicator, error) {
	var i Indicator
	err := row.Scan(&i.ID, &i.LegalEntityID, &i.PlantID, &i.Code, &i.IntervalDays, &i.Version, &i.CreatedAt, &i.UpdatedAt, &i.CreatedBy, &i.UpdatedBy)
	return i, errs.Wrapf(err, "scan cycle count indicator")
}

// loadIndicator reads an indicator of a company the caller may act in,
// under its row lock with lock (FOR NO KEY UPDATE: the materials that
// carry it hold its key, which a change of its interval leaves alone);
// another company's is not found.
func loadIndicator(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (Indicator, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return Indicator{}, fmt.Errorf("inventory: company access for cycle count indicator %s: %w", id, err)
	}
	q := indicatorSelect + ` WHERE id = $1`
	if lock {
		q += ` FOR NO KEY UPDATE`
	}
	i, err := scanIndicator(tx.QueryRow(ctx, q, id))
	if err = authz.Visible(err, access, i.LegalEntityID, problem.NotFoundOf("cycle counting indicator"), "inventory: cycle count indicator %s",
		id); err != nil {
		return Indicator{}, err
	}
	return i, nil
}

// IndicatorInput is an indicator as a request creates it.
type IndicatorInput struct {
	PlantID      uuid.UUID
	Code         string
	IntervalDays int32
}

// CreateIndicator creates a plant's cycle counting indicator: an active
// plant of a company the caller may act in, a code once at the plant (409
// inventory.cycle_count_indicator_exists, the database's). Audited.
func (e *Endpoints) CreateIndicator(ctx context.Context, in IndicatorInput) (Indicator, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Indicator{}, err
	}
	r, err := e.read(ctx, nil, []uuid.UUID{in.PlantID}, nil, nil)
	if err != nil {
		return Indicator{}, err
	}
	plant, err := r.plant(in.PlantID)
	if err != nil {
		return Indicator{}, err
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO inventory.cycle_count_indicator (tenant_id, id, legal_entity_id, plant_id, code, interval_days, created_by,
			updated_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $7)`, p.TenantID, id, plant.LegalEntityID, plant.ID, in.Code, in.IntervalDays, p.ID); err != nil {
		return Indicator{}, fmt.Errorf("inventory: cycle count indicator %s at plant %s: %w", in.Code, plant.Code, err)
	}
	out, err := loadIndicator(ctx, tx, id, false)
	if err != nil {
		return Indicator{}, err
	}
	return out, audit.Record(ctx, "inventory.cycle_count_indicator.create", "inventory.cycle_count_indicator", id, map[string]any{
		"legal_entity_id": out.LegalEntityID, "plant_id": plant.ID, "plant_code": plant.Code, "code": out.Code, "interval_days": out.IntervalDays})
}

// ChangeIndicator changes an indicator's interval at its version (412
// otherwise); the interval it has is 422 inventory.nothing_to_change.
// Audited with the interval before and after.
func (e *Endpoints) ChangeIndicator(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, days int32) (Indicator, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Indicator{}, err
	}
	before, err := loadIndicator(ctx, tx, id, true)
	if err != nil {
		return Indicator{}, err
	}
	if err := ifMatch(before.Version); err != nil {
		return Indicator{}, err
	}
	if days == before.IntervalDays {
		return Indicator{}, problem.Unprocessable("inventory.nothing_to_change", fmt.Sprintf("Indicator %s's interval is %d days already.", before.Code, days))
	}
	if _, err := tx.Exec(ctx, `UPDATE inventory.cycle_count_indicator SET interval_days = $2, version = version + 1, updated_by = $3 WHERE id = $1`,
		id, days, p.ID); err != nil {
		return Indicator{}, fmt.Errorf("inventory: change cycle count indicator %s: %w", before.Code, err)
	}
	out, err := loadIndicator(ctx, tx, id, false)
	if err != nil {
		return Indicator{}, err
	}
	return out, audit.Record(ctx, "inventory.cycle_count_indicator.change", "inventory.cycle_count_indicator", id, map[string]any{
		"plant_id": out.PlantID, "code": out.Code, "before": map[string]any{"interval_days": before.IntervalDays},
		"after": map[string]any{"interval_days": out.IntervalDays}})
}

// CycleMaterial is a material's cycle counting indicator at a plant, with
// the indicator's code and interval as they are now, active or removed.
type CycleMaterial struct {
	ID, LegalEntityID, PlantID, MaterialID, IndicatorID uuid.UUID
	IndicatorCode, Status                               string
	IntervalDays                                        int32
	Version                                             int32
	CreatedAt, UpdatedAt                                time.Time
	CreatedBy, UpdatedBy                                uuid.UUID
}

const cycleMaterialSelect = `SELECT m.id, m.legal_entity_id, m.plant_id, m.material_id, m.indicator_id, i.code, m.status, i.interval_days, m.version,
		m.created_at, m.updated_at, m.created_by, m.updated_by
	FROM inventory.cycle_count_material m JOIN inventory.cycle_count_indicator i ON i.tenant_id = m.tenant_id AND i.id = m.indicator_id`

func scanCycleMaterial(row pgx.Row) (CycleMaterial, error) {
	var c CycleMaterial
	err := row.Scan(&c.ID, &c.LegalEntityID, &c.PlantID, &c.MaterialID, &c.IndicatorID, &c.IndicatorCode, &c.Status, &c.IntervalDays, &c.Version, &c.CreatedAt,
		&c.UpdatedAt, &c.CreatedBy, &c.UpdatedBy)
	return c, errs.Wrapf(err, "scan material cycle count indicator")
}

// loadCycleMaterial reads a material's indicator of a company the caller
// may act in, under its row lock with lock (FOR NO KEY UPDATE: no row
// references it); another company's is not found. The row is locked by
// itself first and read with its indicator after: a lock taken in the
// join would, after waiting for a writer that moved the row to another
// indicator, recheck it against the indicator read before and find no
// row.
func loadCycleMaterial(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (CycleMaterial, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return CycleMaterial{}, fmt.Errorf("inventory: company access for material cycle count indicator %s: %w", id, err)
	}
	if lock {
		if _, err := tx.Exec(ctx, `SELECT FROM inventory.cycle_count_material WHERE id = $1 FOR NO KEY UPDATE`, id); err != nil {
			return CycleMaterial{}, fmt.Errorf("inventory: lock material cycle count indicator %s: %w", id, err)
		}
	}
	c, err := scanCycleMaterial(tx.QueryRow(ctx, cycleMaterialSelect+` WHERE m.id = $1`, id))
	if err = authz.Visible(err, access, c.LegalEntityID, problem.NotFoundOf("material's cycle counting indicator"),
		"inventory: material cycle count indicator %s", id); err != nil {
		return CycleMaterial{}, err
	}
	return c, nil
}

// indicatorOf reads the indicator a material is given, one of a company
// the caller may act in and, with plant, of that plant (422
// inventory.indicator_invalid otherwise).
func indicatorOf(ctx context.Context, tx pgx.Tx, id uuid.UUID, plant *uuid.UUID) (Indicator, error) {
	i, err := loadIndicator(ctx, tx, id, false)
	var p *problem.Problem
	notFound := errors.As(err, &p) && p.Status == http.StatusNotFound
	if notFound || (err == nil && plant != nil && i.PlantID != *plant) {
		return Indicator{}, problem.Invalid("inventory.indicator_invalid", "indicator_id", "No cycle counting indicator with this id of the plant in a "+
			"company you can access.")
	}
	return i, err
}

// CycleMaterialInput is a material's indicator as a request creates it.
type CycleMaterialInput struct {
	MaterialID, IndicatorID uuid.UUID
}

// CreateCycleMaterial gives a material its cycle counting indicator at the
// indicator's plant: an indicator of a company the caller may act in, a
// material a count document takes at the plant (countable), one indicator
// per material and plant (409 inventory.cycle_count_material_exists, the
// database's). Audited.
func (e *Endpoints) CreateCycleMaterial(ctx context.Context, in CycleMaterialInput) (CycleMaterial, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return CycleMaterial{}, err
	}
	ind, err := indicatorOf(ctx, tx, in.IndicatorID, nil)
	if err != nil {
		return CycleMaterial{}, err
	}
	r, err := e.read(ctx, []uuid.UUID{in.MaterialID}, []uuid.UUID{ind.PlantID}, nil, nil)
	if err != nil {
		return CycleMaterial{}, err
	}
	if err := r.countable(in.MaterialID, ind.PlantID, "material_id"); err != nil {
		return CycleMaterial{}, err
	}
	return giveIndicator(ctx, tx, p, ind, r.materials[in.MaterialID], r.units[ind.PlantID], nil)
}

// giveIndicator gives a material the indicator at its plant and audits it,
// with what set it (the ABC analysis, S2c3) when from is given.
func giveIndicator(ctx context.Context, tx pgx.Tx, p reqctx.Principal, ind Indicator, m materials.Material, plant tenancy.Unit,
	from map[string]any) (CycleMaterial, error) {
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO inventory.cycle_count_material (tenant_id, id, legal_entity_id, plant_id, material_id, indicator_id, created_by,
			updated_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $7)`, p.TenantID, id, ind.LegalEntityID, ind.PlantID, m.ID, ind.ID, p.ID); err != nil {
		return CycleMaterial{}, fmt.Errorf("inventory: cycle count indicator %s of material %s at plant %s: %w", ind.Code, m.Number, plant.Code, err)
	}
	out, err := loadCycleMaterial(ctx, tx, id, false)
	if err != nil {
		return CycleMaterial{}, err
	}
	details := map[string]any{"legal_entity_id": out.LegalEntityID, "plant_id": plant.ID, "plant_code": plant.Code, "material_id": m.ID,
		"material_number": m.Number, "indicator_id": out.IndicatorID, "indicator_code": out.IndicatorCode, "interval_days": out.IntervalDays}
	if from != nil {
		details["abc_analysis"] = from
	}
	return out, audit.Record(ctx, "inventory.cycle_count_material.create", "inventory.cycle_count_material", id, details)
}

// ChangeCycleMaterial gives a material another indicator of its plant at
// its version (412 otherwise): one of the same plant (422
// inventory.indicator_invalid), not the one it has (422
// inventory.nothing_to_change). Audited with the indicator before and
// after.
func (e *Endpoints) ChangeCycleMaterial(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, indicator uuid.UUID) (CycleMaterial, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return CycleMaterial{}, err
	}
	before, err := loadCycleMaterial(ctx, tx, id, true)
	if err != nil {
		return CycleMaterial{}, err
	}
	if err := ifMatch(before.Version); err != nil {
		return CycleMaterial{}, err
	}
	if err := removedOne(before); err != nil {
		return CycleMaterial{}, err
	}
	if indicator == before.IndicatorID {
		return CycleMaterial{}, problem.Unprocessable("inventory.nothing_to_change", "The material carries indicator "+before.IndicatorCode+" already.")
	}
	ind, err := indicatorOf(ctx, tx, indicator, &before.PlantID)
	if err != nil {
		return CycleMaterial{}, err
	}
	return changeIndicator(ctx, tx, p, before, ind, nil)
}

// changeIndicator gives a material's active indicator, held under its row
// lock, another indicator of its plant and audits it with the indicator
// before and after, and what set it (the ABC analysis, S2c3) when from
// is given.
func changeIndicator(ctx context.Context, tx pgx.Tx, p reqctx.Principal, before CycleMaterial, ind Indicator, from map[string]any) (CycleMaterial, error) {
	if _, err := tx.Exec(ctx, `UPDATE inventory.cycle_count_material SET indicator_id = $2, version = version + 1, updated_by = $3 WHERE id = $1`,
		before.ID, ind.ID, p.ID); err != nil {
		return CycleMaterial{}, fmt.Errorf("inventory: change material cycle count indicator %s: %w", before.ID, err)
	}
	out, err := loadCycleMaterial(ctx, tx, before.ID, false)
	if err != nil {
		return CycleMaterial{}, err
	}
	details := map[string]any{"plant_id": out.PlantID, "material_id": out.MaterialID,
		"before": map[string]any{"indicator_id": before.IndicatorID, "indicator_code": before.IndicatorCode, "interval_days": before.IntervalDays},
		"after":  map[string]any{"indicator_id": out.IndicatorID, "indicator_code": out.IndicatorCode, "interval_days": out.IntervalDays}}
	if from != nil {
		details["abc_analysis"] = from
	}
	return out, audit.Record(ctx, "inventory.cycle_count_material.change", "inventory.cycle_count_material", before.ID, details)
}

// removedOne refuses a material's indicator taken off: it never changes
// again (409 inventory.cycle_count_material_removed).
func removedOne(c CycleMaterial) error {
	if c.Status == CycleRemoved {
		return problem.Conflict("inventory.cycle_count_material_removed", "The material's cycle counting indicator "+c.IndicatorCode+
			" at this plant is taken off; give the material an indicator again instead.")
	}
	return nil
}

// RemoveCycleMaterial takes a material's indicator off at its version (412
// otherwise), once (409 inventory.cycle_count_material_removed): it is
// kept as removed, and the material is no longer due by an indicator at
// the plant. Audited with the indicator it had.
func (e *Endpoints) RemoveCycleMaterial(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (CycleMaterial, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return CycleMaterial{}, err
	}
	before, err := loadCycleMaterial(ctx, tx, id, true)
	if err != nil {
		return CycleMaterial{}, err
	}
	if err := ifMatch(before.Version); err != nil {
		return CycleMaterial{}, err
	}
	if err := removedOne(before); err != nil {
		return CycleMaterial{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE inventory.cycle_count_material SET status = 'removed', version = version + 1, updated_by = $2 WHERE id = $1`,
		id, p.ID); err != nil {
		return CycleMaterial{}, fmt.Errorf("inventory: remove material cycle count indicator %s: %w", id, err)
	}
	out, err := loadCycleMaterial(ctx, tx, id, false)
	if err != nil {
		return CycleMaterial{}, err
	}
	return out, audit.Record(ctx, "inventory.cycle_count_material.remove", "inventory.cycle_count_material", id, map[string]any{
		"plant_id": out.PlantID, "material_id": out.MaterialID, "indicator_id": out.IndicatorID, "indicator_code": out.IndicatorCode,
		"interval_days": out.IntervalDays})
}

// Due is a count due: a material with an indicator at a storage location
// of its plant where it has stock (StockID, its row, pages the list),
// its last posted count there and the day the count fell due, nil both
// when it was never counted there.
type Due struct {
	StockID, LegalEntityID, PlantID, StorageLocationID, MaterialID, IndicatorID uuid.UUID
	IndicatorCode                                                               string
	IntervalDays                                                                int32
	LastCountID                                                                 *uuid.UUID
	LastCountNumber                                                             *string
	LastCountedOn, DueOn                                                        *time.Time
}

// dueKey is a count due's sort value as its cursor holds it: the day it
// fell due, or -infinity when it was never counted, which sorts first.
func (d Due) dueKey() string {
	if d.DueOn == nil {
		return "-infinity"
	}
	return d.DueOn.Format(time.DateOnly)
}

// dueSelect reads the counts due at a plant ($1) on a day ($4), at a
// storage location ($2) and of an indicator ($3) when given: the stock
// rows holding stock (as listStock and the count planning agent read
// them) of the materials with an active indicator at the plant that no open
// count holds, with their last posted count there (the latest by its date) and
// the day their count fell due. The page's condition and order name the
// columns due_key and id, which only orders.
const dueSelect = `SELECT id, legal_entity_id, plant_id, storage_location_id, material_id, indicator_id, code, interval_days, last_id, last_number,
		last_date, due_on FROM (
	SELECT s.id, s.legal_entity_id, s.plant_id, s.storage_location_id, s.material_id, a.indicator_id, i.code, i.interval_days,
		l.id AS last_id, l.number AS last_number, l.document_date AS last_date, l.document_date + i.interval_days AS due_on,
		coalesce(l.document_date + i.interval_days, '-infinity'::date) AS due_key
	FROM inventory.stock s
	JOIN inventory.cycle_count_material a ON a.tenant_id = s.tenant_id AND a.plant_id = s.plant_id AND a.material_id = s.material_id
		AND a.status = 'active'
	JOIN inventory.cycle_count_indicator i ON i.tenant_id = a.tenant_id AND i.id = a.indicator_id
	LEFT JOIN LATERAL (SELECT d.id, d.number, d.document_date FROM inventory.count_item c
			JOIN inventory.count_document d ON d.tenant_id = c.tenant_id AND d.id = c.count_document_id
			WHERE c.tenant_id = s.tenant_id AND c.material_id = s.material_id AND c.plant_id = s.plant_id AND c.storage_location_id = s.storage_location_id
				AND d.status = 'posted'
			ORDER BY d.document_date DESC, d.created_at DESC, d.id DESC LIMIT 1) l ON true
	WHERE s.plant_id = $1 AND s.quantity <> 0 AND ($2::uuid IS NULL OR s.storage_location_id = $2) AND ($3::uuid IS NULL OR a.indicator_id = $3)
		AND NOT EXISTS (SELECT FROM inventory.count_item o WHERE o.tenant_id = s.tenant_id AND o.material_id = s.material_id AND o.plant_id = s.plant_id
			AND o.storage_location_id = s.storage_location_id AND o.open)) x
	WHERE x.due_key <= $4`

func scanDue(row pgx.Row) (Due, error) {
	var d Due
	err := row.Scan(&d.StockID, &d.LegalEntityID, &d.PlantID, &d.StorageLocationID, &d.MaterialID, &d.IndicatorID, &d.IndicatorCode, &d.IntervalDays,
		&d.LastCountID, &d.LastCountNumber, &d.LastCountedOn, &d.DueOn)
	return d, errs.Wrapf(err, "scan count due")
}
