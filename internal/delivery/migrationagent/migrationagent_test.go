package migrationagent_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/delivery/migrationagent"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// fromLast reads a field of the body of the last tool result.
// It runs in the model server's handler, so it reports with Errorf, not Fatal.
func fromLast(t *testing.T, body, field string) string {
	t.Helper()
	var r struct{ Body map[string]any }
	if err := json.Unmarshal([]byte(llmtest.LastToolResult(body)), &r); err != nil {
		t.Errorf("last tool result: %v", err)
	}
	s, _ := r.Body[field].(string)
	return s
}

func TestAssess(t *testing.T) {
	f := fixture.New(t, "integration.connector.read", "integration.connector.manage", "integration.migration.read", "integration.migration.run",
		"platform.operation.read", "agents.session.record", "agents.session.read")
	f.Tenant.AllCompanies(t, f.Tenant.Agent)
	dir := t.TempDir()
	if err := sap.WriteExtract(filepath.Join(dir, "x.json"), sap.GenerateMock(42, "2026")); err != nil {
		t.Fatal(err)
	}
	f.API = apitest.NewWith(t, func(c *app.Config) { c.ExtractDir = dir })
	srv := httptest.NewServer(f.API.Handler)
	defer srv.Close()
	conn, _ := f.Must(201, "POST", "/v1/connectors", `{"kind": "sap_file", "name": "SAP", "settings": {"path": "x.json"}}`, "")
	api := &apitools.Client{BaseURL: srv.URL, Token: f.API.Issuer.Token(t, f.Tenant.ID, f.Tenant.Agent.Subject, nil)}
	run := func(rehearsal bool) map[string]any {
		return map[string]any{"body": map[string]any{"connector_id": conn["id"], "mode": "documents", "is_rehearsal": rehearsal,
			"scope": map[string]any{"legal_entity_ids": []string{f.DE.String()}, "fiscal_year": 2026}}}
	}
	model := llmtest.New(t,
		llmtest.Tool("createMigrationRun", run(false)),
		llmtest.Step{Tool: "createMigrationRun", Input: func(string) any { return run(true) }, Expect: "only rehearses"},
		llmtest.ToolFn("startMigrationRun", func(body string) any { return map[string]any{"id": fromLast(t, body, "id"), "if_match": `"1"`} }),
		llmtest.ToolFn("getMigrationRun", func(body string) any {
			return map[string]any{"id": strings.TrimPrefix(fromLast(t, body, "resource"), "/v1/migration-runs/")}
		}),
		llmtest.Final("Company 2000 reconciles; it can be cut over."))
	got, err := (&migrationagent.Agent{LLM: model.Runner(), API: api}).Assess(context.Background(), conn["id"].(string), []string{f.DE.String()}, 2026, "documents")
	if err != nil || got != "Company 2000 reconciles; it can be cut over." {
		t.Fatalf("assess: %q %v", got, err)
	}
	if !strings.Contains(model.Requests[4], `\"status\":\"reconciled\"`) {
		t.Fatalf("rehearsal report not read: %s", llmtest.LastToolResult(model.Requests[4]))
	}
	runs, _ := f.Must(200, "GET", "/v1/migration-runs", "", "")
	if items := runs["items"].([]any); len(items) != 1 || items[0].(map[string]any)["is_rehearsal"] != true {
		t.Fatalf("runs: %v", runs)
	}
	if _, err := (&migrationagent.Agent{LLM: llmtest.New(t).Runner(), API: api}).Assess(context.Background(), "c", nil, 2026, "documents"); err == nil {
		t.Fatal("model failure hidden")
	}
	person := &apitools.Client{BaseURL: srv.URL, Token: f.API.Issuer.Token(t, f.Tenant.ID, f.Tenant.Human.Subject, nil)}
	if _, err := (&migrationagent.Agent{LLM: model.Runner(), API: person}).Assess(context.Background(), "c", nil, 2026, "documents"); err == nil {
		t.Fatal("a person ran the agent")
	}
}

// TestAssessStartsOnlyItsRehearsals: the agent starts only a rehearsal its
// session created: a person's run, which would stay loaded, is refused
// before the API, and creations the model sends in one turn, which the
// runner executes concurrently, each run whole (run with -race,
// review:batch76). An operation the contract does not have ends the
// session rather than leave it running.
func TestAssessStartsOnlyItsRehearsals(t *testing.T) {
	f := fixture.New(t, "integration.connector.read", "integration.connector.manage", "integration.migration.read", "integration.migration.run",
		"platform.operation.read", "agents.session.record", "agents.session.read")
	f.Tenant.AllCompanies(t, f.Tenant.Agent)
	srv := httptest.NewServer(f.API.Handler)
	defer srv.Close()
	conn, _ := f.Must(201, "POST", "/v1/connectors", `{"kind": "sap_file", "name": "SAP", "settings": {"path": "x.json"}}`, "")
	body := func(rehearsal bool) map[string]any {
		return map[string]any{"connector_id": conn["id"], "mode": "documents", "is_rehearsal": rehearsal,
			"scope": map[string]any{"legal_entity_ids": []string{f.DE.String()}, "fiscal_year": 2026}}
	}
	persons, _ := json.Marshal(body(false))
	theirs, _ := f.Must(201, "POST", "/v1/migration-runs", string(persons), "")
	api := &apitools.Client{BaseURL: srv.URL, Token: f.API.Issuer.Token(t, f.Tenant.ID, f.Tenant.Agent.Subject, nil)}
	model := llmtest.New(t,
		llmtest.Together(llmtest.Tool("createMigrationRun", map[string]any{"body": body(true)}),
			llmtest.Tool("createMigrationRun", map[string]any{"body": body(true)})),
		llmtest.Tool("startMigrationRun", map[string]any{"id": theirs["id"], "if_match": `"1"`}),
		llmtest.Final("Not started."))
	if _, err := (&migrationagent.Agent{LLM: model.Runner(), API: api}).Assess(context.Background(), conn["id"].(string), []string{f.DE.String()}, 2026, "documents"); err != nil {
		t.Fatalf("assess: %v", err)
	}
	if !strings.Contains(model.Requests[2], "starts only the rehearsals it created") {
		t.Errorf("the start of a person's run: got %s, want it refused", llmtest.LastToolResult(model.Requests[2]))
	}
	run, _ := f.Must(200, "GET", "/v1/migration-runs/"+theirs["id"].(string), "", "")
	if run["status"] != "queued" {
		t.Errorf("the person's run: got %v, want queued", run["status"])
	}
	tools := migrationagent.Tools
	migrationagent.Tools = append(slices.Clone(tools), "noSuchOperation")
	t.Cleanup(func() { migrationagent.Tools = tools })
	if _, err := (&migrationagent.Agent{LLM: llmtest.New(t).Runner(), API: api}).Assess(context.Background(), "c", nil, 2026, "documents"); err == nil ||
		!strings.Contains(err.Error(), "noSuchOperation") {
		t.Errorf("an operation not in the contract: got %v, want it named", err)
	}
	sessions, _ := f.Must(200, "GET", "/v1/agent-sessions?status=running", "", "")
	if items := sessions["items"].([]any); len(items) != 0 {
		t.Errorf("sessions running: got %v, want none", items)
	}
}
