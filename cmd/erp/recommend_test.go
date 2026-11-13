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

// TestAgentAccessRoles: erp agent access-roles needs -role and -period,
// reads the role, its permissions, its holders and their audit events,
// runs the access agent's session and prints what it recommended and the
// model's reply, also when the run failed after starting its session
// (ADR-0072, T106a).
func TestAgentAccessRoles(t *testing.T) {
	t.Chdir(t.TempDir())
	model(t)
	fakeAPI(t, routes{})
	const period = "2026-04-01..2026-06-30"
	fails(t, "flag provided", "agent", "access-roles", "-x")
	fails(t, "want -role CODE -period FROM..TO", "agent", "access-roles", "-role", "clerk")
	fails(t, "want -role CODE -period FROM..TO", "agent", "access-roles", "-period", period)
	fails(t, "want -role CODE -period FROM..TO", "agent", "access-roles", "-role", "clerk", "-period", "2026-04-01")
	fails(t, "a period is two dates", "agent", "access-roles", "-role", "clerk", "-period", "April..June")
	fails(t, "404", "agent", "access-roles", "-role", "clerk", "-period", period)
	reviewed := sessionRoutes(routes{"GET /v1/roles": {ok(`{"items": [{"id": "r1", "code": "clerk", "name": "Clerks"}]}`)},
		"GET /v1/roles/r1/permissions": {ok(`{"items": [{"permission_code": "credit.limit.write"}]}`)},
		"GET /v1/role-assignments":     {ok(`{"items": [{"principal_id": "p1", "valid_from": "2026-01-01"}]}`)},
		"GET /v1/audit-events":         {ok(`{"items": [{"actor_id": "p1", "action": "credit.credit_limit.create"}]}`)},
		"POST /v1/recommendations":     {reply{201, `{"id": "rec1", "object_type": "role", "object_id": "r1", "kind": "keep"}`}}})
	fakeAPI(t, reviewed)
	model(t, llmtest.Tool("createRecommendation", map[string]any{"body": map[string]any{"session_id": agentSession, "object_type": "role",
		"object_id": "r1", "kind": "keep", "content": "Its holder set a credit limit in the period."}}),
		llmtest.Final("The role is kept; an identity administrator decides."))
	got := must(t, "agent", "access-roles", "-role", "clerk", "-period", period)
	if want := "Session " + agentSession + ": recommendations made 1, refused 0.\n  recommendation rec1: keep on role r1\n" +
		"The role is kept; an identity administrator decides.\n"; got != want {
		t.Errorf("agent access-roles: got %q, want %q", got, want)
	}
	model(t, llmtest.Final("Nothing."))
	printFails(t, "agent", "access-roles", "-role", "clerk", "-period", period)
	model(t)
	// A failed run still names its session, then fails.
	fails(t, "Session "+agentSession+": recommendations made 0, refused 0.", "agent", "access-roles", "-role", "clerk", "-period", period)
	fakeAPI(t, routes{}, "ERP_AGENT_TOKEN", "")
	fails(t, "ERP_AGENT_TOKEN is not set", "agent", "access-roles", "-role", "clerk", "-period", period)
}

// TestAgentAccessSod: erp agent access-sod needs -role, reads the
// roles, the assignments, the rules, the principals and the holders'
// grants, runs the access agent's analysis with the permissions -add
// names and prints what it recommended and the model's reply, also when
// the run failed after starting its session (ADR-0072, T106b).
func TestAgentAccessSod(t *testing.T) {
	t.Chdir(t.TempDir())
	model(t)
	fakeAPI(t, routes{})
	fails(t, "flag provided", "agent", "access-sod", "-x")
	fails(t, "want -role CODE [-add PERMISSION,...]", "agent", "access-sod", "-add", "payables.invoice.post")
	fails(t, "404", "agent", "access-sod", "-role", "clerk")
	analysed := sessionRoutes(routes{"GET /v1/roles": {ok(`{"items": [{"id": "r1", "code": "clerk", "name": "Clerks", "status": "active"}]}`)},
		"GET /v1/role-assignments":     {ok(`{"items": [{"principal_id": "p1", "role_id": "r1", "valid_from": "2026-01-01"}]}`)},
		"GET /v1/sod-rules":            {ok(`{"items": []}`)},
		"GET /v1/principals":           {ok(`{"items": [{"id": "p1", "kind": "human", "display_name": "Ruth"}]}`)},
		"GET /v1/roles/r1/permissions": {ok(`{"items": [{"permission_code": "payables.invoice.capture", "valid_from": "2026-01-01"}]}`)},
		"POST /v1/recommendations":     {reply{201, `{"id": "rec1", "object_type": "role", "object_id": "r1", "kind": "no_conflict"}`}}})
	fakeAPI(t, analysed)
	model(t, llmtest.Tool("createRecommendation", map[string]any{"body": map[string]any{"session_id": agentSession, "object_type": "role",
		"object_id": "r1", "kind": "no_conflict", "content": "The tenant has no rule to break."}}),
		llmtest.Final("No conflict; an identity administrator decides."))
	got := must(t, "agent", "access-sod", "-role", "clerk", "-add", "payables.invoice.post")
	if want := "Session " + agentSession + ": recommendations made 1, refused 0.\n  recommendation rec1: no_conflict on role r1\n" +
		"No conflict; an identity administrator decides.\n"; got != want {
		t.Errorf("agent access-sod: got %q, want %q", got, want)
	}
	model(t, llmtest.Final("Nothing."))
	printFails(t, "agent", "access-sod", "-role", "clerk")
	model(t)
	fails(t, "Session "+agentSession+": recommendations made 0, refused 0.", "agent", "access-sod", "-role", "clerk")
	fakeAPI(t, routes{}, "ERP_AGENT_TOKEN", "")
	fails(t, "ERP_AGENT_TOKEN is not set", "agent", "access-sod", "-role", "clerk")
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
		"access without a period":   accessEval(context.Background(), eval.Env{Agent: gone, Task: map[string]any{"role": "clerk", "from": "2026-04-01"}}),
		"access sod without a role": accessEval(context.Background(), eval.Env{Agent: gone, Task: map[string]any{"run": "sod"}}),
		"access sod without its API": accessEval(context.Background(), eval.Env{Agent: gone, LLM: &llm.Runner{},
			Task: map[string]any{"run": "sod", "role": "clerk", "add": "payables.invoice.post"}}),
		"access without its API": accessEval(context.Background(), eval.Env{Agent: gone, LLM: &llm.Runner{},
			Task: map[string]any{"role": "clerk", "from": "2026-04-01", "to": "2026-06-30"}}),
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
