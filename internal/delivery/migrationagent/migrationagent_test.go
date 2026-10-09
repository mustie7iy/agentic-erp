package migrationagent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
	"github.com/mustie7iy/agentic-erp/internal/migration"
	"github.com/mustie7iy/agentic-erp/internal/sap"
	"github.com/mustie7iy/agentic-erp/internal/testkit"
)

func TestAssessWalksTheWholeProcess(t *testing.T) {
	scope := map[string]any{"company": "1000", "year": "2026"}
	srv := llmtest.New(t,
		llmtest.Tool("profile_extract", scope),
		llmtest.Step{Tool: "propose_account_classification", Input: func(string) any { return scope }, Expect: "no tax number"},
		llmtest.Step{Tool: "run_rehearsal", Input: func(string) any { return scope }, Expect: "confidence"},
		llmtest.Step{Tool: "explain_difference", Input: func(string) any {
			return map[string]any{"company": "1000", "year": "2026", "account": "400000"}
		}, Expect: "Total debits posted"},
		llmtest.Step{Tool: "explain_difference", Input: func(string) any {
			return map[string]any{"company": "1000", "year": "2026", "vendor": "100012"}
		}, Expect: "No document-level differences"},
		llmtest.Step{Tool: "propose_cleansing", Input: func(string) any { return scope }, Expect: "No document-level differences"},
		llmtest.Step{Text: "Company 1000 reconciles; three warnings need decisions.", Expect: "Request the tax number"},
	)
	live := core.NewStore()
	a := &Agent{LLM: srv.Runner(), Source: testkit.MemSource{E: testkit.Extract()}, Store: live}
	summary, err := a.Assess(context.Background(), "1000", "2026")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "reconciles") {
		t.Fatalf("summary: %s", summary)
	}
	if strings.Join(srv.ToolNames(), ",") != "profile_extract,propose_account_classification,run_rehearsal,explain_difference,propose_cleansing" {
		t.Fatalf("tool surface changed: %v", srv.ToolNames())
	}
	if len(live.Journal) != 0 {
		t.Fatal("rehearsals must never load into the live store")
	}
}

// corrupt serves the mock extract with one journal line changed, so the
// rehearsal fails and the explanation must find the document.
type corrupt struct{ sap.Source }

func (c corrupt) Extract(ctx context.Context, s sap.Scope) (*sap.Extract, error) {
	e, err := c.Source.Extract(ctx, s)
	if err == nil && len(e.ACDOCA) > 2 {
		e.ACDOCA[2].HSL = "1.00" // first revenue document, bank line
	}
	return e, err
}

func TestRehearsalFailureIsExplainedToTheDocument(t *testing.T) {
	a := &Agent{Source: corrupt{testkit.MemSource{E: testkit.Extract()}}, Store: core.NewStore()}
	p, err := a.Profile(context.Background(), "1000", "2026")
	if err != nil || p["blocking"] != true {
		t.Fatalf("profile must flag the unbalanced document: %v %v", err, p["blocking"])
	}
	res, err := a.Rehearse(context.Background(), "1000", "2026", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Report.Passed || len(res.Load) == 0 {
		t.Fatal("rehearsal of a corrupt extract must fail")
	}
	diffs, err := a.Explain(context.Background(), "1000", "2026", "100000", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) == 0 || diffs[0].Status != "missing_in_core" || diffs[0].Document != "SAP-1000-2026-0100000002" {
		t.Fatalf("explanation: %+v", diffs)
	}
	if _, err := a.Explain(context.Background(), "1000", "2026", "", ""); err == nil {
		t.Fatal("account or vendor required")
	}
	balances, err := a.Rehearse(context.Background(), "1000", "2026", "balances")
	if err != nil || balances.Mode != migration.ModeBalances {
		t.Fatalf("balances rehearsal: %v", err)
	}
}

type failing struct{}

func (failing) Name() string { return "failing" }
func (failing) Extract(context.Context, sap.Scope) (*sap.Extract, error) {
	return nil, errors.New("SAP is down")
}

func TestSourceErrors(t *testing.T) {
	scope := map[string]any{"company": "1000", "year": "2026"}
	srv := llmtest.New(t,
		llmtest.Tool("profile_extract", scope),
		llmtest.Step{Tool: "propose_account_classification", Input: func(string) any { return scope }, Expect: "SAP is down"},
		llmtest.Step{Tool: "run_rehearsal", Input: func(string) any { return scope }, Expect: "SAP is down"},
		llmtest.Step{Tool: "explain_difference", Input: func(string) any { return map[string]any{"company": "1000", "year": "2026", "account": "1"} }, Expect: "SAP is down"},
		llmtest.Step{Tool: "propose_cleansing", Input: func(string) any { return scope }, Expect: "SAP is down"},
		llmtest.Step{Text: "SAP is unavailable.", Expect: "SAP is down"},
	)
	a := &Agent{LLM: srv.Runner(), Source: failing{}, Store: core.NewStore(), Mapping: migration.DefaultMapping()}
	if out, err := a.Assess(context.Background(), "1000", "2026"); err != nil || out != "SAP is unavailable." {
		t.Fatalf("%v %q", err, out)
	}
	refuse := llmtest.New(t, llmtest.Step{StopReason: "refusal"})
	a.LLM = refuse.Runner()
	if _, err := a.Assess(context.Background(), "1000", "2026"); err == nil {
		t.Fatal("refusal must error")
	}
}
