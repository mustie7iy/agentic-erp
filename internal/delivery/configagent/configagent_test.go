package configagent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/delivery/regress"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
	"github.com/mustie7iy/agentic-erp/internal/testkit"
)

func newAgent(t *testing.T, srv *llmtest.Server) *Agent {
	t.Helper()
	repo := testkit.Repo(t)
	store := testkit.Store(t)
	return &Agent{
		LLM: srv.Runner(), Repo: repo, Store: store, Extract: testkit.Extract(),
		Suite: func() (*regress.Suite, error) {
			cfg, v, _ := repo.Active()
			return regress.Capture(store, cfg, v, 0, testkit.Now)
		},
		Now: func() time.Time { return testkit.Now },
	}
}

func TestHandleFilesChangeWithEvidence(t *testing.T) {
	proposed := testkit.Config(t)
	proposed.Company("1000").AutoPostLimit = "100000.00"
	doc := string(testkit.YAML(t, proposed))
	srv := llmtest.New(t,
		llmtest.Tool("get_active_config", map[string]any{}),
		llmtest.Step{Tool: "get_sap_customizing", Input: func(string) any { return map[string]any{} }, Expect: "version 1"},
		llmtest.Step{Tool: "validate_config", Input: func(string) any { return map[string]any{"yaml": doc} }, Expect: "generated_yaml"},
		llmtest.Step{Tool: "propose_change", Input: func(string) any {
			return map[string]any{"title": "Raise auto-post limit for 1000", "rationale": "fewer manual approvals", "yaml": doc}
		}, Expect: "tier 3"},
		llmtest.Step{Text: "Filed CR-0001; it needs the process owner and the controller.", Expect: "pending_approval"},
	)
	a := newAgent(t, srv)
	res, err := a.Handle(context.Background(), "Raise the auto-post limit for company 1000 to 100,000")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(srv.ToolNames(), ",") != "get_active_config,get_sap_customizing,validate_config,propose_change" {
		t.Fatalf("tool surface changed: %v", srv.ToolNames())
	}
	if len(res.Changes) != 1 || !strings.Contains(res.Reply, "CR-0001") {
		t.Fatalf("result: %+v", res)
	}
	cr, _ := a.Repo.Get(res.Changes[0])
	if cr.Author != Identity || cr.Status != config.CRPendingApproval || len(cr.Evidence) != 2 {
		t.Fatalf("change request: %+v", cr)
	}
	if _, v, _ := a.Repo.Active(); v != 1 {
		t.Fatal("the agent must not activate anything")
	}
}

func TestToolErrorsGoBackToTheModel(t *testing.T) {
	srv := llmtest.New(t,
		llmtest.Tool("validate_config", map[string]any{"yaml": "version: ["}),
		llmtest.Step{Tool: "propose_change", Input: func(string) any {
			return map[string]any{"title": "noop", "rationale": "x", "yaml": string(testkit.ConfigYAML)}
		}, Expect: "error: configuration"},
		llmtest.Step{Text: "Nothing to change.", Expect: "does not change anything"},
	)
	a := newAgent(t, srv)
	a.Extract = nil
	res, err := a.Handle(context.Background(), "do nothing")
	if err != nil || len(res.Changes) != 0 {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestNoExtractAndInvalidProposal(t *testing.T) {
	bad := testkit.Config(t)
	bad.Company("1000").Currency = "dollars"
	srv := llmtest.New(t,
		llmtest.Tool("get_sap_customizing", map[string]any{}),
		llmtest.Step{Tool: "propose_change", Input: func(string) any {
			return map[string]any{"title": "Break it", "rationale": "x", "yaml": string(testkit.YAML(t, bad))}
		}, Expect: "No SAP extract"},
		llmtest.Step{Text: "It is invalid.", Expect: "invalid"},
	)
	a := newAgent(t, srv)
	a.Extract = nil
	res, err := a.Handle(context.Background(), "x")
	if err != nil || len(res.Changes) != 1 {
		t.Fatalf("%v %+v", err, res)
	}
	cr, _ := a.Repo.Get(res.Changes[0])
	if cr.Status != config.CRInvalid || len(cr.Evidence) != 0 {
		t.Fatal("invalid proposals get no evidence")
	}
}

func TestProposeWhenEvidenceFails(t *testing.T) {
	a := newAgent(t, llmtest.New(t))
	a.Suite = func() (*regress.Suite, error) { return nil, errors.New("no suite") }
	c := testkit.Config(t)
	c.PaymentTerms[0].NetDays = 21
	cr, err := a.Propose("terms", "x", testkit.YAML(t, c))
	if err == nil || cr == nil || !strings.Contains(err.Error(), "filed, but evidence") {
		t.Fatalf("want filed-without-evidence error, got %v", err)
	}
	if _, err := a.Propose("noop", "x", testkit.ConfigYAML); err == nil {
		t.Fatal("no-op proposal")
	}
}

func TestRefusalAndEmptyRepo(t *testing.T) {
	srv := llmtest.New(t, llmtest.Step{Text: "", StopReason: "refusal"})
	a := newAgent(t, srv)
	if _, err := a.Handle(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "declined") {
		t.Fatalf("refusal: %v", err)
	}
	srv2 := llmtest.New(t,
		llmtest.Tool("get_active_config", map[string]any{}),
		llmtest.Step{Tool: "validate_config", Input: func(string) any { return map[string]any{"yaml": string(testkit.ConfigYAML)} }, Expect: "no active configuration"},
		llmtest.Step{Text: "Bootstrap first.", Expect: "no active configuration"},
	)
	b := newAgent(t, srv2)
	b.Repo, _ = config.OpenRepo(t.TempDir(), nil)
	if res, err := b.Handle(context.Background(), "x"); err != nil || res.Reply != "Bootstrap first." {
		t.Fatalf("%v %+v", err, res)
	}
}
