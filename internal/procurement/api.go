package procurement

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

func optInt(v *int32) gen.OptInt32 {
	if v == nil {
		return gen.OptInt32{}
	}
	return gen.NewOptInt32(*v)
}

func requisitionInputs(items []gen.RequisitionItem) []ItemInput {
	out := make([]ItemInput, len(items))
	for i, it := range items {
		out[i] = ItemInput{No: apitypes.Ptr[int32](it.ItemNo), MaterialID: apitypes.Ptr[uuid.UUID](it.MaterialID), Description: it.Description, Quantity: it.Quantity.Value,
			Unit: it.Quantity.Unit, Price: it.UnitPrice.Amount, Currency: it.UnitPrice.Currency, PriceUnit: it.PriceUnit.Value, AccountID: it.AccountID}
	}
	return out
}

func orderInputs(items []gen.PurchaseOrderItem) []ItemInput {
	out := make([]ItemInput, len(items))
	for i, it := range items {
		in := ItemInput{No: apitypes.Ptr[int32](it.ItemNo), MaterialID: apitypes.Ptr[uuid.UUID](it.MaterialID), Description: it.Description, Quantity: it.Quantity.Value,
			Unit: it.Quantity.Unit, Price: it.UnitPrice.Amount, Currency: it.UnitPrice.Currency, PriceUnit: it.PriceUnit.Value, AccountID: it.AccountID,
			TaxCode: apitypes.Ptr[string](it.TaxCode), ContractItemNo: apitypes.Ptr[int32](it.ContractItemNo)}
		if d, ok := it.OverDeliveryPct.Get(); ok {
			s := string(d)
			in.OverDelivery = &s
		}
		out[i] = in
	}
	return out
}

func contractInputs(items []gen.ContractItem) []ItemInput {
	out := make([]ItemInput, len(items))
	for i, it := range items {
		out[i] = ItemInput{No: apitypes.Ptr[int32](it.ItemNo), MaterialID: apitypes.Ptr[uuid.UUID](it.MaterialID), MaterialKey: it.MaterialKey, Description: it.Description,
			Unit: it.UomCode, Price: it.UnitPrice.Amount, Currency: it.UnitPrice.Currency, PriceUnit: it.PriceUnit.Value, AccountID: it.AccountID}
	}
	return out
}

// expanded fills expanded.supplier when expand=supplier was requested.
func (e *Endpoints) expanded(ctx context.Context, expand []string, supplier *uuid.UUID, le uuid.UUID) (gen.OptExpanded, error) {
	if supplier == nil || !slices.Contains(expand, "supplier") {
		return gen.OptExpanded{}, nil
	}
	s, ok, err := e.Suppliers.Supplier(ctx, *supplier, le)
	if err != nil || !ok {
		return gen.OptExpanded{}, err
	}
	return gen.NewOptExpanded(gen.Expanded{Supplier: gen.NewOptPartnerSummary(gen.PartnerSummary{ID: s.ID, Number: s.Number, Name: s.Name,
		CountryCode: s.CountryCode, Status: s.Status})}), nil
}

func requisitionToAPI(r Requisition) gen.Requisition {
	out := gen.Requisition{ID: r.ID, Version: r.Version, CreatedAt: r.CreatedAt, CreatedBy: r.CreatedBy, UpdatedAt: r.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(r.UpdatedBy), DocumentNumber: r.DocumentNumber, LegalEntityID: r.LegalEntityID, RequesterID: r.RequesterID,
		SupplierID: apitypes.OptUUID(r.SupplierID), ContractID: apitypes.OptUUID(r.ContractID), Currency: r.Currency, Status: r.Status,
		WorkflowRequestID: apitypes.OptUUID(r.WorkflowRequestID), TotalAmount: apitypes.Money(r.Total(), r.Currency, r.Exponent), Extensions: apitypes.EncodeObject(r.Extensions),
		Items: make([]gen.RequisitionItem, len(r.Items))}
	if r.NeededBy != nil {
		out.NeededBy.SetTo(*r.NeededBy)
	}
	for i, it := range r.Items {
		out.Items[i] = gen.RequisitionItem{ItemNo: gen.NewOptInt32(it.No), MaterialID: apitypes.OptUUID(it.MaterialID), Description: it.Description,
			Quantity: gen.Quantity{Value: it.Quantity, Unit: it.Unit}, UnitPrice: apitypes.Money(it.UnitPrice, r.Currency, r.Exponent),
			PriceUnit: gen.NewOptInt32(it.PriceUnit), AccountID: it.AccountID}
	}
	return out
}

func orderToAPI(o PurchaseOrder) gen.PurchaseOrder {
	out := gen.PurchaseOrder{ID: o.ID, Version: o.Version, CreatedAt: o.CreatedAt, CreatedBy: o.CreatedBy, UpdatedAt: o.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(o.UpdatedBy), DocumentNumber: o.DocumentNumber, LegalEntityID: o.LegalEntityID,
		PurchasingOrgID: apitypes.OptUUID(o.PurchasingOrgID), SupplierID: o.SupplierID, Currency: o.Currency, OrderDate: o.OrderDate,
		PaymentTermCode: o.PaymentTermCode, RequisitionID: apitypes.OptUUID(o.RequisitionID), ContractID: apitypes.OptUUID(o.ContractID), Status: o.Status,
		Valuation: gen.NewOptString(o.Valuation), ReleaseRequestID: apitypes.OptUUID(o.ReleaseRequestID),
		TotalAmount: apitypes.Money(o.Total(), o.Currency, o.Exponent), Extensions: apitypes.EncodeObject(o.Extensions), Items: make([]gen.PurchaseOrderItem, len(o.Items))}
	for i, it := range o.Items {
		out.Items[i] = orderItemToAPI(it, o.Currency, o.Exponent)
		out.Items[i].ReceivedQuantity = gen.NewOptQuantity(gen.Quantity{Value: it.Received, Unit: it.Unit})
		if it.ValuationPrice != nil {
			out.Items[i].ValuationPrice.SetTo(apitypes.Money(*it.ValuationPrice, o.Currency, o.Exponent))
			out.Items[i].ValuationPriceUnit.SetTo(*it.ValuationPriceUnit)
		}
	}
	return out
}

// orderItemToAPI is an order item as ordered, without what was received
// and valued, which an order change's items do not carry.
func orderItemToAPI(it Item, currency string, exp int) gen.PurchaseOrderItem {
	return gen.PurchaseOrderItem{ItemNo: gen.NewOptInt32(it.No), MaterialID: apitypes.OptUUID(it.MaterialID), Description: it.Description,
		Quantity: gen.Quantity{Value: it.Quantity, Unit: it.Unit}, UnitPrice: apitypes.Money(it.UnitPrice, currency, exp),
		PriceUnit: gen.NewOptInt32(it.PriceUnit), AccountID: it.AccountID, OverDeliveryPct: gen.NewOptDecimal(gen.Decimal(it.OverDelivery)),
		TaxCode: apitypes.OptString(it.TaxCode), ContractItemNo: optInt(it.ContractItemNo)}
}

func contractToAPI(c Contract) gen.Contract {
	out := gen.Contract{ID: c.ID, Version: c.Version, CreatedAt: c.CreatedAt, CreatedBy: c.CreatedBy, UpdatedAt: c.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(c.UpdatedBy), DocumentNumber: c.DocumentNumber, LegalEntityID: c.LegalEntityID, SupplierID: c.SupplierID,
		Currency: c.Currency, ValidFrom: c.ValidFrom, ValidTo: c.ValidTo, TargetValue: apitypes.Money(c.TargetValue, c.Currency, c.Exponent),
		ConsumedValue: apitypes.Money(c.ConsumedValue, c.Currency, c.Exponent), Status: c.Status, Items: make([]gen.ContractItem, len(c.Items))}
	for i, it := range c.Items {
		out.Items[i] = gen.ContractItem{ItemNo: gen.NewOptInt32(it.No), MaterialID: apitypes.OptUUID(it.MaterialID), MaterialKey: it.MaterialKey,
			Description: it.Description, UnitPrice: apitypes.Money(it.UnitPrice, c.Currency, c.Exponent), PriceUnit: gen.NewOptInt32(it.PriceUnit),
			UomCode: it.Unit, AccountID: it.AccountID}
	}
	return out
}

func receiptToAPI(g GoodsReceipt) gen.GoodsReceipt {
	out := gen.GoodsReceipt{ID: g.ID, CreatedAt: g.PostedAt, CreatedBy: g.PostedBy, DocumentNumber: g.DocumentNumber, LegalEntityID: g.LegalEntityID,
		PlantID: apitypes.OptUUID(g.PlantID), MovementType: g.MovementType, DocumentDate: g.DocumentDate, PostingDate: g.PostingDate,
		Reference: apitypes.OptString(g.Reference), ReversalOfID: apitypes.OptUUID(g.ReversalOfID), JournalEntryID: apitypes.OptUUID(g.JournalEntryID),
		Reason: apitypes.OptString(g.Reason),
		Items:  make([]gen.GoodsReceiptItem, len(g.Items))}
	if g.JournalEntryYear != nil {
		out.JournalEntryYear.SetTo(int32(*g.JournalEntryYear))
	}
	for i, it := range g.Items {
		out.Items[i] = gen.GoodsReceiptItem{ItemNo: gen.NewOptInt32(it.No), PurchaseOrderID: it.PurchaseOrderID, PoItemNo: it.POItemNo,
			Quantity: gen.Quantity{Value: it.Quantity, Unit: it.Unit}}
	}
	return out
}

func receiptTag(g GoodsReceipt) string {
	rep := receiptToAPI(g)
	return etag.Of(&rep)
}

// --- requisitions -----------------------------------------------------------

func (e *Endpoints) requisitionOut(ctx context.Context, r Requisition, expand []string) (gen.Requisition, error) {
	out := requisitionToAPI(r)
	var err error
	out.Expanded, err = e.expanded(ctx, expand, r.SupplierID, r.LegalEntityID)
	return out, err
}

func requisitionOK(r Requisition) *gen.UpdateRequisitionOKHeaders {
	return &gen.UpdateRequisitionOKHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Response: requisitionToAPI(r)}
}

// CreateRequisition implements POST /v1/requisitions.
func (e *Endpoints) CreateRequisition(ctx context.Context, req *gen.RequisitionCreate, _ gen.CreateRequisitionParams) (gen.CreateRequisitionRes, error) {
	r, err := e.createRequisition(ctx, NewRequisition{LegalEntityID: req.LegalEntityID, SupplierID: apitypes.Ptr[uuid.UUID](req.SupplierID),
		ContractID: apitypes.Ptr[uuid.UUID](req.ContractID), Currency: req.Currency, NeededBy: apitypes.Ptr[time.Time](req.NeededBy), Items: requisitionInputs(req.Items),
		Extensions: apitypes.DecodeObject(req.Extensions.Value)})
	if err != nil {
		return nil, err
	}
	return &gen.RequisitionHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Location: gen.NewOptString("/v1/requisitions/" + r.ID.String()),
		Response: requisitionToAPI(r)}, nil
}

// GetRequisition implements GET /v1/requisitions/{id}.
func (e *Endpoints) GetRequisition(ctx context.Context, params gen.GetRequisitionParams) (gen.GetRequisitionRes, error) {
	r, err := GetRequisition(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	out, err := e.requisitionOut(ctx, r, params.Expand)
	if err != nil {
		return nil, err
	}
	return &gen.GetRequisitionOKHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Response: out}, nil
}

var requisitionSort = pagination.Sort{Default: "created_at", DefaultDesc: true, Fields: []pagination.Field{
	{Name: "created_at", Column: "r.created_at", Type: "timestamptz"}, {Name: "document_number", Column: "r.document_number", Type: "text"},
}}

func timestamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// ListRequisitions implements GET /v1/requisitions.
func (e *Endpoints) ListRequisitions(ctx context.Context, params gen.ListRequisitionsParams) (gen.ListRequisitionsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, requester := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.RequesterID)
	scope := fmt.Sprintf("listRequisitions|%s|%s|%s|%s", le, params.Status.Value, requester, params.DocumentNumber.Value)
	ks, err := e.Pages.Keyset(requisitionSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(requisitionSelect+` WHERE ($1 OR r.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR r.legal_entity_id = $3)
		AND ($4 = '' OR r.status = $4) AND ($5::uuid IS NULL OR r.requester_id = $5) AND ($6 = '' OR r.document_number = $6)`,
		[]any{access.All, access.IDs, le, params.Status.Value, requester, params.DocumentNumber.Value})
	rows, _ := tx.Query(ctx, q, args...)
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Requisition, error) { return scanRequisition(r) })
	if err != nil {
		return nil, fmt.Errorf("procurement: list requisitions: %w", err)
	}
	list, next := pagination.Page(ks, list, func(r Requisition) (string, uuid.UUID) {
		if ks.Field() == "document_number" {
			return r.DocumentNumber, r.ID
		}
		return timestamp(r.CreatedAt), r.ID
	})
	page := gen.RequisitionPage{Items: make([]gen.Requisition, len(list))}
	for i, r := range list {
		if page.Items[i], err = e.requisitionOut(ctx, r, params.Expand); err != nil {
			return nil, err
		}
	}
	if next != "" {
		page.NextPageToken.SetTo(next)
	}
	return &gen.RequisitionPageHeaders{Response: page}, nil
}

// UpdateRequisition implements PATCH /v1/requisitions/{id}.
func (e *Endpoints) UpdateRequisition(ctx context.Context, req *gen.RequisitionUpdate, params gen.UpdateRequisitionParams) (gen.UpdateRequisitionRes, error) {
	ch := RequisitionChange{Supplier: req.SupplierID.Set, Contract: req.ContractID.Set, NeededBy: req.NeededBy.Set, Currency: apitypes.Ptr[string](req.Currency),
		Extensions: apitypes.DecodeObject(req.Extensions.Value)}
	if req.SupplierID.Set && !req.SupplierID.Null {
		ch.SupplierID = &req.SupplierID.Value
	}
	if req.ContractID.Set && !req.ContractID.Null {
		ch.ContractID = &req.ContractID.Value
	}
	if req.NeededBy.Set && !req.NeededBy.Null {
		ch.NeededByDate = &req.NeededBy.Value
	}
	if req.Items != nil {
		ch.Items = requisitionInputs(req.Items)
	}
	r, err := e.updateRequisition(ctx, params.ID, etag.IfMatch(params.IfMatch), ch)
	if err != nil {
		return nil, err
	}
	return requisitionOK(r), nil
}

// SubmitRequisition implements POST /v1/requisitions/{id}:submit.
func (e *Endpoints) SubmitRequisition(ctx context.Context, params gen.SubmitRequisitionParams) (gen.SubmitRequisitionRes, error) {
	r, err := e.submitRequisition(ctx, params.ID, etag.IfMatch(params.IfMatch))
	if err != nil {
		return nil, err
	}
	return requisitionOK(r), nil
}

// CancelRequisition implements POST /v1/requisitions/{id}:cancel.
func (e *Endpoints) CancelRequisition(ctx context.Context, params gen.CancelRequisitionParams) (gen.CancelRequisitionRes, error) {
	r, err := e.cancelRequisition(ctx, params.ID, etag.IfMatch(params.IfMatch))
	if err != nil {
		return nil, err
	}
	return requisitionOK(r), nil
}

// --- contracts --------------------------------------------------------------

func contractOK(c Contract) *gen.UpdateContractOKHeaders {
	return &gen.UpdateContractOKHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Response: contractToAPI(c)}
}

// CreateContract implements POST /v1/contracts.
func (e *Endpoints) CreateContract(ctx context.Context, req *gen.ContractCreate, _ gen.CreateContractParams) (gen.CreateContractRes, error) {
	c, err := e.createContract(ctx, NewContract{LegalEntityID: req.LegalEntityID, SupplierID: req.SupplierID, Currency: req.Currency,
		ValidFrom: req.ValidFrom, ValidTo: req.ValidTo, Target: req.TargetValue.Amount, TargetCurrency: req.TargetValue.Currency,
		Items: contractInputs(req.Items)})
	if err != nil {
		return nil, err
	}
	return &gen.ContractHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Location: gen.NewOptString("/v1/contracts/" + c.ID.String()),
		Response: contractToAPI(c)}, nil
}

// GetContract implements GET /v1/contracts/{id}.
func (e *Endpoints) GetContract(ctx context.Context, params gen.GetContractParams) (gen.GetContractRes, error) {
	c, err := GetContract(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	return &gen.GetContractOKHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Response: contractToAPI(c)}, nil
}

var contractSort = pagination.Sort{Default: "valid_from", Fields: []pagination.Field{
	{Name: "valid_from", Column: "c.valid_from", Type: "date"}, {Name: "document_number", Column: "c.document_number", Type: "text"},
}}

// ListContracts implements GET /v1/contracts.
func (e *Endpoints) ListContracts(ctx context.Context, params gen.ListContractsParams) (gen.ListContractsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, supplier := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.SupplierID)
	ks, err := e.Pages.Keyset(contractSort, fmt.Sprintf("listContracts|%s|%s|%s", le, supplier, params.Status.Value),
		apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(contractSelect+` WHERE ($1 OR c.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR c.legal_entity_id = $3)
		AND ($4::uuid IS NULL OR c.supplier_id = $4) AND ($5 = '' OR c.status = $5)`, []any{access.All, access.IDs, le, supplier, params.Status.Value})
	rows, _ := tx.Query(ctx, q, args...)
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Contract, error) { return scanContract(r) })
	if err != nil {
		return nil, fmt.Errorf("procurement: list contracts: %w", err)
	}
	items, next := apitypes.PageOf(ks, list, func(c Contract) (string, uuid.UUID) {
		if ks.Field() == "document_number" {
			return c.DocumentNumber, c.ID
		}
		return c.ValidFrom.Format(time.DateOnly), c.ID
	}, contractToAPI)
	return &gen.ContractPageHeaders{Response: gen.ContractPage{Items: items, NextPageToken: next}}, nil
}

// UpdateContract implements PATCH /v1/contracts/{id}.
func (e *Endpoints) UpdateContract(ctx context.Context, req *gen.ContractUpdate, params gen.UpdateContractParams) (gen.UpdateContractRes, error) {
	ch := ContractChange{ValidFrom: apitypes.Ptr[time.Time](req.ValidFrom), ValidTo: apitypes.Ptr[time.Time](req.ValidTo)}
	if t, ok := req.TargetValue.Get(); ok {
		ch.Target, ch.TargetCurrency = &t.Amount, &t.Currency
	}
	if req.Items != nil {
		ch.Items = contractInputs(req.Items)
	}
	c, err := e.updateContract(ctx, params.ID, etag.IfMatch(params.IfMatch), ch)
	if err != nil {
		return nil, err
	}
	return contractOK(c), nil
}

// ReleaseContract implements POST /v1/contracts/{id}:release.
func (e *Endpoints) ReleaseContract(ctx context.Context, params gen.ReleaseContractParams) (gen.ReleaseContractRes, error) {
	c, err := changeContract(ctx, params.ID, etag.IfMatch(params.IfMatch), releaseContract, "")
	if err != nil {
		return nil, err
	}
	return contractOK(c), nil
}

// CancelContract implements POST /v1/contracts/{id}:cancel.
func (e *Endpoints) CancelContract(ctx context.Context, params gen.CancelContractParams) (gen.CancelContractRes, error) {
	c, err := changeContract(ctx, params.ID, etag.IfMatch(params.IfMatch), cancelContract, "")
	if err != nil {
		return nil, err
	}
	return contractOK(c), nil
}

// TerminateContract implements POST /v1/contracts/{id}:terminate.
func (e *Endpoints) TerminateContract(ctx context.Context, req *gen.ReasonInput, params gen.TerminateContractParams) (gen.TerminateContractRes, error) {
	c, err := changeContract(ctx, params.ID, etag.IfMatch(params.IfMatch), terminateContract, req.Reason)
	if err != nil {
		return nil, err
	}
	return contractOK(c), nil
}

// --- purchase orders --------------------------------------------------------

func orderOK(o PurchaseOrder) *gen.UpdatePurchaseOrderOKHeaders {
	return &gen.UpdatePurchaseOrderOKHeaders{ETag: gen.NewOptString(etag.Format(o.Version)), Response: orderToAPI(o)}
}

func (e *Endpoints) orderOut(ctx context.Context, o PurchaseOrder, expand []string) (gen.PurchaseOrder, error) {
	out := orderToAPI(o)
	var err error
	out.Expanded, err = e.expanded(ctx, expand, &o.SupplierID, o.LegalEntityID)
	return out, err
}

// CreatePurchaseOrder implements POST /v1/purchase-orders.
func (e *Endpoints) CreatePurchaseOrder(ctx context.Context, req *gen.PurchaseOrderCreate, _ gen.CreatePurchaseOrderParams) (gen.CreatePurchaseOrderRes, error) {
	o, err := e.createOrder(ctx, NewOrder{LegalEntityID: req.LegalEntityID, PurchasingOrgID: apitypes.Ptr[uuid.UUID](req.PurchasingOrgID),
		SupplierID: req.SupplierID, Currency: req.Currency, OrderDate: req.OrderDate, PaymentTermCode: apitypes.Ptr[string](req.PaymentTermCode),
		RequisitionID: apitypes.Ptr[uuid.UUID](req.RequisitionID), ContractID: apitypes.Ptr[uuid.UUID](req.ContractID), Items: orderInputs(req.Items),
		Extensions: apitypes.DecodeObject(req.Extensions.Value)})
	if err != nil {
		return nil, err
	}
	return &gen.PurchaseOrderHeaders{ETag: gen.NewOptString(etag.Format(o.Version)), Location: gen.NewOptString("/v1/purchase-orders/" + o.ID.String()),
		Response: orderToAPI(o)}, nil
}

// GetPurchaseOrder implements GET /v1/purchase-orders/{id}.
func (e *Endpoints) GetPurchaseOrder(ctx context.Context, params gen.GetPurchaseOrderParams) (gen.GetPurchaseOrderRes, error) {
	o, err := GetOrder(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	out, err := e.orderOut(ctx, o, params.Expand)
	if err != nil {
		return nil, err
	}
	return &gen.GetPurchaseOrderOKHeaders{ETag: gen.NewOptString(etag.Format(o.Version)), Response: out}, nil
}

var orderSort = pagination.Sort{Default: "order_date", Fields: []pagination.Field{
	{Name: "order_date", Column: "o.order_date", Type: "date"}, {Name: "document_number", Column: "o.document_number", Type: "text"},
}}

// ListPurchaseOrders implements GET /v1/purchase-orders.
func (e *Endpoints) ListPurchaseOrders(ctx context.Context, params gen.ListPurchaseOrdersParams) (gen.ListPurchaseOrdersRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, supplier := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.SupplierID)
	from, to := apitypes.Ptr[time.Time](params.OrderDateFrom), apitypes.Ptr[time.Time](params.OrderDateTo)
	scope := fmt.Sprintf("listPurchaseOrders|%s|%s|%s|%s|%v|%v", le, supplier, params.Status.Value, params.DocumentNumber.Value, from, to)
	ks, err := e.Pages.Keyset(orderSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(orderSelect+` WHERE ($1 OR o.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR o.legal_entity_id = $3)
		AND ($4::uuid IS NULL OR o.supplier_id = $4) AND ($5 = '' OR o.status = $5) AND ($6 = '' OR o.document_number = $6)
		AND ($7::date IS NULL OR o.order_date >= $7) AND ($8::date IS NULL OR o.order_date <= $8)`,
		[]any{access.All, access.IDs, le, supplier, params.Status.Value, params.DocumentNumber.Value, from, to})
	rows, _ := tx.Query(ctx, q, args...)
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (PurchaseOrder, error) { return scanOrder(r) })
	if err != nil {
		return nil, fmt.Errorf("procurement: list purchase orders: %w", err)
	}
	list, next := pagination.Page(ks, list, func(o PurchaseOrder) (string, uuid.UUID) {
		if ks.Field() == "document_number" {
			return o.DocumentNumber, o.ID
		}
		return o.OrderDate.Format(time.DateOnly), o.ID
	})
	page := gen.PurchaseOrderPage{Items: make([]gen.PurchaseOrder, len(list))}
	for i, o := range list {
		if page.Items[i], err = e.orderOut(ctx, o, params.Expand); err != nil {
			return nil, err
		}
	}
	if next != "" {
		page.NextPageToken.SetTo(next)
	}
	return &gen.PurchaseOrderPageHeaders{Response: page}, nil
}

// UpdatePurchaseOrder implements PATCH /v1/purchase-orders/{id}.
func (e *Endpoints) UpdatePurchaseOrder(ctx context.Context, req *gen.PurchaseOrderUpdate, params gen.UpdatePurchaseOrderParams) (gen.UpdatePurchaseOrderRes, error) {
	ch := OrderChange{PurchasingOrg: req.PurchasingOrgID.Set, Contract: req.ContractID.Set, SupplierID: apitypes.Ptr[uuid.UUID](req.SupplierID),
		Currency: apitypes.Ptr[string](req.Currency), OrderDate: apitypes.Ptr[time.Time](req.OrderDate), PaymentTermCode: apitypes.Ptr[string](req.PaymentTermCode),
		Extensions: apitypes.DecodeObject(req.Extensions.Value)}
	if req.PurchasingOrgID.Set && !req.PurchasingOrgID.Null {
		ch.PurchasingOrgID = &req.PurchasingOrgID.Value
	}
	if req.ContractID.Set && !req.ContractID.Null {
		ch.ContractID = &req.ContractID.Value
	}
	if req.Items != nil {
		ch.Items = orderInputs(req.Items)
	}
	o, err := e.updateOrder(ctx, params.ID, etag.IfMatch(params.IfMatch), ch)
	if err != nil {
		return nil, err
	}
	return orderOK(o), nil
}

// ReleasePurchaseOrder implements POST /v1/purchase-orders/{id}:release.
func (e *Endpoints) ReleasePurchaseOrder(ctx context.Context, params gen.ReleasePurchaseOrderParams) (gen.ReleasePurchaseOrderRes, error) {
	o, err := e.releaseOrder(ctx, params.ID, etag.IfMatch(params.IfMatch), false)
	if err != nil {
		return nil, err
	}
	return orderOK(o), nil
}

// ClosePurchaseOrder implements POST /v1/purchase-orders/{id}:close.
func (e *Endpoints) ClosePurchaseOrder(ctx context.Context, params gen.ClosePurchaseOrderParams) (gen.ClosePurchaseOrderRes, error) {
	o, err := e.closeOrder(ctx, params.ID, etag.IfMatch(params.IfMatch))
	if err != nil {
		return nil, err
	}
	return orderOK(o), nil
}

// CancelPurchaseOrder implements POST /v1/purchase-orders/{id}:cancel.
func (e *Endpoints) CancelPurchaseOrder(ctx context.Context, params gen.CancelPurchaseOrderParams) (gen.CancelPurchaseOrderRes, error) {
	o, err := e.cancelOrder(ctx, params.ID, etag.IfMatch(params.IfMatch))
	if err != nil {
		return nil, err
	}
	return orderOK(o), nil
}

// --- goods receipts ---------------------------------------------------------

// PostGoodsReceipt implements POST /v1/goods-receipts.
func (e *Endpoints) PostGoodsReceipt(ctx context.Context, req *gen.GoodsReceiptCreate, _ gen.PostGoodsReceiptParams) (gen.PostGoodsReceiptRes, error) {
	in := ReceiptInput{LegalEntityID: req.LegalEntityID, PlantID: apitypes.Ptr[uuid.UUID](req.PlantID), DocumentDate: req.DocumentDate, PostingDate: req.PostingDate,
		Reference: apitypes.Ptr[string](req.Reference), MovementType: req.MovementType.Value, Reason: apitypes.Ptr[string](req.Reason),
		Items: make([]ReceiptItem, len(req.Items))}
	for i, it := range req.Items {
		in.Items[i] = ReceiptItem{No: int32(i + 1), PurchaseOrderID: it.PurchaseOrderID, POItemNo: it.PoItemNo, Quantity: it.Quantity.Value, Unit: it.Quantity.Unit}
	}
	g, err := e.postReceipt(ctx, in)
	if err != nil {
		return nil, err
	}
	return &gen.GoodsReceiptHeaders{ETag: gen.NewOptString(receiptTag(g)), Location: gen.NewOptString("/v1/goods-receipts/" + g.ID.String()),
		Response: receiptToAPI(g)}, nil
}

// GetGoodsReceipt implements GET /v1/goods-receipts/{id}.
func (e *Endpoints) GetGoodsReceipt(ctx context.Context, params gen.GetGoodsReceiptParams) (gen.GetGoodsReceiptRes, error) {
	g, err := GetReceipt(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	return &gen.GetGoodsReceiptOKHeaders{ETag: gen.NewOptString(receiptTag(g)), Response: receiptToAPI(g)}, nil
}

var receiptSort = pagination.Sort{Default: "posting_date", Fields: []pagination.Field{
	{Name: "posting_date", Column: "m.posting_date", Type: "date"}, {Name: "document_number", Column: "m.document_number", Type: "text"},
}}

// ListGoodsReceipts implements GET /v1/goods-receipts.
func (e *Endpoints) ListGoodsReceipts(ctx context.Context, params gen.ListGoodsReceiptsParams) (gen.ListGoodsReceiptsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, order := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.PurchaseOrderID)
	from, to := apitypes.Ptr[time.Time](params.PostingDateFrom), apitypes.Ptr[time.Time](params.PostingDateTo)
	scope := fmt.Sprintf("listGoodsReceipts|%s|%s|%v|%v|%s", le, order, from, to, params.DocumentNumber.Value)
	ks, err := e.Pages.Keyset(receiptSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(receiptSelect+` WHERE ($1 OR m.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR m.legal_entity_id = $3)
		AND ($4::uuid IS NULL OR EXISTS (SELECT FROM procurement.goods_movement_item i WHERE i.tenant_id = m.tenant_id AND i.goods_movement_id = m.id
			AND i.purchase_order_id = $4))
		AND ($5::date IS NULL OR m.posting_date >= $5) AND ($6::date IS NULL OR m.posting_date <= $6) AND ($7 = '' OR m.document_number = $7)`,
		[]any{access.All, access.IDs, le, order, from, to, params.DocumentNumber.Value})
	rows, _ := tx.Query(ctx, q, args...)
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (GoodsReceipt, error) { return scanReceipt(r) })
	if err != nil {
		return nil, fmt.Errorf("procurement: list goods receipts: %w", err)
	}
	items, next := apitypes.PageOf(ks, list, func(g GoodsReceipt) (string, uuid.UUID) {
		if ks.Field() == "document_number" {
			return g.DocumentNumber, g.ID
		}
		return g.PostingDate.Format(time.DateOnly), g.ID
	}, receiptToAPI)
	return &gen.GoodsReceiptPageHeaders{Response: gen.GoodsReceiptPage{Items: items, NextPageToken: next}}, nil
}

// ReverseGoodsReceipt implements POST /v1/goods-receipts/{id}:reverse.
func (e *Endpoints) ReverseGoodsReceipt(ctx context.Context, req gen.OptGoodsReceiptReverse, params gen.ReverseGoodsReceiptParams) (gen.ReverseGoodsReceiptRes, error) {
	check := func(g GoodsReceipt) error { return etag.Match(params.IfMatch, receiptTag(g)) }
	g, err := e.reverseReceipt(ctx, params.ID, check, apitypes.Ptr[time.Time](req.Value.PostingDate))
	if err != nil {
		return nil, err
	}
	return &gen.ReverseGoodsReceiptOKHeaders{ETag: gen.NewOptString(receiptTag(g)), Response: receiptToAPI(g)}, nil
}
