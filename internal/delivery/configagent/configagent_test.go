package configagent_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/delivery/configagent"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func TestHandle(t *testing.T) {
	f := fixture.New(t, "config.change.read", "config.change.propose", "config.version.read", "agents.session.record", "agents.session.read")
	srv := httptest.NewServer(f.API.Handler)
	defer srv.Close()
	api := &apitools.Client{BaseURL: srv.URL, Token: f.API.Issuer.Token(t, f.Tenant.ID, f.Tenant.Agent.Subject, nil)}
	doc := fixture.Document(t)
	doc.PaymentTerms[0].NetDays = 21
	b, _ := json.Marshal(doc)
	var proposed map[string]any
	if err := json.Unmarshal(b, &proposed); err != nil {
		t.Fatalf("decode the proposed configuration: %v", err)
	}
	model := llmtest.New(t,
		llmtest.Tool("listConfigVersions", map[string]any{"is_active": true}),
		llmtest.Tool("createChangeRequest", map[string]any{"body": map[string]any{"title": "NT14 to 21 days", "rationale": "Asked by AP",
			"base_version": 1, "proposed": proposed}}),
		llmtest.Final("Filed change request CR."))
	a := &configagent.Agent{LLM: model.Runner(), API: api}
	res, err := a.Handle(context.Background(), "Make NT14 21 days", []byte("version: 1"))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(res.ChangeRequests) != 1 {
		t.Fatalf("handle change requests: got %v, want 1", res.ChangeRequests)
	}
	if want := "Filed change request CR."; res.Reply != want {
		t.Errorf("handle reply: got %q, want %q", res.Reply, want)
	}
	if !strings.Contains(model.Requests[0], "Draft generated from the client's SAP customizing") {
		t.Fatal("draft not given to the model")
	}
	// A company's zone is required (T72n7) and a country can span several,
	// so the agent asks for it rather than guess it.
	if want := "ask the person for it rather than guess it from the country"; !strings.Contains(model.Requests[0], want) {
		t.Errorf("system prompt: got none saying %q", want)
	}
	cr, _ := f.Must(200, "GET", "/v1/change-requests/"+res.ChangeRequests[0], "", "")
	if cr["status"] != "draft" {
		t.Errorf("change request status: got %v, want draft", cr["status"])
	}
	if want := f.Tenant.Agent.ID.String(); cr["author_id"] != want {
		t.Errorf("change request author_id: got %v, want %s", cr["author_id"], want)
	}
	if _, err := (&configagent.Agent{LLM: llmtest.New(t).Runner(), API: api}).Handle(context.Background(), "x", nil); err == nil {
		t.Fatal("model failure hidden")
	}
	person := &apitools.Client{BaseURL: srv.URL, Token: f.API.Issuer.Token(t, f.Tenant.ID, f.Tenant.Human.Subject, nil)}
	if _, err := (&configagent.Agent{LLM: model.Runner(), API: person}).Handle(context.Background(), "x", nil); err == nil {
		t.Fatal("a person ran the agent")
	}
}

// TestHandleOwnChangesOnly: the session changes only the change requests
// it created: a person's is refused before the API, and creations the
// model sends in one turn, which the runner executes concurrently, are
// each kept (run with -race, review:batch76). An operation the contract
// does not have ends the session rather than leave it running.
func TestHandleOwnChangesOnly(t *testing.T) {
	f := fixture.New(t, "config.change.read", "config.change.propose", "config.version.read", "agents.session.record", "agents.session.read")
	srv := httptest.NewServer(f.API.Handler)
	defer srv.Close()
	api := &apitools.Client{BaseURL: srv.URL, Token: f.API.Issuer.Token(t, f.Tenant.ID, f.Tenant.Agent.Subject, nil)}
	b, _ := json.Marshal(map[string]any{"title": "A person's", "rationale": "r", "base_version": 1, "proposed": fixture.Document(t)})
	theirs, _ := f.Must(201, "POST", "/v1/change-requests", string(b), "")
	var proposed map[string]any
	if err := json.Unmarshal(b, &proposed); err != nil {
		t.Fatalf("decode the change request: %v", err)
	}
	create := llmtest.Tool("createChangeRequest", map[string]any{"body": proposed})
	model := llmtest.New(t,
		llmtest.Together(create, create),
		llmtest.Tool("withdrawChangeRequest", map[string]any{"id": theirs["id"], "if_match": `"1"`}),
		llmtest.ToolFn("withdrawChangeRequest", func(body string) any {
			return map[string]any{"id": regexp.MustCompile(`\\"id\\":\\"([0-9a-f-]{36})\\"`).FindStringSubmatch(body)[1], "if_match": `"1"`, "body": map[string]any{"reason": "Not wanted."}}
		}),
		llmtest.Final("Filed and withdrawn."))
	res, err := (&configagent.Agent{LLM: model.Runner(), API: api}).Handle(context.Background(), "Twice", nil)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(res.ChangeRequests) != 2 {
		t.Errorf("change requests: got %v, want 2", res.ChangeRequests)
	}
	if !strings.Contains(model.Requests[2], configagent.ErrNotCreated.Error()) {
		t.Errorf("the withdrawal of a person's: got %s, want it refused", llmtest.LastToolResult(model.Requests[2]))
	}
	got, _ := f.Must(200, "GET", "/v1/change-requests/"+theirs["id"].(string), "", "")
	if got["status"] != "draft" {
		t.Errorf("the person's change request: got %v, want draft", got["status"])
	}
	if !strings.Contains(llmtest.LastToolResult(model.Requests[3]), `"status":200`) {
		t.Errorf("the withdrawal of its own: got %s, want 200", llmtest.LastToolResult(model.Requests[3]))
	}
	tools := configagent.Tools
	configagent.Tools = append(slices.Clone(tools), "noSuchOperation")
	t.Cleanup(func() { configagent.Tools = tools })
	if _, err := (&configagent.Agent{LLM: llmtest.New(t).Runner(), API: api}).Handle(context.Background(), "x", nil); err == nil ||
		!strings.Contains(err.Error(), "noSuchOperation") {
		t.Errorf("an operation not in the contract: got %v, want it named", err)
	}
	sessions, _ := f.Must(200, "GET", "/v1/agent-sessions?status=running", "", "")
	if items := sessions["items"].([]any); len(items) != 0 {
		t.Errorf("sessions running: got %v, want none", items)
	}
}
