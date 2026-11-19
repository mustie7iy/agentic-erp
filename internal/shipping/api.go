package shipping

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/sales"
)

// deliveryAPI is a delivery as the API gives it, naming what it names.
func deliveryAPI(d Delivery, r refs) gen.OutboundDelivery {
	plant, loc := r.units[d.PlantID], r.units[d.StorageLocationID]
	out := gen.OutboundDelivery{ID: d.ID, Number: d.Number, LegalEntityID: d.LegalEntityID, CompanyCode: r.companies[d.LegalEntityID].Code,
		FiscalYear: int32(d.FiscalYear), SalesOrderID: d.OrderID, SalesOrderNumber: d.OrderNumber, CustomerID: d.CustomerID,
		CustomerNumber: d.CustomerNumber, PlantID: d.PlantID, PlantCode: plant.Code, PlantName: plant.Name, StorageLocationID: d.StorageLocationID,
		StorageLocationCode: loc.Code, StorageLocationName: loc.Name, Status: d.Status, PickingStatus: d.PickingStatus(),
		Items: make([]gen.OutboundDeliveryItem, len(d.Items)), Version: d.Version, CreatedAt: d.CreatedAt, CreatedBy: d.CreatedBy,
		UpdatedAt: d.UpdatedAt, UpdatedBy: d.UpdatedBy}
	var total money.Minor
	for i, it := range d.Items {
		out.Items[i] = gen.OutboundDeliveryItem{ItemNo: it.ItemNo, OrderItemNo: it.OrderItemNo, MaterialID: it.MaterialID,
			MaterialNumber: r.materials[it.MaterialID].Number, Description: it.Description, Quantity: gen.Quantity{Value: it.Quantity, Unit: it.Unit},
			PickedQuantity: gen.Quantity{Value: it.Picked, Unit: it.Unit}}
		if it.Cost != nil {
			out.Items[i].Cost = gen.NewOptMoney(apitypes.Money(*it.Cost, d.Currency, d.Exponent))
			total += *it.Cost // each a part of one stock's value, at most 2^63 minor units together (inventory.valuation.value is a bigint)
		}
	}
	if d.Status == GoodsIssued {
		out.GoodsIssueDate = apitypes.OptDate(d.GoodsIssueDate)
		out.MaterialDocumentID, out.MaterialDocumentNumber = apitypes.OptUUID(d.MaterialDocumentID), apitypes.OptString(d.MaterialDocumentNumber)
		out.CostOfGoodsSold = gen.NewOptMoney(apitypes.Money(total, d.Currency, d.Exponent))
	}
	return out
}

// one is a single delivery as the API gives it, with its ETag, the
// version every change moves.
func (e *Endpoints) one(ctx context.Context, d Delivery, err error) (gen.OutboundDelivery, gen.OptString, error) {
	if err != nil {
		return gen.OutboundDelivery{}, gen.OptString{}, err
	}
	r, err := e.refs(ctx, []Delivery{d})
	if err != nil {
		return gen.OutboundDelivery{}, gen.OptString{}, err
	}
	return deliveryAPI(d, r), gen.NewOptString(etag.Format(d.Version)), nil
}

// CreateOutboundDelivery implements POST /v1/outbound-deliveries.
func (e *Endpoints) CreateOutboundDelivery(ctx context.Context, req *gen.OutboundDeliveryCreate, _ gen.CreateOutboundDeliveryParams) (gen.CreateOutboundDeliveryRes,
	error) {
	in := Input{OrderID: req.SalesOrderID, PlantID: req.PlantID, StorageLocationID: req.StorageLocationID}
	if req.Items != nil {
		in.Items = make([]sales.ItemQuantity, len(req.Items))
		for i, it := range req.Items {
			in.Items[i] = sales.ItemQuantity{ItemNo: it.OrderItemNo, Quantity: it.Quantity.Value, Unit: it.Quantity.Unit}
		}
	}
	d, err := e.Create(ctx, in)
	out, tag, err := e.one(ctx, d, err)
	if err != nil {
		return nil, err
	}
	return &gen.OutboundDeliveryHeaders{ETag: tag, Location: gen.NewOptString("/v1/outbound-deliveries/" + d.ID.String()), Response: out}, nil
}

// GetOutboundDelivery implements GET /v1/outbound-deliveries/{id}.
func (e *Endpoints) GetOutboundDelivery(ctx context.Context, params gen.GetOutboundDeliveryParams) (gen.GetOutboundDeliveryRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	d, err := accessible(ctx, tx, params.ID, false)
	out, tag, err := e.one(ctx, d, err)
	if err != nil {
		return nil, err
	}
	return &gen.GetOutboundDeliveryOKHeaders{ETag: tag, Response: out}, nil
}

// PickOutboundDelivery implements POST /v1/outbound-deliveries/{id}:pick.
func (e *Endpoints) PickOutboundDelivery(ctx context.Context, req *gen.OutboundDeliveryPick, params gen.PickOutboundDeliveryParams) (gen.PickOutboundDeliveryRes,
	error) {
	in := make([]PickInput, len(req.Items))
	for i, it := range req.Items {
		in[i] = PickInput{ItemNo: it.ItemNo, Quantity: it.Quantity.Value, Unit: it.Quantity.Unit}
	}
	d, err := e.Pick(ctx, params.ID, etag.IfMatch(params.IfMatch), in)
	out, tag, err := e.one(ctx, d, err)
	if err != nil {
		return nil, err
	}
	return &gen.PickOutboundDeliveryOKHeaders{ETag: tag, Response: out}, nil
}

// PostOutboundDeliveryGoodsIssue implements POST
// /v1/outbound-deliveries/{id}:postGoodsIssue.
func (e *Endpoints) PostOutboundDeliveryGoodsIssue(ctx context.Context, params gen.PostOutboundDeliveryGoodsIssueParams) (
	gen.PostOutboundDeliveryGoodsIssueRes, error) {
	d, err := e.PostGoodsIssue(ctx, params.ID, etag.IfMatch(params.IfMatch))
	out, tag, err := e.one(ctx, d, err)
	if err != nil {
		return nil, err
	}
	return &gen.PickOutboundDeliveryOKHeaders{ETag: tag, Response: out}, nil
}

var deliverySort = pagination.Sort{Default: "created_at", DefaultDesc: true,
	Fields: []pagination.Field{{Name: "created_at", Column: "created_at", Type: "timestamptz"}}}

// ListOutboundDeliveries implements GET /v1/outbound-deliveries: the
// deliveries of the companies the caller may act in, the latest first,
// filtered by company, sales order and status.
func (e *Endpoints) ListOutboundDeliveries(ctx context.Context, params gen.ListOutboundDeliveriesParams) (gen.ListOutboundDeliveriesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, order := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.SalesOrderID)
	scope := fmt.Sprintf("listOutboundDeliveries|%s|%s|%s", le, order, params.Status.Value)
	ks, err := e.Pages.Keyset(deliverySort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("shipping: page of outbound deliveries: %w", err)
	}
	a, err := authz.Companies(ctx)
	if err != nil {
		return nil, fmt.Errorf("shipping: company access: %w", err)
	}
	// The deliveries' columns have names of their own, so the page's
	// condition and order name them.
	query, args := ks.SQL(`SELECT * FROM (`+deliverySelect+` WHERE ($1 OR d.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR d.legal_entity_id = $3)
			AND ($4::uuid IS NULL OR d.sales_order_id = $4) AND ($5 = '' OR d.status = $5)) x WHERE true`,
		[]any{a.All, a.IDs, le, order, params.Status.Value})
	rows, _ := tx.Query(ctx, query, args...)
	ds, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Delivery, error) { return scanDelivery(r) })
	if err != nil {
		return nil, fmt.Errorf("shipping: list outbound deliveries: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, ds, func(d Delivery) (string, uuid.UUID) { return pagination.Instant(d.CreatedAt), d.ID },
		func(page []Delivery) (refs, error) { return e.refs(ctx, page) }, deliveryAPI)
	if err != nil {
		return nil, err
	}
	return &gen.OutboundDeliveryPage{Items: items, NextPageToken: next}, nil
}
