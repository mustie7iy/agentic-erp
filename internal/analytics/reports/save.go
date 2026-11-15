package reports

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Save saves a report as the caller's, active (ADR-0076 §2, T95a). It is
// checked as the agent's dashboard is built: every query is run once as
// the caller and the specification validated against the results; the
// run is not kept. Commentary its results do not support is refused, as
// Build refuses it, rather than left out as a run leaves it out.
func (e *Endpoints) Save(ctx context.Context, spec analytics.Spec, queries map[string]analytics.Query) (Report, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Report{}, err
	}
	if err := checkIDs(queries); err != nil {
		return Report{}, err
	}
	_, out, _, err := e.rerun(ctx, spec, queries)
	if err != nil {
		return Report{}, err
	}
	if out.Spec.Commentary != spec.Commentary {
		return Report{}, invalidReport([]string{"the commentary cites figures the queries' results do not support"})
	}
	// Plain data of strings, numbers and nested structs: it encodes.
	specJSON, _ := json.Marshal(spec)
	queriesJSON, _ := json.Marshal(queries)
	var question *string
	if spec.Question != "" {
		question = &spec.Question
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO analytics.report (tenant_id, id, title, question, spec, queries, owner_id) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		p.TenantID, id, spec.Title, question, specJSON, queriesJSON, p.ID); err != nil {
		return Report{}, fmt.Errorf("analytics: save report %q: %w", spec.Title, err)
	}
	r, err := report(ctx, tx, owner(p.ID), id, false)
	if err != nil {
		return Report{}, err
	}
	return r, record(ctx, "create", r, map[string]any{"title": r.Title, "queries": slices.Sorted(maps.Keys(r.Queries))})
}

// Get is a report the caller reads: their own, or one shared with them.
func (e *Endpoints) Get(ctx context.Context, id uuid.UUID) (Report, error) {
	tx, _, v, err := e.request(ctx)
	if err != nil {
		return Report{}, err
	}
	return report(ctx, tx, v, id, false)
}

// Remove removes one of the caller's active reports for good, once, with
// the caller's version of it: it is kept, changes no more and is not run.
func (e *Endpoints) Remove(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (Report, error) {
	tx, _, v, err := e.request(ctx)
	if err != nil {
		return Report{}, err
	}
	if _, err := locked(ctx, tx, v, id, ifMatch); err != nil {
		return Report{}, err
	}
	// A removed report has no schedule (report_removed_unscheduled).
	if _, err := tx.Exec(ctx, `UPDATE analytics.report SET status = 'removed', schedule_frequency = NULL, schedule_company = NULL, schedule_weekday = NULL,
			schedule_day = NULL, scheduled_from = NULL, version = version + 1 WHERE id = $1 AND status = 'active'`, id); err != nil {
		return Report{}, fmt.Errorf("analytics: remove report %s: %w", id, err)
	}
	removed, err := report(ctx, tx, v, id, false)
	if err != nil {
		return Report{}, err
	}
	return removed, record(ctx, "remove", removed, map[string]any{"title": removed.Title})
}

// Results are a report run on current data: its specification as
// answered, and its results by the ids that specification names them by.
type Results struct {
	Report      Report
	Spec        analytics.Spec
	Results     []*analytics.Result // by query id
	DataThrough string
}

// Run runs an active report the caller reads on current data, as the
// caller, with the caller's version of it (ADR-0076 §2, §7): a viewer's
// run is computed with their own access; nothing is kept.
func (e *Endpoints) Run(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (Results, error) {
	tx, _, v, err := e.request(ctx)
	if err != nil {
		return Results{}, err
	}
	r, err := report(ctx, tx, v, id, false)
	if err != nil {
		return Results{}, err
	}
	if err := ifMatch(r.Version); err != nil {
		return Results{}, err
	}
	if r.Status != Active {
		return Results{}, errRemoved(r.Title)
	}
	sess, out, results, err := e.rerun(ctx, r.Spec, r.Queries)
	if err != nil {
		return Results{}, err
	}
	res := Results{Report: r, Spec: out.Spec, DataThrough: sess.DataThrough()}
	for _, qid := range sortedIDs(results) {
		res.Results = append(res.Results, results[qid])
	}
	return res, nil
}

// sortedIDs are a run's result ids in order, which the results are
// answered in.
func sortedIDs(results map[string]*analytics.Result) []string {
	return slices.Sorted(maps.Keys(results))
}

// List is a page of the reports the caller reads by title, filtered by
// status ("": every status) and by whether they are shared with the
// caller (nil: their own and those shared with them).
func (e *Endpoints) List(ctx context.Context, status string, shared *bool, page pagination.Request) ([]Report, *pagination.Keyset, error) {
	tx, _, v, err := e.request(ctx)
	if err != nil {
		return nil, nil, err
	}
	ks, err := e.Pages.Keyset(reportSort, fmt.Sprintf("listReports|%s|%s", status, sharedScope(shared)), page)
	if err != nil {
		return nil, nil, fmt.Errorf("analytics: page of reports: %w", err)
	}
	q, args := ks.SQL(reportSelect+` WHERE `+readable+` AND ($4 = '' OR status = $4) AND ($5::boolean IS NULL OR (owner_id <> $1) = $5)`,
		append(v.args(), status, shared))
	rows, _ := tx.Query(ctx, q, args...)
	reports, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Report, error) {
		rep, err := scanReport(r)
		rep.Access = v.access(rep)
		return rep, err
	})
	if err != nil {
		return nil, nil, fmt.Errorf("analytics: list reports: %w", err)
	}
	return reports, ks, nil
}

// sharedScope is the shared filter as a page token's scope names it.
func sharedScope(shared *bool) string {
	if shared == nil {
		return ""
	}
	return fmt.Sprint(*shared)
}

var reportSort = pagination.Sort{Default: "title", Fields: []pagination.Field{{Name: "title", Column: "title", Type: "text"}}}

// record adds the audit record of a report's change (ADR-0011). A report
// has no event: nothing outside the context reads it (ADR-0076 §2).
func record(ctx context.Context, action string, r Report, detail map[string]any) error {
	if err := audit.Record(ctx, "analytics.report."+action, "analytics.report", r.ID, detail); err != nil {
		return fmt.Errorf("analytics: record %s of report %s: %w", action, r.ID, err)
	}
	return nil
}
