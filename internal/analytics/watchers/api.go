package watchers

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

func filtersFromAPI(in []gen.AnalyticsFilter) []analytics.Filter {
	out := make([]analytics.Filter, len(in))
	for i, f := range in {
		out[i] = analytics.Filter{Dim: f.Dim, Op: f.Op, Values: f.Values}
	}
	return out
}

func filtersToAPI(in []analytics.Filter) []gen.AnalyticsFilter {
	out := make([]gen.AnalyticsFilter, len(in))
	for i, f := range in {
		out[i] = gen.AnalyticsFilter{Dim: f.Dim, Op: f.Op, Values: append([]string{}, f.Values...)}
	}
	return out
}

func conditionFromAPI(c gen.MetricWatcherCondition) Condition {
	return Condition{Kind: c.Kind, Value: c.Value.Value, Currency: c.Currency.Value, Percent: c.Percent.Value}
}

func conditionToAPI(c Condition) gen.MetricWatcherCondition {
	return gen.MetricWatcherCondition{Kind: c.Kind, Value: apitypes.OptText(c.Value), Currency: apitypes.OptText(c.Currency),
		Percent: apitypes.OptText(c.Percent)}
}

// toAPI is a watcher as the API gives it.
func toAPI(w Watcher) gen.MetricWatcher {
	return gen.MetricWatcher{ID: w.ID, Version: w.Version, CreatedAt: w.CreatedAt, CreatedBy: w.CreatedBy, UpdatedAt: w.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(w.UpdatedBy), Name: w.Name, Metric: w.Metric, Filters: filtersToAPI(w.Filters),
		Company: apitypes.OptText(w.Company), Grain: w.Grain, Condition: conditionToAPI(w.Condition), Status: w.Status,
		FailureCode: apitypes.OptText(w.FailureCode), FailureDetail: apitypes.OptText(w.FailureDetail)}
}

// alertToAPI is an alert as the API gives it.
func alertToAPI(a Alert) gen.MetricAlert {
	findings := make([]gen.MetricAlertFinding, len(a.Findings))
	for i, f := range a.Findings {
		findings[i] = gen.MetricAlertFinding{Company: f.Company, Currency: apitypes.OptText(f.Currency), Figure: f.Figure,
			ComparedWith: f.ComparedWith, Percent: apitypes.OptText(f.Percent)}
	}
	return gen.MetricAlert{ID: a.ID, Version: a.Version, WatcherID: a.WatcherID, WatcherName: a.WatcherName, Metric: a.Metric, Grain: a.Grain,
		Condition: conditionToAPI(a.Condition), FiscalYear: a.FiscalYear, FiscalPeriod: apitypes.OptInt32(a.FiscalPeriod),
		Companies: a.Companies, Findings: findings, Status: a.Status, RaisedAt: a.RaisedAt, RaisedAs: a.RaisedAs}
}

// evaluationToAPI is an evaluation as the API gives it.
func evaluationToAPI(ev Evaluation) gen.MetricWatcherEvaluation {
	out := gen.MetricWatcherEvaluation{Watcher: toAPI(ev.Watcher), Series: make([]gen.MetricWatcherSeries, len(ev.Series)), Raised: ev.Raised}
	if at := ev.At; at != nil {
		out.FiscalYear = gen.NewOptInt32(int32(at.year)) //nolint:gosec // a fiscal year of four digits (metric_alert_fiscal_year_check)
		if at.month != 0 {
			out.FiscalPeriod = gen.NewOptInt32(int32(at.month)) //nolint:gosec // a fiscal month, 1 to 12
		}
	}
	for i, j := range ev.Series {
		out.Series[i] = gen.MetricWatcherSeries{Company: j.Company, Currency: apitypes.OptText(j.Currency), Figure: j.Figure.String(),
			ComparedWith: apitypes.OptText(j.ComparedWith), Percent: apitypes.OptText(j.Percent), Holds: j.Holds}
	}
	if ev.Alert != nil {
		out.Alert = gen.NewOptMetricAlert(alertToAPI(*ev.Alert))
	}
	return out
}

func tagOf(w Watcher) gen.OptString { return gen.NewOptString(etag.Format(w.Version)) }

// CreateMetricWatcher implements POST /v1/metric-watchers.
func (e *Endpoints) CreateMetricWatcher(ctx context.Context, req *gen.MetricWatcherCreate, _ gen.CreateMetricWatcherParams) (gen.CreateMetricWatcherRes, error) {
	w, err := e.Set(ctx, NewWatcher{Name: req.Name, Metric: req.Metric, Filters: filtersFromAPI(req.Filters), Company: req.Company.Value,
		Grain: req.Grain, Condition: conditionFromAPI(req.Condition)})
	if err != nil {
		return nil, err
	}
	return &gen.MetricWatcherHeaders{ETag: tagOf(w), Location: gen.NewOptString("/v1/metric-watchers/" + w.ID.String()), Response: toAPI(w)}, nil
}

// GetMetricWatcher implements GET /v1/metric-watchers/{id}.
func (e *Endpoints) GetMetricWatcher(ctx context.Context, params gen.GetMetricWatcherParams) (gen.GetMetricWatcherRes, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	w, err := watcher(ctx, tx, p.ID, params.ID, false)
	if err != nil {
		return nil, err
	}
	return &gen.GetMetricWatcherOKHeaders{ETag: tagOf(w), Response: toAPI(w)}, nil
}

// RemoveMetricWatcher implements POST /v1/metric-watchers/{id}:remove.
func (e *Endpoints) RemoveMetricWatcher(ctx context.Context, params gen.RemoveMetricWatcherParams) (gen.RemoveMetricWatcherRes, error) {
	w, err := Remove(ctx, params.ID, etag.IfMatch(params.IfMatch))
	if err != nil {
		return nil, err
	}
	return &gen.RemoveMetricWatcherOKHeaders{ETag: tagOf(w), Response: toAPI(w)}, nil
}

// EvaluateMetricWatcher implements POST /v1/metric-watchers/{id}:evaluate.
func (e *Endpoints) EvaluateMetricWatcher(ctx context.Context, params gen.EvaluateMetricWatcherParams) (gen.EvaluateMetricWatcherRes, error) {
	ev, err := e.Evaluate(ctx, params.ID, etag.IfMatch(params.IfMatch))
	if err != nil {
		return nil, err
	}
	return &gen.MetricWatcherEvaluationHeaders{ETag: tagOf(ev.Watcher), Response: evaluationToAPI(ev)}, nil
}

var watcherSort = pagination.Sort{Default: "name", Fields: []pagination.Field{{Name: "name", Column: "name", Type: "text"}}}

// ListMetricWatchers implements GET /v1/metric-watchers: the caller's
// watchers by name, filtered by status and metric.
func (e *Endpoints) ListMetricWatchers(ctx context.Context, params gen.ListMetricWatchersParams) (gen.ListMetricWatchersRes, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	status, metric := apitypes.Ptr[string](params.Status), apitypes.Ptr[string](params.Metric)
	ks, err := e.Pages.Keyset(watcherSort, fmt.Sprintf("listMetricWatchers|%s|%q", params.Status.Value, params.Metric.Value),
		apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("analytics: page of metric watchers: %w", err)
	}
	q, args := ks.SQL(watcherSelect+` WHERE created_by = $1 AND ($2::text IS NULL OR status = $2) AND ($3::text IS NULL OR metric = $3)`,
		[]any{p.ID, status, metric})
	rows, _ := tx.Query(ctx, q, args...)
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Watcher, error) { return scanWatcher(r) })
	if err != nil {
		return nil, fmt.Errorf("analytics: list metric watchers: %w", err)
	}
	items, next := apitypes.PageOf(ks, list, func(w Watcher) (string, uuid.UUID) { return w.Name, w.ID }, toAPI)
	return &gen.MetricWatcherPage{Items: items, NextPageToken: next}, nil
}
