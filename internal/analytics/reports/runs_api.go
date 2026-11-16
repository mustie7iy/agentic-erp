package reports

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Run is a row of analytics.report_run with its report's title.
type Run struct {
	ID            uuid.UUID
	ReportID      uuid.UUID
	ReportTitle   string
	Date          time.Time
	Status        string
	RunAs         uuid.UUID
	ObjectKey     *string
	SHA256        []byte
	DataThrough   *string
	FailureCode   *string
	FailureDetail *string
	CreatedAt     time.Time
	// Companies are the codes of the companies the run covered and
	// Permissions those its queries read their data with; nil for a run
	// kept before T95c, which named neither.
	Companies, Permissions []string
	// Covered says whether the reader's company access covers the
	// companies and they hold the permissions (the report's owner alone
	// reads a run that named neither).
	Covered bool
}

// runSelect reads the runs of the reports the reader ($1 to $3, as
// readable) reads, each with its report's title and whether the reader's
// companies ($4) cover the run's and their permissions ($5) hold the
// run's; the join is inside, so the keyset's id is the run's.
const runSelect = `SELECT id, report_id, title, run_date, status, run_as, object_key, sha256, data_through, failure_code, failure_detail, created_at,
		companies, permissions, covered
	FROM (SELECT rr.id, rr.report_id, r.title, rr.run_date, rr.status, rr.run_as, rr.object_key, rr.sha256, rr.data_through,
			rr.failure_code, rr.failure_detail, rr.created_at, rr.companies, rr.permissions,
			coalesce(rr.companies <@ $4::text[] AND rr.permissions <@ $5::text[], r.owner_id = $1) AS covered
		FROM analytics.report_run rr JOIN analytics.report r ON r.tenant_id = rr.tenant_id AND r.id = rr.report_id WHERE ` + readable + `) runs
	WHERE true`

func scanRun(row pgx.Row) (Run, error) {
	var r Run
	err := row.Scan(&r.ID, &r.ReportID, &r.ReportTitle, &r.Date, &r.Status, &r.RunAs, &r.ObjectKey, &r.SHA256, &r.DataThrough,
		&r.FailureCode, &r.FailureDetail, &r.CreatedAt, &r.Companies, &r.Permissions, &r.Covered)
	return r, errs.Wrapf(err, "scan report run")
}

// runsRequest is the request's transaction and the arguments runSelect
// reads the caller by: as a reader, the companies they may read and the
// permissions they hold now.
func (e *Endpoints) runsRequest(ctx context.Context) (pgx.Tx, []any, error) {
	tx, _, v, err := e.request(ctx)
	if err != nil {
		return nil, nil, err
	}
	companies, err := e.companies(ctx)
	if err != nil {
		return nil, nil, err
	}
	held, err := authz.Permissions(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("analytics: permissions of the caller: %w", err)
	}
	return tx, append(v.args(), companies, slices.Sorted(maps.Keys(held))), nil
}

var runSort = pagination.Sort{Default: "run_date", DefaultDesc: true, Fields: []pagination.Field{{Name: "run_date", Column: "run_date", Type: "date"}}}

// Runs lists a page of the runs of the reports the caller reads whose
// companies their access covers, the latest day first, filtered by report
// and status.
func (e *Endpoints) Runs(ctx context.Context, report *uuid.UUID, status string, page pagination.Request) ([]Run, *pagination.Keyset, error) {
	tx, args, err := e.runsRequest(ctx)
	if err != nil {
		return nil, nil, err
	}
	ks, err := e.Pages.Keyset(runSort, fmt.Sprintf("listReportRuns|%s|%s", report, status), page)
	if err != nil {
		return nil, nil, fmt.Errorf("analytics: page of report runs: %w", err)
	}
	q, args := ks.SQL(runSelect+` AND covered AND ($6::uuid IS NULL OR report_id = $6) AND ($7 = '' OR status = $7)`, append(args, report, status))
	rows, _ := tx.Query(ctx, q, args...)
	runs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Run, error) { return scanRun(r) })
	if err != nil {
		return nil, nil, fmt.Errorf("analytics: list report runs: %w", err)
	}
	return runs, ks, nil
}

// GetRun is a run of a report the caller reads, refused (403
// analytics.report_run_access) unless their company access covers every
// company it covered and they may use every permission its queries read
// their data with (ADR-0076 §7).
func (e *Endpoints) GetRun(ctx context.Context, id uuid.UUID) (Run, error) {
	tx, args, err := e.runsRequest(ctx)
	if err != nil {
		return Run{}, err
	}
	r, err := scanRun(tx.QueryRow(ctx, runSelect+` AND id = $6`, append(args, id)...))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Run{}, problem.NotFoundOf("report run")
	case err != nil:
		return Run{}, fmt.Errorf("analytics: read report run %s: %w", id, err)
	case !r.Covered:
		return Run{}, problem.Forbidden("analytics.report_run_access",
			"The run covered companies your company access does not, read data with permissions you do not hold, or was kept before runs named them; run the report to see what your access shows.")
	}
	// A permission held but barred by a segregation-of-duties rule refuses
	// the run as it refuses the query (endpoint.datasets).
	return r, authz.Require(ctx, r.Permissions...)
}

// results reads a succeeded run's results back from object storage,
// compared with the SHA-256 kept (409 analytics.report_run_changed).
func (e *Endpoints) results(ctx context.Context, r Run) (gen.ReportResults, error) {
	if e.Objects == nil {
		return gen.ReportResults{}, errNoObjects()
	}
	body, _, err := e.Objects.Get(ctx, *r.ObjectKey)
	if err != nil {
		return gen.ReportResults{}, fmt.Errorf("analytics: results of report run %s: %w", r.ID, err)
	}
	if sum := sha256.Sum256(body); !bytes.Equal(sum[:], r.SHA256) {
		return gen.ReportResults{}, problem.Conflict("analytics.report_run_changed",
			"The results kept for this run are not the ones recorded: their SHA-256 differs.")
	}
	var out gen.ReportResults
	if err := out.UnmarshalJSON(body); err != nil {
		return gen.ReportResults{}, fmt.Errorf("analytics: results of report run %s: %w", r.ID, err)
	}
	return out, nil
}

// runToAPI is a run as the API lists it, with the name of whom it ran as.
func runToAPI(r Run, names map[uuid.UUID]string) gen.ReportRun {
	out := gen.ReportRun{ID: r.ID, ReportID: r.ReportID, ReportTitle: r.ReportTitle, RunDate: r.Date, Status: r.Status, RunAs: r.RunAs,
		RunAsName: names[r.RunAs], CreatedAt: r.CreatedAt, Companies: r.Companies, Permissions: r.Permissions}
	if r.FailureCode != nil {
		out.Failure = gen.NewOptReportRunFailure(gen.ReportRunFailure{Code: *r.FailureCode, Detail: *r.FailureDetail})
	}
	if r.SHA256 != nil {
		out.SHA256 = gen.NewOptString(hex.EncodeToString(r.SHA256))
		out.DataThrough = gen.NewOptString(*r.DataThrough)
	}
	return out
}

// runNames reads the names of the principals runs acted as.
func (e *Endpoints) runNames(ctx context.Context, runs []Run) (map[uuid.UUID]string, error) {
	reports := make([]Report, len(runs))
	for i, r := range runs {
		reports[i] = Report{OwnerID: r.RunAs}
	}
	return e.names(ctx, reports)
}

// ListReportRuns implements GET /v1/report-runs.
func (e *Endpoints) ListReportRuns(ctx context.Context, params gen.ListReportRunsParams) (gen.ListReportRunsRes, error) {
	runs, ks, err := e.Runs(ctx, apitypes.Ptr[uuid.UUID](params.ReportID), params.Status.Value,
		apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	items, next, err := apitypes.PageWith(ks, runs, func(r Run) (string, uuid.UUID) { return r.Date.Format("2006-01-02"), r.ID },
		func(page []Run) (map[uuid.UUID]string, error) { return e.runNames(ctx, page) }, runToAPI)
	if err != nil {
		return nil, err
	}
	return &gen.ReportRunPage{Items: items, NextPageToken: next}, nil
}

// GetReportRun implements GET /v1/report-runs/{id}: the run with its
// results, or one result as CSV or an Excel workbook; a format the
// contract does not list is 422 request.validation_failed.
func (e *Endpoints) GetReportRun(ctx context.Context, params gen.GetReportRunParams) (gen.GetReportRunRes, error) {
	r, err := e.GetRun(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	f := params.Format.Value
	if f != "" && f != "json" && f != "csv" && f != "xlsx" {
		return nil, problem.Invalid("request.validation_failed", "format", "json, csv or xlsx")
	}
	one := f == "csv" || f == "xlsx"
	if r.Status != Succeeded {
		if one {
			return nil, problem.Conflict("analytics.report_run_failed", "The run failed: it has no results.")
		}
		return e.answer(ctx, r, nil)
	}
	res, err := e.results(ctx, r)
	if err != nil {
		return nil, err
	}
	if !one {
		return e.answer(ctx, r, &res)
	}
	picked, err := pickResult(res, params.QueryID)
	if err != nil {
		return nil, err
	}
	switch {
	case f == "csv":
		return resultCSV(picked), nil
	case picked.Truncated:
		// The workbook holds the rows alone and could not say they were cut.
		return nil, problem.Unprocessable("analytics.result_truncated",
			"The result was cut at the rows a query answers; a workbook could not say so. Read it as JSON, which does.")
	}
	return resultXLSX(picked), nil
}

// answer is the run as getReportRun answers it as JSON.
func (e *Endpoints) answer(ctx context.Context, r Run, res *gen.ReportResults) (*gen.ReportRunHeaders, error) {
	out, err := e.oneRun(ctx, r, res)
	if err != nil {
		return nil, err
	}
	return &gen.ReportRunHeaders{Response: *out}, nil
}

// oneRun is a run as getReportRun gives it, with its results when it has
// them.
func (e *Endpoints) oneRun(ctx context.Context, r Run, res *gen.ReportResults) (*gen.ReportRun, error) {
	names, err := e.runNames(ctx, []Run{r})
	if err != nil {
		return nil, err
	}
	out := runToAPI(r, names)
	if res != nil {
		out.Spec, out.Results = gen.NewOptReportSpec(res.Spec), res.Results
	}
	return &out, nil
}

// pickResult is the result named, or the run's only one, as the
// analytics package reads a result (analytics.DecodeResult).
func pickResult(res gen.ReportResults, queryID gen.OptString) (*analytics.Result, error) {
	var picked *gen.AnalyticsResult
	for i, r := range res.Results {
		if (queryID.Set && r.QueryID == queryID.Value) || (!queryID.Set && len(res.Results) == 1) {
			picked = &res.Results[i]
		}
	}
	if picked == nil {
		return nil, problem.Invalid("analytics.unknown_query", "query_id", "Name one of the run's results by its query_id.")
	}
	b, err := picked.MarshalJSON()
	if err == nil {
		var out *analytics.Result
		if out, err = analytics.DecodeResult(b); err == nil {
			return out, nil
		}
	}
	return nil, fmt.Errorf("analytics: result %s of a report run: %w", picked.QueryID, err)
}

// resultCSV is a result as CSV: a header row of its columns' names, a
// money measure followed by its currency's column (<name>_currency), then
// its rows, a measure that cannot be computed empty (analytics.Records,
// which the workbook lays out alike). A result cut at the rows a query
// answers says so in Result-Truncated, as its JSON's truncated does: its
// rows could not (review:T95d).
func resultCSV(r *analytics.Result) *gen.GetReportRunOKTextCsvHeaders {
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	// A bytes.Buffer never fails a write, so neither does the CSV writer.
	_ = w.WriteAll(analytics.Records(r))
	out := &gen.GetReportRunOKTextCsvHeaders{Response: gen.GetReportRunOKTextCsv{Data: &b}}
	if r.Truncated {
		out.ResultTruncated.SetTo(true)
	}
	return out
}

// resultXLSX is a result as an Excel workbook (T95d, ADR-0076 §8).
func resultXLSX(r *analytics.Result) *gen.GetReportRunOKApplicationVndOpenxmlformatsOfficedocumentSpreadsheetmlSheetHeaders {
	var b bytes.Buffer
	// A bytes.Buffer never fails a write, so neither does the workbook.
	_ = analytics.WriteXLSX(&b, analytics.SheetReport, r)
	return &gen.GetReportRunOKApplicationVndOpenxmlformatsOfficedocumentSpreadsheetmlSheetHeaders{
		Response: gen.GetReportRunOKApplicationVndOpenxmlformatsOfficedocumentSpreadsheetmlSheet{Data: &b}}
}
