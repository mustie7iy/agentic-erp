package main

import (
	"context"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/eval"
	"github.com/mustie7iy/agentic-erp/internal/llm"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
)

// TestAgentPlan: erp agent plan needs -company, -category and a year,
// finds the company and the category, runs the planning agent's session
// and prints what it recommended and the model's reply, also when the run
// failed after starting its session (ADR-0072, T105e).
func TestAgentPlan(t *testing.T) {
	t.Chdir(t.TempDir())
	model(t)
	fakeAPI(t, routes{})
	fails(t, "flag provided", "agent", "plan", "-x")
	for _, args := range [][]string{{"-category", "FC", "-year", "2027"}, {"-company", "3000", "-year", "2027"}, {"-company", "3000", "-category", "FC"},
		{"-company", "3000", "-category", "FC", "-year", "3000"}} {
		fails(t, "want -company CODE -category CODE -year YYYY", append([]string{"agent", "plan"}, args...)...)
	}
	run := []string{"agent", "plan", "-company", "3000", "-category", "FC", "-year", "2027"}
	fails(t, "404", run...)
	center, account := "01a1aa00-0000-7000-8000-000000000d01", "01a1aa00-0000-7000-8000-000000000e01"
	planned := sessionRoutes(routes{"GET /v1/legal-entities": {ok(`{"items": [{"id": "le", "code": "3000", "local_currency": "CAD"}]}`)},
		"GET /v1/plan-categories": {ok(`{"items": [{"id": "fc", "code": "FC", "name": "Forecast", "locked": false}]}`)},
		"GET /v1/cost-centers":    {ok(`{"items": [{"id": "` + center + `", "legal_entity_id": "le", "code": "CC-1100", "status": "active"}]}`)},
		"GET /v1/cost-center-plan-actual": {ok(`{"items": [{"legal_entity_id": "le", "cost_center_id": "` + center + `", "account_id": "` + account + `", "fiscal_period_from": 11,
			"plan": {"amount": "1050.00", "currency": "CAD"}, "actual": {"amount": "1250.00", "currency": "CAD"}}]}`)},
		"GET /v1/accounts":         {ok(`{"items": [{"id": "` + account + `", "number": "6000", "is_balance_sheet": false}]}`)},
		"POST /v1/recommendations": {reply{201, `{"id": "r1", "object_type": "plan_category", "object_id": "fc", "kind": "plan_proposal"}`}}})
	fakeAPI(t, planned)
	model(t, llmtest.Tool("listCostCenters", map[string]any{"legal_entity_id": "le"}),
		llmtest.Tool("listCostCenterPlanActual", map[string]any{"plan_category_id": "fc", "legal_entity_id": "le", "fiscal_year": 2026}),
		llmtest.Tool("listAccounts", map[string]any{"legal_entity_id": "le"}),
		llmtest.Tool("createRecommendation", map[string]any{"body": map[string]any{"session_id": agentSession, "object_type": "plan_category",
			"object_id": "fc", "kind": "plan_proposal", "content": "Line " + center + " CC-1100 account " + account + " 6000 period 11: 1250.00 CAD\n" +
				"Why CC-1100: actual 1250.00 CAD against a plan of 1050.00 CAD."}}),
		llmtest.Final("Proposed; the controller decides."))
	got := must(t, run...)
	if want := "Session " + agentSession + ": recommendations made 1, refused 0.\n  recommendation r1: plan_proposal on plan_category fc\n" +
		"Proposed; the controller decides.\n"; got != want {
		t.Errorf("agent plan: got %q, want %q", got, want)
	}
	model(t)
	// A failed run still names its session, then fails.
	fails(t, "Session "+agentSession+": recommendations made 0, refused 0.", run...)
	fakeAPI(t, routes{}, "ERP_AGENT_TOKEN", "")
	fails(t, "ERP_AGENT_TOKEN is not set", run...)
}

// TestPlanningAdapterFailures: the planning agent's evaluation adapter
// fails its case on a task without its values, on a year that is no
// number and on an agent that could not run, naming its type.
func TestPlanningAdapterFailures(t *testing.T) {
	gone := &apitools.Client{BaseURL: "http://127.0.0.1:1", Token: "t"}
	for name, c := range map[string]struct {
		env  eval.Env
		want string
	}{
		"without a year":   {eval.Env{Agent: gone, Task: map[string]any{"company": "3000", "category": "FC"}}, "the task has no year"},
		"a year no number": {eval.Env{Agent: gone, Task: map[string]any{"company": "3000", "category": "FC", "year": "next"}}, `the task's year "next"`},
		"without its API":  {eval.Env{Agent: gone, LLM: &llm.Runner{}, Task: map[string]any{"company": "3000", "category": "FC", "year": 2027}}, "planning: planningagent: "},
	} {
		t.Run(name, func(t *testing.T) {
			if err := planningEval(context.Background(), c.env); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error with %q", err, c.want)
			}
		})
	}
}
