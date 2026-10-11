package testagent_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"regexp"
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
	f := fixture.New(t, "config.change.read", "config.change.propose", "config.version.read", "agents.session.record", "agents.session.read")
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
		llmtest.ToolFn("createRecommendation", func(body string) any {
			return map[string]any{"body": map[string]any{"session_id": sessionRe.FindStringSubmatch(body)[1], "object_type": "change_request",
				"object_id": id, "kind": "approve", "content": "Operational change within policy."}}
		}),
		llmtest.Final("## UAT\n1. Post an invoice 2.5% above the order: it posts."))
	rev, err := (&testagent.Agent{LLM: model.Runner(), API: api}).Review(context.Background(), id)
	if err != nil || !rev.Recommended || rev.Scripts == "" {
		t.Fatalf("review: %+v %v", rev, err)
	}
	if _, err := (&testagent.Agent{LLM: llmtest.New(t).Runner(), API: api}).Review(context.Background(), id); err == nil {
		t.Fatal("model failure hidden")
	}
	person := &apitools.Client{BaseURL: srv.URL, Token: f.API.Issuer.Token(t, f.Tenant.ID, f.Tenant.Human.Subject, nil)}
	if _, err := (&testagent.Agent{LLM: model.Runner(), API: person}).Review(context.Background(), id); err == nil {
		t.Fatal("a person ran the agent")
	}
}
