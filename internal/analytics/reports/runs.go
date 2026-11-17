package reports

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/analytics/endpoint"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
	"github.com/mustie7iy/agentic-erp/internal/platform/worker"
)

// The outcomes of a kept run.
const (
	Succeeded = "succeeded"
	Failed    = "failed"
)

// Actors reads who a scheduled run acts as, its report's owner, and
// refuses one no longer active (identity.Actors; product owner
// 2026-11-03).
type Actors interface {
	Actor(ctx context.Context, tx pgx.Tx, id uuid.UUID) (reqctx.Principal, error)
	MayAct(ctx context.Context, tx pgx.Tx, id uuid.UUID) error
}

// Scheduler makes the due runs of scheduled reports (ADR-0076 §5): each
// round it finds the active reports with a schedule across tenants and
// runs each one due as its owner, in a transaction of the owner's tenant
// under an advisory lock of the report, so replicas never run one report
// at once; a run is kept once per report and day (report_run_once).
type Scheduler struct {
	Reports    *Endpoints
	Actors     Actors
	Begin      database.BeginFunc
	AppRole    string        // erp_app: the run's transaction, as the owner
	WorkerRole string        // erp_worker: finds the scheduled reports across tenants
	Interval   time.Duration // idle wait between rounds; default 30 seconds
	// Agents admits an agent owner's run in its transaction: the tenant's
	// AI kill switch (agents.Admit), as the unit of work admits an agent's
	// request; nil admits every agent.
	Agents func(ctx context.Context, tx pgx.Tx) error
	Log    worker.Logger
}

// scheduled is a report with a schedule.
type scheduled struct{ tenant, report, owner uuid.UUID }

// RunOnce makes every due run and returns how many it kept. A report
// whose run fails to be made (not a run that failed: that is kept) is
// left for the next round; the others go on.
func (s *Scheduler) RunOnce(ctx context.Context) (int, error) {
	reports, err := s.scheduled(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, r := range reports {
		ran, err := s.runReport(ctx, r)
		if ran {
			n++
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return n, errors.Join(errs...)
}

// Run makes due runs until ctx ends; errors are logged and retried.
func (s *Scheduler) Run(ctx context.Context) {
	worker.Loop(ctx, s.Interval, 30*time.Second, func(ctx context.Context) (bool, error) {
		_, err := s.RunOnce(ctx)
		return false, err
	}, s.Log, "analytics report runs failed")
}

// scheduled are the active tenants' active reports with a schedule.
func (s *Scheduler) scheduled(ctx context.Context) ([]scheduled, error) {
	var out []scheduled
	job := uow.Job{Begin: s.Begin, Name: "analytics: scheduled reports", Scope: database.Scope{Role: s.WorkerRole}}
	err := job.Tx(ctx, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		rows, _ := tx.Query(ctx, `SELECT tenant_id, id, owner_id FROM analytics.report
			WHERE schedule_frequency IS NOT NULL AND status = 'active' AND tenant_id IN `+database.ActiveTenants+` ORDER BY tenant_id, id`)
		var err error
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (scheduled, error) {
			var sc scheduled
			return sc, r.Scan(&sc.tenant, &sc.report, &sc.owner)
		})
		if err != nil {
			return false, fmt.Errorf("analytics: scheduled reports: %w", err)
		}
		return false, nil
	})
	return out, err
}

// runReport makes the report's run when one is due, as its owner, and
// reports whether it kept one. Its queries run in one transaction, its
// results are stored with none open (an object store's write may take a
// while, and a transaction held across it would hold a connection and the
// report's lock as long), and the run is kept in a second, which finds
// the day still due before it keeps it: a run another replica kept
// meanwhile leaves this one's object unreferenced, never two runs of a
// day.
func (s *Scheduler) runReport(ctx context.Context, sc scheduled) (bool, error) {
	var run *keptRun
	var body []byte
	if err := s.asOwner(ctx, sc, func(ctx context.Context, tx pgx.Tx, owner reqctx.Principal) (err error) {
		run, body, err = s.due(ctx, tx, owner, sc)
		return err
	}); err != nil || run == nil {
		return false, err
	}
	if body != nil {
		if err := s.Reports.Objects.Put(ctx, run.ObjectKey, body, blobstore.PutOptions{ContentType: "application/json"}); err != nil {
			return false, fmt.Errorf("analytics: keep run %s of report %s: %w", run.ID, run.Report.ID, err)
		}
	}
	ran := false
	err := s.asOwner(ctx, sc, func(ctx context.Context, tx pgx.Tx, p reqctx.Principal) error {
		// The report may have been removed or unscheduled while its results
		// were stored, or the day kept by another replica: then nothing is kept.
		r, err := report(ctx, tx, owner(p.ID), sc.report, false)
		if err != nil || r.Schedule == nil || !r.Schedule.On(run.Date) {
			return err
		}
		done, err := kept(ctx, tx, run.Report.ID, run.Date)
		if err != nil || done {
			return err
		}
		ran = true
		return keep(ctx, tx, p, *run)
	})
	return ran && err == nil, err
}

// asOwner runs fn in a transaction of the report's tenant as its owner,
// holding the report's run lock; a report another replica holds is
// skipped.
func (s *Scheduler) asOwner(ctx context.Context, sc scheduled, fn func(ctx context.Context, tx pgx.Tx, owner reqctx.Principal) error) error {
	var (
		tx    pgx.Tx
		owner reqctx.Principal
	)
	job := uow.Job{Begin: s.Begin, Name: "analytics: run report " + sc.report.String(), Scope: database.Scope{Role: s.AppRole, TenantID: sc.tenant},
		Now: s.Reports.Now}
	return job.As(ctx, func(ctx context.Context, t pgx.Tx) (reqctx.Principal, bool, error) {
		var mine bool
		if err := t.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended('analytics.report_run:' || $1::text, 0))`, sc.report).Scan(&mine); err != nil {
			return reqctx.Principal{}, false, fmt.Errorf("analytics: lock the runs of report %s: %w", sc.report, err)
		}
		if !mine { // another replica is running it
			return reqctx.Principal{}, false, nil
		}
		p, err := s.Actors.Actor(ctx, t, sc.owner)
		if err != nil {
			return reqctx.Principal{}, false, fmt.Errorf("analytics: the owner of report %s: %w", sc.report, err)
		}
		tx, owner = t, p
		return p, true, nil
	}, func(ctx context.Context) error {
		return fn(ctx, tx, owner)
	})
}

// due makes the report's run for the company's date today when one is due
// and none is kept, as its owner p (in ctx, with tx): the run to keep and,
// for a success, the results to store at its object key; nil when none is
// due.
func (s *Scheduler) due(ctx context.Context, tx pgx.Tx, p reqctx.Principal, sc scheduled) (*keptRun, []byte, error) {
	r, err := report(ctx, tx, owner(p.ID), sc.report, false)
	// Removed or unscheduled since it was found: a removed report has no
	// schedule (report_removed_unscheduled).
	if err != nil || r.Schedule == nil {
		return nil, nil, err
	}
	// A company the configuration no longer has dates the run in UTC, as
	// a company without a time zone does (config.Config.CompanyDay).
	day, _, err := s.Reports.companyDay(ctx, r.Schedule.Company)
	if err != nil {
		return nil, nil, fmt.Errorf("analytics: run report %s: %w", r.ID, err)
	}
	done, err := kept(ctx, tx, r.ID, day.Date)
	if err != nil || done || !r.Schedule.On(day.Date) {
		return nil, nil, err
	}
	run := keptRun{ID: uuid.Must(uuid.NewV7()), Report: r, Date: day.Date, RunAs: p.ID}
	body, err := s.execute(ctx, tx, p, &run)
	if err != nil {
		return nil, nil, err
	}
	return &run, body, nil
}

// kept reports whether the report has a run of the day.
func kept(ctx context.Context, tx pgx.Tx, report uuid.UUID, day time.Time) (bool, error) {
	var done bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM analytics.report_run WHERE report_id = $1 AND run_date = $2)`, report, day).Scan(&done); err != nil {
		return false, fmt.Errorf("analytics: runs of report %s: %w", report, err)
	}
	return done, nil
}

// keptRun is a run as it is kept.
type keptRun struct {
	ID          uuid.UUID
	Report      Report
	Date        time.Time
	RunAs       uuid.UUID
	ObjectKey   string
	SHA256      []byte
	DataThrough string
	// Companies are the codes of the companies a success covered, the
	// owner's company access when it ran, and Permissions those its
	// queries read their data with (ADR-0076 §7); none for a failure.
	Companies, Permissions []string
	// FailureCode and FailureDetail are the problem that failed the run;
	// "" for a success.
	FailureCode, FailureDetail string
}

// execute runs the report's queries as its owner and returns its results
// to store at the run's object key, or records the problem that failed it
// (no results): the owner no longer active, or a refusal of a query (below
// 500). Any other error is returned, leaving the day due.
func (s *Scheduler) execute(ctx context.Context, tx pgx.Tx, p reqctx.Principal, run *keptRun) ([]byte, error) {
	if err := s.mayRun(ctx, tx, p); err != nil {
		return nil, failed(run, err)
	}
	// The companies the queries cover, read in the same transaction as
	// they run (endpoint's principal reads the owner's access too).
	companies, err := s.Reports.companies(ctx)
	if err != nil {
		return nil, err
	}
	sess, out, results, err := s.Reports.rerun(reqctx.WithActing(ctx, p), run.Report.Spec, run.Report.Queries)
	if err != nil {
		return nil, failed(run, err)
	}
	permissions, err := s.Reports.permissions(ctx, run.Report.Queries)
	if err != nil {
		return nil, err
	}
	res := Results{Report: run.Report, Spec: out.Spec, DataThrough: sess.DataThrough()}
	for _, qid := range sortedIDs(results) {
		res.Results = append(res.Results, results[qid])
	}
	api := resultsToAPI(res)
	body, _ := api.MarshalJSON() // the generated type always encodes
	sum := sha256.Sum256(body)
	key := blobstore.Key(p.TenantID, blobstore.KindReportRuns, run.ID.String()+".json")
	run.ObjectKey, run.SHA256, run.DataThrough, run.Companies, run.Permissions = key, sum[:], res.DataThrough, companies, permissions
	return body, nil
}

// mayRun authorizes the run as the owner's own runReport is authorized
// (ADR-0076 §5): an owner still active, an agent the tenant's AI kill
// switch admits, and the permissions of runReport, segregation-of-duties
// rules applied.
func (s *Scheduler) mayRun(ctx context.Context, tx pgx.Tx, p reqctx.Principal) error {
	if err := s.Actors.MayAct(ctx, tx, p.ID); err != nil {
		return err
	}
	if s.Agents != nil && p.Kind == reqctx.KindAgent {
		if err := s.Agents(ctx, tx); err != nil {
			return err
		}
	}
	return authz.Require(ctx, "analytics.report.read", "analytics.query.run")
}

// failed records the problem of a refusal on the run; anything else is
// returned.
func failed(run *keptRun, err error) error {
	var p *problem.Problem
	if !errors.As(err, &p) || p.Status >= 500 {
		return err
	}
	d := p.Detail
	if d == "" {
		d = p.Title
	}
	if utf8.RuneCountInString(d) > maxDetail {
		d = string([]rune(d)[:maxDetail])
	}
	code := p.Code
	if code == "" { // every problem the API answers has one; a run is kept with one whatever reached it
		code = "analytics.report_run_refused"
	}
	run.FailureCode, run.FailureDetail = code, d
	return nil
}

// maxDetail is the longest problem detail a run keeps, in characters.
const maxDetail = 2000

// keep inserts the run with its audit record and ReportRunCompleted.
func keep(ctx context.Context, tx pgx.Tx, p reqctx.Principal, run keptRun) error {
	status, code, detail, sum, through := Succeeded, (*string)(nil), (*string)(nil), (*string)(nil), (*string)(nil)
	var key *string
	companies, permissions := run.Companies, run.Permissions
	if companies == nil { // a failure covered none and read nothing
		companies, permissions = []string{}, []string{}
	}
	if run.FailureCode != "" {
		status = Failed
		code, detail = &run.FailureCode, &run.FailureDetail
	} else {
		h := hex.EncodeToString(run.SHA256)
		key, sum, through = &run.ObjectKey, &h, &run.DataThrough
	}
	if _, err := tx.Exec(ctx, `INSERT INTO analytics.report_run (tenant_id, id, report_id, run_date, status, run_as, object_key, sha256, data_through,
			failure_code, failure_detail, companies, permissions) VALUES ($1, $2, $3, $4, $5, $6, $7, decode($8, 'hex'), $9, $10, $11, $12, $13)`,
		p.TenantID, run.ID, run.Report.ID, run.Date, status, run.RunAs, key, sum, through, code, detail, companies, permissions); err != nil {
		return fmt.Errorf("analytics: keep run %s of report %s: %w", run.ID, run.Report.ID, err)
	}
	date := run.Date.Format("2006-01-02")
	payload := map[string]any{"report_run_id": run.ID, "report_id": run.Report.ID, "report_title": run.Report.Title, "run_as": run.RunAs,
		"run_date": date, "status": status, "failure_code": code, "sha256": sum}
	if err := audit.Record(ctx, "analytics.report_run.complete", "analytics.report_run", run.ID,
		map[string]any{"report_id": run.Report.ID, "run_date": date, "status": status, "failure_code": code, "sha256": sum},
		outbox.Event{Context: "analytics", Name: "report_run_completed", AggregateType: "analytics.report_run", AggregateID: run.ID, Payload: payload}); err != nil {
		return fmt.Errorf("analytics: record run %s of report %s: %w", run.ID, run.Report.ID, err)
	}
	return nil
}

// resultsToAPI is a run's results as the contract writes them.
func resultsToAPI(res Results) gen.ReportResults {
	out := gen.ReportResults{ReportID: res.Report.ID, Title: res.Report.Title, Spec: specToAPI(res.Spec), DataThrough: res.DataThrough,
		Results: make([]gen.AnalyticsResult, len(res.Results))}
	for i, r := range res.Results {
		out.Results[i] = endpoint.ResultToAPI(r)
	}
	return out
}

// permissions are those the queries read their data with, under the
// active model, in order: a kept run's results are read only by whoever
// holds them all (ADR-0076 §7).
func (e *Endpoints) permissions(ctx context.Context, queries map[string]analytics.Query) ([]string, error) {
	_, m, err := e.Queries.Runner(ctx)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, q := range queries {
		for _, p := range e.Queries.Permissions(m, q) {
			set[p] = true
		}
	}
	return slices.Sorted(maps.Keys(set)), nil
}
