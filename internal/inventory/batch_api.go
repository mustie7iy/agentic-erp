package inventory

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// batchAPI is a batch as the API gives it, naming its material.
func batchAPI(b Batch, ms map[uuid.UUID]materials.Material) gen.Batch {
	m := ms[b.MaterialID]
	return gen.Batch{ID: b.ID, MaterialID: b.MaterialID, MaterialNumber: m.Number, MaterialDescription: m.Description, Number: b.Number,
		ManufactureDate: b.ManufactureDate, ExpiryDate: apitypes.OptDate(b.ExpiryDate), CreatedAt: b.CreatedAt, CreatedBy: b.CreatedBy}
}

// batchMaterials reads the materials of batches.
func (e *Endpoints) batchMaterials(ctx context.Context, bs []Batch) (map[uuid.UUID]materials.Material, error) {
	ids := make([]uuid.UUID, len(bs))
	for i, b := range bs {
		ids[i] = b.MaterialID
	}
	ms, err := e.Materials.Materials(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("inventory: materials of %d batches: %w", len(bs), err)
	}
	return ms, nil
}

// CreateBatch implements POST /v1/batches.
func (e *Endpoints) CreateBatch(ctx context.Context, req *gen.BatchCreate, _ gen.CreateBatchParams) (gen.CreateBatchRes, error) {
	b, m, err := e.AddBatch(ctx, BatchInput{MaterialID: req.MaterialID, Number: req.Number, ManufactureDate: req.ManufactureDate,
		ExpiryDate: apitypes.Ptr[time.Time](req.ExpiryDate)})
	if err != nil {
		return nil, err
	}
	out := batchAPI(b, map[uuid.UUID]materials.Material{m.ID: m})
	return &gen.BatchHeaders{ETag: gen.NewOptString(etag.Of(&out)), Location: gen.NewOptString("/v1/batches/" + b.ID.String()), Response: out}, nil
}

// GetBatch implements GET /v1/batches/{id}.
func (e *Endpoints) GetBatch(ctx context.Context, params gen.GetBatchParams) (gen.GetBatchRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	b, err := loadBatch(ctx, tx, params.ID)
	if err != nil {
		return nil, err
	}
	ms, err := e.batchMaterials(ctx, []Batch{b})
	if err != nil {
		return nil, err
	}
	out := batchAPI(b, ms)
	return &gen.GetBatchOKHeaders{ETag: gen.NewOptString(etag.Of(&out)), Response: out}, nil
}

var batchSort = pagination.Sort{Default: "created_at", DefaultDesc: true,
	Fields: []pagination.Field{{Name: "created_at", Column: "created_at", Type: "timestamptz"}}}

// ListBatches implements GET /v1/batches: the tenant's batches, the
// latest first, filtered by material and number.
func (e *Endpoints) ListBatches(ctx context.Context, params gen.ListBatchesParams) (gen.ListBatchesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	material := apitypes.Ptr[uuid.UUID](params.MaterialID)
	scope := fmt.Sprintf("listBatches|%s|%s", material, params.Number.Value)
	ks, err := e.Pages.Keyset(batchSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("inventory: page of batches: %w", err)
	}
	query, args := ks.SQL(batchSelect+` WHERE ($1::uuid IS NULL OR material_id = $1) AND ($2 = '' OR number = $2)`, []any{material, params.Number.Value})
	rows, _ := tx.Query(ctx, query, args...)
	bs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Batch, error) { return scanBatch(r) })
	if err != nil {
		return nil, fmt.Errorf("inventory: list batches: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, bs, func(b Batch) (string, uuid.UUID) { return pagination.Instant(b.CreatedAt), b.ID },
		func(page []Batch) (map[uuid.UUID]materials.Material, error) { return e.batchMaterials(ctx, page) }, batchAPI)
	if err != nil {
		return nil, err
	}
	return &gen.BatchPage{Items: items, NextPageToken: next}, nil
}

// batchStockRow is a row of inventory.batch_stock with its batch.
type batchStockRow struct {
	stockRow
	batch Batch
}

// ListBatchStock implements GET /v1/batch-stock: the stock by batch,
// plant and storage location of the companies the caller may act in, now;
// a batch with none at a location is left out.
func (e *Endpoints) ListBatchStock(ctx context.Context, params gen.ListBatchStockParams) (gen.ListBatchStockRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	material, plant, loc := apitypes.Ptr[uuid.UUID](params.MaterialID), apitypes.Ptr[uuid.UUID](params.PlantID), apitypes.Ptr[uuid.UUID](params.StorageLocationID)
	batch := apitypes.Ptr[uuid.UUID](params.BatchID)
	ks, err := e.Pages.Keyset(stockSort, fmt.Sprintf("listBatchStock|%s|%s|%s|%s", material, plant, loc, batch),
		apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("inventory: page of batch stock: %w", err)
	}
	a, err := access(ctx)
	if err != nil {
		return nil, err
	}
	// The rows' columns have names of their own, so the page's condition
	// and order name them.
	query, args := ks.SQL(`SELECT * FROM (SELECT s.id, s.legal_entity_id, s.material_id, s.plant_id, s.storage_location_id, s.created_at,
			trim_scale(s.quantity)::text AS quantity, b.id AS batch_id, b.number, b.manufacture_date, b.expiry_date
		FROM inventory.batch_stock s JOIN inventory.batch b ON b.tenant_id = s.tenant_id AND b.id = s.batch_id
		WHERE s.quantity <> 0 AND ($1 OR s.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR s.material_id = $3) AND ($4::uuid IS NULL OR s.plant_id = $4)
			AND ($5::uuid IS NULL OR s.storage_location_id = $5) AND ($6::uuid IS NULL OR s.batch_id = $6)) x WHERE true`,
		[]any{a.All, a.IDs, material, plant, loc, batch})
	rows, _ := tx.Query(ctx, query, args...)
	ss, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (batchStockRow, error) {
		var s batchStockRow
		return s, r.Scan(&s.ID, &s.LegalEntityID, &s.MaterialID, &s.PlantID, &s.StorageLocationID, &s.CreatedAt, &s.Quantity, &s.batch.ID, &s.batch.Number,
			&s.batch.ManufactureDate, &s.batch.ExpiryDate)
	})
	if err != nil {
		return nil, fmt.Errorf("inventory: list batch stock: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, ss, func(s batchStockRow) (string, uuid.UUID) { return pagination.Instant(s.CreatedAt), s.ID },
		func(page []batchStockRow) (refs, error) {
			rows := make([]stockRow, len(page))
			for i, s := range page {
				rows[i] = s.stockRow
			}
			return e.stockRefs(ctx, rows)
		}, batchStockAPI)
	if err != nil {
		return nil, err
	}
	return &gen.BatchStockPage{Items: items, NextPageToken: next}, nil
}

func batchStockAPI(s batchStockRow, r refs) gen.BatchStock {
	st := stockAPI(s.stockRow, r)
	return gen.BatchStock{MaterialID: st.MaterialID, MaterialNumber: st.MaterialNumber, MaterialDescription: st.MaterialDescription, BatchID: s.batch.ID,
		BatchNumber: s.batch.Number, ManufactureDate: s.batch.ManufactureDate, ExpiryDate: apitypes.OptDate(s.batch.ExpiryDate), PlantID: st.PlantID,
		PlantCode: st.PlantCode, PlantName: st.PlantName, StorageLocationID: st.StorageLocationID, StorageLocationCode: st.StorageLocationCode,
		StorageLocationName: st.StorageLocationName, LegalEntityID: st.LegalEntityID, CompanyCode: st.CompanyCode, Quantity: st.Quantity}
}
