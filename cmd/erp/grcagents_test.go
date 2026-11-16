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

// monitoredControl is a control of the fake matrix.
const monitoredControl = "01a1aa00-0000-7000-8000-000000000b01"

// TestAgentControlMonitor: erp agent control-monitor reads the control
// matrix, runs the control monitoring agent's session and prints what it
// recommended on each control, named by its code, and the model's reply,
// also when the run failed after starting its session; with no control
// it prints why and starts no session (ADR-0072, T105f).
func TestAgentControlMonitor(t *testing.T) {
	t.Chdir(t.TempDir())
	model(t)
	fakeAPI(t, routes{})
	fails(t, "flag provided", "agent", "control-monitor", "-x")
	fails(t, "404", "agent", "control-monitor")
	fakeAPI(t, routes{"GET /v1/control-matrix": {ok(`{"items": []}`)}})
	if got, want := must(t, "agent", "control-monitor", "-process", "Treasury"),
		"No control to monitor: the control matrix has none of the active controls of the process \"Treasury\".\n"; got != want {
		t.Errorf("agent control-monitor without controls: got %q, want %q", got, want)
	}
	monitored := sessionRoutes(routes{"GET /v1/control-matrix": {ok(`{"items": [{"control": {"id": "` + monitoredControl + `", "code": "PAY-01"},
			"links": [{"id": "01a1aa00-0000-7000-8000-000000000c01", "kind": "permission", "permission_code": "payments.run.release", "holds": true}]}]}`)},
		"POST /v1/recommendations": {reply{201, `{"id": "r1", "object_type": "control", "object_id": "` + monitoredControl + `", "kind": "no_exception",
			"content": "Its link holds.\nNothing read shows an exception."}`}}})
	fakeAPI(t, monitored)
	model(t, llmtest.Tool("createRecommendation", map[string]any{"body": map[string]any{"session_id": agentSession, "object_type": "control",
		"object_id": monitoredControl, "kind": "no_exception", "content": "Its link holds.\nNothing read shows an exception."}}),
		llmtest.Final("No exception found."))
	got := must(t, "agent", "control-monitor")
	if want := "Session " + agentSession + ": recommendations made 1, refused 0.\n  recommendation r1: no_exception on control " + monitoredControl +
		" (PAY-01): Its link holds.\nNo exception found.\n"; got != want {
		t.Errorf("agent control-monitor: got %q, want %q", got, want)
	}
	model(t)
	// A failed run still names its session, then fails.
	fails(t, "Session "+agentSession+": recommendations made 0, refused 0.", "agent", "control-monitor")
	fakeAPI(t, routes{}, "ERP_AGENT_TOKEN", "")
	fails(t, "ERP_AGENT_TOKEN is not set", "agent", "control-monitor")
}

// TestGRCAdapterFailures: the evaluation adapters of the agents of
// controls and risks fail their case on an agent that could not run,
// naming the agent type.
func TestGRCAdapterFailures(t *testing.T) {
	gone := &apitools.Client{BaseURL: "http://127.0.0.1:1", Token: "t"}
	for name, c := range map[string]struct {
		err  error
		want string
	}{
		"control_monitoring without its API": {controlMonitoringEval(context.Background(), eval.Env{Agent: gone, LLM: &llm.Runner{},
			Task: map[string]any{"process": "Payments"}}), "control_monitoring: "},
		"risk without its API": {riskEval(context.Background(), eval.Env{Agent: gone, LLM: &llm.Runner{}, Task: map[string]any{}}), "risk: "},
	} {
		t.Run(name, func(t *testing.T) {
			if c.err == nil || !strings.Contains(c.err.Error(), c.want) {
				t.Errorf("got %v, want an error naming %q", c.err, c.want)
			}
		})
	}
}

// risky is a risk of the fake register.
const risky = "01a1aa00-0000-7000-8000-000000000b02"

// TestAgentRisk: erp agent risk reads who the agent is, the open register,
// the controls and the recommendations on them, runs the risk agent's
// session and prints what it recommended on each risk, named by its
// title, and on the tenant, and the model's reply, also when the run
// failed after starting its session (ADR-0072, T105g).
func TestAgentRisk(t *testing.T) {
	t.Chdir(t.TempDir())
	model(t)
	fakeAPI(t, routes{})
	fails(t, "flag provided", "agent", "risk", "-x")
	fails(t, "404", "agent", "risk")
	reviewed := sessionRoutes(routes{"GET /v1/me": {ok(`{"tenant_id": "01a1aa00-0000-7000-8000-000000000a02"}`)},
		"GET /v1/risks":    {ok(`{"items": [{"id": "` + risky + `", "title": "Duplicate payments", "likelihood": 2, "impact": 4, "controls": []}]}`)},
		"GET /v1/controls": {ok(`{"items": []}`)}, "GET /v1/recommendations": {ok(`{"items": []}`)},
		"POST /v1/recommendations": {reply{201, `{"id": "r1", "object_type": "risk", "object_id": "` + risky + `", "kind": "keep",
			"content": "Nothing read shows it changed."}`}}})
	fakeAPI(t, reviewed)
	model(t, llmtest.Tool("createRecommendation", map[string]any{"body": map[string]any{"session_id": agentSession, "object_type": "risk",
		"object_id": risky, "kind": "keep", "content": "Nothing read shows it changed."}}),
		llmtest.Final("Kept the risk."))
	got := must(t, "agent", "risk")
	if want := "Session " + agentSession + ": recommendations made 1, refused 0.\n  recommendation r1: keep on risk " + risky +
		" (Duplicate payments): Nothing read shows it changed.\nKept the risk.\n"; got != want {
		t.Errorf("agent risk: got %q, want %q", got, want)
	}
	model(t)
	// A failed run still names its session, then fails.
	fails(t, "Session "+agentSession+": recommendations made 0, refused 0.", "agent", "risk")
	fakeAPI(t, routes{}, "ERP_AGENT_TOKEN", "")
	fails(t, "ERP_AGENT_TOKEN is not set", "agent", "risk")
}
