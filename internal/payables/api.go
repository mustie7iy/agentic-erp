package payables

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/go-faster/jx"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/operations"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

func ifMatch(header string) func(int32) error {
	return func(v int32) error { return etag.Check(header, v) }
}

func ptr[T any](o interface{ Get() (T, bool) }) *T {
	if v, ok := o.Get(); ok {
		return &v
	}
	return nil
}

func fromExtensions(x gen.Extensions) map[string]any {
	if x == nil {
		return nil
	}
	out := make(map[string]any, len(x))
	for k, raw := range x {
		var v any
		_ = json.Unmarshal(raw, &v) // the generated decoder only passes valid JSON
		out[k] = v
	}
	return out
}

func toExtensions(m map[string]any) gen.Extensions {
	out := make(gen.Extensions, len(m))
	for k, v := range m {
		b, _ := json.Marshal(v) // values decoded from JSON always encode
		out[k] = jx.Raw(b)
	}
	return out
}

func moneyOf(minor int64, currency string, exp int) gen.Money {
	return gen.Money{Amount: money.Minor(minor).Plain(exp), Currency: currency}
}

func fromItems(items []gen.SupplierInvoiceItem) []ItemInput {
	if items == nil {
		return nil
	}
	out := make([]ItemInput, len(items))
	for i, it := range items {
		out[i] = ItemInput{PurchaseOrderID: ptr[uuid.UUID](it.PurchaseOrderID), POItemNo: ptr[int32](it.PoItemNo), Description: ptr[string](it.Description),
			Quantity: it.Quantity.Value, Unit: it.Quantity.Unit, Amount: Amount{Value: it.Amount.Amount, Currency: it.Amount.Currency},
			TaxCode: ptr[string](it.TaxCode), AccountID: ptr[uuid.UUID](it.AccountID)}
	}
	return out
}

func toAPI(v Invoice) gen.SupplierInvoice {
	out := gen.SupplierInvoice{ID: v.ID, Version: v.Version, CreatedAt: v.CreatedAt, CreatedBy: v.CreatedBy, UpdatedAt: v.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(v.UpdatedBy), DocumentNumber: v.DocumentNumber, LegalEntityID: v.LegalEntityID, SupplierID: v.SupplierID,
		SupplierReference: v.SupplierReference, InvoiceDate: v.InvoiceDate, GrossAmount: moneyOf(v.Gross, v.Currency, v.Exponent),
		TaxAmount: moneyOf(v.Tax, v.Currency, v.Exponent), Status: v.Status, Source: v.Source, Extensions: toExtensions(v.Extensions),
		Items: make([]gen.SupplierInvoiceItem, len(v.Items))}
	opt := func(id *uuid.UUID) gen.OptUUID {
		if id == nil {
			return gen.OptUUID{}
		}
		return gen.NewOptUUID(*id)
	}
	if v.PostingDate != nil {
		out.PostingDate.SetTo(*v.PostingDate)
	}
	if v.HoldReason != nil {
		out.HoldReason.SetTo(*v.HoldReason)
	}
	out.PurchaseOrderID, out.JournalEntryID, out.ReversalEntryID, out.WorkflowRequestID =
		opt(v.PurchaseOrderID), opt(v.JournalEntryID), opt(v.ReversalEntryID), opt(v.WorkflowRequestID)
	for i, it := range v.Items {
		item := gen.SupplierInvoiceItem{ItemNo: gen.NewOptInt32(it.ItemNo), PurchaseOrderID: opt(it.PurchaseOrderID),
			Quantity: gen.Quantity{Value: it.Quantity, Unit: it.Unit}, Amount: moneyOf(it.Amount, v.Currency, v.Exponent), AccountID: opt(it.AccountID)}
		if it.POItemNo != nil {
			item.PoItemNo.SetTo(*it.POItemNo)
		}
		if it.Description != nil {
			item.Description.SetTo(*it.Description)
		}
		if it.TaxCode != nil {
			item.TaxCode.SetTo(*it.TaxCode)
		}
		out.Items[i] = item
	}
	return out
}

// expand adds the supplier summary when the caller asked for it.
func (e *Endpoints) expand(ctx context.Context, v Invoice, expand []string) (gen.SupplierInvoice, error) {
	out := toAPI(v)
	if !slices.Contains(expand, "supplier") {
		return out, nil
	}
	s, _, err := e.Suppliers.Supplier(ctx, v.SupplierID, v.LegalEntityID)
	if err != nil {
		return out, err
	}
	out.Expanded.SetTo(gen.Expanded{Supplier: gen.NewOptPartnerSummary(gen.PartnerSummary{ID: s.ID, Number: s.Number, Name: s.Name,
		CountryCode: s.CountryCode, Status: s.Status})})
	return out, nil
}

func ok(v Invoice) *gen.UpdateSupplierInvoiceOKHeaders {
	return &gen.UpdateSupplierInvoiceOKHeaders{ETag: gen.NewOptString(etag.Format(v.Version)), Response: toAPI(v)}
}

func matchToAPI(m Match) gen.MatchResult {
	out := gen.MatchResult{ID: m.ID, SupplierInvoiceID: m.InvoiceID, EvaluatedAt: m.EvaluatedAt, IsClean: m.IsClean,
		Issues: make([]gen.MatchIssue, len(m.Issues)), ConfigVersion: m.ConfigVersion, EvaluatedBy: m.EvaluatedBy}
	for i, is := range m.Issues {
		issue := gen.MatchIssue{Code: is.Code, Message: is.Message}
		if is.ItemNo != nil {
			issue.ItemNo.SetTo(*is.ItemNo)
		}
		out.Issues[i] = issue
	}
	return out
}

// CreateSupplierInvoice implements POST /v1/supplier-invoices.
func (e *Endpoints) CreateSupplierInvoice(ctx context.Context, req *gen.SupplierInvoiceCreate, _ gen.CreateSupplierInvoiceParams) (gen.CreateSupplierInvoiceRes, error) {
	v, err := e.CreateInvoice(ctx, Input{LegalEntityID: req.LegalEntityID, SupplierID: req.SupplierID, SupplierReference: req.SupplierReference,
		InvoiceDate: req.InvoiceDate, PostingDate: ptr[time.Time](req.PostingDate), Gross: Amount{req.GrossAmount.Amount, req.GrossAmount.Currency},
		Tax: Amount{req.TaxAmount.Amount, req.TaxAmount.Currency}, PurchaseOrderID: ptr[uuid.UUID](req.PurchaseOrderID), Items: fromItems(req.Items),
		Extensions: fromExtensions(req.Extensions.Value)}, req.Source.Or("manual"))
	if err != nil {
		return nil, err
	}
	return &gen.SupplierInvoiceHeaders{ETag: gen.NewOptString(etag.Format(v.Version)), Location: gen.NewOptString("/v1/supplier-invoices/" + v.ID.String()),
		Response: toAPI(v)}, nil
}

// CaptureSupplierInvoice implements POST /v1/supplier-invoices:capture.
func (e *Endpoints) CaptureSupplierInvoice(ctx context.Context, req *gen.SupplierInvoiceCaptureMultipart, _ gen.CaptureSupplierInvoiceParams) (gen.CaptureSupplierInvoiceRes, error) {
	op, err := e.CaptureInvoice(ctx, req.LegalEntityID, []byte(req.Document))
	if err != nil {
		return nil, err
	}
	return &gen.OperationHeaders{Location: gen.NewOptString("/v1/operations/" + op.ID.String()), Response: operations.ToAPI(op)}, nil
}

// GetSupplierInvoiceDocument implements GET /v1/supplier-invoices/{id}/document.
func (e *Endpoints) GetSupplierInvoiceDocument(ctx context.Context, params gen.GetSupplierInvoiceDocumentParams) (gen.GetSupplierInvoiceDocumentRes, error) {
	content, _, err := e.Document(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	return &gen.GetSupplierInvoiceDocumentOKHeaders{Response: gen.GetSupplierInvoiceDocumentOK{Data: bytes.NewReader(content)}}, nil
}

// GetSupplierInvoice implements GET /v1/supplier-invoices/{id}.
func (e *Endpoints) GetSupplierInvoice(ctx context.Context, params gen.GetSupplierInvoiceParams) (gen.GetSupplierInvoiceRes, error) {
	v, err := GetInvoice(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	rep, err := e.expand(ctx, v, params.Expand)
	if err != nil {
		return nil, err
	}
	return &gen.GetSupplierInvoiceOKHeaders{ETag: gen.NewOptString(etag.Format(v.Version)), Response: rep}, nil
}

var invoiceSort = pagination.Sort{Default: "invoice_date", Fields: []pagination.Field{
	{Name: "invoice_date", Column: "i.invoice_date", Type: "date"}, {Name: "document_number", Column: "i.document_number", Type: "text"},
}}

// ListSupplierInvoices implements GET /v1/supplier-invoices.
func (e *Endpoints) ListSupplierInvoices(ctx context.Context, params gen.ListSupplierInvoicesParams) (gen.ListSupplierInvoicesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, sup, po := ptr[uuid.UUID](params.LegalEntityID), ptr[uuid.UUID](params.SupplierID), ptr[uuid.UUID](params.PurchaseOrderID)
	from, to := ptr[time.Time](params.InvoiceDateFrom), ptr[time.Time](params.InvoiceDateTo)
	scope := fmt.Sprintf("listSupplierInvoices|%s|%s|%s|%s|%s|%v|%v", le, sup, params.Status.Value, params.DocumentNumber.Value, po, from, to)
	ks, err := e.Pages.Keyset(invoiceSort, scope, pagination.Request{OrderBy: params.OrderBy.Value, PageSize: params.PageSize.Value,
		PageSizeSet: params.PageSize.Set, PageToken: params.PageToken.Value, PageTokenSet: params.PageToken.Set})
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(invoiceSelect+` WHERE ($1 OR i.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR i.legal_entity_id = $3)
		AND ($4::uuid IS NULL OR i.supplier_id = $4) AND ($5 = '' OR i.status = $5) AND ($6 = '' OR i.document_number = $6)
		AND ($7::uuid IS NULL OR i.purchase_order_id = $7) AND ($8::date IS NULL OR i.invoice_date >= $8) AND ($9::date IS NULL OR i.invoice_date <= $9)`,
		[]any{access.All, access.IDs, le, sup, params.Status.Value, params.DocumentNumber.Value, po, from, to})
	rows, _ := tx.Query(ctx, q, args...)
	invoices, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Invoice, error) { return scanInvoice(r) })
	if err != nil {
		return nil, err
	}
	invoices, next := pagination.Page(ks, invoices, func(v Invoice) (string, uuid.UUID) {
		if ks.Field() == "document_number" {
			return v.DocumentNumber, v.ID
		}
		return v.InvoiceDate.Format(time.DateOnly), v.ID
	})
	page := gen.SupplierInvoicePage{Items: make([]gen.SupplierInvoice, len(invoices))}
	for i, v := range invoices {
		if page.Items[i], err = e.expand(ctx, v, params.Expand); err != nil {
			return nil, err
		}
	}
	if next != "" {
		page.NextPageToken.SetTo(next)
	}
	return &gen.SupplierInvoicePageHeaders{Response: page}, nil
}

// UpdateSupplierInvoice implements PATCH /v1/supplier-invoices/{id}.
func (e *Endpoints) UpdateSupplierInvoice(ctx context.Context, req *gen.SupplierInvoiceUpdate, params gen.UpdateSupplierInvoiceParams) (gen.UpdateSupplierInvoiceRes, error) {
	ch := Change{SupplierReference: ptr[string](req.SupplierReference), InvoiceDate: ptr[time.Time](req.InvoiceDate), Items: fromItems(req.Items),
		Extensions: fromExtensions(req.Extensions.Value)}
	if req.PostingDate.Set {
		d := ptr[time.Time](req.PostingDate)
		ch.PostingDate = &d
	}
	if req.PurchaseOrderID.Set {
		po := ptr[uuid.UUID](req.PurchaseOrderID)
		ch.PurchaseOrderID = &po
	}
	if g, ok := req.GrossAmount.Get(); ok {
		ch.Gross = &Amount{g.Amount, g.Currency}
	}
	if t, ok := req.TaxAmount.Get(); ok {
		ch.Tax = &Amount{t.Amount, t.Currency}
	}
	v, err := e.UpdateInvoice(ctx, params.ID, ifMatch(params.IfMatch), ch)
	if err != nil {
		return nil, err
	}
	return ok(v), nil
}

// MatchSupplierInvoice implements POST /v1/supplier-invoices/{id}:match.
func (e *Endpoints) MatchSupplierInvoice(ctx context.Context, params gen.MatchSupplierInvoiceParams) (gen.MatchSupplierInvoiceRes, error) {
	m, err := e.MatchInvoice(ctx, params.ID, ifMatch(params.IfMatch))
	if err != nil {
		return nil, err
	}
	rep := matchToAPI(m)
	return &gen.MatchSupplierInvoiceOKHeaders{ETag: gen.NewOptString(etag.Of(&rep)), Response: rep}, nil
}

// PostSupplierInvoice implements POST /v1/supplier-invoices/{id}:post.
func (e *Endpoints) PostSupplierInvoice(ctx context.Context, req gen.OptSupplierInvoicePost, params gen.PostSupplierInvoiceParams) (gen.PostSupplierInvoiceRes, error) {
	v, err := e.PostInvoice(ctx, params.ID, ifMatch(params.IfMatch), ptr[time.Time](req.Value.PostingDate))
	if err != nil {
		return nil, err
	}
	return ok(v), nil
}

// HoldSupplierInvoice implements POST /v1/supplier-invoices/{id}:hold.
func (e *Endpoints) HoldSupplierInvoice(ctx context.Context, req *gen.ReasonInput, params gen.HoldSupplierInvoiceParams) (gen.HoldSupplierInvoiceRes, error) {
	v, err := HoldInvoice(ctx, params.ID, ifMatch(params.IfMatch), req.Reason)
	if err != nil {
		return nil, err
	}
	return ok(v), nil
}

// ReleaseSupplierInvoice implements POST /v1/supplier-invoices/{id}:release.
func (e *Endpoints) ReleaseSupplierInvoice(ctx context.Context, req gen.OptDecisionInput, params gen.ReleaseSupplierInvoiceParams) (gen.ReleaseSupplierInvoiceRes, error) {
	v, err := e.ReleaseInvoice(ctx, params.ID, ifMatch(params.IfMatch), req.Value.Comment.Value)
	if err != nil {
		return nil, err
	}
	return ok(v), nil
}

// CancelSupplierInvoice implements POST /v1/supplier-invoices/{id}:cancel.
func (e *Endpoints) CancelSupplierInvoice(ctx context.Context, req *gen.ReasonInput, params gen.CancelSupplierInvoiceParams) (gen.CancelSupplierInvoiceRes, error) {
	v, err := e.CancelInvoice(ctx, params.ID, ifMatch(params.IfMatch), req.Reason)
	if err != nil {
		return nil, err
	}
	return ok(v), nil
}

// ReverseSupplierInvoice implements POST /v1/supplier-invoices/{id}:reverse.
func (e *Endpoints) ReverseSupplierInvoice(ctx context.Context, req *gen.SupplierInvoiceReverse, params gen.ReverseSupplierInvoiceParams) (gen.ReverseSupplierInvoiceRes, error) {
	v, err := e.ReverseInvoice(ctx, params.ID, ifMatch(params.IfMatch), req.Reason, ptr[time.Time](req.PostingDate))
	if err != nil {
		return nil, err
	}
	return ok(v), nil
}

// GetMatchResult implements GET /v1/match-results/{id}.
func (e *Endpoints) GetMatchResult(ctx context.Context, params gen.GetMatchResultParams) (gen.GetMatchResultRes, error) {
	m, err := GetMatch(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	rep := matchToAPI(m)
	return &gen.MatchResultHeaders{ETag: gen.NewOptString(etag.Of(&rep)), Response: rep}, nil
}

var matchSort = pagination.Sort{Default: "evaluated_at", DefaultDesc: true, Fields: []pagination.Field{
	{Name: "evaluated_at", Column: "evaluated_at", Type: "timestamptz"},
}}

// ListMatchResults implements GET /v1/supplier-invoices/{id}/match-results,
// newest first.
func (e *Endpoints) ListMatchResults(ctx context.Context, params gen.ListMatchResultsParams) (gen.ListMatchResultsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	ks, err := e.Pages.Keyset(matchSort, "listMatchResults|"+params.ID.String(), pagination.Request{PageSize: params.PageSize.Value,
		PageSizeSet: params.PageSize.Set, PageToken: params.PageToken.Value, PageTokenSet: params.PageToken.Set})
	if err != nil {
		return nil, err
	}
	if _, err := get(ctx, tx, params.ID, false); err != nil {
		return nil, err
	}
	q, args := ks.SQL(`SELECT `+matchColumns+` FROM payables.match_result WHERE supplier_invoice_id = $1`, []any{params.ID})
	rows, _ := tx.Query(ctx, q, args...)
	matches, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Match, error) { return scanMatch(r) })
	if err != nil {
		return nil, err
	}
	matches, next := pagination.Page(ks, matches, func(m Match) (string, uuid.UUID) {
		return m.EvaluatedAt.UTC().Format("2006-01-02T15:04:05.999999Z07:00"), m.ID
	})
	page := gen.MatchResultPage{Items: make([]gen.MatchResult, len(matches))}
	for i, m := range matches {
		page.Items[i] = matchToAPI(m)
	}
	if next != "" {
		page.NextPageToken.SetTo(next)
	}
	return &gen.MatchResultPageHeaders{Response: page}, nil
}
