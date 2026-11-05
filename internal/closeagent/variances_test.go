package closeagent_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/closeagent"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
)

// comment is the model's comment on the close's row of office supplies
// against basis: the conversation's session, the close named (or another),
// citing the entries given.
func (a accruing) comment(t *testing.T, closeID, basis string, citations ...string) func(string) any {
	t.Helper()
	expense := a.Account(t, "2000", "520000")
	return func(conversation string) any {
		if closeID == "" {
			closeID = closeRe.FindStringSubmatch(conversation)[1]
		}
		return map[string]any{"body": map[string]any{"session_id": sessionRe.FindStringSubmatch(conversation)[1], "close_id": closeID,
			"basis": basis, "account_id": expense.String(), "text": "The 100.00 on office supplies is the courier's invoice cited.",
			"citations": citations}}
	}
}

// TestVariances: one session on the close, the prompt naming every basis
// the company configures; a comment for another close or on a basis the
// run does not explain is refused before the API, one the API refuses is
// counted refused; the one it accepts is reported with its basis and
// account, once though the model retries it (ADR-0058 §12, T64j).
func TestVariances(t *testing.T) {
	a := setup(t)
	model := llmtest.New(t,
		llmtest.ToolFn("getPeriodClose", func(conversation string) any {
			return map[string]any{"id": closeRe.FindStringSubmatch(conversation)[1]}
		}),
		llmtest.ToolFn("proposeVarianceComment", a.comment(t, uuid.NewString(), "prior_period", a.entry)),
		llmtest.ToolFn("proposeVarianceComment", a.comment(t, "", "plan", a.entry)),
		llmtest.ToolFn("proposeVarianceComment", a.comment(t, "", "prior_period", uuid.NewString())),
		llmtest.ToolFn("proposeVarianceComment", a.comment(t, "", "prior_period", a.entry)),
		llmtest.ToolFn("proposeVarianceComment", a.comment(t, "", "prior_period", a.entry)),
		llmtest.Final("Explained office supplies by the courier's invoice."))
	res, err := (&closeagent.Agent{LLM: model.Runner(), API: a.api}).Variances(context.Background(), a.close, "")
	if err != nil {
		t.Fatalf("variances: %v", err)
	}
	list, _ := a.Must(200, "GET", "/v1/variance-comments?close_id="+a.close, "", "")
	items := apitest.List(t, list, "items")
	sessions, _ := a.Must(200, "GET", "/v1/agent-sessions", "", "")
	if len(res.Proposals) != 1 || len(items) != 1 {
		t.Fatalf("comments: got %v reported and %d listed, want one of each", res.Proposals, len(items))
	}
	item := items[0].(map[string]any)
	checks{
		"the comment reported":               {res.Proposals[0].ID, item["id"]},
		"its basis":                          {res.Proposals[0].Basis, "prior_period"},
		"its account":                        {res.Proposals[0].AccountNumber, "520000"},
		"proposed, for a person":             {item["status"], "proposed"},
		"its session":                        {item["session_id"], res.Session},
		"comments refused":                   {res.Refused, 3},
		"the reply":                          {res.Reply, "Explained office supplies by the courier's invoice."},
		"tools offered":                      {fmt.Sprint(slices.Sorted(slices.Values(model.ToolNames()))), fmt.Sprint(slices.Sorted(slices.Values(closeagent.VarianceTools)))},
		"the bases named":                    {strings.Contains(model.Requests[0], "Explain its rows against: prior_period, prior_year."), true},
		"another close refused to the model": {strings.Contains(model.Requests[2], closeagent.ErrOtherClose.Error()), true},
		"another basis refused to the model": {strings.Contains(model.Requests[3], closeagent.ErrOtherBasis.Error()), true},
		"one session":                        {len(apitest.List(t, sessions, "items")), 1},
		"the session ended":                  {apitest.List(t, sessions, "items")[0].(map[string]any)["status"], "completed"},
	}.check(t)
}

// TestVariancesBasis: a run on one basis names it alone; a basis the
// company does not configure, a close that cannot be read, and a company
// that configures none of Bases stop the run before a session or a model
// call.
func TestVariancesBasis(t *testing.T) {
	a := setup(t)
	t.Run("one basis", func(t *testing.T) {
		model := llmtest.New(t, llmtest.Final("Nothing requires a comment."))
		if _, err := (&closeagent.Agent{LLM: model.Runner(), API: a.api}).Variances(context.Background(), a.close, "prior_year"); err != nil {
			t.Fatalf("variances: %v", err)
		}
		if !strings.Contains(model.Requests[0], "Explain its rows against: prior_year.") {
			t.Errorf("prompt: got %s, want the basis prior_year alone", model.Requests[0])
		}
	})
	bases := closeagent.Bases
	for name, c := range map[string]struct {
		close, basis string
		bases        []string
		want         string
	}{
		"a basis not configured": {a.close, "plan", bases, "closing.basis_not_configured"},
		"no such close":          {uuid.NewString(), "", bases, "404"},
	} {
		t.Run(name, func(t *testing.T) {
			closeagent.Bases = c.bases
			t.Cleanup(func() { closeagent.Bases = bases })
			model := llmtest.New(t)
			_, err := (&closeagent.Agent{LLM: model.Runner(), API: a.api}).Variances(context.Background(), c.close, c.basis)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error with %s", err, c.want)
			}
			if model.Calls() != 0 {
				t.Errorf("model calls: got %d, want 0", model.Calls())
			}
		})
	}
	t.Run("no basis configured is ErrNoBasis", func(t *testing.T) {
		closeagent.Bases = []string{"plan"}
		t.Cleanup(func() { closeagent.Bases = bases })
		_, err := (&closeagent.Agent{LLM: llmtest.New(t).Runner(), API: a.api}).Variances(context.Background(), a.close, "")
		if !errors.Is(err, closeagent.ErrNoBasis) {
			t.Errorf("got %v, want ErrNoBasis", err)
		}
	})
}
