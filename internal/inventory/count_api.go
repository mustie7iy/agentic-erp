package inventory

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

const countPath = "/v1/physical-inventory-documents/"

// countAPI is a count document as the API gives it, naming its company,
// plant, storage location and materials.
func countAPI(d CountDocument, r refs) gen.PhysicalInventoryDocument {
	plant, loc := r.units[d.PlantID], r.units[d.StorageLocationID]
	out := gen.PhysicalInventoryDocument{ID: d.ID, Version: d.Version, CreatedAt: d.CreatedAt, CreatedBy: d.CreatedBy, UpdatedAt: d.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(d.UpdatedBy), Number: d.Number, LegalEntityID: d.LegalEntityID, CompanyCode: r.companies[d.LegalEntityID].Code,
		FiscalYear: int32(d.FiscalYear), PlantID: d.PlantID, PlantCode: plant.Code, PlantName: plant.Name, StorageLocationID: d.StorageLocationID,
		StorageLocationCode: loc.Code, StorageLocationName: loc.Name, DocumentDate: d.DocumentDate, Status: d.Status,
		Items: make([]gen.PhysicalInventoryItem, len(d.Items))}
	for i, it := range d.Items {
		m := r.materials[it.MaterialID]
		item := gen.PhysicalInventoryItem{ItemNo: int32(it.ItemNo), MaterialID: it.MaterialID, MaterialNumber: m.Number, MaterialDescription: m.Description,
			BookQuantity: gen.Quantity{Value: it.Book, Unit: m.BaseUnit}, CountedAt: apitypes.OptDateTime(it.CountedAt), CountedBy: apitypes.OptUUID(it.CountedBy)}
		if it.Counted != nil {
			item.CountedQuantity = gen.NewOptQuantity(gen.Quantity{Value: *it.Counted, Unit: m.BaseUnit})
			item.EntryQuantity = gen.NewOptQuantity(gen.Quantity{Value: *it.Entry, Unit: *it.EntryUnit})
		}
		if it.Difference != nil {
			item.Difference = gen.NewOptQuantity(gen.Quantity{Value: *it.Difference, Unit: m.BaseUnit})
			item.DifferenceValue = gen.NewOptMoney(apitypes.Money(*it.Amount, d.Currency, d.Exponent))
		}
		item.MaterialDocumentID, item.MaterialDocumentNumber = apitypes.OptUUID(it.MaterialDocumentID), apitypes.OptString(it.MaterialDocumentNumber)
		out.Items[i] = item
	}
	out.WorkflowRequestID = apitypes.OptUUID(d.WorkflowRequestID)
	if d.DifferenceValue != nil {
		out.DifferenceValue = gen.NewOptMoney(apitypes.Money(*d.DifferenceValue, d.Currency, d.Exponent))
	}
	return out
}

// PostPhysicalInventoryDifferences implements POST
// /v1/physical-inventory-documents/{id}:postDifferences.
func (e *Endpoints) PostPhysicalInventoryDifferences(ctx context.Context, params gen.PostPhysicalInventoryDifferencesParams) (gen.PostPhysicalInventoryDifferencesRes, error) {
	d, err := e.PostDifferences(ctx, params.ID, versionOf(params.IfMatch))
	out, tag, err := e.oneCount(ctx, d, err)
	if err != nil {
		return nil, err
	}
	return &gen.EnterPhysicalInventoryCountOKHeaders{ETag: tag, Response: out}, nil
}

// oneCount is a single count document as the API gives it, with its ETag,
// its version.
func (e *Endpoints) oneCount(ctx context.Context, d CountDocument, err error) (gen.PhysicalInventoryDocument, gen.OptString, error) {
	if err != nil {
		return gen.PhysicalInventoryDocument{}, gen.OptString{}, err
	}
	r, err := e.documentCountRefs(ctx, []CountDocument{d})
	if err != nil {
		return gen.PhysicalInventoryDocument{}, gen.OptString{}, err
	}
	return countAPI(d, r), gen.NewOptString(etag.Format(d.Version)), nil
}

// CreatePhysicalInventoryDocument implements POST /v1/physical-inventory-documents.
func (e *Endpoints) CreatePhysicalInventoryDocument(ctx context.Context, req *gen.PhysicalInventoryDocumentCreate,
	_ gen.CreatePhysicalInventoryDocumentParams) (gen.CreatePhysicalInventoryDocumentRes, error) {
	ms := make([]uuid.UUID, len(req.Items))
	for i, it := range req.Items {
		ms[i] = it.MaterialID
	}
	d, err := e.CreateCount(ctx, CountInput{PlantID: req.PlantID, StorageLocationID: req.StorageLocationID, Materials: ms})
	out, tag, err := e.oneCount(ctx, d, err)
	if err != nil {
		return nil, err
	}
	return &gen.PhysicalInventoryDocumentHeaders{ETag: tag, Location: gen.NewOptString(countPath + d.ID.String()), Response: out}, nil
}

// GetPhysicalInventoryDocument implements GET /v1/physical-inventory-documents/{id}.
func (e *Endpoints) GetPhysicalInventoryDocument(ctx context.Context, params gen.GetPhysicalInventoryDocumentParams) (gen.GetPhysicalInventoryDocumentRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	d, err := loadCount(ctx, tx, params.ID, false)
	out, tag, err := e.oneCount(ctx, d, err)
	if err != nil {
		return nil, err
	}
	return &gen.GetPhysicalInventoryDocumentOKHeaders{ETag: tag, Response: out}, nil
}

// EnterPhysicalInventoryCount implements POST /v1/physical-inventory-documents/{id}:enterCount.
func (e *Endpoints) EnterPhysicalInventoryCount(ctx context.Context, req *gen.PhysicalInventoryCountEntry,
	params gen.EnterPhysicalInventoryCountParams) (gen.EnterPhysicalInventoryCountRes, error) {
	entries := make([]CountEntry, len(req.Items))
	for i, it := range req.Items {
		entries[i] = CountEntry{ItemNo: int(it.ItemNo), Quantity: it.Quantity.Value, Unit: it.Quantity.Unit}
	}
	d, err := e.EnterCount(ctx, params.ID, versionOf(params.IfMatch), entries)
	out, tag, err := e.oneCount(ctx, d, err)
	if err != nil {
		return nil, err
	}
	return &gen.EnterPhysicalInventoryCountOKHeaders{ETag: tag, Response: out}, nil
}

// versionOf checks a count document against the version a request read.
func versionOf(ifMatch string) func(CountDocument) error {
	return func(d CountDocument) error { return etag.Check(ifMatch, d.Version) }
}

var countSort = pagination.Sort{Default: "created_at", DefaultDesc: true,
	Fields: []pagination.Field{{Name: "created_at", Column: "created_at", Type: "timestamptz"}}}

// ListPhysicalInventoryDocuments implements GET
// /v1/physical-inventory-documents: the count documents of the companies
// the caller may act in, the latest first, filtered by plant, storage
// location, status and a material one of their items counts.
func (e *Endpoints) ListPhysicalInventoryDocuments(ctx context.Context, params gen.ListPhysicalInventoryDocumentsParams) (gen.ListPhysicalInventoryDocumentsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	plant, loc, material := apitypes.Ptr[uuid.UUID](params.PlantID), apitypes.Ptr[uuid.UUID](params.StorageLocationID), apitypes.Ptr[uuid.UUID](params.MaterialID)
	scope := fmt.Sprintf("listPhysicalInventoryDocuments|%s|%s|%s|%s", plant, loc, material, params.Status.Value)
	ks, err := e.Pages.Keyset(countSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("inventory: page of count documents: %w", err)
	}
	a, err := access(ctx)
	if err != nil {
		return nil, err
	}
	query, args := ks.SQL(`SELECT * FROM (`+countSelect+` WHERE ($1 OR d.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR d.plant_id = $3)
		AND ($4::uuid IS NULL OR d.storage_location_id = $4) AND ($5 = '' OR d.status = $5)
		AND ($6::uuid IS NULL OR EXISTS (SELECT FROM inventory.count_item i WHERE i.tenant_id = d.tenant_id AND i.count_document_id = d.id
			AND i.material_id = $6))) x WHERE true`,
		[]any{a.All, a.IDs, plant, loc, params.Status.Value, material})
	rows, _ := tx.Query(ctx, query, args...)
	ds, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (CountDocument, error) { return scanCount(r) })
	if err != nil {
		return nil, fmt.Errorf("inventory: list count documents: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, ds, func(d CountDocument) (string, uuid.UUID) { return pagination.Instant(d.CreatedAt), d.ID },
		func(page []CountDocument) (refs, error) { return e.documentCountRefs(ctx, page) }, countAPI)
	if err != nil {
		return nil, err
	}
	return &gen.PhysicalInventoryDocumentPage{Items: items, NextPageToken: next}, nil
}
