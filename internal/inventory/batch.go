package inventory

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Batches (S2f; ADR-0082 As built S2f): a material managed in batches
// (materials.Material.BatchManaged, fixed once created) is moved only by
// items that name a batch of it, created first (AddBatch, SAP MSC1N);
// each such movement at a storage location adds to the batch's stock
// there (inventory.batch_stock, SAP MCHB) as it adds to the location's,
// so the batches at a location add up to its stock. A posting locks the
// batch stock rows its lines move after the location's stock rows, in
// one order, and refuses a line that takes a batch below zero. The
// movement types that carry no batch yet (a purchase order's goods
// receipt, a delivery's goods issue, a stock transport order's, a
// count's) refuse a batch-managed material.

func init() {
	problem.RegisterConstraint("batch_number_once", func() *problem.Problem {
		return problem.Conflict("inventory.batch_exists", "The material has a batch with this number.")
	})
	problem.RegisterConstraint("batch_dates", func() *problem.Problem {
		return problem.Invalid("inventory.batch_dates", "expiry_date", "A batch expires on or after the day it was made.")
	})
	// takeBatches refuses a batch's shortage first, naming it; this is the
	// database's answer should one pass it.
	problem.RegisterConstraint("batch_stock_not_negative", func() *problem.Problem {
		return problem.Conflict("inventory.insufficient_stock", "A goods movement would take a batch's stock below zero.")
	})
}

// Batch is a batch of a batch-managed material (inventory.batch): its
// number, unique for the material, the day it was made and the day it is
// to be used by, nil when it does not expire.
type Batch struct {
	ID              uuid.UUID
	MaterialID      uuid.UUID
	Number          string
	ManufactureDate time.Time
	ExpiryDate      *time.Time
	CreatedAt       time.Time
	CreatedBy       uuid.UUID
}

// BatchInput is a batch as a request creates it.
type BatchInput struct {
	MaterialID      uuid.UUID
	Number          string
	ManufactureDate time.Time
	ExpiryDate      *time.Time
}

const batchSelect = `SELECT id, material_id, number, manufacture_date, expiry_date, created_at, created_by FROM inventory.batch`

func scanBatch(row pgx.Row) (Batch, error) {
	var b Batch
	err := row.Scan(&b.ID, &b.MaterialID, &b.Number, &b.ManufactureDate, &b.ExpiryDate, &b.CreatedAt, &b.CreatedBy)
	return b, errs.Wrapf(err, "scan batch")
}

// loadBatch reads a batch of the tenant.
func loadBatch(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Batch, error) {
	b, err := scanBatch(tx.QueryRow(ctx, batchSelect+` WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Batch{}, problem.NotFoundOf("batch")
	}
	if err != nil {
		return Batch{}, fmt.Errorf("inventory: read batch %s: %w", id, err)
	}
	return b, nil
}

// AddBatch creates a batch of a batch-managed material (As built S2f):
// the material one of the tenant's, not pending and managed in batches
// (which only a material kept in stock is, materials' create); its number unique for the material (409
// inventory.batch_exists, batch_number_once); its expiry date, when it
// has one, not before its manufacture date. Audited; no event, as a batch
// moves no stock: the material documents that name it publish theirs.
func (e *Endpoints) AddBatch(ctx context.Context, in BatchInput) (Batch, materials.Material, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Batch{}, materials.Material{}, err
	}
	ms, err := e.Materials.Materials(ctx, []uuid.UUID{in.MaterialID})
	if err != nil {
		return Batch{}, materials.Material{}, fmt.Errorf("inventory: material %s of a batch: %w", in.MaterialID, err)
	}
	m, ok := ms[in.MaterialID]
	switch {
	case !ok:
		return Batch{}, materials.Material{}, problem.Invalid("inventory.material_invalid", "material_id", "No material with this id.")
	case m.Status == materials.Pending:
		return Batch{}, materials.Material{}, problem.Invalid("inventory.material_not_usable", "material_id", "Material "+m.Number+" is pending its approval.")
	case !m.BatchManaged:
		return Batch{}, materials.Material{}, problem.Invalid("inventory.material_not_batch_managed", "material_id", "Material "+m.Number+" is not managed in batches.")
	case in.ExpiryDate != nil && in.ExpiryDate.Before(in.ManufactureDate):
		return Batch{}, materials.Material{}, problem.Invalid("inventory.batch_dates", "expiry_date", "A batch expires on or after the day it was made, "+
			in.ManufactureDate.Format(time.DateOnly)+".")
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO inventory.batch (tenant_id, id, material_id, number, manufacture_date, expiry_date, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, p.TenantID, id, in.MaterialID, in.Number, in.ManufactureDate, in.ExpiryDate, p.ID); err != nil {
		return Batch{}, materials.Material{}, fmt.Errorf("inventory: insert batch %s of material %s: %w", in.Number, m.Number, err)
	}
	b, err := loadBatch(ctx, tx, id)
	if err != nil {
		return Batch{}, materials.Material{}, err
	}
	detail := map[string]any{"material_id": m.ID, "material_number": m.Number, "number": b.Number,
		"manufacture_date": b.ManufactureDate.Format(time.DateOnly), "expiry_date": nil}
	if b.ExpiryDate != nil {
		detail["expiry_date"] = b.ExpiryDate.Format(time.DateOnly)
	}
	return b, m, audit.Record(ctx, "inventory.batch.create", "inventory.batch", b.ID, detail) // its error names the action
}

// readBatches reads the batches the items name, by id; a request's
// items alone name batches (Post), every other posting's none.
func readBatches(ctx context.Context, tx pgx.Tx, items []ItemInput) (map[uuid.UUID]Batch, error) {
	var ids []uuid.UUID
	for _, it := range items {
		if it.BatchID != nil {
			ids = append(ids, *it.BatchID)
		}
	}
	out := map[uuid.UUID]Batch{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, _ := tx.Query(ctx, batchSelect+` WHERE id = ANY($1) ORDER BY id`, ids)
	bs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Batch, error) { return scanBatch(row) })
	if err != nil {
		return nil, fmt.Errorf("inventory: read %d batches: %w", len(ids), err)
	}
	for _, b := range bs {
		out[b.ID] = b
	}
	return out, nil
}

// batchOf checks an item's batch: a batch of its material when the
// material is batch-managed and the movement type carries batches, none
// when it is not.
func batchOf(at string, it ItemInput, mt movementType, m materials.Material, r refs) error {
	switch {
	case m.BatchManaged && !mt.tracked:
		return problem.Invalid("inventory.batch_not_supported", at+"material_id", "Material "+m.Number+" is managed in batches; a "+
			lowerFirst(mt.name)+" does not carry batches yet.")
	case m.BatchManaged && it.BatchID == nil:
		return problem.Invalid("inventory.batch_required", at+"batch_id", "Material "+m.Number+" is managed in batches; the item names its batch.")
	case !m.BatchManaged && it.BatchID != nil:
		return problem.Invalid("inventory.batch_not_allowed", at+"batch_id", "Material "+m.Number+" is not managed in batches; the item names no batch.")
	case it.BatchID != nil && r.batches[*it.BatchID].MaterialID != m.ID:
		return problem.Invalid("inventory.batch_invalid", at+"batch_id", "No batch of material "+m.Number+" with this id.")
	}
	return nil
}

// batchKey is what a batch stock row is of: a batch at a storage
// location of a plant.
type batchKey struct {
	material, plant, location, batch uuid.UUID
}

// compare orders batch keys as every posting locks their rows, as
// PostgreSQL orders their uuids (bytewise).
func (k batchKey) compare(o batchKey) int {
	return cmp.Or(bytes.Compare(k.material[:], o.material[:]), bytes.Compare(k.plant[:], o.plant[:]), bytes.Compare(k.location[:], o.location[:]),
		bytes.Compare(k.batch[:], o.batch[:]))
}

// takeBatches locks the batch stock rows the lines move, writing a row
// with no quantity for a batch moved to a location the first time, in
// batchKey order, after the caller locked the locations' stock rows; then
// it walks the lines in their order, as movement_stock applies them, and
// refuses a line that takes a batch's stock below zero (409
// inventory.insufficient_stock, naming the batch). A batch never holds
// more than its location, whose range takeStock checks.
func takeBatches(ctx context.Context, tx pgx.Tx, p reqctx.Principal, company uuid.UUID, lines []Line, r refs) error {
	var keys []batchKey
	for _, l := range lines {
		if k, ok := batchKeyOf(l); ok && !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	slices.SortFunc(keys, batchKey.compare)
	current, numbers, err := lockBatches(ctx, tx, p, company, keys)
	if err != nil {
		return err
	}
	for _, l := range lines {
		k, ok := batchKeyOf(l)
		if !ok {
			continue
		}
		move := money.ParseRat(l.Quantity)
		after := new(big.Rat).Add(current[k], move)
		if after.Sign() < 0 {
			m := r.materials[k.material]
			return problem.Conflict("inventory.insufficient_stock", fmt.Sprintf("Batch %s of material %s %s has %s %s at %s when the document's line "+
				"takes %s, more than that.", numbers[k.batch], m.Number, m.Description, money.PlainQuantity(current[k]), m.BaseUnit,
				r.where(stockKey{material: k.material, plant: k.plant, location: k.location}), money.PlainQuantity(new(big.Rat).Neg(move))))
		}
		current[k] = after
	}
	return nil
}

// batchKeyOf is the batch stock a line moves, ok = false when it moves
// none: a line of a material not batch-managed (a line in transit is of
// one).
func batchKeyOf(l Line) (batchKey, bool) {
	if l.BatchID == nil {
		return batchKey{}, false
	}
	return batchKey{l.MaterialID, l.PlantID, l.StorageLocationID, *l.BatchID}, true
}

// lockBatches writes the batch stock rows of the keys a posting moves the
// first time, with no quantity, and locks every one in the keys' order.
// It answers each row's quantity and each batch's number.
func lockBatches(ctx context.Context, tx pgx.Tx, p reqctx.Principal, company uuid.UUID, keys []batchKey) (map[batchKey]*big.Rat, map[uuid.UUID]string,
	error) {
	n := len(keys)
	ids, ms, plants, locations, batches := make([]uuid.UUID, n), make([]uuid.UUID, n), make([]uuid.UUID, n), make([]uuid.UUID, n), make([]uuid.UUID, n)
	for i, k := range keys {
		ids[i], ms[i], plants[i], locations[i], batches[i] = uuid.Must(uuid.NewV7()), k.material, k.plant, k.location, k.batch
	}
	if _, err := tx.Exec(ctx, `INSERT INTO inventory.batch_stock (tenant_id, id, legal_entity_id, material_id, plant_id, storage_location_id, batch_id)
		SELECT $1, x.id, $2, x.m, x.p, x.s, x.b FROM unnest($3::uuid[], $4::uuid[], $5::uuid[], $6::uuid[], $7::uuid[]) WITH ORDINALITY AS x (id, m, p, s, b, n)
		ORDER BY x.n ON CONFLICT (tenant_id, material_id, plant_id, storage_location_id, batch_id) DO NOTHING`,
		p.TenantID, company, ids, ms, plants, locations, batches); err != nil {
		return nil, nil, fmt.Errorf("inventory: batch stock rows of %d batches: %w", n, err)
	}
	rows, _ := tx.Query(ctx, `SELECT s.material_id, s.plant_id, s.storage_location_id, s.batch_id, b.number, s.quantity::text FROM inventory.batch_stock s
		JOIN inventory.batch b ON b.tenant_id = s.tenant_id AND b.id = s.batch_id
		JOIN unnest($1::uuid[], $2::uuid[], $3::uuid[], $4::uuid[]) AS x (m, p, l, b)
			ON s.material_id = x.m AND s.plant_id = x.p AND s.storage_location_id = x.l AND s.batch_id = x.b
		ORDER BY s.material_id, s.plant_id, s.storage_location_id, s.batch_id FOR UPDATE OF s`, ms, plants, locations, batches)
	out, numbers := map[batchKey]*big.Rat{}, map[uuid.UUID]string{}
	var k batchKey
	var number, q string
	if _, err := pgx.ForEachRow(rows, []any{&k.material, &k.plant, &k.location, &k.batch, &number, &q}, func() error {
		out[k], numbers[k.batch] = money.ParseRat(q), number
		return nil
	}); err != nil {
		return nil, nil, fmt.Errorf("inventory: lock the stock of %d batches: %w", n, err)
	}
	return out, numbers, nil
}
