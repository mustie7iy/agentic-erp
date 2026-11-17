package reports_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/analytics/reports"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// facts answer the handlers' queries without the API: the governed
// model over no facts, which is what a direct call has.
type facts struct{ err error }

func (f facts) Runner(context.Context) (analytics.Runner, *analytics.Model, error) {
	m, err := analytics.DefaultModel()
	if err != nil || f.err != nil {
		return nil, nil, errors.Join(err, f.err)
	}
	s := analytics.NewSession(m, &analytics.Facts{}, analytics.Principal{ID: "test"})
	return runner{s}, m, nil
}

// Permissions are what a query of the general ledger reads with.
func (facts) Permissions(*analytics.Model, analytics.Query) []string {
	return []string{"ledger.entry.read"}
}

type runner struct{ s *analytics.Session }

func (r runner) Query(_ context.Context, q analytics.Query) (*analytics.Result, error) {
	return analytics.Execute(r.s.Model, r.s.Facts, r.s.Principal, q)
}

// endpoints are the reports' endpoints as the API wires them, but
// answering queries over no facts.
func endpoints(t *testing.T, q reports.Queries) *reports.Endpoints {
	t.Helper()
	pages, err := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	h := app.Reports(app.Config{Pages: pages}, nil)
	h.Queries = q
	return h
}

// table is a report of one table of revenue.
func table() (gen.ReportSpec, map[string]gen.ReportQuery) {
	return gen.ReportSpec{Title: "Revenue", Tables: []gen.ReportTable{{Title: "Revenue", QueryID: "rev"}}},
		map[string]gen.ReportQuery{"rev": {Measures: []string{"revenue"}, By: []string{"company"}}}
}

// TestDatabaseFailures calls every handler outside a unit of work and
// with each of its database statements failing in turn.
func TestDatabaseFailures(t *testing.T) {
	e := setup(t)
	h := endpoints(t, facts{})
	saved, _ := e.save(t, reportOf(""))
	id := uuid.MustParse(apitest.String(t, saved, "id"))
	spare, _ := e.save(t, reportOf(""))
	spec, queries := table()
	tableOnly, _ := e.save(t, `{"spec": {"title": "Revenue", "tables": [{"title": "Revenue", "query_id": "rev"}]},
		"queries": {"rev": {"measures": ["revenue"], "by": ["company"]}}}`)
	runnable := uuid.MustParse(apitest.String(t, tableOnly, "id"))
	calls := map[string]func(ctx context.Context) error{
		"create": func(ctx context.Context) error {
			_, err := h.CreateReport(ctx, &gen.ReportCreate{Spec: spec, Queries: queries}, gen.CreateReportParams{})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetReport(ctx, gen.GetReportParams{ID: id})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListReports(ctx, gen.ListReportsParams{})
			return err
		},
		"run": func(ctx context.Context) error {
			_, err := h.RunReport(ctx, gen.RunReportParams{ID: runnable, IfMatch: `"1"`})
			return err
		},
		"remove": func(ctx context.Context) error {
			_, err := h.RemoveReport(ctx, gen.RemoveReportParams{ID: uuid.MustParse(apitest.String(t, spare, "id")), IfMatch: `"1"`})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, call)
		})
	}
	t.Run("list page size", func(t *testing.T) {
		err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			_, err := h.ListReports(ctx, gen.ListReportsParams{PageSize: gen.NewOptInt32(0)})
			return err
		})
		if p := problem.From(err); p.Code != "request.invalid_page_size" {
			t.Errorf("got %v, want request.invalid_page_size", p)
		}
	})
}

// TestQueriesFailing: a failure to read the model or run a query fails
// the save and the run, and a removed report's run checks its version
// first.
func TestQueriesFailing(t *testing.T) {
	e := setup(t)
	saved, _ := e.save(t, reportOf(""))
	id := uuid.MustParse(apitest.String(t, saved, "id"))
	broken := errors.New("no model")
	h := endpoints(t, facts{err: broken})
	spec, queries := table()
	for name, call := range map[string]func(ctx context.Context) error{
		"create": func(ctx context.Context) error {
			_, err := h.CreateReport(ctx, &gen.ReportCreate{Spec: spec, Queries: queries}, gen.CreateReportParams{})
			return err
		},
		"run": func(ctx context.Context) error {
			_, err := h.RunReport(ctx, gen.RunReportParams{ID: id, IfMatch: `"1"`})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, call); !errors.Is(err, broken) {
				t.Errorf("got %v, want the model's failure", err)
			}
		})
	}
	t.Run("a query failing", func(t *testing.T) {
		h := endpoints(t, failing{})
		err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			_, err := h.RunReport(ctx, gen.RunReportParams{ID: id, IfMatch: `"1"`})
			return err
		})
		if !errors.Is(err, errQuery) {
			t.Errorf("got %v, want the query's failure", err)
		}
	})
	t.Run("a stale version", func(t *testing.T) {
		err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			_, err := h.RunReport(ctx, gen.RunReportParams{ID: id, IfMatch: `"7"`})
			return err
		})
		if p := problem.From(err); p.Code != "request.version_mismatch" {
			t.Errorf("got %v, want request.version_mismatch", p)
		}
	})
}

var errQuery = errors.New("query failed")

// failing runs no query.
type failing struct{}

func (failing) Runner(context.Context) (analytics.Runner, *analytics.Model, error) {
	m, err := analytics.DefaultModel()
	return failing{}, m, err
}

func (failing) Query(context.Context, analytics.Query) (*analytics.Result, error) {
	return nil, errQuery
}

func (failing) Permissions(*analytics.Model, analytics.Query) []string { return nil }

// modelLost answers the queries of its Queries, but has no model the
// second time it is asked for one: after a run, for its permissions.
type modelLost struct {
	reports.Queries
	asked *int
}

func (q modelLost) Runner(ctx context.Context) (analytics.Runner, *analytics.Model, error) {
	*q.asked++
	if *q.asked > 1 {
		return nil, nil, errQuery
	}
	return q.Queries.Runner(ctx)
}

// people answer no name.
type people struct{}

var errPeople = errors.New("no names")

func (people) Names(context.Context, []uuid.UUID) (map[uuid.UUID]string, error) {
	return nil, errPeople
}

// TestPortAndStoredData: the owners' names failing fails a read; a
// report whose stored specification or queries no longer read is an
// error, never a report; an audit record that cannot be added fails the
// change.
func TestPortAndStoredData(t *testing.T) {
	e := setup(t)
	saved, _ := e.save(t, reportOf(""))
	id := uuid.MustParse(apitest.String(t, saved, "id"))
	h := endpoints(t, facts{})
	h.People = people{}
	for name, call := range map[string]func(ctx context.Context) error{
		"list": func(ctx context.Context) error {
			_, err := h.ListReports(ctx, gen.ListReportsParams{})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetReport(ctx, gen.GetReportParams{ID: id})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, call); !errors.Is(err, errPeople) {
				t.Errorf("got %v, want the names' failure", err)
			}
		})
	}
	t.Run("no audit buffer", func(t *testing.T) {
		h := endpoints(t, facts{})
		spec, queries := table()
		err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			tx, _ := reqctx.TxFrom(ctx)
			p, _ := reqctx.PrincipalFrom(ctx)
			_, err := h.CreateReport(reqctx.WithPrincipal(reqctx.WithTx(context.Background(), tx), p), &gen.ReportCreate{Spec: spec, Queries: queries},
				gen.CreateReportParams{})
			return err
		})
		if !errors.Is(err, audit.ErrNoBuffer) {
			t.Errorf("got %v, want the audit's failure", err)
		}
	})
	for name, set := range map[string]string{"spec": `spec = '{"title": 5}'`, "queries": `queries = '{"rev": 5}'`} {
		t.Run(name, func(t *testing.T) {
			rep, _ := e.save(t, reportOf(""))
			pgtest.Exec(t, `UPDATE analytics.report SET `+set+`, version = 2 WHERE id = $1`, rep["id"])
			if code, body, _ := e.In(t).Do("GET", "/v1/reports/"+apitest.String(t, rep, "id"), "", ""); code != 500 {
				t.Errorf("got %d %v, want 500", code, body)
			}
		})
	}
}

// TestRunsDatabaseFailures calls the schedule's and the runs' handlers
// outside a unit of work and with each of their statements failing.
func TestRunsDatabaseFailures(t *testing.T) {
	e := withObjects(t)
	id := uuid.MustParse(e.scheduled(t))
	e.runOnce(t)
	run := uuid.MustParse(apitest.String(t, e.runs(t, id.String())[0], "id"))
	spare, _ := e.save(t, reportOf(""))
	h := endpoints(t, facts{})
	h.Objects = blobstore.Dir{Root: e.root}
	for name, call := range map[string]func(ctx context.Context) error{
		"schedule": func(ctx context.Context) error {
			_, err := h.ScheduleReport(ctx, &gen.ReportScheduleSet{Frequency: "daily", Company: "2000"},
				gen.ScheduleReportParams{ID: uuid.MustParse(apitest.String(t, spare, "id")), IfMatch: `"1"`})
			return err
		},
		"unschedule": func(ctx context.Context) error {
			_, err := h.UnscheduleReport(ctx, gen.UnscheduleReportParams{ID: id, IfMatch: `"2"`})
			return err
		},
		"list runs": func(ctx context.Context) error {
			_, err := h.ListReportRuns(ctx, gen.ListReportRunsParams{})
			return err
		},
		"get run": func(ctx context.Context) error {
			_, err := h.GetReportRun(ctx, gen.GetReportRunParams{ID: run})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, call)
		})
	}
	in := func(t *testing.T, h *reports.Endpoints, call func(ctx context.Context, h *reports.Endpoints) error) error {
		t.Helper()
		return apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error { return call(ctx, h) })
	}
	getRun := func(ctx context.Context, h *reports.Endpoints) error {
		_, err := h.GetReportRun(ctx, gen.GetReportRunParams{ID: run})
		return err
	}
	t.Run("list page size", func(t *testing.T) {
		err := in(t, h, func(ctx context.Context, h *reports.Endpoints) error {
			_, err := h.ListReportRuns(ctx, gen.ListReportRunsParams{PageSize: gen.NewOptInt32(0)})
			return err
		})
		if p := problem.From(err); p.Code != "request.invalid_page_size" {
			t.Errorf("got %v, want request.invalid_page_size", p)
		}
	})
	t.Run("no object storage", func(t *testing.T) {
		h := endpoints(t, facts{})
		if err := in(t, h, getRun); problem.From(err).Status != 503 {
			t.Errorf("got %v, want 503", err)
		}
	})
	t.Run("the object missing", func(t *testing.T) {
		h := endpoints(t, facts{})
		h.Objects = blobstore.Dir{Root: t.TempDir()}
		if err := in(t, h, getRun); !errors.Is(err, blobstore.ErrNotFound) {
			t.Errorf("got %v, want the store's not found", err)
		}
	})
	t.Run("no names", func(t *testing.T) {
		h := endpoints(t, facts{})
		h.Objects, h.People = blobstore.Dir{Root: e.root}, people{}
		if err := in(t, h, getRun); !errors.Is(err, errPeople) {
			t.Errorf("get: got %v, want the names' failure", err)
		}
		err := in(t, h, func(ctx context.Context, h *reports.Endpoints) error {
			_, err := h.ListReportRuns(ctx, gen.ListReportRunsParams{})
			return err
		})
		if !errors.Is(err, errPeople) {
			t.Errorf("list: got %v, want the names' failure", err)
		}
	})
	// kept is a run's results of one result, its revenue the figure and
	// truncated or not.
	kept := func(figure string, truncated bool) string {
		return `{"report_id": "` + id.String() + `", "title": "T", "data_through": "", "spec": {"title": "T"}, "results": [{"query_id": "q", ` +
			`"columns": [{"name": "revenue", "label": "Revenue", "kind": "measure", "type": "money"}], "rows": [[{"value": "` + figure + `", "currency": "EUR"}]], ` +
			`"source": "database", "as_of": "2026-01-01T00:00:00Z", "data_through": "", "config_version": 1, "definitions": [], "truncated": ` + fmt.Sprint(truncated) + `}]}`
	}
	t.Run("results that do not read", func(t *testing.T) {
		for name, c := range map[string]struct {
			body, format, date, want string
		}{
			"not JSON": {`[`, "", "2026-01-01", "report run"},
			// A figure that is not a decimal, kept with its SHA-256, fails
			// rather than be served wrong.
			"a figure that is not a decimal": {kept("5x", false), "csv", "2026-01-02", `"5x"`},
		} {
			t.Run(name, func(t *testing.T) {
				err := in(t, h, keptRun(t, e, id, c.date, []byte(c.body), c.format))
				if problem.From(err).Status != 500 || !strings.Contains(err.Error(), c.want) {
					t.Errorf("got %v, want a failure naming %s", err, c.want)
				}
			})
		}
	})
	// A result cut at the rows a query answers is refused as a workbook,
	// which could not say so (T95d).
	// Its CSV says so in Result-Truncated, as its JSON does (review:T95d);
	// one not cut says nothing.
	t.Run("a truncated result as CSV", func(t *testing.T) {
		for name, c := range map[string]struct {
			date      string
			truncated bool
			want      gen.OptBool
		}{
			"cut":     {"2026-01-04", true, gen.NewOptBool(true)},
			"not cut": {"2026-01-05", false, gen.OptBool{}},
		} {
			t.Run(name, func(t *testing.T) {
				var got gen.OptBool
				if err := in(t, h, keptCSV(t, e, id, c.date, []byte(kept("5.00", c.truncated)), &got)); err != nil {
					t.Fatal(err)
				}
				if got != c.want {
					t.Errorf("Result-Truncated: got %v, want %v", got, c.want)
				}
			})
		}
	})
	t.Run("a truncated result as a workbook", func(t *testing.T) {
		err := in(t, h, keptRun(t, e, id, "2026-01-03", []byte(kept("5.00", true)), "xlsx"))
		if p := problem.From(err); p.Code != "analytics.result_truncated" {
			t.Errorf("got %v, want analytics.result_truncated", err)
		}
	})
}

// keptRun keeps a succeeded run of the report on the date with the body
// as its results and reads it in the format.
func keptRun(t *testing.T, e stored, report uuid.UUID, date string, body []byte, format string) func(ctx context.Context, h *reports.Endpoints) error {
	t.Helper()
	run := keepRun(t, e, report, date, body)
	return func(ctx context.Context, h *reports.Endpoints) error {
		params := gen.GetReportRunParams{ID: run}
		if format != "" {
			params.Format = gen.NewOptString(format)
		}
		_, err := h.GetReportRun(ctx, params)
		return err
	}
}

// keepRun keeps a succeeded run of the report on the date with the body
// as its results, as its run would, and returns its id.
func keepRun(t *testing.T, e stored, report uuid.UUID, date string, body []byte) uuid.UUID {
	t.Helper()
	sum := sha256.Sum256(body)
	run := uuid.Must(uuid.NewV7())
	key := blobstore.Key(e.Tenant.ID, blobstore.KindReportRuns, run.String()+".json")
	if err := (blobstore.Dir{Root: e.root}).Put(context.Background(), key, body, blobstore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	pgtest.Exec(t, `INSERT INTO analytics.report_run (tenant_id, id, report_id, run_date, status, run_as, object_key, sha256, data_through)
		VALUES ($1, $2, $3, $7, 'succeeded', $4, $5, $6, '')`, e.Tenant.ID, run, report, e.Tenant.Human.ID, key, sum[:], date)
	return run
}

// keptCSV keeps a succeeded run of the report on the date with the body
// as its results and reads its CSV's Result-Truncated header.
func keptCSV(t *testing.T, e stored, report uuid.UUID, date string, body []byte, truncated *gen.OptBool) func(ctx context.Context, h *reports.Endpoints) error {
	t.Helper()
	run := keepRun(t, e, report, date, body)
	return func(ctx context.Context, h *reports.Endpoints) error {
		res, err := h.GetReportRun(ctx, gen.GetReportRunParams{ID: run, Format: gen.NewOptString("csv")})
		if err != nil {
			return err
		}
		*truncated = res.(*gen.GetReportRunOKTextCsvHeaders).ResultTruncated
		return nil
	}
}

// configs are a configuration port that fails, or answers a document.
type configs struct {
	doc *config.Config
	err error
}

func (c configs) Document(context.Context) (*config.Config, int32, error) { return c.doc, 1, c.err }

// storeFailing keeps nothing.
type storeFailing struct{ blobstore.Dir }

var errStore = errors.New("store down")

func (storeFailing) Put(context.Context, string, []byte, blobstore.PutOptions) error { return errStore }

// refusing refuses every query with the problem.
type refusing struct{ p *problem.Problem }

func (r refusing) Runner(context.Context) (analytics.Runner, *analytics.Model, error) {
	m, err := analytics.DefaultModel()
	return r, m, err
}

func (r refusing) Query(context.Context, analytics.Query) (*analytics.Result, error) { return nil, r.p }

func (refusing) Permissions(*analytics.Model, analytics.Query) []string { return nil }

// TestSchedulerPorts: the configuration, the store or a query failing
// fails the round and keeps nothing; a query refused is kept as the run's
// failure, its detail the problem's title when it has none and cut at
// 2,000 characters.
func TestSchedulerPorts(t *testing.T) {
	broken := config.Config{Companies: []config.Company{{Code: "2000", TimeZone: "Nowhere/Zone"}}}
	for name, c := range map[string]struct {
		change func(r *reports.Endpoints)
		want   error
	}{
		"the configuration":        {func(r *reports.Endpoints) { r.Configs = configs{err: errStore} }, errStore},
		"a zone":                   {func(r *reports.Endpoints) { r.Configs = configs{doc: &broken} }, nil},
		"the store":                {func(r *reports.Endpoints) { r.Objects = storeFailing{} }, errStore},
		"a query":                  {func(r *reports.Endpoints) { r.Queries = failing{} }, errQuery},
		"the model, after the run": {func(r *reports.Endpoints) { r.Queries = modelLost{Queries: r.Queries, asked: new(int)} }, errQuery},
		"a server problem":         {func(r *reports.Endpoints) { r.Queries = refusing{problem.Unavailable("busy", 1)} }, nil},
	} {
		t.Run(name, func(t *testing.T) {
			e := withObjects(t)
			id := e.scheduled(t)
			s := *e.s
			rep := *s.Reports
			c.change(&rep)
			s.Reports = &rep
			_, err := s.RunOnce(context.Background())
			if err == nil || (c.want != nil && !errors.Is(err, c.want)) {
				t.Errorf("round: got %v, want %v", err, c.want)
			}
			if runs := e.runs(t, id); len(runs) != 0 {
				t.Errorf("runs: got %v, want none", runs)
			}
		})
	}
	for name, c := range map[string]struct {
		p    *problem.Problem
		want int
	}{
		"no detail":   {problem.Unprocessable("x.refused", ""), len("Unprocessable request")},
		"long detail": {problem.Unprocessable("x.refused", strings.Repeat("é", 2500)), 2000},
		"no code":     {&problem.Problem{Status: 422, Title: "Refused", Detail: "Refused."}, len("Refused.")},
	} {
		t.Run(name, func(t *testing.T) {
			e := withObjects(t)
			id := e.scheduled(t)
			s := *e.s
			rep := *s.Reports
			rep.Queries = refusing{c.p}
			s.Reports = &rep
			if n, err := s.RunOnce(context.Background()); n < 1 || err != nil {
				t.Fatalf("round: got %d, %v; want the failed run", n, err)
			}
			var detail int
			pgtest.Scan(t, `SELECT length(failure_detail) FROM analytics.report_run WHERE report_id = $1`, []any{id}, &detail)
			if detail != c.want {
				t.Errorf("detail: got %d characters, want %d", detail, c.want)
			}
		})
	}
}

// replicaMeanwhile is a store whose first write lets another replica make
// its round first, as one that ran while this one stored its results.
type replicaMeanwhile struct {
	blobstore.Dir
	other func()
	done  *bool
}

func (r replicaMeanwhile) Put(ctx context.Context, key string, body []byte, opts blobstore.PutOptions) error {
	if !*r.done {
		*r.done = true
		r.other()
	}
	return r.Dir.Put(ctx, key, body, opts)
}

// TestSchedulerRunKeptMeanwhile: a replica that keeps the day's run while
// this one stores its results (no transaction is open then) leaves this
// one nothing to keep: one run of the day, kept by the other.
func TestSchedulerRunKeptMeanwhile(t *testing.T) {
	e := withObjects(t)
	id := e.scheduled(t)
	other := *e.s
	s := *e.s
	rep := *s.Reports
	rep.Objects = replicaMeanwhile{Dir: blobstore.Dir{Root: e.root}, done: new(bool), other: func() {
		if _, err := other.RunOnce(context.Background()); err != nil {
			t.Errorf("the other replica's round: %v", err)
		}
	}}
	s.Reports = &rep
	if _, err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("round: %v", err)
	}
	if runs := e.runs(t, id); len(runs) != 1 {
		t.Errorf("runs: got %d, want 1, the other replica's", len(runs))
	}
}

// stopsAfterWrite is a store whose write lands and then fails, as a
// replica that stopped after storing a run's results and before keeping
// the run.
type stopsAfterWrite struct{ blobstore.Dir }

func (s stopsAfterWrite) Put(ctx context.Context, key string, body []byte, opts blobstore.PutOptions) error {
	if err := s.Dir.Put(ctx, key, body, opts); err != nil {
		return err
	}
	return errStore
}

// TestSchedulerKeepsTheResultsStoredBefore: results an attempt stored and
// never kept are the day's: the next round keeps its run on them, with
// their digest, and the store holds one object of the day, not one no
// run refers to (review:batch77).
func TestSchedulerKeepsTheResultsStoredBefore(t *testing.T) {
	e := withObjects(t)
	id := e.scheduled(t)
	stopped := *e.s
	rep := *stopped.Reports
	rep.Objects = stopsAfterWrite{blobstore.Dir{Root: e.root}}
	stopped.Reports = &rep
	if _, err := stopped.RunOnce(context.Background()); !errors.Is(err, errStore) {
		t.Fatalf("the stopped round: got %v, want %v", err, errStore)
	}
	if _, err := e.s.RunOnce(context.Background()); err != nil {
		t.Fatalf("round: %v", err)
	}
	runs := e.runs(t, id)
	if len(runs) != 1 {
		t.Fatalf("runs: got %d, want 1", len(runs))
	}
	var objects []string
	if err := filepath.WalkDir(e.root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".json") {
			objects = append(objects, p)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(objects) != 1 {
		t.Fatalf("objects: got %v, want the day's one", objects)
	}
	body, err := os.ReadFile(objects[0])
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	if got, want := fmt.Sprint(runs[0]["sha256"]), hex.EncodeToString(sum[:]); got != want {
		t.Errorf("run's digest: got %s, want the stored results' %s", got, want)
	}
}

// takenKey is a store whose every key is taken, by results it cannot read
// back (unreadable) or that are no report's.
type takenKey struct {
	blobstore.Dir
	unreadable bool
}

func (s takenKey) Put(context.Context, string, []byte, blobstore.PutOptions) error {
	return blobstore.ErrExists
}

func (s takenKey) Get(context.Context, string) ([]byte, string, error) {
	if s.unreadable {
		return nil, "", errStore
	}
	return []byte("not a report"), "application/json", nil
}

// TestSchedulerResultsStoredBeforeUnusable: results stored before that
// cannot be read, or are no report's, keep no run: the day stays due.
func TestSchedulerResultsStoredBeforeUnusable(t *testing.T) {
	for name, c := range map[string]struct {
		store takenKey
		want  string
	}{
		"unreadable":     {takenKey{unreadable: true}, errStore.Error()},
		"not a report's": {takenKey{}, "not a report's"},
	} {
		t.Run(name, func(t *testing.T) {
			e := withObjects(t)
			id := e.scheduled(t)
			s := *e.s
			rep := *s.Reports
			rep.Objects = c.store
			s.Reports = &rep
			if _, err := s.RunOnce(context.Background()); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("round: got %v, want an error saying %q", err, c.want)
			}
			if runs := e.runs(t, id); len(runs) != 0 {
				t.Errorf("runs: got %v, want none", runs)
			}
		})
	}
}
