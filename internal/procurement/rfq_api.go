package procurement

import (
	"context"
	"fmt"
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

// The handlers of requests for quotation (ADR-0069), in a file of their
// own beside rfq.go.

func rfqInputs(items []gen.RfqItemInput) []ItemInput {
	out := make([]ItemInput, len(items))
	for i, it := range items {
		out[i] = ItemInput{No: apitypes.Ptr[int32](it.ItemNo), MaterialKey: it.MaterialKey, Description: it.Description, Quantity: it.Quantity.Value,
			Unit: it.Quantity.Unit, AccountID: it.AccountID}
	}
	return out
}

func rfqToAPI(r RFQ) gen.Rfq {
	out := gen.Rfq{ID: r.ID, Version: r.Version, CreatedAt: r.CreatedAt, CreatedBy: r.CreatedBy, UpdatedAt: r.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(r.UpdatedBy), DocumentNumber: r.DocumentNumber, LegalEntityID: r.LegalEntityID, CompanyCode: r.CompanyCode,
		CompanyName: r.CompanyName, Currency: r.Currency,
		DueDate: r.DueDate, Status: r.Status, Items: make([]gen.RfqItem, len(r.Items)), Suppliers: make([]gen.RfqSupplier, len(r.Suppliers))}
	for i, it := range r.Items {
		out.Items[i] = gen.RfqItem{ItemNo: gen.NewOptInt32(it.No), MaterialKey: it.MaterialKey, Description: it.Description,
			Quantity: gen.Quantity{Value: it.Quantity, Unit: it.Unit}, AccountID: it.AccountID,
			AccountNumber: r.Accounts[it.AccountID].Number, AccountName: r.Accounts[it.AccountID].Name}
	}
	for i, s := range r.Suppliers {
		out.Suppliers[i] = gen.RfqSupplier{SupplierID: s.ID, SupplierNumber: s.Number, SupplierName: s.Name}
	}
	return out
}

// CreateRfq implements POST /v1/rfqs.
func (e *Endpoints) CreateRfq(ctx context.Context, req *gen.RfqCreate, _ gen.CreateRfqParams) (gen.CreateRfqRes, error) {
	r, err := e.createRFQ(ctx, NewRFQ{LegalEntityID: req.LegalEntityID, Currency: req.Currency, DueDate: req.DueDate, Items: rfqInputs(req.Items),
		Suppliers: req.SupplierIds})
	if err != nil {
		return nil, err
	}
	return &gen.RfqHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Location: gen.NewOptString("/v1/rfqs/" + r.ID.String()),
		Response: rfqToAPI(r)}, nil
}

// GetRfq implements GET /v1/rfqs/{id}.
func (e *Endpoints) GetRfq(ctx context.Context, params gen.GetRfqParams) (gen.GetRfqRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	r, err := getRFQ(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	named, err := e.named(ctx, r)
	if err != nil {
		return nil, err
	}
	return &gen.GetRfqOKHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Response: rfqToAPI(named[0])}, nil
}

var rfqSort = pagination.Sort{Default: "due_date", Fields: []pagination.Field{
	{Name: "due_date", Column: "r.due_date", Type: "date"}, {Name: "document_number", Column: "r.document_number", Type: "text"},
}}

// ListRfqs implements GET /v1/rfqs.
func (e *Endpoints) ListRfqs(ctx context.Context, params gen.ListRfqsParams) (gen.ListRfqsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, supplier := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.SupplierID)
	ks, err := e.Pages.Keyset(rfqSort, fmt.Sprintf("listRfqs|%v|%v|%s|%s", le, supplier, params.Status.Value, params.DocumentNumber.Value),
		apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(rfqSelect+` WHERE ($1 OR r.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR r.legal_entity_id = $3)
		AND ($4::uuid IS NULL OR EXISTS (SELECT FROM procurement.rfq_supplier s WHERE s.tenant_id = r.tenant_id AND s.rfq_id = r.id AND s.supplier_id = $4))
		AND ($5 = '' OR r.status = $5) AND ($6 = '' OR r.document_number = $6)`,
		[]any{access.All, access.IDs, le, supplier, params.Status.Value, params.DocumentNumber.Value})
	rows, _ := tx.Query(ctx, q, args...)
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (RFQ, error) { return scanRFQ(r) })
	if err != nil {
		return nil, fmt.Errorf("procurement: list RFQs: %w", err)
	}
	if list, err = e.named(ctx, list...); err != nil {
		return nil, err
	}
	items, next := apitypes.PageOf(ks, list, func(r RFQ) (string, uuid.UUID) {
		if ks.Field() == "document_number" {
			return r.DocumentNumber, r.ID
		}
		return r.DueDate.Format(time.DateOnly), r.ID
	}, rfqToAPI)
	return &gen.RfqPage{Items: items, NextPageToken: next}, nil
}

// PublishRfq implements POST /v1/rfqs/{id}:publish.
func (e *Endpoints) PublishRfq(ctx context.Context, params gen.PublishRfqParams) (gen.PublishRfqRes, error) {
	r, err := e.publishRFQ(ctx, params.ID, etag.IfMatch(params.IfMatch))
	if err != nil {
		return nil, err
	}
	return &gen.PublishRfqOKHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Response: rfqToAPI(r)}, nil
}
