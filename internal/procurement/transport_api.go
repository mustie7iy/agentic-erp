package procurement

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// transportRefs are what orders name: their companies, plants, storage
// locations and materials.
type transportRefs struct {
	companies map[uuid.UUID]tenancy.Entity
	units     map[uuid.UUID]tenancy.Unit
	materials map[uuid.UUID]materials.Material
}

// transportRefsOf reads what the orders name, each once.
func (e *Endpoints) transportRefsOf(ctx context.Context, os []TransportOrder) (transportRefs, error) {
	r := transportRefs{companies: map[uuid.UUID]tenancy.Entity{}}
	var units, ms []uuid.UUID
	for _, o := range os {
		if _, ok := r.companies[o.LegalEntityID]; !ok {
			le, _, err := e.LegalEntities.Entity(ctx, o.LegalEntityID) // an order's company exists
			if err != nil {
				return transportRefs{}, fmt.Errorf("procurement: company of stock transport order %s: %w", o.Number, err)
			}
			r.companies[o.LegalEntityID] = le
		}
		units = append(units, o.SupplyingPlantID, o.ReceivingPlantID)
		for _, it := range o.Items {
			units, ms = append(units, it.SupplyingStorageLocationID, it.ReceivingStorageLocationID), append(ms, it.MaterialID)
		}
	}
	var err error
	if r.units, err = e.Units.OrgUnits(ctx, units); err != nil {
		return transportRefs{}, fmt.Errorf("procurement: plants and storage locations of %d stock transport orders: %w", len(os), err)
	}
	if r.materials, err = e.Materials.Materials(ctx, ms); err != nil {
		return transportRefs{}, fmt.Errorf("procurement: materials of %d stock transport orders: %w", len(os), err)
	}
	return r, nil
}

// transportAPI is an order as the API gives it, naming what it names.
func transportAPI(o TransportOrder, r transportRefs) gen.StockTransportOrder {
	from, to := r.units[o.SupplyingPlantID], r.units[o.ReceivingPlantID]
	out := gen.StockTransportOrder{ID: o.ID, Number: o.Number, LegalEntityID: o.LegalEntityID, CompanyCode: r.companies[o.LegalEntityID].Code,
		FiscalYear: int32(o.FiscalYear), OrderDate: o.OrderDate, SupplyingPlantID: o.SupplyingPlantID, SupplyingPlantCode: from.Code,
		SupplyingPlantName: from.Name, ReceivingPlantID: o.ReceivingPlantID, ReceivingPlantCode: to.Code, ReceivingPlantName: to.Name, Status: o.Status,
		IssueDocumentID: apitypes.OptUUID(o.IssueDocumentID), IssueDocumentNumber: apitypes.OptString(o.IssueDocumentNumber),
		IssueDate: apitypes.OptDate(o.IssueDate), ReceiptDocumentID: apitypes.OptUUID(o.ReceiptDocumentID),
		ReceiptDocumentNumber: apitypes.OptString(o.ReceiptDocumentNumber), ReceiptDate: apitypes.OptDate(o.ReceiptDate),
		Items: make([]gen.StockTransportOrderItem, len(o.Items)), Version: o.Version, CreatedAt: o.CreatedAt, CreatedBy: o.CreatedBy,
		UpdatedAt: o.UpdatedAt, UpdatedBy: o.UpdatedBy}
	for i, it := range o.Items {
		m, sl, rl := r.materials[it.MaterialID], r.units[it.SupplyingStorageLocationID], r.units[it.ReceivingStorageLocationID]
		transit := "0"
		if o.Status == TransportInTransit {
			transit = it.BaseQuantity
		}
		out.Items[i] = gen.StockTransportOrderItem{ItemNo: int32(it.ItemNo), MaterialID: it.MaterialID, MaterialNumber: m.Number, MaterialDescription: m.Description,
			Quantity: gen.Quantity{Value: it.Quantity, Unit: it.Unit}, BaseQuantity: gen.Quantity{Value: it.BaseQuantity, Unit: m.BaseUnit},
			InTransitQuantity: gen.Quantity{Value: transit, Unit: m.BaseUnit}, SupplyingStorageLocationID: it.SupplyingStorageLocationID,
			SupplyingStorageLocationCode: sl.Code, SupplyingStorageLocationName: sl.Name, ReceivingStorageLocationID: it.ReceivingStorageLocationID,
			ReceivingStorageLocationCode: rl.Code, ReceivingStorageLocationName: rl.Name}
	}
	return out
}

// oneTransport is a single order as the API gives it, with its ETag, the
// version every step moves.
func (e *Endpoints) oneTransport(ctx context.Context, o TransportOrder, err error) (gen.StockTransportOrder, gen.OptString, error) {
	if err != nil {
		return gen.StockTransportOrder{}, gen.OptString{}, err
	}
	r, err := e.transportRefsOf(ctx, []TransportOrder{o})
	if err != nil {
		return gen.StockTransportOrder{}, gen.OptString{}, err
	}
	return transportAPI(o, r), gen.NewOptString(etag.Format(o.Version)), nil
}

// CreateStockTransportOrder implements POST /v1/stock-transport-orders.
func (e *Endpoints) CreateStockTransportOrder(ctx context.Context, req *gen.StockTransportOrderCreate, _ gen.CreateStockTransportOrderParams) (
	gen.CreateStockTransportOrderRes, error) {
	in := TransportInput{SupplyingPlantID: req.SupplyingPlantID, ReceivingPlantID: req.ReceivingPlantID, Items: make([]TransportItemInput, len(req.Items))}
	for i, it := range req.Items {
		in.Items[i] = TransportItemInput{MaterialID: it.MaterialID, Quantity: it.Quantity.Value, Unit: it.Quantity.Unit,
			SupplyingStorageLocationID: it.SupplyingStorageLocationID, ReceivingStorageLocationID: it.ReceivingStorageLocationID}
	}
	o, err := e.CreateTransport(ctx, in)
	out, tag, err := e.oneTransport(ctx, o, err)
	if err != nil {
		return nil, err
	}
	return &gen.StockTransportOrderHeaders{ETag: tag, Location: gen.NewOptString("/v1/stock-transport-orders/" + o.ID.String()), Response: out}, nil
}

// GetStockTransportOrder implements GET /v1/stock-transport-orders/{id}.
func (e *Endpoints) GetStockTransportOrder(ctx context.Context, params gen.GetStockTransportOrderParams) (gen.GetStockTransportOrderRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	o, err := loadTransport(ctx, tx, params.ID, false)
	out, tag, err := e.oneTransport(ctx, o, err)
	if err != nil {
		return nil, err
	}
	return &gen.GetStockTransportOrderOKHeaders{ETag: tag, Response: out}, nil
}

// PostStockTransportGoodsIssue implements POST
// /v1/stock-transport-orders/{id}:postGoodsIssue.
func (e *Endpoints) PostStockTransportGoodsIssue(ctx context.Context, params gen.PostStockTransportGoodsIssueParams) (gen.PostStockTransportGoodsIssueRes,
	error) {
	o, err := e.PostTransportIssue(ctx, params.ID, etag.IfMatch(params.IfMatch))
	out, tag, err := e.oneTransport(ctx, o, err)
	if err != nil {
		return nil, err
	}
	return &gen.CancelStockTransportOrderOKHeaders{ETag: tag, Response: out}, nil
}

// PostStockTransportGoodsReceipt implements POST
// /v1/stock-transport-orders/{id}:postGoodsReceipt.
func (e *Endpoints) PostStockTransportGoodsReceipt(ctx context.Context, params gen.PostStockTransportGoodsReceiptParams) (
	gen.PostStockTransportGoodsReceiptRes, error) {
	o, err := e.PostTransportReceipt(ctx, params.ID, etag.IfMatch(params.IfMatch))
	out, tag, err := e.oneTransport(ctx, o, err)
	if err != nil {
		return nil, err
	}
	return &gen.CancelStockTransportOrderOKHeaders{ETag: tag, Response: out}, nil
}

// CancelStockTransportOrder implements POST
// /v1/stock-transport-orders/{id}:cancel.
func (e *Endpoints) CancelStockTransportOrder(ctx context.Context, params gen.CancelStockTransportOrderParams) (gen.CancelStockTransportOrderRes, error) {
	o, err := e.CancelTransport(ctx, params.ID, etag.IfMatch(params.IfMatch))
	out, tag, err := e.oneTransport(ctx, o, err)
	if err != nil {
		return nil, err
	}
	return &gen.CancelStockTransportOrderOKHeaders{ETag: tag, Response: out}, nil
}

var transportSort = pagination.Sort{Default: "created_at", DefaultDesc: true,
	Fields: []pagination.Field{{Name: "created_at", Column: "created_at", Type: "timestamptz"}}}

// ListStockTransportOrders implements GET /v1/stock-transport-orders: the
// orders of the companies the caller may act in, the latest first,
// filtered by company, number, a plant they are from or to, and status.
func (e *Endpoints) ListStockTransportOrders(ctx context.Context, params gen.ListStockTransportOrdersParams) (gen.ListStockTransportOrdersRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, plant := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.PlantID)
	scope := fmt.Sprintf("listStockTransportOrders|%s|%s|%s|%s", le, plant, params.Status.Value, params.Number.Value)
	ks, err := e.Pages.Keyset(transportSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("procurement: page of stock transport orders: %w", err)
	}
	a, err := authz.Companies(ctx)
	if err != nil {
		return nil, fmt.Errorf("procurement: company access: %w", err)
	}
	// The orders' columns have names of their own, so the page's condition
	// and order name them.
	query, args := ks.SQL(`SELECT * FROM (`+transportSelect+` WHERE ($1 OR o.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR o.legal_entity_id = $3)
			AND ($4::uuid IS NULL OR $4 IN (o.supplying_plant_id, o.receiving_plant_id)) AND ($5 = '' OR o.status = $5) AND ($6 = '' OR o.number = $6)) x
			WHERE true`,
		[]any{a.All, a.IDs, le, plant, params.Status.Value, params.Number.Value})
	rows, _ := tx.Query(ctx, query, args...)
	os, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (TransportOrder, error) { return scanTransport(r) })
	if err != nil {
		return nil, fmt.Errorf("procurement: list stock transport orders: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, os, func(o TransportOrder) (string, uuid.UUID) { return pagination.Instant(o.CreatedAt), o.ID },
		func(page []TransportOrder) (transportRefs, error) { return e.transportRefsOf(ctx, page) }, transportAPI)
	if err != nil {
		return nil, err
	}
	return &gen.StockTransportOrderPage{Items: items, NextPageToken: next}, nil
}
