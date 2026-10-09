package testagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
	"github.com/mustie7iy/agentic-erp/internal/testkit"
)

func newAgent(t *testing.T, srv *llmtest.Server) *Agent {
	t.Helper()
	dir := t.TempDir()
	return &Agent{LLM: srv.Runner(), Repo: testkit.Repo(t), Store: testkit.Store(t),
		SuiteDir: filepath.Join(dir, "suites"), UATDir: filepath.Join(dir, "uat"), Now: func() time.Time { return testkit.Now }}
}

func TestReviewRunsSuiteAndWritesUAT(t *testing.T) {
	script := "# UAT for CR-0001\n1. Post invoice X. Expected: posted automatically."
	srv := llmtest.New(t,
		llmtest.Tool("capture_suite", map[string]any{"max_cases": 20}),
		llmtest.Step{Tool: "run_suite", Input: func(string) any { return map[string]any{"change_request": "CR-0001"} }, Expect: "strata"},
		llmtest.Step{Tool: "save_uat_script", Input: func(string) any {
			return map[string]any{"change_request": "CR-0001", "script": script}
		}, Expect: "failed"},
		llmtest.Step{Text: "Approve with the noted change: one invoice now posts automatically.", Expect: "Saved to"},
	)
	a := newAgent(t, srv)
	c := testkit.Config(t)
	c.Company("1000").AutoPostLimit = "100000.00"
	if _, err := a.Repo.Propose("agent:configuration", "limit", "", testkit.YAML(t, c)); err != nil {
		t.Fatal(err)
	}
	rev, err := a.Review(context.Background(), "CR-0001")
	if err != nil {
		t.Fatal(err)
	}
	if len(rev.UATScripts) != 1 || !strings.Contains(rev.Recommendation, "Approve") {
		t.Fatalf("review: %+v", rev)
	}
	got, _ := os.ReadFile(rev.UATScripts[0])
	if string(got) != script {
		t.Fatal("UAT script not saved")
	}
	if strings.Join(srv.ToolNames(), ",") != "capture_suite,run_suite,save_uat_script" {
		t.Fatalf("tool surface changed: %v", srv.ToolNames())
	}
	// The run_suite result reached the model and showed the changed case.
	if !strings.Contains(srv.Requests[2], "auto_post") {
		t.Fatal("regression result should show the changed outcome")
	}
}

func TestDirectMethodsAndErrors(t *testing.T) {
	a := newAgent(t, llmtest.New(t))
	if _, err := a.RunFor("CR-0001"); err == nil {
		t.Fatal("missing CR")
	}
	c := testkit.Config(t)
	c.PaymentTerms[0].NetDays = 21
	a.Repo.Propose("agent:configuration", "terms", "", testkit.YAML(t, c))
	if _, err := a.RunFor("CR-0001"); err == nil || !strings.Contains(err.Error(), "no regression suite") {
		t.Fatalf("no suite yet: %v", err)
	}
	s, path, err := a.Capture(0)
	if err != nil || len(s.Cases) == 0 || path == "" {
		t.Fatalf("capture: %v", err)
	}
	r, err := a.RunFor("CR-0001")
	if err != nil || !r.Passed() {
		t.Fatalf("payment terms do not change invoice decisions: %v %+v", err, r)
	}
	if _, err := a.SaveUAT("CR-0001", " "); err == nil {
		t.Fatal("empty script")
	}
	if _, err := a.SaveUAT("CR-9999", "x"); err == nil {
		t.Fatal("missing CR")
	}
	if _, err := a.Review(context.Background(), "CR-9999"); err == nil {
		t.Fatal("review of missing CR")
	}
	blocked := filepath.Join(t.TempDir(), "f")
	os.WriteFile(blocked, nil, 0o644)
	a.UATDir = filepath.Join(blocked, "x")
	if _, err := a.SaveUAT("CR-0001", "x"); err == nil {
		t.Fatal("unwritable UAT dir")
	}
}

func TestToolErrorsReachTheModel(t *testing.T) {
	srv := llmtest.New(t,
		llmtest.Tool("run_suite", map[string]any{"change_request": "CR-0001"}),
		llmtest.Step{Tool: "save_uat_script", Input: func(string) any { return map[string]any{"change_request": "CR-0001", "script": ""} }, Expect: "no regression suite"},
		llmtest.Step{Tool: "capture_suite", Input: func(string) any { return map[string]any{} }, Expect: "the script is empty"},
		llmtest.Step{Text: "Done.", Expect: "error"},
	)
	a := newAgent(t, srv)
	c := testkit.Config(t)
	c.PaymentTerms[0].NetDays = 21
	a.Repo.Propose("agent:configuration", "terms", "", testkit.YAML(t, c))
	a.Store.Invoices = nil // nothing to capture
	if _, err := a.Review(context.Background(), "CR-0001"); err != nil {
		t.Fatal(err)
	}
	refuse := newAgent(t, llmtest.New(t, llmtest.Step{StopReason: "refusal"}))
	refuse.Repo.Propose("agent:configuration", "terms", "", testkit.YAML(t, c))
	if _, err := refuse.Review(context.Background(), "CR-0001"); err == nil {
		t.Fatal("refusal must error")
	}
}
