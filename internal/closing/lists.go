package closing

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/idref"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

var closeSort = pagination.Sort{Default: "period_end", Fields: []pagination.Field{
	{Name: "period_end", Column: "c.period_end", Type: "date"}, {Name: "created_at", Column: "c.created_at", Type: "timestamptz"},
}}

// ListPeriodCloses implements GET /v1/period-closes: the closes of the
// legal entities the caller may act in, without their tasks.
func (e *Endpoints) ListPeriodCloses(ctx context.Context, params gen.ListPeriodClosesParams) (gen.ListPeriodClosesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, year := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[int32](params.FiscalYear)
	// The year by its Opt value: a pointer prints its address, which would
	// bind the token to one request.
	scope := fmt.Sprintf("listPeriodCloses|%s|%v|%s", le, params.FiscalYear, params.Status.Value)
	ks, err := e.Pages.Keyset(closeSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(closeSelect+` WHERE ($1 OR c.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR c.legal_entity_id = $3)
		AND ($4::int IS NULL OR c.fiscal_year = $4) AND ($5 = '' OR c.status = $5)`, []any{access.All, access.IDs, le, year, params.Status.Value})
	rows, _ := tx.Query(ctx, q, args...)
	cs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (periodClose, error) { return scanClose(r) })
	if err != nil {
		return nil, fmt.Errorf("closing: list period closes: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, cs, func(c periodClose) (string, uuid.UUID) {
		if ks.Field() == "created_at" {
			return c.CreatedAt.UTC().Format(time.RFC3339Nano), c.ID
		}
		return c.PeriodEnd.Format(time.DateOnly), c.ID
	}, func(page []periodClose) (view, error) {
		return e.view(ctx, idref.Of(page, func(c periodClose) *uuid.UUID { return &c.LegalEntityID }), nil, signers(page...)...)
	}, func(c periodClose, v view) gen.PeriodCloseItem { return v.itemToAPI(c) })
	if err != nil {
		return nil, err
	}
	return &gen.PeriodClosePage{Items: items, NextPageToken: next}, nil
}

// taskCursor is a worklist page's position: its last task's due date,
// close and code.
type taskCursor struct {
	DueOn string    `json:"d"`
	Close uuid.UUID `json:"c"`
	Code  string    `json:"k"`
}

// taskFilter is the worklist's filters.
type taskFilter struct {
	assignee, legalEntity, closeID *uuid.UUID
	status                         string
	dueBefore                      *time.Time
}

// filterOf reads the worklist's filters; assignee me is the caller.
func filterOf(p reqctx.Principal, params gen.ListCloseTasksParams) taskFilter {
	f := taskFilter{legalEntity: apitypes.Ptr[uuid.UUID](params.LegalEntityID), closeID: apitypes.Ptr[uuid.UUID](params.CloseID),
		status: params.Status.Value, dueBefore: apitypes.Ptr[time.Time](params.DueBefore)}
	if a, ok := params.Assignee.Get(); ok {
		id := p.ID
		if a != "me" {
			id = uuid.MustParse(a) // the contract's pattern admits me or a uuid alone
		}
		f.assignee = &id
	}
	return f
}

// ListCloseTasks implements GET /v1/close-tasks: tasks across the closes
// of the legal entities the caller may act in, in due date, close and
// code order: a person's worklist. It pages by its own cursor, so it
// passes pagination.PlanPerCall itself: from a cached statement's sixth
// run PostgreSQL took a generic plan that keeps every optional condition
// as a filter and reads all the tenant's closes and tasks (72,000 tasks:
// 0.3-1.9 ms to 23-55 ms a page).
func (e *Endpoints) ListCloseTasks(ctx context.Context, params gen.ListCloseTasksParams) (gen.ListCloseTasksRes, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	size, err := pagination.Size(params.PageSize.Value, params.PageSize.Set)
	if err != nil {
		return nil, err
	}
	f := filterOf(p, params)
	scope := fmt.Sprintf("listCloseTasks|%v|%v|%v|%s|%v", f.assignee, f.legalEntity, f.closeID, f.status, f.dueBefore)
	var after taskCursor
	var afterDue *time.Time
	if params.PageToken.Set {
		if err := e.Pages.Decode(scope, params.PageToken.Value, &after); err != nil {
			return nil, err
		}
		d, _ := time.Parse(time.DateOnly, after.DueOn) // signed by this endpoint, from a date
		afterDue = &d
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, taskSelect+` WHERE ($1 OR c.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR t.assignee_id = $3)
			AND ($4::uuid IS NULL OR c.legal_entity_id = $4) AND ($5::uuid IS NULL OR t.close_id = $5) AND ($6 = '' OR t.status = $6)
			AND ($7::date IS NULL OR t.due_on < $7) AND ($8::date IS NULL OR (t.due_on, t.close_id, t.code) > ($8::date, $9::uuid, $10::text))
		ORDER BY t.due_on, t.close_id, t.code LIMIT $11`, pagination.PlanPerCall,
		access.All, access.IDs, f.assignee, f.legalEntity, f.closeID, f.status, f.dueBefore, afterDue, after.Close, after.Code, size+1)
	ts, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (task, error) { return scanTask(r) })
	if err != nil {
		return nil, fmt.Errorf("closing: list close tasks: %w", err)
	}
	page := &gen.CloseTaskPage{Items: []gen.CloseTask{}}
	if len(ts) > size {
		ts = ts[:size]
		last := ts[size-1]
		tok, _ := e.Pages.Encode(scope, taskCursor{DueOn: last.DueOn.Format(time.DateOnly), Close: last.CloseID, Code: last.Code}) // strings and a uuid always encode
		page.NextPageToken = gen.NewOptString(tok)
	}
	v, err := e.view(ctx, idref.Of(ts, func(t task) *uuid.UUID { return &t.LegalEntityID }), ts)
	if err != nil {
		return nil, err
	}
	for _, t := range ts {
		page.Items = append(page.Items, v.taskToAPI(t))
	}
	return page, nil
}
