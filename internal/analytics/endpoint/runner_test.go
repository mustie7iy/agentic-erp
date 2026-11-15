package endpoint

import (
	"context"
	"errors"
	"testing"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestRunnerModel: a saved report's runner is refused as a query is when
// the tenant has no model to answer under.
func TestRunnerModel(t *testing.T) {
	e := (&Endpoints{Configs: docs{err: problem.Conflict("config.not_bootstrapped", "none")}}).Init()
	if _, _, err := e.Runner(context.Background()); problem.From(err).Code != "config.not_bootstrapped" {
		t.Errorf("runner: got %v, want config.not_bootstrapped", err)
	}
}

// TestForecastModelMissing: a tenant without a configuration has no
// model to forecast by.
func TestForecastModelMissing(t *testing.T) {
	e := (&Endpoints{Configs: docs{err: problem.Conflict("config.not_bootstrapped", "none")}}).Init()
	_, err := e.RunAnalyticsForecast(context.Background(), &gen.AnalyticsForecastRequest{Metric: "revenue", Grain: "month", PeriodsAhead: 1, Method: "moving_average"},
		gen.RunAnalyticsForecastParams{})
	if problem.From(err).Code != "config.not_bootstrapped" {
		t.Errorf("got %v, want config.not_bootstrapped", err)
	}
}

// TestForecastProblem: an error other than a refused forecast passes as
// it is.
func TestForecastProblem(t *testing.T) {
	broken := errors.New("broken")
	if err := forecastProblem(broken); !errors.Is(err, broken) {
		t.Errorf("got %v, want the error itself", err)
	}
}
