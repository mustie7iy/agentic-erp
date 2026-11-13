package reports_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/analytics/reports"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
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
