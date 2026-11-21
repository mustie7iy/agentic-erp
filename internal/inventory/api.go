package inventory

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// documentAPI is a material document as the API gives it, naming what its
// lines name.
func documentAPI(d Document, r refs) gen.MaterialDocument {
	out := gen.MaterialDocument{ID: d.ID, Number: d.Number, LegalEntityID: d.LegalEntityID, CompanyCode: r.companies[d.LegalEntityID].Code,
		FiscalYear: int32(d.FiscalYear), MovementType: d.MovementType, MovementTypeName: d.typeName(), PostingDate: d.PostingDate,
		HeaderText: apitypes.OptString(d.HeaderText), ReversalOfID: apitypes.OptUUID(d.ReversalOfID), ReversalOfNumber: apitypes.OptString(d.ReversalOfNumber),
		ReversedByID: apitypes.OptUUID(d.ReversedByID), ReversedByNumber: apitypes.OptString(d.ReversedByNumber), CreatedAt: d.CreatedAt, CreatedBy: d.CreatedBy,
		JournalEntryID: apitypes.OptUUID(d.EntryID), JournalEntryNumber: apitypes.OptString(d.EntryNumber), Reference: apitypes.OptString(d.Reference),
		Lines: make([]gen.MaterialDocumentLine, len(d.Lines))}
	if d.EntryYear != nil {
		out.JournalEntryYear = gen.NewOptInt32(int32(*d.EntryYear))
	}
	for i, l := range d.Lines {
		m, plant, loc := r.materials[l.MaterialID], r.units[l.PlantID], r.units[l.StorageLocationID]
		line := gen.MaterialDocumentLine{LineNo: int32(l.LineNo), ItemNo: int32(l.ItemNo), MaterialID: l.MaterialID, MaterialNumber: m.Number,
			MaterialDescription: m.Description, PlantID: l.PlantID, PlantCode: plant.Code, PlantName: plant.Name, StorageLocationID: l.StorageLocationID,
			StorageLocationCode: loc.Code, StorageLocationName: loc.Name, Quantity: gen.Quantity{Value: l.Quantity, Unit: m.BaseUnit},
			EntryQuantity: gen.Quantity{Value: l.EntryQuantity, Unit: l.EntryUnit}, CostCenterID: apitypes.OptUUID(l.CostCenterID), InTransit: l.InTransit,
			BatchID: apitypes.OptUUID(l.BatchID), BatchNumber: apitypes.OptString(l.BatchNumber),
			SerialNumbers: l.SerialNumbers}
		if l.CostCenterID != nil {
			line.CostCenterCode = gen.NewOptString(r.centers[*l.CostCenterID].Code)
		}
		if d.Currency != "" {
			line.Amount = gen.NewOptMoney(apitypes.Money(l.Amount, d.Currency, d.Exponent))
			line.PriceDifference = gen.NewOptMoney(apitypes.Money(l.Difference, d.Currency, d.Exponent))
		}
		out.Lines[i] = line
	}
	return out
}

// one is a single material document as the API gives it, with its ETag,
// which moves when the document is reversed.
func (e *Endpoints) one(ctx context.Context, d Document, err error) (gen.MaterialDocument, gen.OptString, error) {
	if err != nil {
		return gen.MaterialDocument{}, gen.OptString{}, err
	}
	r, err := e.documentRefs(ctx, []Document{d})
	if err != nil {
		return gen.MaterialDocument{}, gen.OptString{}, err
	}
	out := documentAPI(d, r)
	return out, gen.NewOptString(documentTag(out)), nil
}

func documentTag(d gen.MaterialDocument) string { return etag.Of(&d) }

// CreateMaterialDocument implements POST /v1/material-documents.
func (e *Endpoints) CreateMaterialDocument(ctx context.Context, req *gen.MaterialDocumentCreate, _ gen.CreateMaterialDocumentParams) (gen.CreateMaterialDocumentRes, error) {
	items := make([]ItemInput, len(req.Items))
	for i, it := range req.Items {
		items[i] = ItemInput{MaterialID: it.MaterialID, PlantID: it.PlantID, StorageLocationID: it.StorageLocationID, Quantity: it.Quantity.Value,
			Unit: it.Quantity.Unit, CostCenterID: apitypes.Ptr[uuid.UUID](it.CostCenterID), ToPlantID: apitypes.Ptr[uuid.UUID](it.ToPlantID),
			ToStorageLocationID: apitypes.Ptr[uuid.UUID](it.ToStorageLocationID), BatchID: apitypes.Ptr[uuid.UUID](it.BatchID),
			SerialNumbers: it.SerialNumbers}
		if a, ok := it.Amount.Get(); ok {
			items[i].Amount = &Amount{Value: a.Amount, Currency: a.Currency}
		}
	}
	d, err := e.Post(ctx, Input{MovementType: req.MovementType, HeaderText: apitypes.Ptr[string](req.HeaderText), Items: items})
	out, tag, err := e.one(ctx, d, err)
	if err != nil {
		return nil, err
	}
	return &gen.MaterialDocumentHeaders{ETag: tag, Location: gen.NewOptString("/v1/material-documents/" + d.ID.String()), Response: out}, nil
}

// GetMaterialDocument implements GET /v1/material-documents/{id}.
func (e *Endpoints) GetMaterialDocument(ctx context.Context, params gen.GetMaterialDocumentParams) (gen.GetMaterialDocumentRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	d, err := load(ctx, tx, params.ID)
	out, tag, err := e.one(ctx, d, err)
	if err != nil {
		return nil, err
	}
	return &gen.GetMaterialDocumentOKHeaders{ETag: tag, Response: out}, nil
}

// ReverseMaterialDocument implements POST /v1/material-documents/{id}:reverse.
func (e *Endpoints) ReverseMaterialDocument(ctx context.Context, params gen.ReverseMaterialDocumentParams) (gen.ReverseMaterialDocumentRes, error) {
	check := func(d Document) error {
		_, tag, err := e.one(ctx, d, nil)
		if err != nil {
			return err
		}
		return etag.Match(params.IfMatch, tag.Value)
	}
	d, err := e.Reverse(ctx, params.ID, check)
	out, tag, err := e.one(ctx, d, err)
	if err != nil {
		return nil, err
	}
	return &gen.ReverseMaterialDocumentOKHeaders{ETag: tag, Response: out}, nil
}

var documentSort = pagination.Sort{Default: "created_at", DefaultDesc: true,
	Fields: []pagination.Field{{Name: "created_at", Column: "created_at", Type: "timestamptz"}}}

// ListMaterialDocuments implements GET /v1/material-documents: the
// documents of the companies the caller may act in, the latest first,
// filtered by a material or plant one of their lines names and by
// movement type.
func (e *Endpoints) ListMaterialDocuments(ctx context.Context, params gen.ListMaterialDocumentsParams) (gen.ListMaterialDocumentsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	material, plant := apitypes.Ptr[uuid.UUID](params.MaterialID), apitypes.Ptr[uuid.UUID](params.PlantID)
	scope := fmt.Sprintf("listMaterialDocuments|%s|%s|%s", material, plant, params.MovementType.Value)
	ks, err := e.Pages.Keyset(documentSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("inventory: page of material documents: %w", err)
	}
	a, err := access(ctx)
	if err != nil {
		return nil, err
	}
	// The documents' columns have names of their own, so the page's
	// condition and order name them.
	query, args := ks.SQL(`SELECT * FROM (`+documentSelect+` WHERE ($1 OR d.legal_entity_id = ANY($2)) AND ($3 = '' OR d.movement_type = $3)
		AND ($4::uuid IS NULL OR EXISTS (SELECT FROM inventory.movement m WHERE m.tenant_id = d.tenant_id AND m.material_document_id = d.id AND m.material_id = $4))
		AND ($5::uuid IS NULL OR EXISTS (SELECT FROM inventory.movement m WHERE m.tenant_id = d.tenant_id AND m.material_document_id = d.id AND m.plant_id = $5))
		) x WHERE true`,
		[]any{a.All, a.IDs, params.MovementType.Value, material, plant})
	rows, _ := tx.Query(ctx, query, args...)
	ds, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Document, error) { return scanDocument(r) })
	if err != nil {
		return nil, fmt.Errorf("inventory: list material documents: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, ds, func(d Document) (string, uuid.UUID) { return pagination.Instant(d.CreatedAt), d.ID },
		func(page []Document) (refs, error) { return e.documentRefs(ctx, page) }, documentAPI)
	if err != nil {
		return nil, err
	}
	return &gen.MaterialDocumentPage{Items: items, NextPageToken: next}, nil
}

// stockRow is a row of inventory.stock with its quantity now or as at a
// date.
type stockRow struct {
	ID, LegalEntityID, MaterialID, PlantID, StorageLocationID uuid.UUID
	CreatedAt                                                 time.Time
	Quantity                                                  string
}

var stockSort = pagination.Sort{Default: "created_at", Fields: []pagination.Field{{Name: "created_at", Column: "created_at", Type: "timestamptz"}}}

// ListStock implements GET /v1/stock: the stock by plant and storage
// location of the companies the caller may act in, now, or as at the end
// of a day as the sum of the movements of the documents posted up to it;
// a location with none is left out.
func (e *Endpoints) ListStock(ctx context.Context, params gen.ListStockParams) (gen.ListStockRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	material, plant, loc := apitypes.Ptr[uuid.UUID](params.MaterialID), apitypes.Ptr[uuid.UUID](params.PlantID), apitypes.Ptr[uuid.UUID](params.StorageLocationID)
	asOf := apitypes.Ptr[time.Time](params.AsOf)
	scope := fmt.Sprintf("listStock|%s|%s|%s|%s", material, plant, loc, apitypes.OptDate(asOf).Value.Format(time.DateOnly))
	ks, err := e.Pages.Keyset(stockSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("inventory: page of stock: %w", err)
	}
	a, err := access(ctx)
	if err != nil {
		return nil, err
	}
	query, args := ks.SQL(`SELECT id, legal_entity_id, material_id, plant_id, storage_location_id, created_at, trim_scale(qty)::text FROM (
			SELECT s.id, s.legal_entity_id, s.material_id, s.plant_id, s.storage_location_id, s.created_at,
				CASE WHEN $1::date IS NULL THEN s.quantity ELSE (SELECT coalesce(sum(m.quantity), 0) FROM inventory.movement m
					JOIN inventory.material_document d ON d.tenant_id = m.tenant_id AND d.id = m.material_document_id
					WHERE m.tenant_id = s.tenant_id AND m.material_id = s.material_id AND m.plant_id = s.plant_id
						AND m.storage_location_id = s.storage_location_id AND NOT m.in_transit AND d.posting_date <= $1) END AS qty
			FROM inventory.stock s WHERE ($2 OR s.legal_entity_id = ANY($3)) AND ($4::uuid IS NULL OR s.material_id = $4)
				AND ($5::uuid IS NULL OR s.plant_id = $5) AND ($6::uuid IS NULL OR s.storage_location_id = $6)) x
		WHERE qty <> 0`, []any{asOf, a.All, a.IDs, material, plant, loc})
	rows, _ := tx.Query(ctx, query, args...)
	ss, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (stockRow, error) {
		var s stockRow
		return s, r.Scan(&s.ID, &s.LegalEntityID, &s.MaterialID, &s.PlantID, &s.StorageLocationID, &s.CreatedAt, &s.Quantity)
	})
	if err != nil {
		return nil, fmt.Errorf("inventory: list stock: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, ss, func(s stockRow) (string, uuid.UUID) { return pagination.Instant(s.CreatedAt), s.ID },
		func(page []stockRow) (refs, error) { return e.stockRefs(ctx, page) }, stockAPI)
	if err != nil {
		return nil, err
	}
	return &gen.StockPage{Items: items, NextPageToken: next}, nil
}

// stockRefs reads what a page of stock names.
func (e *Endpoints) stockRefs(ctx context.Context, ss []stockRow) (refs, error) {
	var ms, units, companies []uuid.UUID
	for _, s := range ss {
		ms, units, companies = append(ms, s.MaterialID), append(units, s.PlantID, s.StorageLocationID), append(companies, s.LegalEntityID)
	}
	return e.read(ctx, ms, units, nil, companies)
}

func stockAPI(s stockRow, r refs) gen.Stock {
	m, plant, loc := r.materials[s.MaterialID], r.units[s.PlantID], r.units[s.StorageLocationID]
	return gen.Stock{MaterialID: s.MaterialID, MaterialNumber: m.Number, MaterialDescription: m.Description, PlantID: s.PlantID, PlantCode: plant.Code,
		PlantName: plant.Name, StorageLocationID: s.StorageLocationID, StorageLocationCode: loc.Code, StorageLocationName: loc.Name,
		LegalEntityID: s.LegalEntityID, CompanyCode: r.companies[s.LegalEntityID].Code, Quantity: gen.Quantity{Value: s.Quantity, Unit: m.BaseUnit}}
}
