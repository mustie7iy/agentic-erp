package main

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/eval"
	"github.com/mustie7iy/agentic-erp/internal/llm"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
)

// enablementVersion is the active configuration version the fake API
// answers: company 1000's price tolerance.
var enablementVersion = ok(`{"items": [{"id": "v1", "number": 3, "document": {"companies": [{"code": "1000", "invoice_tolerance": {"price_percent": 2}}]}}]}`)

// enablementDoc is a description the enablement agent's check admits.
const enablementDoc = "# Purchase to pay, configuration version 3\nAn invoice above `companies[1000].invoice_tolerance.price_percent` = `2` percent is held."

// TestAgentEnablementDocs: erp agent enablement-docs needs -process, runs
// the enablement agent's session, prints what it recommended and writes
// the description to the file -out, PROCESS.md by default; a run that
// recommended nothing writes no file; a file it cannot write fails the
// command after printing (ADR-0072 As built, T109a).
func TestAgentEnablementDocs(t *testing.T) {
	t.Chdir(t.TempDir())
	model(t)
	fakeAPI(t, routes{})
	fails(t, "flag provided", "agent", "enablement-docs", "-x")
	fails(t, "want -process purchase_to_pay|order_to_cash", "agent", "enablement-docs")
	fails(t, `a process is one of order_to_cash, purchase_to_pay, not "hire_to_retire"`, "agent", "enablement-docs", "-process", "hire_to_retire")
	content, _ := json.Marshal(enablementDoc) // a string always encodes
	documented := sessionRoutes(routes{"GET /v1/config-versions": {enablementVersion}, "GET /v1/roles": {ok(`{"items": []}`)},
		"POST /v1/recommendations": {reply{201, `{"id": "r1", "object_type": "config_version", "object_id": "v1", "kind": "process_document", "content": ` +
			string(content) + `}`}}})
	script := func() {
		model(t, llmtest.Tool("createRecommendation", map[string]any{"body": map[string]any{"session_id": agentSession, "object_type": "config_version",
			"object_id": "v1", "kind": "process_document", "content": enablementDoc}}), llmtest.Final("Described purchase to pay."))
	}
	fakeAPI(t, documented)
	script()
	got := must(t, "agent", "enablement-docs", "-process", "purchase_to_pay")
	if want := "Session " + agentSession + ": recommendations made 1, refused 0.\n  recommendation r1: process_document on config_version v1\n" +
		"Described purchase to pay.\nDescription written to purchase_to_pay.md.\n"; got != want {
		t.Errorf("agent enablement-docs: got %q, want %q", got, want)
	}
	if b, err := os.ReadFile("purchase_to_pay.md"); err != nil || string(b) != enablementDoc+"\n" {
		t.Errorf("the file: got %q (%v), want the description", b, err)
	}
	script()
	fails(t, "agent enablement-docs: write .", "agent", "enablement-docs", "-process", "purchase_to_pay", "-out", ".")
	script()
	printFails(t, "agent", "enablement-docs", "-process", "purchase_to_pay", "-out", "p2p.md")
	model(t)
	// A failed run still names its session, then fails, and writes nothing.
	fails(t, "Session "+agentSession+": recommendations made 0, refused 0.", "agent", "enablement-docs", "-process", "order_to_cash")
	if _, err := os.Stat("order_to_cash.md"); !os.IsNotExist(err) {
		t.Errorf("a run without a description: got a file (%v), want none", err)
	}
	fakeAPI(t, routes{}, "ERP_AGENT_TOKEN", "")
	fails(t, "ERP_AGENT_TOKEN is not set", "agent", "enablement-docs", "-process", "purchase_to_pay")
}

// TestEnablementAdapterFailures: the enablement agent's evaluation adapter
// fails its case on a task without a process, and on an agent that could
// not run.
func TestEnablementAdapterFailures(t *testing.T) {
	gone := &apitools.Client{BaseURL: "http://127.0.0.1:1", Token: "t"}
	for name, err := range map[string]error{
		"no process":                enablementEval(context.Background(), eval.Env{Agent: gone, Task: map[string]any{}}),
		"a process without its API": enablementEval(context.Background(), eval.Env{Agent: gone, LLM: &llm.Runner{}, Task: map[string]any{"process": "order_to_cash"}}),
	} {
		t.Run(name, func(t *testing.T) {
			if err == nil {
				t.Error("got no error, want the case failed")
			}
		})
	}
}
