package assets

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
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// sourcePayables is the source context of a supplier invoice's entry.
const sourcePayables = "payables"

// docView is what assets documents are shown with: their companies, the
// minor-unit digits of their currencies, the ledgers in order, the
// supplier invoices they came from, and the offset accounts of manual
// acquisitions.
type docView struct {
	companies map[uuid.UUID]Company
	exponents map[string]int
	ledgers   []LedgerRef
	invoices  map[uuid.UUID]InvoiceSource
	accounts  map[uuid.UUID]AccountRef
}

// exponent adds a currency's minor-unit digits to the view.
func (v *docView) exponent(ctx context.Context, tx pgx.Tx, code string) error {
	if _, ok := v.exponents[code]; ok {
		return nil
	}
	exp, err := currency.Exponent(ctx, tx, code, nil)
	if err != nil {
		return fmt.Errorf("assets: currency %s: %w", code, err)
	}
	v.exponents[code] = exp
	return nil
}

// money is an amount in a currency of the view.
func (v docView) money(amount money.Minor, code string) gen.Money {
	return apitypes.Money(amount, code, v.exponents[code])
}

// docViewOf reads what documents of the companies, in the currencies and
// from the supplier invoices are shown with.
func (e *Endpoints) docViewOf(ctx context.Context, tx pgx.Tx, companies []uuid.UUID, currencies []string, invoices []uuid.UUID) (docView, error) {
	v := docView{companies: map[uuid.UUID]Company{}, exponents: map[string]int{}, invoices: map[uuid.UUID]InvoiceSource{}}
	for _, le := range companies {
		if _, ok := v.companies[le]; ok {
			continue
		}
		// A document's legal entity was checked when it was posted and
		// legal entities are never removed, so it is always found.
		co, _, err := e.Companies.Company(ctx, le)
		if err != nil {
			return docView{}, fmt.Errorf("assets: legal entity %s: %w", le, err)
		}
		v.companies[le] = co
	}
	for _, c := range currencies {
		if err := v.exponent(ctx, tx, c); err != nil {
			return docView{}, err
		}
	}
	var err error
	if v.ledgers, err = e.Ledger.Ledgers(ctx, nil); err != nil {
		return docView{}, fmt.Errorf("assets: ledgers: %w", err)
	}
	if len(invoices) == 0 {
		return v, nil
	}
	if v.invoices, err = e.Invoices.Sources(ctx, invoices); err != nil {
		return docView{}, fmt.Errorf("assets: sources of %d supplier invoices: %w", len(invoices), err)
	}
	return v, nil
}

// invoiceOf is the supplier invoice an entry of a source context was
// posted for, when it is one.
func invoiceOf(context string, doc *uuid.UUID) (uuid.UUID, bool) {
	if context != sourcePayables || doc == nil {
		return uuid.Nil, false
	}
	return *doc, true
}

// supplierInvoice shows the supplier invoice an entry was posted for, when
// the view has it.
func (v docView) supplierInvoice(context string, doc *uuid.UUID) gen.OptAssetSupplierInvoiceRef {
	id, ok := invoiceOf(context, doc)
	if !ok {
		return gen.OptAssetSupplierInvoiceRef{}
	}
	s, ok := v.invoices[id]
	if !ok {
		return gen.OptAssetSupplierInvoiceRef{}
	}
	return gen.NewOptAssetSupplierInvoiceRef(gen.AssetSupplierInvoiceRef{DocumentNumber: s.DocumentNumber, SupplierReference: s.SupplierReference,
		SupplierID: s.SupplierID, SupplierName: s.SupplierName})
}

// entries shows a document's entries in ledger order, each with its
// reversal once reversed.
func (v docView) entries(es []DocumentEntry) []gen.AssetDocumentEntry {
	out := []gen.AssetDocumentEntry{}
	for _, l := range v.ledgers {
		i := slices.IndexFunc(es, func(en DocumentEntry) bool { return en.LedgerID == l.ID })
		if i < 0 {
			continue
		}
		en := gen.AssetDocumentEntry{LedgerID: l.ID, LedgerCode: l.Code, LedgerName: l.Name, FiscalYear: int32(es[i].FiscalYear),
			JournalEntryID: es[i].EntryID, DocumentNumber: es[i].DocumentNumber}
		if r := es[i].Reversal; r != nil {
			en.ReversalFiscalYear, en.ReversalJournalEntryID = gen.NewOptInt32(int32(r.FiscalYear)), gen.NewOptUUID(r.EntryID)
			en.ReversalDocumentNumber = gen.NewOptString(r.DocumentNumber)
		}
		out = append(out, en)
	}
	return out
}

// acquisitionView reads what acquisitions are shown with.
func (e *Endpoints) acquisitionView(ctx context.Context, tx pgx.Tx, qs []Acquisition) (docView, error) {
	var companies, invoices, offsets []uuid.UUID
	var currencies []string
	for _, q := range qs {
		companies, currencies = append(companies, q.LegalEntityID), append(currencies, q.Currency, q.CurrencyLC, q.CurrencyGC)
		if q.Source == nil {
			offsets = append(offsets, *q.OffsetAccountID) // set without a source (acquisition_source)
			continue
		}
		if id, ok := invoiceOf(q.Source.Context, q.Source.DocumentID); ok {
			invoices = append(invoices, id)
		}
	}
	v, err := e.docViewOf(ctx, tx, companies, currencies, invoices)
	if err != nil || len(offsets) == 0 {
		return v, err
	}
	if v.accounts, err = e.Ledger.AccountNames(ctx, offsets); err != nil {
		return docView{}, fmt.Errorf("assets: names of %d offset accounts: %w", len(offsets), err)
	}
	return v, nil
}

// acquisitionAPI shows an acquisition.
func (v docView) acquisitionAPI(q Acquisition) gen.AssetAcquisition {
	out := gen.AssetAcquisition{ID: q.ID, Version: q.Version, CreatedAt: q.CreatedAt, CreatedBy: q.CreatedBy, UpdatedAt: q.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(q.UpdatedBy), LegalEntityID: q.LegalEntityID, CompanyCode: v.companies[q.LegalEntityID].Code, AssetID: q.AssetID,
		AssetNumber: q.AssetNumber, AssetDescription: q.AssetDescription, ValueDate: q.ValueDate, PostingDate: q.PostingDate,
		FiscalYear: int32(q.FiscalYear), Amount: v.money(q.Amount, q.Currency), AmountLc: v.money(q.LC, q.CurrencyLC),
		AmountGc: v.money(q.GC, q.CurrencyGC), Text: apitypes.OptString(q.Text), Status: q.Status, Entries: v.entries(q.Entries)}
	if s := q.Source; s != nil {
		out.Source = gen.NewOptAssetAcquisitionSource(gen.AssetAcquisitionSource{FiscalYear: int32(s.FiscalYear), EntryID: s.EntryID, LineNo: s.LineNo,
			DocumentNumber: s.DocumentNumber, SourceContext: s.Context, SourceDocumentID: apitypes.OptUUID(s.DocumentID),
			SupplierInvoice: v.supplierInvoice(s.Context, s.DocumentID)})
	}
	if id := q.OffsetAccountID; id != nil {
		// Accounts are never removed, so the offset account is named.
		a := v.accounts[*id]
		out.OffsetAccount = gen.NewOptAssetOffsetAccount(gen.AssetOffsetAccount{ID: *id, Number: a.Number, Name: a.Name})
	}
	if r := q.Reversal; r != nil {
		out.Reversal = gen.NewOptAssetAcquisitionReversal(gen.AssetAcquisitionReversal{PostingDate: r.PostingDate, Reason: r.Reason})
	}
	return out
}

// acquisitionOut is an acquisition as a single read or write shows it.
func (e *Endpoints) acquisitionOut(ctx context.Context, q Acquisition) (gen.AssetAcquisition, error) {
	tx, _ := reqctx.TxFrom(ctx) // the read or write before it found it
	v, err := e.acquisitionView(ctx, tx, []Acquisition{q})
	if err != nil {
		return gen.AssetAcquisition{}, err
	}
	return v.acquisitionAPI(q), nil
}

// CreateAssetAcquisition implements POST /v1/asset-acquisitions.
func (e *Endpoints) CreateAssetAcquisition(ctx context.Context, req *gen.AssetAcquisitionCreate, _ gen.CreateAssetAcquisitionParams) (gen.CreateAssetAcquisitionRes, error) {
	in, err := e.newAcquisition(ctx, req)
	if err != nil {
		return nil, err
	}
	q, err := e.postAcquisition(ctx, in)
	if err != nil {
		return nil, err
	}
	out, err := e.acquisitionOut(ctx, q)
	if err != nil {
		return nil, err
	}
	return &gen.AssetAcquisitionHeaders{ETag: gen.NewOptString(etag.Format(q.Version)), Location: gen.NewOptString("/v1/asset-acquisitions/" + q.ID.String()),
		Response: out}, nil
}

// newAcquisition reads an acquisition from its request: from a source
// line or against an offset account, one of the two (422
// request.validation_failed on source), and against an offset account
// with a positive amount (on amount).
func (e *Endpoints) newAcquisition(ctx context.Context, req *gen.AssetAcquisitionCreate) (NewAcquisition, error) {
	in := NewAcquisition{AssetID: req.AssetID, OffsetAccountID: apitypes.Ptr[uuid.UUID](req.OffsetAccountID), ValueDate: apitypes.Ptr[time.Time](req.ValueDate),
		PostingDate: apitypes.Ptr[time.Time](req.PostingDate), Text: apitypes.Ptr[string](req.Text)}
	if s, ok := req.Source.Get(); ok {
		in.Line = &LineKey{EntryID: s.EntryID, LineNo: s.LineNo}
	}
	if (in.Line == nil) == (in.OffsetAccountID == nil) {
		return NewAcquisition{}, validation("source", "a source line or an offset account (offset_account_id), one of the two")
	}
	if m, ok := req.Amount.Get(); ok {
		amount, err := e.amountOf(ctx, m, "amount")
		if err != nil {
			return NewAcquisition{}, err
		}
		in.Amount, in.AmountCurrency = &amount, m.Currency
	}
	if in.OffsetAccountID != nil && (in.Amount == nil || *in.Amount <= 0) {
		return NewAcquisition{}, validation("amount", "a positive amount, required against an offset account")
	}
	return in, nil
}

// ReverseAssetAcquisition implements POST /v1/asset-acquisitions/{id}:reverse.
func (e *Endpoints) ReverseAssetAcquisition(ctx context.Context, req *gen.AssetAcquisitionReverse, params gen.ReverseAssetAcquisitionParams) (gen.ReverseAssetAcquisitionRes, error) {
	q, err := e.reverseAcquisition(ctx, params.ID, etag.IfMatch(params.IfMatch), apitypes.Ptr[time.Time](req.PostingDate), req.Reason)
	if err != nil {
		return nil, err
	}
	out, err := e.acquisitionOut(ctx, q)
	if err != nil {
		return nil, err
	}
	return &gen.ReverseAssetAcquisitionOKHeaders{ETag: gen.NewOptString(etag.Format(q.Version)), Response: out}, nil
}

// amountOf parses an amount of the request's field with its currency's
// minor-unit digits (400 request.invalid_amount otherwise; an unknown
// currency is one).
func (e *Endpoints) amountOf(ctx context.Context, m gen.Money, field string) (money.Minor, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return 0, err
	}
	bad := problem.BadRequest("request.invalid_amount", "The amount is not one of its currency.",
		problem.FieldError{Field: field, Code: "invalid", Message: "an amount with its currency's decimals"})
	exp, err := currency.Exponent(ctx, tx, m.Currency, bad)
	if err != nil {
		return 0, err
	}
	amount, err := money.ParseAmount(m.Amount, exp)
	if err != nil {
		return 0, bad
	}
	return amount, nil
}

// GetAssetAcquisition implements GET /v1/asset-acquisitions/{id}.
func (e *Endpoints) GetAssetAcquisition(ctx context.Context, params gen.GetAssetAcquisitionParams) (gen.GetAssetAcquisitionRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	q, err := readAcquisition(ctx, tx, params.ID)
	if err != nil {
		return nil, err
	}
	out, err := e.acquisitionOut(ctx, q)
	if err != nil {
		return nil, err
	}
	return &gen.GetAssetAcquisitionOKHeaders{ETag: gen.NewOptString(etag.Format(q.Version)), Response: out}, nil
}

var acquisitionSort = pagination.Sort{Default: "posting_date", Fields: []pagination.Field{
	{Name: "posting_date", Column: "q.posting_date", Type: "date"}, {Name: "created_at", Column: "q.created_at", Type: "timestamptz"},
}}

// ListAssetAcquisitions implements GET /v1/asset-acquisitions: the
// acquisitions of the legal entities the caller may act in, filtered by
// company, asset, source entry, posting dates and status.
func (e *Endpoints) ListAssetAcquisitions(ctx context.Context, params gen.ListAssetAcquisitionsParams) (gen.ListAssetAcquisitionsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, asset, entry := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.AssetID), apitypes.Ptr[uuid.UUID](params.SourceEntryID)
	from, to := apitypes.Ptr[time.Time](params.PostingDateFrom), apitypes.Ptr[time.Time](params.PostingDateTo)
	scope := fmt.Sprintf("listAssetAcquisitions|%s|%s|%s|%s|%s|%s", le, asset, entry, scopeDate(from), scopeDate(to), params.Status.Value)
	ks, err := e.Pages.Keyset(acquisitionSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	// The keyset pages on (column, id): selected from a subquery, id is the
	// acquisition's alone, not also its asset's.
	q, args := ks.SQL(`SELECT * FROM (`+acquisitionSelect+`) q WHERE ($1 OR q.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR q.legal_entity_id = $3)
		AND ($4::uuid IS NULL OR q.asset_id = $4) AND ($5::uuid IS NULL OR q.source_entry_id = $5) AND ($6::date IS NULL OR q.posting_date >= $6)
		AND ($7::date IS NULL OR q.posting_date <= $7) AND ($8 = '' OR q.status = $8)`,
		[]any{access.All, access.IDs, le, asset, entry, from, to, params.Status.Value})
	rows, _ := tx.Query(ctx, q, args...)
	qs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Acquisition, error) { return scanAcquisition(r) })
	if err != nil {
		return nil, fmt.Errorf("assets: list acquisitions: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, qs, func(q Acquisition) (string, uuid.UUID) {
		if ks.Field() == "created_at" {
			return q.CreatedAt.UTC().Format(time.RFC3339Nano), q.ID
		}
		return q.PostingDate.Format(time.DateOnly), q.ID
	}, func(page []Acquisition) (docView, error) { return e.acquisitionPage(ctx, tx, page) }, func(q Acquisition, v docView) gen.AssetAcquisition {
		return v.acquisitionAPI(q)
	})
	if err != nil {
		return nil, err
	}
	return &gen.AssetAcquisitionPage{Items: items, NextPageToken: next}, nil
}

// scopeDate is a date for a page token's scope, "" for none.
func scopeDate(d *time.Time) string {
	if d == nil {
		return ""
	}
	return d.Format(time.DateOnly)
}

// acquisitionPage reads the entries of a page's acquisitions into them and
// what they are shown with.
func (e *Endpoints) acquisitionPage(ctx context.Context, tx pgx.Tx, page []Acquisition) (docView, error) {
	ids := make([]uuid.UUID, len(page))
	for i, q := range page {
		ids[i] = q.ID
	}
	entries, err := readEntries(ctx, tx, "acquisition", ids)
	if err != nil {
		return docView{}, err
	}
	for i := range page {
		page[i].Entries = entries[page[i].ID]
	}
	return e.acquisitionView(ctx, tx, page)
}
