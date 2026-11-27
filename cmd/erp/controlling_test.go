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

// TestAgentRepost: erp agent repost needs -company, a year and a period,
// finds the company and its ledgers, runs the controlling agent's session
// and prints the repostings it parked and the model's reply, also when
// the run failed after starting its session (ADR-0050 §12, T56j1).
func TestAgentRepost(t *testing.T) {
	t.Chdir(t.TempDir())
	model(t)
	fakeAPI(t, routes{})
	fails(t, "flag provided", "agent", "repost", "-x")
	fails(t, "want -company CODE -year YYYY -period P", "agent", "repost", "-year", "2026", "-period", "3")
	fails(t, "a run names a fiscal year and a period from 1 to 16", "agent", "repost", "-company", "2000", "-year", "2026")
	run := []string{"agent", "repost", "-company", "2000", "-year", "2026", "-period", "3"}
	fails(t, "404", run...)
	parked := sessionRoutes(routes{"GET /v1/legal-entities": {ok(`{"items": [{"id": "le", "code": "2000"}]}`)},
		"GET /v1/ledgers": {ok(`{"items": [{"id": "0L"}]}`)},
		"GET /v1/journal-entries/e1": {ok(`{"id": "e1", "legal_entity_id": "le", "ledger_id": "0L", "leading_ledger": true, "fiscal_year": 2026,
			"fiscal_period": 3, "document_type": "SA", "document_number": "100001", "posting_date": "2026-03-10", "local_currency": "EUR",
			"lines": [{"line_no": 1, "account_id": "a", "amount_lc": {"amount": "100.00", "currency": "EUR"}, "cost_center_id": "cc1"}]}`)},
		"GET /v1/parked-journal-entries":  {ok(`{"items": []}`)},
		"POST /v1/parked-journal-entries": {reply{201, `{"id": "p1"}`}}})
	fakeAPI(t, parked)
	model(t, llmtest.Tool("proposeReposting", map[string]any{"journal_entry_id": "e1", "line_no": 1, "cost_center_id": "cc2",
		"reason": "The sales team's travel, which the company posts on cc2."}),
		llmtest.Final("Proposed; a person posts it."))
	got := must(t, run...)
	if want := "Session " + agentSession + ": repostings parked 1, refused 0.\n  parked entry p1: RP 2026 SA 100001/1, journal entry e1 line 1\n" +
		"Proposed; a person posts it.\n"; got != want {
		t.Errorf("agent repost: got %q, want %q", got, want)
	}
	model(t)
	// A failed run still names its session, then fails.
	fails(t, "Session "+agentSession+": repostings parked 0, refused 0.", run...)
	model(t, llmtest.Final("Nothing."))
	printFails(t, run...)
	fakeAPI(t, routes{}, "ERP_AGENT_TOKEN", "")
	fails(t, "ERP_AGENT_TOKEN is not set", run...)
}

// TestControllingAdapterFailures: the controlling agent's evaluation
// adapter fails its case on a task without its values, on a year or a
// period that is no number and on an agent that could not run, naming its
// type.
func TestControllingAdapterFailures(t *testing.T) {
	gone := &apitools.Client{BaseURL: "http://127.0.0.1:1", Token: "t"}
	for name, c := range map[string]struct {
		task map[string]any
		want string
	}{
		"without a period":   {map[string]any{"company": "2000", "year": 2026}, "the task has no period"},
		"a year no number":   {map[string]any{"company": "2000", "year": "this", "period": 3}, "the task's year and period"},
		"a period no number": {map[string]any{"company": "2000", "year": 2026, "period": "March"}, "the task's year and period"},
		"without its API":    {map[string]any{"company": "2000", "year": 2026, "period": 3}, "controlling: controllingagent: "},
	} {
		t.Run(name, func(t *testing.T) {
			err := controllingEval(context.Background(), eval.Env{Agent: gone, LLM: &llm.Runner{}, Task: c.task})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error with %q", err, c.want)
			}
		})
	}
}
