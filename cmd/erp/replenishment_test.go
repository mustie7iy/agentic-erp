package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/eval"
	"github.com/mustie7iy/agentic-erp/internal/llm"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
)

// TestAgentReplenish: erp agent replenish needs -run, a planning run's
// id, reads the run, runs the replenishment agent's session and prints
// what it recommended on each proposal, named, and the model's reply; a
// run that proposed nothing starts no session and says so (ADR-0072,
// S2l).
func TestAgentReplenish(t *testing.T) {
	t.Chdir(t.TempDir())
	model(t)
	fakeAPI(t, routes{})
	fails(t, "flag provided", "agent", "replenish", "-x")
	fails(t, "want -run ID", "agent", "replenish")
	fails(t, "want -run ID", "agent", "replenish", "-run", "R1")
	const (
		run      = "01a1aa00-0000-7000-8000-000000000a71"
		req      = "01a1aa00-0000-7000-8000-000000000a72"
		material = "01a1aa00-0000-7000-8000-000000000a73"
		item     = "01a1aa00-0000-7000-8000-000000000a74"
		supplier = "01a1aa00-0000-7000-8000-000000000a75"
	)
	args := []string{"agent", "replenish", "-run", run}
	order := "Order 30 EA from supplier " + supplier + " by 2026-11-21: catalog item " + item
	fails(t, "404", args...)
	completed := `{"id": "` + run + `", "plant_code": "P100", "status": "completed", "run_date": "2026-11-21", "lines": [`
	fakeAPI(t, routes{"GET /v1/mrp-runs/" + run: {ok(completed + `{"material_number": "MA1"}]}`)}})
	if got, want := must(t, args...), "Planning run "+run+" of plant P100 proposed nothing: no recommendation.\n"; got != want {
		t.Errorf("nothing proposed: got %q, want %q", got, want)
	}
	fakeAPI(t, sessionRoutes(routes{"GET /v1/mrp-runs/" + run: {ok(completed + `{"material_number": "MA1", "requisition_id": "` + req + `", "requisition_number": "PR7"}]}`)},
		"GET /v1/requisitions/" + req: {ok(`{"id": "` + req + `", "legal_entity_id": "le", "status": "draft", "needed_by": "2026-11-21",
			"items": [{"material_id": "` + material + `", "quantity": {"value": "30", "unit": "EA"}}]}`)},
		"GET /v1/catalog-items": {ok(`{"items": [{"id": "` + item + `", "legal_entity_id": "le", "supplier_id": "` + supplier + `", "material_id": "` + material +
			`", "status": "active", "unit_price": {"amount": "1.10", "currency": "CAD"}}]}`)},
		"GET /v1/business-partners/" + supplier: {ok(`{"id": "` + supplier + `", "status": "active", "roles": ["supplier"]}`)},
		"POST /v1/recommendations":              {reply{201, `{"id": "r1", "object_type": "requisition", "object_id": "` + req + `", "kind": "order", "content": "` + order + `\nWhy: 1.10 CAD."}`}}}))
	model(t, llmtest.Tool("getRequisition", map[string]any{"id": req}),
		llmtest.Tool("listCatalogItems", map[string]any{"legal_entity_id": "le", "text": "MA1"}),
		llmtest.Tool("getBusinessPartner", map[string]any{"id": supplier}),
		llmtest.Tool("createRecommendation", map[string]any{"body": map[string]any{"session_id": agentSession, "object_type": "requisition",
			"object_id": req, "kind": "order", "content": order + "\nWhy: the catalog's price is 1.10 CAD."}}),
		llmtest.Final("Recommended an order; the buyer decides."))
	got := must(t, args...)
	if want := "Session " + agentSession + ": recommendations made 1, refused 0.\n  recommendation r1: order on requisition " + req +
		" (requisition PR7 of MA1): " + order + "\n" +
		"Recommended an order; the buyer decides.\n"; got != want {
		t.Errorf("agent replenish: got %q, want %q", got, want)
	}
	fakeAPI(t, routes{}, "ERP_AGENT_TOKEN", "")
	fails(t, "ERP_AGENT_TOKEN is not set", args...)
}

// TestReplenishmentAdapterFailures: the replenishment agent's evaluation
// adapter fails its case on a task without its run and on an agent that
// could not run, naming its type.
func TestReplenishmentAdapterFailures(t *testing.T) {
	gone := &apitools.Client{BaseURL: "http://127.0.0.1:1", Token: "t"}
	for name, c := range map[string]struct {
		env  eval.Env
		want string
	}{
		"without a run":   {eval.Env{Agent: gone, Task: map[string]any{}}, "the task has no run"},
		"without its API": {eval.Env{Agent: gone, LLM: &llm.Runner{}, Task: map[string]any{"run": "R1"}}, "replenishment: planning run R1: "},
	} {
		t.Run(name, func(t *testing.T) {
			if err := replenishmentEval(context.Background(), c.env); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error with %q", err, c.want)
			}
		})
	}
}

// TestReplenishmentAdapterWaits: the adapter waits for its case's
// planning run to be carried out before the agent reviews it, and fails
// the case on a run still queued when the wait ends.
func TestReplenishmentAdapterWaits(t *testing.T) {
	const run = "01a1aa00-0000-7000-8000-000000000a71"
	wait := runWait
	t.Cleanup(func() { runWait = wait })
	runWait = 200 * time.Millisecond
	queued := routes{"GET /v1/mrp-runs/" + run: {ok(`{"id": "` + run + `", "status": "queued"}`)}}
	fakeAPI(t, queued)
	agent, err := client("ERP_AGENT_TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	env := eval.Env{Agent: agent, LLM: &llm.Runner{}, Task: map[string]any{"run": run}}
	if err := replenishmentEval(context.Background(), env); err == nil || !strings.Contains(err.Error(), "still queued") {
		t.Errorf("a run still queued: got %v, want it still queued", err)
	}
	// A case stopped while it waits ends with its context.
	poll := runPoll
	t.Cleanup(func() { runPoll = poll })
	runPoll, runWait = time.Hour, time.Hour
	stopped, stop := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer stop()
	if err := replenishmentEval(stopped, env); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a case stopped: got %v, want its context's end", err)
	}
	runPoll = poll
	// A run carried out but failed reaches the agent, which refuses it.
	fakeAPI(t, routes{"GET /v1/mrp-runs/" + run: {ok(`{"id": "` + run + `", "status": "failed", "error": {"code": "platform.internal"}}`)}})
	if agent, err = client("ERP_AGENT_TOKEN"); err != nil {
		t.Fatal(err)
	}
	env = eval.Env{Agent: agent, LLM: &llm.Runner{}, Task: map[string]any{"run": run}}
	if err := replenishmentEval(context.Background(), env); err == nil || !strings.Contains(err.Error(), "replenishment: replenishmentagent: ") {
		t.Errorf("a failed run: got %v, want the agent's refusal", err)
	}
	carried := routes{"GET /v1/mrp-runs/" + run: {ok(`{"id": "` + run + `", "status": "queued"}`), ok(`{"id": "` + run + `", "status": "completed", "plant_code": "P100", "lines": []}`)}}
	fakeAPI(t, carried)
	if agent, err = client("ERP_AGENT_TOKEN"); err != nil {
		t.Fatal(err)
	}
	env = eval.Env{Agent: agent, LLM: &llm.Runner{}, Task: map[string]any{"run": run}, Reply: func(any) {}}
	if err := replenishmentEval(context.Background(), env); err != nil {
		t.Errorf("a run carried out: got %v, want the agent's review", err)
	}
}
