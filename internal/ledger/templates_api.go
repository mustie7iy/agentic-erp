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

func templateToAPI(t EntryTemplate) gen.EntryTemplate {
	out := gen.EntryTemplate{ID: t.ID, Version: t.Version, CreatedAt: t.CreatedAt, CreatedBy: t.CreatedBy, UpdatedAt: t.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(t.UpdatedBy), LegalEntityID: t.LegalEntityID, LedgerID: t.LedgerID, LedgerSpecific: gen.NewOptBool(t.LedgerSpecific),
		LedgerGroup: apitypes.OptString(t.LedgerGroup), Name: t.Name, DocumentType: t.DocumentType, Currency: t.Currency,
		Reference: apitypes.OptString(t.Reference), HeaderText: apitypes.OptString(t.HeaderText), Status: t.Status, CreatedByKind: t.CreatedByKind,
		OwnerID: apitypes.OptUUID(t.OwnerID), NextDueOn: apitypes.OptDate(t.NextDueOn), LastOccurrenceOn: apitypes.OptDate(t.LastOccurrenceOn),
		Lines: linesToAPI(t.Lines, t.Currency, t.Exp)}
	if s := t.Schedule; s != nil {
		out.Schedule.SetTo(gen.EntrySchedule{Frequency: s.Frequency, DayOfMonth: int32(s.Day), StartsOn: s.StartsOn, EndsOn: apitypes.OptDate(s.EndsOn),
			Occurrences: apitypes.OptInt32(s.Count)})
	}
	if r := t.Refusal; r != nil {
		out.LastRefusal.SetTo(gen.EntryTemplateRefusal{OccurrenceDate: r.On, Code: r.Code, Detail: r.Detail})
	}
	return out
}

// scheduleOf is the schedule a request gives, or nil.
func scheduleOf(s gen.EntrySchedule) *Schedule {
	return &Schedule{Frequency: s.Frequency, Day: int(s.DayOfMonth), StartsOn: s.StartsOn, EndsOn: apitypes.Ptr[time.Time](s.EndsOn),
		Count: apitypes.Ptr[int32](s.Occurrences)}
}

// templateHeaders is the answer that saved a template.
func templateHeaders(t EntryTemplate) *gen.EntryTemplateHeaders {
	return &gen.EntryTemplateHeaders{ETag: gen.NewOptString(etag.Format(t.Version)),
		Location: gen.NewOptString("/v1/entry-templates/" + t.ID.String()), Response: templateToAPI(t)}
}

// CreateEntryTemplate implements POST /v1/entry-templates.
func (e *Endpoints) CreateEntryTemplate(ctx context.Context, req *gen.EntryTemplateCreate, _ gen.CreateEntryTemplateParams) (gen.CreateEntryTemplateRes, error) {
	lines := newEntry(&gen.JournalEntryCreate{Lines: req.Lines}).Lines
	in := NewTemplate{LegalEntityID: req.LegalEntityID, LedgerID: apitypes.Ptr[uuid.UUID](req.LedgerID), LedgerGroup: apitypes.Ptr[string](req.LedgerGroup),
		Name: req.Name, DocumentType: req.DocumentType.Or("SA"), Currency: req.Currency, Reference: apitypes.Ptr[string](req.Reference),
		HeaderText: apitypes.Ptr[string](req.HeaderText), Lines: lines}
	if s, ok := req.Schedule.Get(); ok {
		in.Schedule = scheduleOf(s)
	}
	t, err := e.createTemplate(ctx, in)
	if err != nil {
		return nil, err
	}
	return templateHeaders(t), nil
}

// GetEntryTemplate implements GET /v1/entry-templates/{id}.
func (e *Endpoints) GetEntryTemplate(ctx context.Context, params gen.GetEntryTemplateParams) (gen.GetEntryTemplateRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	t, err := getTemplate(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	return &gen.GetEntryTemplateOKHeaders{ETag: gen.NewOptString(etag.Format(t.Version)), Response: templateToAPI(t)}, nil
}

var templateSort = pagination.Sort{Default: "created_at", DefaultDesc: true, Fields: []pagination.Field{
	{Name: "created_at", Column: "t.created_at", Type: "timestamptz"},
}}

// ListEntryTemplates implements GET /v1/entry-templates.
func (e *Endpoints) ListEntryTemplates(ctx context.Context, params gen.ListEntryTemplatesParams) (gen.ListEntryTemplatesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le := apitypes.Ptr[uuid.UUID](params.LegalEntityID)
	scope := fmt.Sprintf("listEntryTemplates|%v|%s", le, params.Status.Value)
	ks, err := e.Pages.Keyset(templateSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(templateSelect+` WHERE ($1 OR t.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR t.legal_entity_id = $3)
		AND ($4 = '' OR t.status = $4)`, []any{access.All, access.IDs, le, params.Status.Value})
	rows, _ := tx.Query(ctx, q, args...)
	templates, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (EntryTemplate, error) { return scanTemplate(r) })
	if err != nil {
		return nil, fmt.Errorf("ledger: list entry templates: %w", err)
	}
	items, next := apitypes.PageOf(ks, templates, func(t EntryTemplate) (string, uuid.UUID) {
		return t.CreatedAt.UTC().Format(time.RFC3339Nano), t.ID
	}, templateToAPI)
	return &gen.EntryTemplatePage{Items: items, NextPageToken: next}, nil
}

// UpdateEntryTemplate implements PATCH /v1/entry-templates/{id}.
func (e *Endpoints) UpdateEntryTemplate(ctx context.Context, req *gen.EntryTemplateUpdate,
	params gen.UpdateEntryTemplateParams) (gen.UpdateEntryTemplateRes, error) {
	ch := TemplateChange{Name: apitypes.Ptr[string](req.Name), DocumentType: apitypes.Ptr[string](req.DocumentType),
		Currency: apitypes.Ptr[string](req.Currency), Reference: apitypes.OptNilString(req.Reference), HeaderText: apitypes.OptNilString(req.HeaderText)}
	if req.Schedule.IsSet() {
		var s *Schedule
		if v, ok := req.Schedule.Get(); ok {
			s = scheduleOf(gen.EntrySchedule(v))
		}
		ch.Schedule = &s
	}
	if req.Lines != nil {
		ch.Lines = newEntry(&gen.JournalEntryCreate{Lines: req.Lines}).Lines
	}
	t, err := e.changeTemplate(ctx, params.ID, etag.IfMatch(params.IfMatch), ch)
	if err != nil {
		return nil, err
	}
	return &gen.UpdateEntryTemplateOKHeaders{ETag: gen.NewOptString(etag.Format(t.Version)), Response: templateToAPI(t)}, nil
}

// RetireEntryTemplate implements POST /v1/entry-templates/{id}:retire.
func (e *Endpoints) RetireEntryTemplate(ctx context.Context, params gen.RetireEntryTemplateParams) (gen.RetireEntryTemplateRes, error) {
	t, err := retireTemplate(ctx, params.ID, etag.IfMatch(params.IfMatch))
	if err != nil {
		return nil, err
	}
	return &gen.UpdateEntryTemplateOKHeaders{ETag: gen.NewOptString(etag.Format(t.Version)), Response: templateToAPI(t)}, nil
}

// ParkFromEntryTemplate implements POST /v1/entry-templates/{id}:park.
func (e *Endpoints) ParkFromEntryTemplate(ctx context.Context, req *gen.EntryTemplateUse,
	params gen.ParkFromEntryTemplateParams) (gen.ParkFromEntryTemplateRes, error) {
	pe, err := e.useTemplate(ctx, params.ID, etag.IfMatch(params.IfMatch), TemplateUse{PostingDate: req.PostingDate, DocumentDate: apitypes.Ptr[time.Time](req.DocumentDate),
		Reference: apitypes.Ptr[string](req.Reference)})
	if err != nil {
		return nil, err
	}
	return parkedHeaders(pe), nil
}
