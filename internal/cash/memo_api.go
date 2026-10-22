package cash

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

func optDate(d *time.Time) gen.OptDate {
	if d == nil {
		return gen.OptDate{}
	}
	return gen.NewOptDate(*d)
}

func optInt32(n *int32) gen.OptInt32 {
	if n == nil {
		return gen.OptInt32{}
	}
	return gen.NewOptInt32(*n)
}

func lineToAPI(l LineRef) gen.StatementLineRef {
	return gen.StatementLineRef{StatementID: l.StatementID, LineNo: l.LineNo}
}

// memoToAPI shows a record with its status as of the read date.
func memoToAPI(m MemoRecord, read time.Time) gen.MemoRecord {
	direction := "inflow"
	if m.Amount < 0 {
		direction = "outflow"
	}
	out := gen.MemoRecord{ID: m.ID, Version: m.Version, CreatedAt: m.CreatedAt, CreatedBy: m.CreatedBy, UpdatedAt: m.UpdatedAt, UpdatedBy: gen.NewOptUUID(m.UpdatedBy),
		LegalEntityID: m.LegalEntityID, HouseBankID: apitypes.OptUUID(m.HouseBankID), Amount: apitypes.Money(m.Amount, m.Currency, m.Exponent),
		Direction: direction, ValueDate: m.ValueDate, ExpiresOn: m.ExpiresOn, Certainty: m.Certainty, LiquidityItem: m.Item,
		Description: apitypes.OptString(m.Description), Reference: apitypes.OptString(m.Reference), RecurrenceOf: apitypes.OptUUID(m.RecurrenceOf),
		OccurrenceDate: optDate(m.OccurrenceDate), Status: m.StatusOn(read), Origin: m.Origin, SessionID: apitypes.OptUUID(m.SessionID),
		Reason: apitypes.OptString(m.Reason), Evidence: make([]gen.StatementLineRef, len(m.Evidence)), BatchKey: apitypes.OptString(m.BatchKey),
		DecidedBy: apitypes.OptUUID(m.DecidedBy), Comment: apitypes.OptString(m.Comment)}
	for i, l := range m.Evidence {
		out.Evidence[i] = lineToAPI(l)
	}
	if r := m.Recurrence; r != nil {
		out.Recurrence.SetTo(gen.MemoRecurrence{Frequency: r.Frequency, Until: optDate(r.Until), Count: optInt32(r.Count)})
	}
	if m.RealisedBy != nil {
		out.RealisedBy.SetTo(lineToAPI(*m.RealisedBy))
	}
	if m.DecidedAt != nil {
		out.DecidedAt.SetTo(*m.DecidedAt)
	}
	return out
}

func recurrenceOf(r gen.OptMemoRecurrence) *Recurrence {
	v, ok := r.Get()
	if !ok {
		return nil
	}
	return &Recurrence{Frequency: v.Frequency, Until: apitypes.Ptr[time.Time](v.Until), Count: apitypes.Ptr[int32](v.Count)}
}

// read is the read date, as a record's status is judged by it.
func (e *Endpoints) read() time.Time { return clock.Day(clock.Now(e.Now)) }

func (e *Endpoints) headers(m MemoRecord) *gen.MemoRecordHeaders {
	return &gen.MemoRecordHeaders{ETag: gen.NewOptString(etag.Format(m.Version)), Location: gen.NewOptString("/v1/memo-records/" + m.ID.String()),
		Response: memoToAPI(m, e.read())}
}

func (e *Endpoints) changed(m MemoRecord) *gen.UpdateMemoRecordOKHeaders {
	return &gen.UpdateMemoRecordOKHeaders{ETag: gen.NewOptString(etag.Format(m.Version)), Response: memoToAPI(m, e.read())}
}

// CreateMemoRecord implements POST /v1/memo-records.
func (e *Endpoints) CreateMemoRecord(ctx context.Context, req *gen.MemoRecordCreate, _ gen.CreateMemoRecordParams) (gen.CreateMemoRecordRes, error) {
	m, err := e.CreateMemo(ctx, MemoInput{LegalEntityID: req.LegalEntityID, HouseBankID: apitypes.Ptr[uuid.UUID](req.HouseBankID), Amount: req.Amount.Amount,
		Currency: req.Amount.Currency, ValueDate: req.ValueDate, ExpiresOn: apitypes.Ptr[time.Time](req.ExpiresOn), Certainty: req.Certainty.Value,
		Item: req.LiquidityItem, Description: apitypes.Ptr[string](req.Description), Reference: apitypes.Ptr[string](req.Reference),
		Recurrence: recurrenceOf(req.Recurrence), RecurrenceOf: apitypes.Ptr[uuid.UUID](req.RecurrenceOf), OccurrenceDate: apitypes.Ptr[time.Time](req.OccurrenceDate)})
	if err != nil {
		return nil, err
	}
	return e.headers(m), nil
}

// ProposeMemoRecord implements POST /v1/memo-record-proposals.
func (e *Endpoints) ProposeMemoRecord(ctx context.Context, req *gen.MemoRecordProposal, _ gen.ProposeMemoRecordParams) (gen.ProposeMemoRecordRes, error) {
	evidence := make([]LineRef, len(req.Evidence))
	for i, l := range req.Evidence {
		evidence[i] = LineRef{StatementID: l.StatementID, LineNo: l.LineNo}
	}
	m, err := e.ProposeMemo(ctx, MemoInput{LegalEntityID: req.LegalEntityID, HouseBankID: apitypes.Ptr[uuid.UUID](req.HouseBankID), Amount: req.Amount.Amount,
		Currency: req.Amount.Currency, ValueDate: req.ValueDate, ExpiresOn: apitypes.Ptr[time.Time](req.ExpiresOn), Certainty: req.Certainty.Value,
		Item: req.LiquidityItem, Description: apitypes.Ptr[string](req.Description), Reference: apitypes.Ptr[string](req.Reference),
		Recurrence: recurrenceOf(req.Recurrence)}, req.SessionID, req.Reason, evidence)
	if err != nil {
		return nil, err
	}
	return e.headers(m), nil
}

// GetMemoRecord implements GET /v1/memo-records/{id}.
func (e *Endpoints) GetMemoRecord(ctx context.Context, params gen.GetMemoRecordParams) (gen.GetMemoRecordRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	m, err := memo(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	return &gen.GetMemoRecordOKHeaders{ETag: gen.NewOptString(etag.Format(m.Version)), Response: memoToAPI(m, e.read())}, nil
}

// UpdateMemoRecord implements PATCH /v1/memo-records/{id}.
func (e *Endpoints) UpdateMemoRecord(ctx context.Context, req *gen.MemoRecordUpdate, params gen.UpdateMemoRecordParams) (gen.UpdateMemoRecordRes, error) {
	ch := MemoPatch{ValueDate: apitypes.Ptr[time.Time](req.ValueDate), ExpiresOn: apitypes.Ptr[time.Time](req.ExpiresOn),
		Certainty: apitypes.Ptr[string](req.Certainty), Item: apitypes.Ptr[string](req.LiquidityItem), Description: apitypes.OptNilString(req.Description),
		Reference: apitypes.OptNilString(req.Reference), Recurrence: recurrenceOf(req.Recurrence)}
	if a, ok := req.Amount.Get(); ok {
		ch.Amount, ch.Currency = &a.Amount, &a.Currency
	}
	if req.HouseBankID.Set {
		var bank *uuid.UUID
		if !req.HouseBankID.Null {
			bank = &req.HouseBankID.Value
		}
		ch.HouseBankID = &bank
	}
	m, err := e.UpdateMemo(ctx, params.ID, etag.IfMatch(params.IfMatch), ch)
	if err != nil {
		return nil, err
	}
	return e.changed(m), nil
}

// CancelMemoRecord implements POST /v1/memo-records/{id}:cancel.
func (e *Endpoints) CancelMemoRecord(ctx context.Context, req gen.OptMemoRecordCancel, params gen.CancelMemoRecordParams) (gen.CancelMemoRecordRes, error) {
	m, created, err := e.CancelMemo(ctx, params.ID, etag.IfMatch(params.IfMatch), Closing{Occurrence: apitypes.Ptr[time.Time](req.Value.OccurrenceDate),
		Comment: apitypes.Ptr[string](req.Value.Comment)})
	if err != nil {
		return nil, err
	}
	if created { // the occurrence's override
		return e.headers(m), nil
	}
	return e.changed(m), nil
}

// RealiseMemoRecord implements POST /v1/memo-records/{id}:realise.
func (e *Endpoints) RealiseMemoRecord(ctx context.Context, req gen.OptMemoRecordRealise, params gen.RealiseMemoRecordParams) (gen.RealiseMemoRecordRes, error) {
	c := Closing{Occurrence: apitypes.Ptr[time.Time](req.Value.OccurrenceDate), Comment: apitypes.Ptr[string](req.Value.Comment)}
	statement, line := req.Value.StatementID, req.Value.LineNo
	if statement.Set != line.Set {
		return nil, invalid("request.validation_failed", "A statement line is named by its statement and its number, both.", "line_no", "with statement_id")
	}
	if statement.Set {
		c.Line = &LineRef{StatementID: statement.Value, LineNo: line.Value}
	}
	m, created, err := e.RealiseMemo(ctx, params.ID, etag.IfMatch(params.IfMatch), c)
	if err != nil {
		return nil, err
	}
	if created { // the occurrence's override
		return e.headers(m), nil
	}
	return e.changed(m), nil
}

// AcceptMemoRecordProposal implements POST /v1/memo-records/{id}:acceptProposal.
func (e *Endpoints) AcceptMemoRecordProposal(ctx context.Context, req gen.OptMemoRecordAccept, params gen.AcceptMemoRecordProposalParams) (gen.AcceptMemoRecordProposalRes, error) {
	m, err := e.AcceptMemo(ctx, params.ID, etag.IfMatch(params.IfMatch), apitypes.Ptr[string](req.Value.Comment))
	if err != nil {
		return nil, err
	}
	return e.changed(m), nil
}

// RejectMemoRecordProposal implements POST /v1/memo-records/{id}:rejectProposal.
func (e *Endpoints) RejectMemoRecordProposal(ctx context.Context, req *gen.ReasonInput, params gen.RejectMemoRecordProposalParams) (gen.RejectMemoRecordProposalRes, error) {
	m, err := e.RejectMemo(ctx, params.ID, etag.IfMatch(params.IfMatch), req.Reason)
	if err != nil {
		return nil, err
	}
	return e.changed(m), nil
}

var memoSort = pagination.Sort{Default: "value_date", Fields: []pagination.Field{
	{Name: "value_date", Column: "m.value_date", Type: "date"}, {Name: "created_at", Column: "m.created_at", Type: "timestamptz"},
}}

// ListMemoRecords implements GET /v1/memo-records: the records of the
// legal entities the caller may act in, a status judged as of the read
// date.
func (e *Endpoints) ListMemoRecords(ctx context.Context, params gen.ListMemoRecordsParams) (gen.ListMemoRecordsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, bank := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.HouseBankID)
	from, to := apitypes.Ptr[time.Time](params.ValueFrom), apitypes.Ptr[time.Time](params.ValueTo)
	scope := fmt.Sprintf("listMemoRecords|%s|%s|%s|%s|%v|%v", le, params.Status.Value, bank, params.BatchKey.Value, from, to)
	ks, err := e.Pages.Keyset(memoSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	read := e.read()
	q, args := ks.SQL(memoSelect+` WHERE ($1 OR m.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR m.legal_entity_id = $3)
		AND ($4::uuid IS NULL OR m.house_bank_id = $4) AND ($5 = '' OR m.batch_key = $5) AND ($6::date IS NULL OR m.value_date >= $6)
		AND ($7::date IS NULL OR m.value_date <= $7)
		AND CASE $8 WHEN '' THEN true WHEN 'expired' THEN m.status = 'active' AND m.counts_until < $9
			WHEN 'active' THEN m.status = 'active' AND (m.counts_until IS NULL OR m.counts_until >= $9) ELSE m.status = $8 END`,
		[]any{access.All, access.IDs, le, bank, params.BatchKey.Value, from, to, params.Status.Value, read})
	rows, _ := tx.Query(ctx, q, args...)
	ms, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (MemoRecord, error) { return scanMemo(r) })
	if err != nil {
		return nil, fmt.Errorf("cash: list memo records: %w", err)
	}
	items, next := apitypes.PageOf(ks, ms, func(m MemoRecord) (string, uuid.UUID) {
		if ks.Field() == "created_at" {
			return m.CreatedAt.UTC().Format(time.RFC3339Nano), m.ID
		}
		return m.ValueDate.Format(time.DateOnly), m.ID
	}, func(m MemoRecord) gen.MemoRecord { return memoToAPI(m, read) })
	return &gen.MemoRecordPageHeaders{Response: gen.MemoRecordPage{Items: items, NextPageToken: next}}, nil
}

// UploadMemoRecords implements POST /v1/memo-record-uploads.
func (e *Endpoints) UploadMemoRecords(ctx context.Context, req *gen.MemoRecordUploadMultipart, _ gen.UploadMemoRecordsParams) (gen.UploadMemoRecordsRes, error) {
	r, err := e.UploadMemos(ctx, req.LegalEntityID, req.BatchKey, []byte(req.File))
	if err != nil {
		return nil, err
	}
	return &gen.MemoRecordUploadResultHeaders{Response: gen.MemoRecordUploadResult{LegalEntityID: req.LegalEntityID, BatchKey: req.BatchKey,
		Rows: int32(r.Rows), Created: int32(r.Created), Changed: int32(r.Changed), Unchanged: int32(r.Unchanged), Cancelled: int32(r.Cancelled),
		KeptRealised: int32(r.KeptRealised)}}, nil
}
