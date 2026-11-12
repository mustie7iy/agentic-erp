package procurement

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
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
		DueDate: r.DueDate, Status: r.Status, ContractID: apitypes.OptUUID(r.ContractID), ContractDocumentNumber: apitypes.OptString(r.ContractNumber),
		Items: make([]gen.RfqItem, len(r.Items)), Suppliers: make([]gen.RfqSupplier, len(r.Suppliers))}
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
	// The generator names the identical answer of :publish and :award after the first of them.
	return &gen.AwardRfqOKHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Response: rfqToAPI(r)}, nil
}

// quotationToAPI shows quotation q beside its RFQ r, whose suppliers are
// named: each price with the RFQ item it prices and its value, the total
// and the RFQ items left out.
func quotationToAPI(r RFQ, q Quotation) gen.SupplierQuotation {
	priced, unquoted := q.priced(r)
	supplier := r.Suppliers[slices.Index(r.supplierIDs(), q.SupplierID)]
	out := gen.SupplierQuotation{ID: q.ID, Version: q.Version, CreatedAt: q.CreatedAt, CreatedBy: q.CreatedBy, UpdatedAt: q.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(q.UpdatedBy), RfqID: r.ID, RfqDocumentNumber: r.DocumentNumber, SupplierID: q.SupplierID,
		SupplierNumber: supplier.Number, SupplierName: supplier.Name, SupplierReference: q.Reference, ValidUntil: q.ValidUntil, Status: q.Status,
		Complete: len(unquoted) == 0, UnquotedItemNos: unquoted, Total: apitypes.Money(total(priced), r.Currency, r.Exponent),
		Items: make([]gen.SupplierQuotationItem, len(priced))}
	for i, it := range priced {
		value, _ := money.Scale(it.UnitPrice, it.Quantity, int64(it.PriceUnit)) // in range: recording refused it otherwise
		out.Items[i] = gen.SupplierQuotationItem{RfqItemNo: it.No, MaterialKey: it.MaterialKey, Description: it.Description,
			Quantity: gen.Quantity{Value: it.Quantity, Unit: it.Unit}, UnitPrice: apitypes.Money(it.UnitPrice, r.Currency, r.Exponent),
			PriceUnit: it.PriceUnit, Amount: apitypes.Money(value, r.Currency, r.Exponent)}
	}
	return out
}

// CreateSupplierQuotation implements POST /v1/rfqs/{id}/quotations. The
// quotation has no address of its own: Location is the RFQ's list of
// quotations filtered to its supplier, which holds this one alone.
func (e *Endpoints) CreateSupplierQuotation(ctx context.Context, req *gen.SupplierQuotationCreate, params gen.CreateSupplierQuotationParams) (gen.CreateSupplierQuotationRes, error) {
	in := NewQuotation{SupplierID: req.SupplierID, Reference: req.SupplierReference, ValidUntil: req.ValidUntil, Prices: make([]PriceInput, len(req.Items))}
	for i, it := range req.Items {
		in.Prices[i] = PriceInput{RFQItemNo: it.RfqItemNo, Price: it.UnitPrice.Amount, Currency: it.UnitPrice.Currency, PriceUnit: it.PriceUnit.Value}
	}
	r, q, err := e.recordQuotation(ctx, params.ID, in)
	if err != nil {
		return nil, err
	}
	return &gen.SupplierQuotationHeaders{ETag: gen.NewOptString(etag.Format(q.Version)),
		Location: gen.NewOptString("/v1/rfqs/" + r.ID.String() + "/quotations?supplier_id=" + q.SupplierID.String()), Response: quotationToAPI(r, q)}, nil
}

var quotationSort = pagination.Sort{Default: "created_at", Fields: []pagination.Field{
	{Name: "created_at", Column: "q.created_at", Type: "timestamptz"},
}}

// ListSupplierQuotations implements GET /v1/rfqs/{id}/quotations.
func (e *Endpoints) ListSupplierQuotations(ctx context.Context, params gen.ListSupplierQuotationsParams) (gen.ListSupplierQuotationsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	r, err := getRFQ(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	supplier := apitypes.Ptr[uuid.UUID](params.SupplierID)
	ks, err := e.Pages.Keyset(quotationSort, fmt.Sprintf("listSupplierQuotations|%s|%v", r.ID, supplier),
		apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(quotationSelect+` WHERE q.rfq_id = $1 AND ($2::uuid IS NULL OR q.supplier_id = $2)`, []any{r.ID, supplier})
	rows, _ := tx.Query(ctx, q, args...)
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Quotation, error) { return scanQuotation(row) })
	if err != nil {
		return nil, fmt.Errorf("procurement: list supplier quotations of RFQ %s: %w", r.ID, err)
	}
	named, err := e.named(ctx, r)
	if err != nil {
		return nil, err
	}
	items, next := apitypes.PageOf(ks, list, func(q Quotation) (string, uuid.UUID) { return timestamp(q.CreatedAt), q.ID },
		func(q Quotation) gen.SupplierQuotation { return quotationToAPI(named[0], q) })
	return &gen.SupplierQuotationPage{Items: items, NextPageToken: next}, nil
}

// comparisonToAPI shows a comparison: the RFQ's items with the lowest
// amount quoted for each, and one row per supplier invited.
func comparisonToAPI(c Comparison) *gen.RfqComparison {
	r := c.RFQ
	answered, ranked := c.counts()
	out := &gen.RfqComparison{RfqID: r.ID, RfqDocumentNumber: r.DocumentNumber, LegalEntityID: r.LegalEntityID, CompanyCode: r.CompanyCode,
		CompanyName: r.CompanyName, Currency: r.Currency, Status: r.Status, DueDate: r.DueDate, AsOf: c.AsOf, InvitedCount: int32(len(c.Rows)),
		AnsweredCount: answered, RankedCount: ranked, Items: make([]gen.RfqComparisonItem, len(r.Items)),
		Suppliers: make([]gen.RfqComparisonSupplier, len(c.Rows))}
	for i, it := range r.Items {
		out.Items[i] = gen.RfqComparisonItem{RfqItemNo: it.No, MaterialKey: it.MaterialKey, Description: it.Description,
			Quantity: gen.Quantity{Value: it.Quantity, Unit: it.Unit}}
		if low, ok := c.Lowest[it.No]; ok {
			out.Items[i].LowestAmount.SetTo(apitypes.Money(low, r.Currency, r.Exponent))
		}
	}
	for i, row := range c.Rows {
		out.Suppliers[i] = comparedToAPI(c, row)
	}
	return out
}

// comparedToAPI shows one supplier's row of comparison c.
func comparedToAPI(c Comparison, row Compared) gen.RfqComparisonSupplier {
	r := c.RFQ
	out := gen.RfqComparisonSupplier{SupplierID: row.Supplier.ID, SupplierNumber: row.Supplier.Number, SupplierName: row.Supplier.Name,
		Answered: row.Quotation != nil, Complete: row.Quotation != nil && len(row.Unquoted) == 0, Expired: row.Expired,
		UnquotedItemNos: row.Unquoted, Prices: make([]gen.RfqComparisonPrice, len(row.Priced))}
	if row.Rank > 0 {
		out.Rank.SetTo(row.Rank)
	}
	if q := row.Quotation; q != nil {
		out.SupplierQuotationID.SetTo(q.ID)
		out.SupplierReference.SetTo(q.Reference)
		out.ValidUntil.SetTo(q.ValidUntil)
		out.QuotationStatus.SetTo(q.Status)
		out.Total.SetTo(apitypes.Money(row.Total, r.Currency, r.Exponent))
	}
	for i, it := range row.Priced {
		out.Prices[i] = gen.RfqComparisonPrice{RfqItemNo: it.No, UnitPrice: apitypes.Money(it.UnitPrice, r.Currency, r.Exponent),
			PriceUnit: it.PriceUnit, Amount: apitypes.Money(row.Amounts[i], r.Currency, r.Exponent), Lowest: c.lowest(row, i)}
	}
	return out
}

// GetRfqComparison implements GET /v1/rfqs/{id}/comparison.
func (e *Endpoints) GetRfqComparison(ctx context.Context, params gen.GetRfqComparisonParams) (gen.GetRfqComparisonRes, error) {
	c, err := e.comparison(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	return comparisonToAPI(c), nil
}

// AwardRfq implements POST /v1/rfqs/{id}:award.
func (e *Endpoints) AwardRfq(ctx context.Context, req *gen.RfqAward, params gen.AwardRfqParams) (gen.AwardRfqRes, error) {
	r, err := e.awardRFQ(ctx, params.ID, etag.IfMatch(params.IfMatch), Award{QuotationID: req.QuotationID, ValidFrom: req.ValidFrom, ValidTo: req.ValidTo})
	if err != nil {
		return nil, err
	}
	return &gen.AwardRfqOKHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Response: rfqToAPI(r)}, nil
}
