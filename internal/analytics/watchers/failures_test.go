package watchers_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/analytics/watchers"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// queries answers every watcher's query with the result, or fails.
type queries struct {
	m        *analytics.Model
	modelErr error
	res      *analytics.Result
	err      error
}

func (q queries) ActiveModel(context.Context) (*analytics.Model, error) { return q.m, q.modelErr }

func (q queries) Answer(context.Context, *analytics.Model, analytics.Query, []string) (*analytics.Result, error) {
	return q.res, q.err
}

// model is the seed model.
func model(t *testing.T) *analytics.Model {
	t.Helper()
	m, err := analytics.DefaultModel()
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// march is 2000's revenue of March, 500.00 EUR, as the query answers it.
func march() *analytics.Result {
	return &analytics.Result{Rows: []analytics.Row{{"2000", "2026", "3", analytics.Value{Units: 50000, Scale: 2, Currency: "EUR"}}}}
}

// endpoints are the watchers' endpoints over q.
func endpoints(t *testing.T, q watchers.Queries) *watchers.Endpoints {
	t.Helper()
	pages, err := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	return &watchers.Endpoints{Pages: pages, Queries: q, Now: func() time.Time { return today }}
}

// TestDatabaseFailures calls every handler outside a unit of work and
// with each of its database statements failing in turn.
func TestDatabaseFailures(t *testing.T) {
	e := setup(t)
	h := endpoints(t, queries{m: model(t), res: march()})
	w := watch(t, e.Client, `{"name": "Above", "metric": "revenue", "grain": "month", "condition": {"kind": "above", "value": "1", "currency": "EUR"}}`)
	id := uuid.MustParse(apitest.String(t, w, "id"))
	calls := map[string]func(ctx context.Context) error{
		"create": func(ctx context.Context) error {
			_, err := h.CreateMetricWatcher(ctx, &gen.MetricWatcherCreate{Name: "x", Metric: "revenue", Grain: "year",
				Condition: gen.MetricWatcherCondition{Kind: "change", Percent: gen.NewOptString("1")}}, gen.CreateMetricWatcherParams{})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetMetricWatcher(ctx, gen.GetMetricWatcherParams{ID: id})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListMetricWatchers(ctx, gen.ListMetricWatchersParams{})
			return err
		},
		"remove": func(ctx context.Context) error {
			_, err := h.RemoveMetricWatcher(ctx, gen.RemoveMetricWatcherParams{ID: id, IfMatch: `"1"`})
			return err
		},
		"evaluate": func(ctx context.Context) error {
			_, err := h.EvaluateMetricWatcher(ctx, gen.EvaluateMetricWatcherParams{ID: id, IfMatch: `"1"`})
			return err
		},
		"fail": func(ctx context.Context) error {
			_, err := endpoints(t, queries{m: &analytics.Model{}}).EvaluateMetricWatcher(ctx, gen.EvaluateMetricWatcherParams{ID: id, IfMatch: `"1"`})
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
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := h.ListMetricWatchers(ctx, gen.ListMetricWatchersParams{PageSize: gen.NewOptInt32(0)})
		return err
	})
	if p := problem.From(err); p.Code != "request.invalid_page_size" {
		t.Errorf("page size 0: got %v, want request.invalid_page_size", p)
	}
}

// TestEvaluationRefusals: a model or an answer that fails fails the
// evaluation; an answer cut at 500 rows refuses it; a metric the model no
// longer has fails the watcher, once, with an audit record.
func TestEvaluationRefusals(t *testing.T) {
	e := setup(t)
	w := watch(t, e.Client, `{"name": "Above", "metric": "revenue", "grain": "month", "condition": {"kind": "above", "value": "1", "currency": "EUR"}}`)
	id := uuid.MustParse(apitest.String(t, w, "id"))
	down := errors.New("down")
	for name, c := range map[string]struct {
		q    queries
		want string
	}{
		"model":     {queries{modelErr: down}, "down"},
		"answer":    {queries{m: model(t), err: down}, "down"},
		"truncated": {queries{m: model(t), res: &analytics.Result{Truncated: true}}, "analytics.watcher_too_many_series"},
	} {
		t.Run(name, func(t *testing.T) {
			err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
				_, err := endpoints(t, c.q).Evaluate(ctx, id, func(int32) error { return nil })
				return err
			})
			pgtest.CheckErr(t, err, c.want)
		})
	}
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := endpoints(t, queries{modelErr: down}).Set(ctx, watchers.NewWatcher{Name: "x"})
		return err
	}); !errors.Is(err, down) {
		t.Errorf("set without a model: got %v, want down", err)
	}
	var failed watchers.Evaluation
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		var err error
		failed, err = endpoints(t, queries{m: &analytics.Model{}}).Evaluate(ctx, id, func(int32) error { return nil })
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if failed.Watcher.Status != watchers.Failed || failed.Watcher.FailureCode != "analytics.watcher_metric_removed" || failed.Alert != nil {
		t.Errorf("metric removed: got %+v, want the watcher failed, no alert", failed)
	}
	got, _ := e.Must(200, "GET", "/v1/metric-watchers/"+id.String(), "", "")
	if got["status"] != "failed" || got["failure_code"] != "analytics.watcher_metric_removed" || got["failure_detail"] == nil {
		t.Errorf("failed watcher: got %v, want failed with its reason", got)
	}
	e.Problem(409, "analytics.watcher_inactive", "POST", "/v1/metric-watchers/"+id.String()+":evaluate", "", etag(got))
	var audits int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND object_id = $2 AND action = 'analytics.metric_watcher.fail'`,
		[]any{e.Tenant.ID, id}, &audits)
	if audits != 1 {
		t.Errorf("audit records of the failure: got %d, want 1", audits)
	}
}

// TestUnreadable: a watcher's filters or an alert's findings that do not
// read as their types fail the read rather than show something else.
func TestUnreadable(t *testing.T) {
	e := setup(t)
	w := watch(t, e.Client, `{"name": "Above", "metric": "revenue", "grain": "month", "condition": {"kind": "above", "value": "1", "currency": "EUR"}}`)
	evaluate(t, e.Client, w)
	pgtest.Exec(t, `UPDATE analytics.metric_alert SET findings = '[1]' WHERE tenant_id = $1`, e.Tenant.ID)
	code, _, _ := e.Do("POST", "/v1/metric-watchers/"+apitest.String(t, w, "id")+":evaluate", "", etag(w))
	if code != 500 {
		t.Errorf("unreadable findings: got %d, want 500", code)
	}
	pgtest.Exec(t, `UPDATE analytics.metric_watcher SET filters = '[1]', version = version + 1 WHERE tenant_id = $1`, e.Tenant.ID)
	if code, _, _ := e.Do("GET", "/v1/metric-watchers/"+apitest.String(t, w, "id"), "", ""); code != 500 {
		t.Errorf("unreadable filters: got %d, want 500", code)
	}
}

// TestWithoutAuditBuffer: a change outside the unit of work's setup, with
// no audit buffer, is refused, not left unrecorded.
func TestWithoutAuditBuffer(t *testing.T) {
	e := setup(t)
	w := watch(t, e.Client, `{"name": "Above", "metric": "revenue", "grain": "month", "condition": {"kind": "above", "value": "1", "currency": "EUR"}}`)
	ctx := context.Background()
	tx, err := pgtest.Pool(t).Begin(ctx, database.Scope{Role: "erp_app", TenantID: e.Tenant.ID})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := database.SetActor(ctx, tx, e.Tenant.Human.ID); err != nil {
		t.Fatal(err)
	}
	bare := reqctx.WithPrincipal(reqctx.WithTx(ctx, tx), reqctx.Principal{ID: e.Tenant.Human.ID, Kind: e.Tenant.Human.Kind, TenantID: e.Tenant.ID})
	if _, err := watchers.Remove(bare, uuid.MustParse(apitest.String(t, w, "id")), func(int32) error { return nil }); !errors.Is(err, audit.ErrNoBuffer) {
		t.Errorf("remove without an audit buffer: got %v, want %v", err, audit.ErrNoBuffer)
	}
}
