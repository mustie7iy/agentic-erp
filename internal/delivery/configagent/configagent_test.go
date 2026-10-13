package configagent_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
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
