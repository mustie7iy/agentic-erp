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
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

func parkedToAPI(pe ParkedEntry) gen.ParkedJournalEntry {
	out := gen.ParkedJournalEntry{ID: pe.ID, Version: pe.Version, CreatedAt: pe.CreatedAt, CreatedBy: pe.CreatedBy, UpdatedAt: pe.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(pe.UpdatedBy), LegalEntityID: pe.LegalEntityID, LedgerID: pe.LedgerID, DocumentType: pe.DocumentType,
		DocumentDate: pe.DocumentDate, PostingDate: pe.PostingDate, TaxDate: apitypes.OptDate(pe.TaxDate), Currency: pe.Currency,
		Reference:  apitypes.OptString(pe.Reference),
		HeaderText: apitypes.OptString(pe.HeaderText), Status: pe.Status, CreatedByKind: pe.CreatedByKind, OwnerID: apitypes.OptUUID(pe.OwnerID),
		ApprovalRequestID: apitypes.OptUUID(pe.ApprovalRequestID), JournalEntryID: apitypes.OptUUID(pe.JournalEntryID),
		LedgerSpecific: gen.NewOptBool(pe.LedgerSpecific), LedgerGroup: apitypes.OptString(pe.LedgerGroup),
		TemplateID: apitypes.OptUUID(pe.TemplateID), TemplateName: apitypes.OptString(pe.TemplateName), OccurrenceDate: apitypes.OptDate(pe.OccurrenceDate), Lines: linesToAPI(pe.Lines, pe.Currency, pe.Exp)}
	if pe.JournalFiscalYear != nil {
		out.JournalFiscalYear.SetTo(int32(*pe.JournalFiscalYear))
	}
	if pe.FiscalPeriod != nil {
		out.FiscalPeriod.SetTo(int32(*pe.FiscalPeriod))
	}
	if l := pe.Limit; l != nil {
		out.LimitExceeded.SetTo(gen.ParkedEntryLimit{Kind: l.Kind, Debits: apitypes.Money(l.Debits, l.DebitsCurrency, l.DebitsExp),
			Limit: apitypes.Money(l.Limit, l.LimitCurrency, l.LimitExp), Detail: l.Detail()})
	}
	return out
}

// linesToAPI are kept lines, in minor units of currency with exp digits,
// as the API gives a parked entry's or a template's.
func linesToAPI(lines []Line, currency string, exp int) []gen.ParkedJournalLine {
	out := make([]gen.ParkedJournalLine, len(lines))
	for i, l := range lines {
		line := gen.ParkedJournalLine{LineNo: l.LineNo, AccountID: l.AccountID, Amount: apitypes.Money(l.TC, currency, exp),
			PartnerID: apitypes.OptUUID(l.PartnerID), TaxCode: apitypes.OptString(l.TaxCode), LineText: apitypes.OptString(l.Text),
			TaxJurisdiction: apitypes.OptString(l.TaxJurisdiction), CostCenterID: apitypes.OptUUID(l.CostCenterID),
			CostCenterCode: apitypes.OptString(l.CostCenterCode), ProfitCenterID: apitypes.OptUUID(l.ProfitCenterID),
			ProfitCenterCode: apitypes.OptString(l.ProfitCenterCode), SegmentID: apitypes.OptUUID(l.SegmentID), SegmentCode: apitypes.OptString(l.SegmentCode)}
		if l.Quantity != nil {
			line.Quantity.SetTo(gen.Quantity{Value: *l.Quantity, Unit: *l.Unit})
		}
		if l.TaxBase != nil {
			line.TaxBase.SetTo(apitypes.Money(*l.TaxBase, currency, exp))
		}
		out[i] = line
	}
	return out
}

// ParkJournalEntry implements POST /v1/parked-journal-entries.
func (e *Endpoints) ParkJournalEntry(ctx context.Context, req *gen.JournalEntryCreate, _ gen.ParkJournalEntryParams) (gen.ParkJournalEntryRes, error) {
	pe, err := e.parkEntry(ctx, newEntry(req))
	if err != nil {
		return nil, err
	}
	return parkedHeaders(pe), nil
}

// parkedHeaders is the answer that parked an entry: the entry, its ETag
// and where to read it.
func parkedHeaders(pe ParkedEntry) *gen.ParkedJournalEntryHeaders {
	return &gen.ParkedJournalEntryHeaders{ETag: gen.NewOptString(etag.Format(pe.Version)),
		Location: gen.NewOptString("/v1/parked-journal-entries/" + pe.ID.String()), Response: parkedToAPI(pe)}
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
	rep := parkedToAPI(pe)
	partners := make([]gen.OptUUID, len(rep.Lines))
	for i, l := range rep.Lines {
		partners[i] = l.PartnerID
	}
	if rep.Expanded, err = e.expandPartners(ctx, params.Expand, partners); err != nil {
		return nil, err
	}
	return &gen.GetParkedJournalEntryOKHeaders{ETag: gen.NewOptString(etag.Format(pe.Version)), Response: rep}, nil
}

var parkedSort = pagination.Sort{Default: "created_at", DefaultDesc: true, Fields: []pagination.Field{
	{Name: "created_at", Column: "p.created_at", Type: "timestamptz"},
}}

// ListParkedJournalEntries implements GET /v1/parked-journal-entries.
// owned_by=me is the caller's own entries (ADR-0056): with status=parked,
// what waits for the caller to post it, those the caller's templates'
// schedules parked among them.
func (e *Endpoints) ListParkedJournalEntries(ctx context.Context, params gen.ListParkedJournalEntriesParams) (gen.ListParkedJournalEntriesRes, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	owner, err := ownedBy(p, params.OwnedBy.Value)
	if err != nil {
		return nil, err
	}
	le, template := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.TemplateID)
	scope := fmt.Sprintf("listParkedJournalEntries|%v|%s|%s|%v|%s", le, params.Status.Value, params.CreatedByKind.Value, template, params.OwnedBy.Value)
	ks, err := e.Pages.Keyset(parkedSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(parkedSelect+` WHERE ($1 OR p.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR p.legal_entity_id = $3)
		AND ($4 = '' OR p.status = $4) AND ($5 = '' OR p.created_by_kind = $5) AND ($6::uuid IS NULL OR p.template_id = $6)
		AND ($7::uuid IS NULL OR p.owner_id = $7)`,
		[]any{access.All, access.IDs, le, params.Status.Value, params.CreatedByKind.Value, template, owner})
	rows, _ := tx.Query(ctx, q, args...)
	entries, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ParkedEntry, error) { return scanParked(r) })
	if err != nil {
		return nil, fmt.Errorf("ledger: list parked entries: %w", err)
	}
	items, next := apitypes.PageOf(ks, entries, func(pe ParkedEntry) (string, uuid.UUID) {
		return pe.CreatedAt.UTC().Format(time.RFC3339Nano), pe.ID
	}, parkedToAPI)
	return &gen.ParkedJournalEntryPage{Items: items, NextPageToken: next}, nil
}

// ownedBy is the owner owned_by asks for: none for "", the caller for
// "me". Only a person owns entries, so an agent's or a service's "me" is
// an owner no entry has (uuid.Nil) and its list is empty.
func ownedBy(p reqctx.Principal, by string) (*uuid.UUID, error) {
	switch {
	case by == "":
		return nil, nil
	case by != "me":
		return nil, problem.BadRequest("request.invalid_parameter", "Parameter owned_by is me or absent.",
			problem.FieldError{Field: "owned_by", Code: "invalid", Message: "me"})
	case p.Kind != reqctx.KindHuman:
		return &uuid.Nil, nil
	}
	return &p.ID, nil
}

// UpdateParkedJournalEntry implements PATCH /v1/parked-journal-entries/{id}.
func (e *Endpoints) UpdateParkedJournalEntry(ctx context.Context, req *gen.ParkedJournalEntryUpdate,
	params gen.UpdateParkedJournalEntryParams) (gen.UpdateParkedJournalEntryRes, error) {
	ch := EntryChange{DocumentType: apitypes.Ptr[string](req.DocumentType), DocumentDate: apitypes.Ptr[time.Time](req.DocumentDate),
		PostingDate: apitypes.Ptr[time.Time](req.PostingDate), TaxDate: apitypes.OptNilDate(req.TaxDate), Currency: apitypes.Ptr[string](req.Currency),
		Reference: apitypes.OptNilString(req.Reference), HeaderText: apitypes.OptNilString(req.HeaderText), FiscalPeriod: specialChange(req.FiscalPeriod)}
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

// PostParkedJournalEntry implements POST /v1/parked-journal-entries/{id}:post.
func (e *Endpoints) PostParkedJournalEntry(ctx context.Context, params gen.PostParkedJournalEntryParams) (gen.PostParkedJournalEntryRes, error) {
	pe, err := e.postParked(ctx, params.ID, etag.IfMatch(params.IfMatch))
	if err != nil {
		return nil, err
	}
	return &gen.UpdateParkedJournalEntryOKHeaders{ETag: gen.NewOptString(etag.Format(pe.Version)), Response: parkedToAPI(pe)}, nil
}
