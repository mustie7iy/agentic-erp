package endpoint

import (
	"context"
	"errors"
	"time"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// RunAnalyticsForecast implements POST /v1/analytics-forecasts (ADR-0076
// §8, T97a): the metric's history by fiscal period, answered as
// runAnalyticsQuery answers the caller, and its forecast by the method
// named, an arithmetic projection stored nowhere.
func (e *Endpoints) RunAnalyticsForecast(ctx context.Context, req *gen.AnalyticsForecastRequest, _ gen.RunAnalyticsForecastParams) (gen.RunAnalyticsForecastRes, error) {
	m, version, err := e.model(ctx, 0)
	if err != nil {
		return nil, err
	}
	s := analytics.ForecastSpec{Metric: req.Metric, Grain: req.Grain, Ahead: int(req.PeriodsAhead), Method: req.Method, Window: int(req.Window.Or(0)),
		Companies: req.Companies}
	for _, f := range req.Where {
		s.Where = append(s.Where, analytics.Filter{Dim: f.Dim, Op: f.Op, Values: f.Values})
	}
	if err := s.Check(m); err != nil {
		return nil, forecastProblem(err)
	}
	res, err := e.answer(ctx, m, s.Query(), s.Companies, time.Time{})
	if err != nil {
		return nil, err
	}
	f, err := analytics.NewForecast(m, s, res)
	if err != nil {
		return nil, forecastProblem(err)
	}
	return &gen.AnalyticsForecastHeaders{Response: forecastToAPI(f, res, version)}, nil
}

// forecastProblem is a refused forecast as the API answers it (422); any
// other error passes.
func forecastProblem(err error) error {
	var fe *analytics.ForecastError
	if !errors.As(err, &fe) {
		return err
	}
	switch fe.Code {
	case analytics.ForecastRatio:
		return problem.Unprocessable("analytics.forecast_ratio", fe.Detail)
	case analytics.ForecastHistoryShort:
		return problem.Unprocessable("analytics.forecast_history_short", fe.Detail)
	case analytics.ForecastCurrencies:
		return problem.Unprocessable("analytics.invalid_query", fe.Detail)
	}
	return problem.Invalid("request.validation_failed", fe.Field, fe.Detail)
}

// forecastToAPI is a forecast as the contract writes it, each figure an
// exact decimal beside its currency.
func forecastToAPI(f *analytics.Forecast, res *analytics.Result, version int32) gen.AnalyticsForecast {
	out := gen.AnalyticsForecast{Metric: f.Spec.Metric, MetricLabel: f.Label, Type: string(f.Type), Grain: f.Spec.Grain, Method: f.Spec.Method,
		PeriodsAhead: int32(f.Spec.Ahead), Used: f.UsedLabels(), HistoryTotal: f.HistoryTotal.String(), ForecastTotal: f.ProjectionTotal.String(),
		Basis: f.Basis(), Source: res.Source, AsOf: res.AsOf, DataThrough: res.DataThrough, ConfigVersion: version}
	if f.Currency != "" {
		out.Currency = gen.NewOptString(f.Currency)
	}
	if f.Spec.Method == analytics.MovingAverage {
		out.Window = gen.NewOptInt32(int32(f.Spec.Window))
	}
	for _, pt := range f.History {
		p := pointToAPI(f, pt)
		p.HasData = gen.NewOptBool(pt.Data)
		out.History = append(out.History, p)
	}
	for _, pt := range f.Projection {
		out.Forecast = append(out.Forecast, pointToAPI(f, pt))
	}
	return out
}

// pointToAPI is a period's figure as the contract writes it.
func pointToAPI(f *analytics.Forecast, pt analytics.Point) gen.AnalyticsForecastPoint {
	out := gen.AnalyticsForecastPoint{FiscalYear: int32(pt.Period.Year), Label: pt.Period.Label(f.Spec.Grain), Value: pt.Value.String()}
	if pt.Period.Number != 0 {
		out.Period = gen.NewOptInt32(int32(pt.Period.Number))
	}
	if f.Currency != "" {
		out.Currency = gen.NewOptString(f.Currency)
	}
	return out
}
