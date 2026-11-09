package consolidation

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// runView is what runs are shown with: the documents of the configuration
// versions they read (their groups' names), the tenant's companies by id
// (their names now), the runs' members and their differences counted per
// status.
type runView struct {
	docs        map[int32]*config.Config
	companies   map[uuid.UUID]Company
	members     map[uuid.UUID][]Member
	differences map[uuid.UUID][]gen.ConsolidationDifferenceCount
}

// viewOf reads what the runs are shown with.
func (e *Endpoints) viewOf(ctx context.Context, tx pgx.Tx, rs []Run) (runView, error) {
	v := runView{docs: map[int32]*config.Config{}, companies: map[uuid.UUID]Company{}}
	ids := make([]uuid.UUID, len(rs))
	for i, r := range rs {
		ids[i] = r.ID
		if _, ok := v.docs[r.ConfigVersion]; ok {
			continue
		}
		doc, err := e.version(ctx, r.ConfigVersion)
		if err != nil {
			return runView{}, fmt.Errorf("consolidation: configuration version %d: %w", r.ConfigVersion, err)
		}
		v.docs[r.ConfigVersion] = doc
	}
	entities, err := e.Companies.Entities(ctx)
	if err != nil {
		return runView{}, fmt.Errorf("consolidation: companies: %w", err)
	}
	for _, c := range entities {
		v.companies[c.ID] = c
	}
	if v.members, err = members(ctx, tx, ids); err != nil {
		return runView{}, err
	}
	v.differences, err = differenceCounts(ctx, tx, ids)
	return v, err
}

// differenceCounts are runs' differences counted per status, in the
// statuses' order, by run.
func differenceCounts(ctx context.Context, tx pgx.Tx, runs []uuid.UUID) (map[uuid.UUID][]gen.ConsolidationDifferenceCount, error) {
	rows, _ := tx.Query(ctx, `SELECT run_id, status, count(*)::int FROM consolidation.run_difference WHERE run_id = ANY($1)
		GROUP BY run_id, status ORDER BY run_id, array_position($2::text[], status)`, runs, differenceStatuses)
	out := map[uuid.UUID][]gen.ConsolidationDifferenceCount{}
	var run uuid.UUID
	var c gen.ConsolidationDifferenceCount
	_, err := pgx.ForEachRow(rows, []any{&run, &c.Status, &c.Count}, func() error { out[run] = append(out[run], c); return nil })
	return out, errs.Wrapf(err, "consolidation: differences of %d runs", len(runs))
}

// runAPI shows a run with its companies; validation keeps the run's group
// in the version it read.
func (v runView) runAPI(r Run) gen.ConsolidationRun {
	out := gen.ConsolidationRun{ID: r.ID, GroupCode: r.GroupCode, GroupName: v.docs[r.ConfigVersion].Consolidation.Group(r.GroupCode).Name,
		FiscalYear: int32(r.FiscalYear), Period: int32(r.Period), PeriodStart: r.PeriodStart, PeriodEnd: r.PeriodEnd, Status: r.Status,
		ConfigVersion: r.ConfigVersion, GroupCurrency: r.GroupCurrency, RateType: r.RateType, OperationID: r.OperationID,
		Members: []gen.ConsolidationRunMember{}, DifferenceCounts: append([]gen.ConsolidationDifferenceCount{}, v.differences[r.ID]...), Version: r.Version,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
	if r.Problem != nil {
		out.Problem = gen.NewOptConsolidationRunProblem(gen.ConsolidationRunProblem{Code: r.Problem.Code, Detail: r.Problem.Detail})
	}
	if r.ComputedAt != nil {
		out.ComputedAt = gen.NewOptDateTime(*r.ComputedAt)
	}
	for _, m := range v.members[r.ID] {
		x := gen.ConsolidationRunMember{LegalEntityID: m.LegalEntityID, CompanyCode: m.CompanyCode, CompanyName: v.companies[m.LegalEntityID].Name,
			LocalCurrency: m.LocalCurrency, Role: m.Role, OwnershipPercent: apitypes.OptString(m.OwnershipPercent), ClosingRate: plainRate(m.ClosingRate),
			PeriodStatus: apitypes.OptString(m.PeriodStatus)}
		if m.OwnershipPercent != nil {
			x.OwnershipPercent = gen.NewOptString(plainRate(*m.OwnershipPercent))
		}
		if m.FirstYear != nil {
			x.FirstFiscalYear, x.FirstFiscalPeriod = gen.NewOptInt32(int32(*m.FirstYear)), gen.NewOptInt32(int32(*m.FirstPeriod))
		}
		if m.RateValidFrom != nil {
			x.RateValidFrom = gen.NewOptDate(*m.RateValidFrom)
		}
		if m.PeriodVersion != nil {
			x.PeriodVersion = gen.NewOptInt32(*m.PeriodVersion)
		}
		if m.TranslatedAt != nil {
			x.TranslatedAt = gen.NewOptDateTime(*m.TranslatedAt)
		}
		out.Members = append(out.Members, x)
	}
	return out
}

// shown reads a run of the request, visible to the caller (readRun), as
// the API shows it.
func (e *Endpoints) shown(ctx context.Context, tx pgx.Tx, r Run) (gen.ConsolidationRun, error) {
	v, err := e.viewOf(ctx, tx, []Run{r})
	if err != nil {
		return gen.ConsolidationRun{}, err
	}
	return v.runAPI(r), nil
}

// GetConsolidationRun implements GET /v1/consolidation-runs/{id}.
func (e *Endpoints) GetConsolidationRun(ctx context.Context, params gen.GetConsolidationRunParams) (gen.GetConsolidationRunRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	r, err := readRun(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	out, err := e.shown(ctx, tx, r)
	if err != nil {
		return nil, err
	}
	return &gen.ConsolidationRunHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Response: out}, nil
}

// runCursor is where a page of runs ends: newest first.
type runCursor struct {
	At time.Time `json:"t"`
	ID uuid.UUID `json:"i"`
}

// ListConsolidationRuns implements GET /v1/consolidation-runs: the runs of
// the groups whose companies the caller may all act in, newest first,
// filtered by group, fiscal year, period and status.
func (e *Endpoints) ListConsolidationRuns(ctx context.Context, params gen.ListConsolidationRunsParams) (gen.ListConsolidationRunsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	size, err := pagination.Size(params.PageSize.Value, params.PageSize.Set)
	if err != nil {
		return nil, err
	}
	scope := fmt.Sprintf("listConsolidationRuns|%s|%t %d|%t %d|%s", params.GroupCode.Value, params.FiscalYear.Set, params.FiscalYear.Value,
		params.Period.Set, params.Period.Value, params.Status.Value)
	after := runCursor{At: time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), ID: uuid.Max}
	if params.PageToken.Set {
		if err := e.Pages.Decode(scope, params.PageToken.Value, &after); err != nil {
			return nil, err
		}
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, runSelect+` WHERE ($1 OR companies <@ $2) AND ($3 = '' OR group_code = $3) AND ($4::smallint IS NULL OR fiscal_year = $4)
			AND ($5::smallint IS NULL OR fiscal_period = $5) AND ($6 = '' OR status = $6) AND (created_at, id) < ($7, $8)
		ORDER BY created_at DESC, id DESC LIMIT $9`, access.All, access.IDs, params.GroupCode.Value, apitypes.Ptr[int32](params.FiscalYear),
		apitypes.Ptr[int32](params.Period), params.Status.Value, after.At, after.ID, size+1)
	rs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Run, error) { return scanRun(r) })
	if err != nil {
		return nil, fmt.Errorf("consolidation: list runs: %w", err)
	}
	page := gen.ConsolidationRunPage{Items: []gen.ConsolidationRun{}}
	if len(rs) > size {
		rs = rs[:size]
		last := rs[size-1]
		tok, _ := e.Pages.Encode(scope, runCursor{At: last.CreatedAt, ID: last.ID}) // a time and an id always encode
		page.NextPageToken.SetTo(tok)
	}
	v, err := e.viewOf(ctx, tx, rs)
	if err != nil {
		return nil, err
	}
	for _, r := range rs {
		page.Items = append(page.Items, v.runAPI(r))
	}
	return &page, nil
}

// DiscardConsolidationRun implements POST
// /v1/consolidation-runs/{id}:discard (ADR-0059 §6): a computed or failed
// run discarded (409 consolidation.run_not_discardable otherwise),
// audited and published.
func (e *Endpoints) DiscardConsolidationRun(ctx context.Context, params gen.DiscardConsolidationRunParams) (gen.DiscardConsolidationRunRes, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	r, err := readRun(ctx, tx, params.ID, true)
	if err != nil {
		return nil, err
	}
	if err := etag.Check(params.IfMatch, r.Version); err != nil {
		return nil, errs.Wrapf(err, "consolidation: discard run %s", r.ID)
	}
	if r.Status != RunComputed && r.Status != RunFailed {
		return nil, problem.Conflict("consolidation.run_not_discardable", fmt.Sprintf("Run %s is %s; only a computed or failed run is discarded.",
			r.ID, r.Status))
	}
	if r, err = setStatus(ctx, tx, p, r, RunDiscarded, ""); err != nil {
		return nil, err
	}
	out, err := e.shown(ctx, tx, r)
	if err != nil {
		return nil, err
	}
	return &gen.DiscardConsolidationRunOKHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Response: out}, nil
}
