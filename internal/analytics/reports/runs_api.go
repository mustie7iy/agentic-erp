package reports

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
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
	// Companies are the codes of the companies the run covered; nil for a
	// run kept before T95c, which named none.
	Companies []string
	// Covered says whether the reader's company access covers them (the
	// report's owner alone reads a run that named none).
	Covered bool
}

// runSelect reads the runs of the reports the reader ($1 to $3, as
// readable) reads, each with its report's title and whether the reader's
// companies ($4) cover the run's; the join is inside, so the keyset's id
// is the run's.
const runSelect = `SELECT id, report_id, title, run_date, status, run_as, object_key, sha256, data_through, failure_code, failure_detail, created_at,
		companies, covered
	FROM (SELECT rr.id, rr.report_id, r.title, rr.run_date, rr.status, rr.run_as, rr.object_key, rr.sha256, rr.data_through,
			rr.failure_code, rr.failure_detail, rr.created_at, rr.companies, coalesce(rr.companies <@ $4::text[], r.owner_id = $1) AS covered
		FROM analytics.report_run rr JOIN analytics.report r ON r.tenant_id = rr.tenant_id AND r.id = rr.report_id WHERE ` + readable + `) runs
	WHERE true`

func scanRun(row pgx.Row) (Run, error) {
	var r Run
	err := row.Scan(&r.ID, &r.ReportID, &r.ReportTitle, &r.Date, &r.Status, &r.RunAs, &r.ObjectKey, &r.SHA256, &r.DataThrough,
		&r.FailureCode, &r.FailureDetail, &r.CreatedAt, &r.Companies, &r.Covered)
	return r, errs.Wrapf(err, "scan report run")
}

// runsRequest is the request's transaction and the arguments runSelect
// reads the caller by: as a reader, and the companies they may read now.
func (e *Endpoints) runsRequest(ctx context.Context) (pgx.Tx, []any, error) {
	tx, _, v, err := e.request(ctx)
	if err != nil {
		return nil, nil, err
	}
	companies, err := e.companies(ctx)
	if err != nil {
		return nil, nil, err
	}
	return tx, append(v.args(), companies), nil
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
	q, args := ks.SQL(runSelect+` AND covered AND ($5::uuid IS NULL OR report_id = $5) AND ($6 = '' OR status = $6)`, append(args, report, status))
	rows, _ := tx.Query(ctx, q, args...)
	runs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Run, error) { return scanRun(r) })
	if err != nil {
		return nil, nil, fmt.Errorf("analytics: list report runs: %w", err)
	}
	return runs, ks, nil
}

// GetRun is a run of a report the caller reads, refused (403
// analytics.report_run_companies) unless their company access covers
// every company it covered (ADR-0076 §7).
func (e *Endpoints) GetRun(ctx context.Context, id uuid.UUID) (Run, error) {
	tx, args, err := e.runsRequest(ctx)
	if err != nil {
		return Run{}, err
	}
	r, err := scanRun(tx.QueryRow(ctx, runSelect+` AND id = $5`, append(args, id)...))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Run{}, problem.NotFoundOf("report run")
	case err != nil:
		return Run{}, fmt.Errorf("analytics: read report run %s: %w", id, err)
	case !r.Covered:
		return Run{}, problem.Forbidden("analytics.report_run_companies",
			"The run covered companies your company access does not, or was kept before runs named their companies; run the report to see your companies' figures.")
	}
	return r, nil
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
		RunAsName: names[r.RunAs], CreatedAt: r.CreatedAt, Companies: r.Companies}
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
// results, or one result as CSV; a format the contract does not list is
// 422 request.validation_failed.
func (e *Endpoints) GetReportRun(ctx context.Context, params gen.GetReportRunParams) (gen.GetReportRunRes, error) {
	r, err := e.GetRun(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	if f := params.Format.Value; f != "" && f != "json" && f != "csv" {
		return nil, problem.Invalid("request.validation_failed", "format", "json or csv")
	}
	csv := params.Format.Value == "csv"
	if r.Status != Succeeded {
		if csv {
			return nil, problem.Conflict("analytics.report_run_failed", "The run failed: it has no results.")
		}
		return e.oneRun(ctx, r, nil)
	}
	res, err := e.results(ctx, r)
	if err != nil {
		return nil, err
	}
	if csv {
		return resultCSV(res, params.QueryID)
	}
	return e.oneRun(ctx, r, &res)
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

// resultCSV is the result named, or the run's only one, as CSV.
func resultCSV(res gen.ReportResults, queryID gen.OptString) (*gen.GetReportRunOKTextCsv, error) {
	var picked *gen.AnalyticsResult
	for i, r := range res.Results {
		if (queryID.Set && r.QueryID == queryID.Value) || (!queryID.Set && len(res.Results) == 1) {
			picked = &res.Results[i]
		}
	}
	if picked == nil {
		return nil, problem.Invalid("analytics.unknown_query", "query_id", "Name one of the run's results by its query_id.")
	}
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	// A bytes.Buffer never fails a write, so neither does the CSV writer.
	_ = w.Write(csvHeader(picked.Columns))
	for _, row := range picked.Rows {
		_ = w.Write(csvRow(picked.Columns, row))
	}
	w.Flush()
	return &gen.GetReportRunOKTextCsv{Data: &b}, nil
}

// csvHeader names a result's columns, a money measure followed by its
// currency's column (<name>_currency): an amount is never without it.
func csvHeader(cols []gen.AnalyticsColumn) []string {
	var out []string
	for _, c := range cols {
		out = append(out, c.Name)
		if c.Type.Value == "money" {
			out = append(out, c.Name+"_currency")
		}
	}
	return out
}

// csvRow is a row's cells as csvHeader names them; a measure that cannot
// be computed is empty.
func csvRow(cols []gen.AnalyticsColumn, row []gen.AnalyticsCell) []string {
	var out []string
	for i, c := range cols {
		out = append(out, row[i].Value.Or(""))
		if c.Type.Value == "money" {
			out = append(out, row[i].Currency.Or(""))
		}
	}
	return out
}
