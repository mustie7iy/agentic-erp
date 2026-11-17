package testagent_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/delivery/testagent"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var sessionRe = regexp.MustCompile(`session id is ([0-9a-f-]{36})`)

func TestReview(t *testing.T) {
	f := fixture.New(t, "config.change.read", "config.change.propose", "config.version.read", "config.regression.manage", "agents.session.record", "agents.session.read")
	srv := httptest.NewServer(f.API.Handler)
	defer srv.Close()
	api := &apitools.Client{BaseURL: srv.URL, Token: f.API.Issuer.Token(t, f.Tenant.ID, f.Tenant.Agent.Subject, nil)}
	doc := fixture.Document(t)
	doc.Companies[0].InvoiceTolerance.PricePercent = 3
	b, _ := json.Marshal(map[string]any{"title": "Tolerance", "rationale": "r", "base_version": 1, "proposed": doc})
	cr, _ := f.Must(201, "POST", "/v1/change-requests", string(b), "")
	id := cr["id"].(string)
	model := llmtest.New(t,
		llmtest.Tool("getChangeRequest", map[string]any{"id": id}),
		llmtest.Tool("listRegressionSuites", map[string]any{}),
		llmtest.Tool("createRegressionSuite", map[string]any{"body": map[string]any{"name": "Before the tolerance change"}}), // 422: nothing to capture yet
		llmtest.ToolFn("createRecommendation", func(body string) any {
			return map[string]any{"body": map[string]any{"session_id": sessionRe.FindStringSubmatch(body)[1], "object_type": "change_request",
				"object_id": id, "kind": "approve", "content": "Operational change within policy."}}
		}),
		llmtest.Final("## UAT\n1. Post an invoice 2.5% above the order: it posts."))
	rev, err := (&testagent.Agent{LLM: model.Runner(), API: api}).Review(context.Background(), id)
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if !rev.Recommended {
		t.Error("review recommended: got false, want true")
	}
	if rev.Scripts == "" {
		t.Error("review scripts: got none, want UAT scripts")
	}
	if _, err := (&testagent.Agent{LLM: llmtest.New(t).Runner(), API: api}).Review(context.Background(), id); err == nil {
		t.Fatal("model failure hidden")
	}
	person := &apitools.Client{BaseURL: srv.URL, Token: f.API.Issuer.Token(t, f.Tenant.ID, f.Tenant.Human.Subject, nil)}
	if _, err := (&testagent.Agent{LLM: model.Runner(), API: person}).Review(context.Background(), id); err == nil {
		t.Fatal("a person ran the agent")
	}
}

// TestReviewHeldToTheChange: the run recommends once, on its change
// request, and captures one suite: a second recommendation, one on another
// change request and a second capture are refused before the API, and
// calls the model sends in one turn, which the runner executes
// concurrently, each run whole (run with -race, review:batch76). An
// operation the contract does not have ends the session rather than leave
// it running.
func TestReviewHeldToTheChange(t *testing.T) {
	f := fixture.New(t, "config.change.read", "config.change.propose", "config.version.read", "config.regression.manage", "agents.session.record", "agents.session.read")
	srv := httptest.NewServer(f.API.Handler)
	defer srv.Close()
	api := &apitools.Client{BaseURL: srv.URL, Token: f.API.Issuer.Token(t, f.Tenant.ID, f.Tenant.Agent.Subject, nil)}
	b, _ := json.Marshal(map[string]any{"title": "Tolerance", "rationale": "r", "base_version": 1, "proposed": fixture.Document(t)})
	cr, _ := f.Must(201, "POST", "/v1/change-requests", string(b), "")
	other, _ := f.Must(201, "POST", "/v1/change-requests", string(b), "")
	on := func(id string) func(string) any {
		return func(body string) any {
			return map[string]any{"body": map[string]any{"session_id": sessionRe.FindStringSubmatch(body)[1], "object_type": "change_request",
				"object_id": id, "kind": "approve", "content": "Within policy."}}
		}
	}
	capture := llmtest.Tool("createRegressionSuite", map[string]any{"body": map[string]any{"name": "Empty", "allow_empty": true}})
	model := llmtest.New(t,
		llmtest.Together(llmtest.ToolFn("createRecommendation", on(cr["id"].(string))), llmtest.ToolFn("createRecommendation", on(cr["id"].(string))),
			llmtest.ToolFn("createRecommendation", on(other["id"].(string))), capture, capture),
		llmtest.Final("## UAT"))
	rev, err := (&testagent.Agent{LLM: model.Runner(), API: api}).Review(context.Background(), cr["id"].(string))
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if !rev.Recommended {
		t.Error("recommended: got false, want true")
	}
	recs, _ := f.Must(200, "GET", "/v1/recommendations", "", "")
	if got := len(recs["items"].([]any)); got != 1 {
		t.Errorf("recommendations recorded: got %d, want 1", got)
	}
	suites, _ := f.Must(200, "GET", "/v1/regression-suites", "", "")
	if got := len(suites["items"].([]any)); got != 1 {
		t.Errorf("suites captured: got %d, want 1", got)
	}
	tools := testagent.Tools
	testagent.Tools = append(slices.Clone(tools), "noSuchOperation")
	t.Cleanup(func() { testagent.Tools = tools })
	if _, err := (&testagent.Agent{LLM: llmtest.New(t).Runner(), API: api}).Review(context.Background(), cr["id"].(string)); err == nil ||
		!strings.Contains(err.Error(), "noSuchOperation") {
		t.Errorf("an operation not in the contract: got %v, want it named", err)
	}
	sessions, _ := f.Must(200, "GET", "/v1/agent-sessions?status=running", "", "")
	if items := sessions["items"].([]any); len(items) != 0 {
		t.Errorf("sessions running: got %v, want none", items)
	}
}
