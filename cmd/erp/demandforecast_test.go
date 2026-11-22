package main

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/eval"
	"github.com/mustie7iy/agentic-erp/internal/llm"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
)

// TestAgentDemandForecast: erp agent demand-forecast needs -company and
// -material, finds the company and the material, runs the demand
// forecasting agent's session and prints what it recommended and the
// model's reply, and with nothing to forecast prints the reply alone
// (ADR-0072, ADR-0082 As built S2m).
func TestAgentDemandForecast(t *testing.T) {
	t.Chdir(t.TempDir())
	model(t)
	fakeAPI(t, routes{})
	fails(t, "flag provided", "agent", "demand-forecast", "-x")
	for _, args := range [][]string{{"-material", "MA1"}, {"-company", "3000"}} {
		fails(t, "want -company CODE -material NUMBER", append([]string{"agent", "demand-forecast"}, args...)...)
	}
	run := []string{"agent", "demand-forecast", "-company", "3000", "-material", "MA1"}
	fails(t, "404", run...)
	hinge := "01a1aa00-0000-7000-8000-000000000d01"
	material := routes{"GET /v1/legal-entities": {ok(`{"items": [{"id": "le", "code": "3000"}]}`)},
		"GET /v1/materials": {ok(`{"items": [{"id": "` + hinge + `", "number": "MA1", "description": "Hinge H30", "base_unit": "EA",
			"plants": [{"plant_id": "pl", "plant_code": "P100", "legal_entity_id": "le"}]}]}`)},
		"GET /v1/legal-entities/le/today": {ok(`{"time_zone": "America/Toronto"}`)},
		"GET /v1/sales-orders":            {noItems}}
	quiet := sessionRoutes(material)
	quiet["GET /v1/material-documents"] = []reply{noItems}
	fakeAPI(t, quiet)
	if got := must(t, run...); !strings.HasPrefix(got, "Nothing to forecast for material MA1 in company 3000 on ") {
		t.Errorf("agent demand-forecast with nothing issued: got %q, want the reply alone", got)
	}
	issued := sessionRoutes(material)
	issued["GET /v1/material-documents"] = []reply{ok(`{"items": [{"posting_date": "2999-01-01", "movement_type": "201", "lines": []},
		{"posting_date": "2000-01-01", "movement_type": "201", "lines": []}]}`)}
	issued["GET /v1/sales-orders"] = []reply{ok(`{"items": [{"document_number": "SO1", "status": "confirmed", "order_date": "2000-01-01", "plant_id": "pl",
		"items": [{"material_id": "` + hinge + `", "quantity": {"value": "6", "unit": "EA"}}]}]}`)}
	issued["POST /v1/recommendations"] = []reply{{201, `{"id": "r1", "object_type": "material", "object_id": "` + hinge + `", "kind": "demand_forecast",
		"content": "Forecast MA1 at P100 (as the API answers it)\nThe open order."}`}}
	fakeAPI(t, issued)
	line := regexp.MustCompile(`Forecast MA1 at [^\\"]*`)
	model(t, llmtest.ToolFn("createRecommendation", func(conversation string) any {
		return map[string]any{"body": map[string]any{"session_id": agentSession, "object_type": "material", "object_id": hinge, "kind": "demand_forecast",
			"content": line.FindString(conversation) + "\nThe open order is all the demand there is."}}
	}), llmtest.Final("Forecast; the planner decides."))
	got := must(t, run...)
	if want := "Session " + agentSession + ": recommendations made 1, refused 0.\n  recommendation r1: demand_forecast on material " + hinge + " (MA1 Hinge H30): " +
		"Forecast MA1 at P100 (as the API answers it)\nForecast; the planner decides.\n"; got != want {
		t.Errorf("agent demand-forecast: got %q, want %q", got, want)
	}
	model(t)
	// A failed run still names its session, then fails.
	fails(t, "Session "+agentSession+": recommendations made 0, refused 0.", run...)
	fakeAPI(t, routes{}, "ERP_AGENT_TOKEN", "")
	fails(t, "ERP_AGENT_TOKEN is not set", run...)
}

// TestDemandForecastAdapterFailures: the demand forecasting agent's
// evaluation adapter fails its case on a task without its values and on
// an agent that could not run, naming its type.
func TestDemandForecastAdapterFailures(t *testing.T) {
	gone := &apitools.Client{BaseURL: "http://127.0.0.1:1", Token: "t"}
	for name, c := range map[string]struct {
		env  eval.Env
		want string
	}{
		"without a material": {eval.Env{Agent: gone, Task: map[string]any{"company": "3000"}}, "the task has no material"},
		"without its API":    {eval.Env{Agent: gone, LLM: &llm.Runner{}, Task: map[string]any{"company": "3000", "material": "MA1"}}, "demand_forecast: demandagent: "},
	} {
		t.Run(name, func(t *testing.T) {
			if err := demandForecastEval(context.Background(), c.env); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error with %q", err, c.want)
			}
		})
	}
}
