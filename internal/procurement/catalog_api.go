package procurement

import (
	"cmp"
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// catalogItemToAPI is a catalog item with its supplier's number and name
// beside the id (ADR-0071 §3), as a payment shows its supplier's.
func catalogItemToAPI(c CatalogItem, names map[uuid.UUID]partner.Named) gen.CatalogItem {
	return gen.CatalogItem{ID: c.ID, Version: c.Version, CreatedAt: c.CreatedAt, CreatedBy: c.CreatedBy, UpdatedAt: c.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(c.UpdatedBy), LegalEntityID: c.LegalEntityID, SupplierID: c.SupplierID,
		SupplierNumber: names[c.SupplierID].Number, SupplierName: names[c.SupplierID].Name, ItemKey: c.ItemKey, Description: c.Description,
		Unit: c.Unit, UnitPrice: apitypes.Money(c.UnitPrice, c.Currency, c.Exponent), PriceUnit: c.PriceUnit,
		ContractID: apitypes.OptUUID(c.ContractID), ContractNumber: apitypes.OptString(c.ContractNumber),
		ContractItemNo: apitypes.OptInt32(c.ContractItemNo), Status: c.Status}
}

// catalogSuppliers reads the numbers and names of the items' suppliers in
// one call.
func (e *Endpoints) catalogSuppliers(ctx context.Context, items []CatalogItem) (map[uuid.UUID]partner.Named, error) {
	ids := make([]uuid.UUID, len(items))
	for i, c := range items {
		ids[i] = c.SupplierID
	}
	return e.SupplierNames.Names(ctx, ids)
}

// catalogItemOut is one catalog item as the API shows it.
func (e *Endpoints) catalogItemOut(ctx context.Context, c CatalogItem) (gen.CatalogItem, error) {
	names, err := e.catalogSuppliers(ctx, []CatalogItem{c})
	if err != nil {
		return gen.CatalogItem{}, err
	}
	return catalogItemToAPI(c, names), nil
}

// CreateCatalogItem implements POST /v1/catalog-items.
func (e *Endpoints) CreateCatalogItem(ctx context.Context, req *gen.CatalogItemCreate, _ gen.CreateCatalogItemParams) (gen.CreateCatalogItemRes, error) {
	c, err := e.createCatalogItem(ctx, NewCatalogItem{LegalEntityID: req.LegalEntityID, SupplierID: req.SupplierID, ItemKey: req.ItemKey,
		Description: req.Description, Unit: req.Unit, Price: req.UnitPrice.Amount, Currency: req.UnitPrice.Currency, PriceUnit: req.PriceUnit.Value,
		ContractID: apitypes.Ptr[uuid.UUID](req.ContractID), ContractItemNo: apitypes.Ptr[int32](req.ContractItemNo)})
	if err != nil {
		return nil, err
	}
	out, err := e.catalogItemOut(ctx, c)
	if err != nil {
		return nil, err
	}
	return &gen.CatalogItemHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Location: gen.NewOptString("/v1/catalog-items/" + c.ID.String()),
		Response: out}, nil
}

// GetCatalogItem implements GET /v1/catalog-items/{id}.
func (e *Endpoints) GetCatalogItem(ctx context.Context, params gen.GetCatalogItemParams) (gen.GetCatalogItemRes, error) {
	c, err := GetCatalogItem(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	out, err := e.catalogItemOut(ctx, c)
	if err != nil {
		return nil, err
	}
	return &gen.GetCatalogItemOKHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Response: out}, nil
}

// catalogSort orders the search: by item key, or by when an item was
// loaded; the keyset ends in the id, so items with one key (of several
// suppliers) keep their order from page to page.
var catalogSort = pagination.Sort{Default: "item_key", Fields: []pagination.Field{
	{Name: "item_key", Column: "ci.item_key", Type: "text"}, {Name: "created_at", Column: "ci.created_at", Type: "timestamptz"},
}}

// ListCatalogItems implements GET /v1/catalog-items: the search of one
// company's catalog (ADR-0071 §3). The text is matched in the stored
// lower-case key and description; active items unless a status is asked
// for.
func (e *Endpoints) ListCatalogItems(ctx context.Context, params gen.ListCatalogItemsParams) (gen.ListCatalogItemsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	supplier, status := apitypes.Ptr[uuid.UUID](params.SupplierID), cmp.Or(params.Status.Value, Active)
	scope := fmt.Sprintf("listCatalogItems|%s|%s|%q|%s", params.LegalEntityID, supplier, params.Text.Value, status)
	ks, err := e.Pages.Keyset(catalogSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(catalogSelect+` WHERE ($1 OR ci.legal_entity_id = ANY($2)) AND ci.legal_entity_id = $3
		AND ($4::uuid IS NULL OR ci.supplier_id = $4) AND ($5 = '' OR strpos(ci.search_text, lower($5)) > 0) AND ci.status = $6`,
		[]any{access.All, access.IDs, params.LegalEntityID, supplier, params.Text.Value, status})
	rows, _ := tx.Query(ctx, q, args...)
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (CatalogItem, error) { return scanCatalogItem(r) })
	if err != nil {
		return nil, fmt.Errorf("procurement: search the catalog of legal entity %s: %w", params.LegalEntityID, err)
	}
	items, next, err := apitypes.PageWith(ks, list, func(c CatalogItem) (string, uuid.UUID) {
		if ks.Field() == "created_at" {
			return timestamp(c.CreatedAt), c.ID
		}
		return c.ItemKey, c.ID
	}, func(page []CatalogItem) (map[uuid.UUID]partner.Named, error) { return e.catalogSuppliers(ctx, page) }, catalogItemToAPI)
	if err != nil {
		return nil, err
	}
	return &gen.CatalogItemPage{Items: items, NextPageToken: next}, nil
}

// RetireCatalogItem implements POST /v1/catalog-items/{id}:retire.
func (e *Endpoints) RetireCatalogItem(ctx context.Context, params gen.RetireCatalogItemParams) (gen.RetireCatalogItemRes, error) {
	c, err := e.retireCatalogItem(ctx, params.ID, etag.IfMatch(params.IfMatch))
	if err != nil {
		return nil, err
	}
	out, err := e.catalogItemOut(ctx, c)
	if err != nil {
		return nil, err
	}
	return &gen.RetireCatalogItemOKHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Response: out}, nil
}
