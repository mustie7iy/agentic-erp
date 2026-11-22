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

// TestAgentCountPlan: erp agent count-plan needs -company and -plant,
// finds the company and the plant, runs the count planning agent's
// session and prints what it recommended and the model's reply, and with
// nothing due prints the reply alone (ADR-0072, ADR-0082 As built S2e).
func TestAgentCountPlan(t *testing.T) {
	t.Chdir(t.TempDir())
	model(t)
	fakeAPI(t, routes{})
	fails(t, "flag provided", "agent", "count-plan", "-x")
	for _, args := range [][]string{{"-plant", "P100"}, {"-company", "3000"}} {
		fails(t, "want -company CODE -plant CODE", append([]string{"agent", "count-plan"}, args...)...)
	}
	run := []string{"agent", "count-plan", "-company", "3000", "-plant", "P100"}
	fails(t, "404", run...)
	bracket := "01a1aa00-0000-7000-8000-000000000d01"
	plant := routes{"GET /v1/legal-entities": {ok(`{"items": [{"id": "le", "code": "3000"}]}`)},
		"GET /v1/org-units":               {ok(`{"items": [{"id": "pl", "legal_entity_id": "le", "code": "P100", "name": "Toronto plant", "status": "active"}]}`)},
		"GET /v1/legal-entities/le/today": {ok(`{"time_zone": "America/Toronto"}`)},
		"GET /v1/materials":               {ok(`{"items": [{"id": "` + bracket + `", "status": "active"}]}`)},
		"GET /v1/material-documents":      {noItems}, "GET /v1/physical-inventory-documents": {noItems}}
	quiet := sessionRoutes(plant)
	quiet["GET /v1/stock"] = []reply{noItems}
	fakeAPI(t, quiet)
	if got := must(t, run...); !strings.HasPrefix(got, "Nothing is due for a count at plant P100 on ") {
		t.Errorf("agent count-plan with nothing in stock: got %q, want the reply alone", got)
	}
	due := sessionRoutes(plant)
	due["GET /v1/stock"] = []reply{ok(`{"items": [{"material_id": "` + bracket + `", "material_number": "MA1", "storage_location_id": "sl", "storage_location_code": "SL01"}]}`)}
	due["POST /v1/recommendations"] = []reply{{201, `{"id": "r1", "object_type": "plant", "object_id": "pl", "kind": "count_proposal",
		"content": "Count MA1 at P100/SL01 (as the API answers it)\nNever counted."}`}}
	fakeAPI(t, due)
	line := regexp.MustCompile(`Count MA1 at [^\\"]*`)
	model(t, llmtest.ToolFn("createRecommendation", func(conversation string) any {
		return map[string]any{"body": map[string]any{"session_id": agentSession, "object_type": "plant", "object_id": "pl", "kind": "count_proposal",
			"content": line.FindString(conversation) + "\nNever counted at the main store."}}
	}), llmtest.Final("Proposed; the controller decides."))
	got := must(t, run...)
	if want := "Session " + agentSession + ": recommendations made 1, refused 0.\n  recommendation r1: count_proposal on plant pl (P100 Toronto plant): " +
		"Count MA1 at P100/SL01 (as the API answers it)\nProposed; the controller decides.\n"; got != want {
		t.Errorf("agent count-plan: got %q, want %q", got, want)
	}
	model(t)
	// A failed run still names its session, then fails.
	fails(t, "Session "+agentSession+": recommendations made 0, refused 0.", run...)
	fakeAPI(t, routes{}, "ERP_AGENT_TOKEN", "")
	fails(t, "ERP_AGENT_TOKEN is not set", run...)
}

// TestCountPlanningAdapterFailures: the count planning agent's evaluation
// adapter fails its case on a task without its values and on an agent
// that could not run, naming its type.
func TestCountPlanningAdapterFailures(t *testing.T) {
	gone := &apitools.Client{BaseURL: "http://127.0.0.1:1", Token: "t"}
	for name, c := range map[string]struct {
		env  eval.Env
		want string
	}{
		"without a plant": {eval.Env{Agent: gone, Task: map[string]any{"company": "3000"}}, "the task has no plant"},
		"without its API": {eval.Env{Agent: gone, LLM: &llm.Runner{}, Task: map[string]any{"company": "3000", "plant": "P100"}}, "count_planning: countagent: "},
	} {
		t.Run(name, func(t *testing.T) {
			if err := countPlanningEval(context.Background(), c.env); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error with %q", err, c.want)
			}
		})
	}
}
