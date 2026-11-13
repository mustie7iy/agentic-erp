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

// agentSession is the session the fake API starts for an agent's run.
const agentSession = "01a1aa00-0000-7000-8000-000000000a01"

// sessionRoutes are the routes of an agent's session on the fake API:
// started, its tool calls recorded, ended.
func sessionRoutes(more routes) routes {
	r := routes{"POST /v1/agent-sessions": {reply{201, `{"id": "` + agentSession + `", "autonomy_level": 1}`}},
		"POST /v1/agent-sessions/" + agentSession + ":end":        {ok(`{}`)},
		"POST /v1/agent-sessions/" + agentSession + "/tool-calls": {reply{201, `{}`}}}
	for k, v := range more {
		r[k] = v
	}
	return r
}

// TestAgentCreditScore: erp agent credit-score needs -company and
// -customer, reads the customer's exposure, runs the credit scoring
// agent's session and prints what it recommended and the model's reply,
// also when the run failed after starting its session (ADR-0072, T105a).
func TestAgentCreditScore(t *testing.T) {
	t.Chdir(t.TempDir())
	model(t)
	fakeAPI(t, routes{})
	fails(t, "flag provided", "agent", "credit-score", "-x")
	fails(t, "want -company CODE -customer ID", "agent", "credit-score", "-company", "3000")
	fails(t, "want -company CODE -customer ID", "agent", "credit-score", "-customer", "c1")
	fails(t, "404", "agent", "credit-score", "-company", "3000", "-customer", "c1")
	scored := sessionRoutes(routes{"GET /v1/legal-entities": {ok(`{"items": [{"id": "le", "code": "3000"}]}`)},
		"GET /v1/credit-exposure": {ok(`{"company_code": "3000", "customer_id": "c1", "customer_number": "100042", "currency": "CAD", "has_limit": true,
			"credit_limit_id": "l1", "limit": {"amount": "2470.00", "currency": "CAD"}}`)},
		"POST /v1/recommendations": {reply{201, `{"id": "r1", "object_type": "credit_limit", "object_id": "l1", "kind": "keep_limit"}`}}})
	fakeAPI(t, scored)
	model(t, llmtest.Tool("getCreditExposure", map[string]any{"legal_entity_id": "le", "customer_id": "c1"}),
		llmtest.Tool("createRecommendation", map[string]any{"body": map[string]any{"session_id": agentSession, "object_type": "credit_limit",
			"object_id": "l1", "kind": "keep_limit", "content": "Limit 2470.00 CAD: nothing is overdue."}}),
		llmtest.Final("The limit is kept; a credit manager decides."))
	got := must(t, "agent", "credit-score", "-company", "3000", "-customer", "c1")
	if want := "Session " + agentSession + ": recommendations made 1, refused 0.\n  recommendation r1: keep_limit on credit_limit l1\n" +
		"The limit is kept; a credit manager decides.\n"; got != want {
		t.Errorf("agent credit-score: got %q, want %q", got, want)
	}
	model(t, llmtest.Final("Nothing."))
	printFails(t, "agent", "credit-score", "-company", "3000", "-customer", "c1")
	model(t)
	// A failed run still names its session, then fails.
	fails(t, "Session "+agentSession+": recommendations made 0, refused 0.", "agent", "credit-score", "-company", "3000", "-customer", "c1")
	fails(t, "500", "agent", "credit-score", "-company", "3000", "-customer", "c1")
	fakeAPI(t, routes{}, "ERP_AGENT_TOKEN", "")
	fails(t, "ERP_AGENT_TOKEN is not set", "agent", "credit-score", "-company", "3000", "-customer", "c1")
}

// TestRecommendingAdapterFailures: a recommending agent's evaluation
// adapter fails its case on a task without its values and on an agent
// that could not run.
func TestRecommendingAdapterFailures(t *testing.T) {
	gone := &apitools.Client{BaseURL: "http://127.0.0.1:1", Token: "t"}
	for name, err := range map[string]error{
		"credit_scoring without a customer": creditScoringEval(context.Background(), eval.Env{Agent: gone, Task: map[string]any{"company": "3000"}}),
		"credit_scoring without its API": creditScoringEval(context.Background(), eval.Env{Agent: gone, LLM: &llm.Runner{},
			Task: map[string]any{"company": "3000", "customer": "c1"}}),
	} {
		t.Run(name, func(t *testing.T) {
			if err == nil {
				t.Error("got no error")
			}
		})
	}
	if err := creditScoringEval(context.Background(), eval.Env{Agent: gone, LLM: &llm.Runner{}, Task: map[string]any{"company": "3000", "customer": "c1"}}); err == nil ||
		!strings.Contains(err.Error(), "credit_scoring: ") {
		t.Errorf("an agent that could not run: got %v, want the agent type named", err)
	}
}
