package ledger

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

func parkedToAPI(pe ParkedEntry) gen.ParkedJournalEntry {
	out := gen.ParkedJournalEntry{ID: pe.ID, Version: pe.Version, CreatedAt: pe.CreatedAt, CreatedBy: pe.CreatedBy, UpdatedAt: pe.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(pe.UpdatedBy), LegalEntityID: pe.LegalEntityID, LedgerID: pe.LedgerID, DocumentType: pe.DocumentType,
		DocumentDate: pe.DocumentDate, PostingDate: pe.PostingDate, Currency: pe.Currency, Reference: apitypes.OptString(pe.Reference),
		HeaderText: apitypes.OptString(pe.HeaderText), Status: pe.Status, CreatedByKind: pe.CreatedByKind, OwnerID: apitypes.OptUUID(pe.OwnerID),
		ApprovalRequestID: apitypes.OptUUID(pe.ApprovalRequestID), JournalEntryID: apitypes.OptUUID(pe.JournalEntryID),
		Lines: make([]gen.ParkedJournalLine, len(pe.Lines))}
	if pe.JournalFiscalYear != nil {
		out.JournalFiscalYear.SetTo(int32(*pe.JournalFiscalYear))
	}
	for i, l := range pe.Lines {
		line := gen.ParkedJournalLine{LineNo: l.LineNo, AccountID: l.AccountID, Amount: apitypes.Money(l.TC, pe.Currency, pe.Exp),
			PartnerID: apitypes.OptUUID(l.PartnerID), TaxCode: apitypes.OptString(l.TaxCode), LineText: apitypes.OptString(l.Text)}
		if l.Quantity != nil {
			line.Quantity.SetTo(gen.Quantity{Value: *l.Quantity, Unit: *l.Unit})
		}
		out.Lines[i] = line
	}
	return out
}

// ParkJournalEntry implements POST /v1/parked-journal-entries.
func (e *Endpoints) ParkJournalEntry(ctx context.Context, req *gen.JournalEntryCreate, _ gen.ParkJournalEntryParams) (gen.ParkJournalEntryRes, error) {
	pe, err := e.parkEntry(ctx, newEntry(req))
	if err != nil {
		return nil, err
	}
	return &gen.ParkedJournalEntryHeaders{ETag: gen.NewOptString(etag.Format(pe.Version)),
		Location: gen.NewOptString("/v1/parked-journal-entries/" + pe.ID.String()), Response: parkedToAPI(pe)}, nil
}

// GetParkedJournalEntry implements GET /v1/parked-journal-entries/{id}.
func (e *Endpoints) GetParkedJournalEntry(ctx context.Context, params gen.GetParkedJournalEntryParams) (gen.GetParkedJournalEntryRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	pe, err := getParked(ctx, tx, params.ID)
	if err != nil {
		return nil, err
	}
	return &gen.GetParkedJournalEntryOKHeaders{ETag: gen.NewOptString(etag.Format(pe.Version)), Response: parkedToAPI(pe)}, nil
}

var parkedSort = pagination.Sort{Default: "created_at", DefaultDesc: true, Fields: []pagination.Field{
	{Name: "created_at", Column: "p.created_at", Type: "timestamptz"},
}}

// ListParkedJournalEntries implements GET /v1/parked-journal-entries.
func (e *Endpoints) ListParkedJournalEntries(ctx context.Context, params gen.ListParkedJournalEntriesParams) (gen.ListParkedJournalEntriesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le := apitypes.Ptr[uuid.UUID](params.LegalEntityID)
	scope := fmt.Sprintf("listParkedJournalEntries|%v|%s|%s", le, params.Status.Value, params.CreatedByKind.Value)
	ks, err := e.Pages.Keyset(parkedSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(parkedSelect+` WHERE ($1 OR p.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR p.legal_entity_id = $3)
		AND ($4 = '' OR p.status = $4) AND ($5 = '' OR p.created_by_kind = $5)`,
		[]any{access.All, access.IDs, le, params.Status.Value, params.CreatedByKind.Value})
	rows, _ := tx.Query(ctx, q, args...)
	entries, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ParkedEntry, error) { return scanParked(r) })
	if err != nil {
		return nil, fmt.Errorf("ledger: list parked entries: %w", err)
	}
	items, next := apitypes.PageOf(ks, entries, func(pe ParkedEntry) (string, uuid.UUID) {
		return pe.CreatedAt.UTC().Format(time.RFC3339Nano), pe.ID
	}, parkedToAPI)
	return &gen.ParkedJournalEntryPageHeaders{Response: gen.ParkedJournalEntryPage{Items: items, NextPageToken: next}}, nil
}

// UpdateParkedJournalEntry implements PATCH /v1/parked-journal-entries/{id}.
func (e *Endpoints) UpdateParkedJournalEntry(ctx context.Context, req *gen.ParkedJournalEntryUpdate,
	params gen.UpdateParkedJournalEntryParams) (gen.UpdateParkedJournalEntryRes, error) {
	ch := EntryChange{DocumentType: apitypes.Ptr[string](req.DocumentType), DocumentDate: apitypes.Ptr[time.Time](req.DocumentDate),
		PostingDate: apitypes.Ptr[time.Time](req.PostingDate), Currency: apitypes.Ptr[string](req.Currency),
		Reference: apitypes.OptNilString(req.Reference), HeaderText: apitypes.OptNilString(req.HeaderText)}
	if req.Lines != nil {
		ch.Lines = newEntry(&gen.JournalEntryCreate{Lines: req.Lines}).Lines
	}
	pe, err := e.changeEntry(ctx, params.ID, etag.IfMatch(params.IfMatch), ch)
	if err != nil {
		return nil, err
	}
	return &gen.UpdateParkedJournalEntryOKHeaders{ETag: gen.NewOptString(etag.Format(pe.Version)), Response: parkedToAPI(pe)}, nil
}

// CancelParkedJournalEntry implements POST /v1/parked-journal-entries/{id}:cancel.
func (e *Endpoints) CancelParkedJournalEntry(ctx context.Context, req *gen.ReasonInput,
	params gen.CancelParkedJournalEntryParams) (gen.CancelParkedJournalEntryRes, error) {
	pe, err := cancelEntry(ctx, params.ID, etag.IfMatch(params.IfMatch), req.Reason)
	if err != nil {
		return nil, err
	}
	return &gen.UpdateParkedJournalEntryOKHeaders{ETag: gen.NewOptString(etag.Format(pe.Version)), Response: parkedToAPI(pe)}, nil
}
