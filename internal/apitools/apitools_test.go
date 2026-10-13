package apitools_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	anthropic "github.com/anthropics/anthropic-sdk-go"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var perms = []string{"integration.connector.read", "integration.connector.manage", "agents.session.read", "agents.session.record",
	"payables.invoice.read"}

type env struct {
	person, agent *apitools.Client
	api           apitest.Client
}

func setup(t *testing.T) env {
	t.Helper()
	ten := pgtest.NewTenant(t, perms...)
	a := apitest.New(t)
	srv := httptest.NewServer(a.Handler)
	t.Cleanup(srv.Close)
	return env{
		person: &apitools.Client{BaseURL: srv.URL + "/", Token: a.Issuer.Token(t, ten.ID, ten.Human.Subject, nil)},
		agent:  &apitools.Client{BaseURL: srv.URL, Token: a.Issuer.Token(t, ten.ID, ten.Agent.Subject, nil)},
		api:    apitest.Client{T: t, API: a, Tenant: ten, As: ten.Human},
	}
}

func schemaOf(t *testing.T, tool *apitools.Tool) map[string]any {
	t.Helper()
	b, _ := json.Marshal(tool.InputSchema())
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode the input schema: %v", err)
	}
	return m
}

func TestToolsFromTheContract(t *testing.T) {
	tools, err := apitools.Tools(nil, nil, "getSupplierInvoice", "updateConnector", "createConnector")
	if err != nil {
		t.Fatal(err)
	}
	get, update, create := schemaOf(t, tools[0]), schemaOf(t, tools[1]), schemaOf(t, tools[2])
	props := get["properties"].(map[string]any)
	if tools[0].Name() != "getSupplierInvoice" || !strings.HasPrefix(tools[0].Description(), "Get a supplier invoice. ") ||
		props["id"].(map[string]any)["format"] != "uuid" || props["expand"].(map[string]any)["type"] != "array" || len(get["required"].([]any)) != 1 {
		t.Fatalf("getSupplierInvoice: %v", get)
	}
	if req := update["required"].([]any); len(req) != 3 || update["properties"].(map[string]any)["if_match"] == nil {
		t.Fatalf("updateConnector: %v", update)
	}
	body := create["properties"].(map[string]any)["body"].(map[string]any)
	kind := body["properties"].(map[string]any)["kind"].(map[string]any)
	if !strings.Contains(kind["description"].(string), "Known values: sap_odata") || body["examples"] != nil {
		t.Fatalf("createConnector body: %v", body)
	}
	if _, err := apitools.Tools(nil, nil, "approveApprovalRequest"); err == nil || !strings.Contains(err.Error(), "x-agent") {
		t.Fatalf("approval decisions are tools: %v", err)
	}
	if _, err := apitools.Tools(nil, nil, "noSuchOperation"); err == nil {
		t.Fatal("unknown operation accepted")
	}
	if op, err := apitools.Operation(nil, "approveApprovalRequest"); err != nil || op.Name() != "approveApprovalRequest" {
		t.Fatalf("people's operation: %v", err)
	}
	if _, err := apitools.Operation(nil, "noSuchOperation"); err == nil {
		t.Fatal("unknown operation accepted")
	}
	// Read-only properties of a resource are left out of request bodies.
	end, _ := apitools.Tools(nil, nil, "getConnector", "createRecommendation")
	rec := schemaOf(t, end[1])["properties"].(map[string]any)["body"].(map[string]any)["properties"].(map[string]any)
	if rec["agent_id"] != nil || rec["session_id"] == nil {
		t.Fatalf("recommendation body: %v", rec)
	}
}

func call(t *testing.T, tool *apitools.Tool, input string) map[string]any {
	t.Helper()
	out, err := tool.Execute(context.Background(), json.RawMessage(input))
	if err != nil {
		t.Fatalf("%s: %v", tool.Name(), err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out[0].OfText.Text), &m); err != nil {
		t.Fatalf("%s: %s", tool.Name(), out[0].OfText.Text)
	}
	return m
}

func TestToolsCallTheAPIInASession(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if _, err := apitools.StartSession(ctx, e.person, "p", "m"); err == nil {
		t.Fatal("a person started an agent session")
	}
	s, err := apitools.StartSession(ctx, e.agent, "Set up connectors", "claude-opus-5-5")
	if err != nil {
		t.Fatal(err)
	}
	tools, _ := apitools.Tools(e.agent, s, "createConnector", "getConnector", "updateConnector", "listConnectors", "getSupplierInvoice")
	var seen []int
	tools[0].Observe(func(r apitools.Response) { seen = append(seen, r.Status) })
	created := call(t, tools[0], `{"body": {"kind": "sap_file", "name": "SAP", "settings": {"path": "x.json"}}}`)
	if len(seen) != 1 || seen[0] != 201 {
		t.Fatalf("observed: %v", seen)
	}
	body := created["body"].(map[string]any)
	if created["status"] != float64(201) || created["etag"] != `"1"` || body["name"] != "SAP" {
		t.Fatalf("created: %v", created)
	}
	id := body["id"].(string)
	if got := call(t, tools[1], `{"id": "`+id+`"}`); got["body"].(map[string]any)["id"] != id {
		t.Fatalf("got: %v", got)
	}
	if got := call(t, tools[2], `{"id": "`+id+`", "if_match": "\"1\"", "body": {"name": "SAP PRD"}}`); got["body"].(map[string]any)["name"] != "SAP PRD" {
		t.Fatalf("updated: %v", got)
	}
	if got := call(t, tools[2], `{"id": "`+id+`", "if_match": "\"1\"", "body": {"name": "x"}}`); got["status"] != float64(412) {
		t.Fatalf("stale: %v", got)
	}
	if got := call(t, tools[3], `{"kind": "sap_file", "page_size": 1}`); len(got["body"].(map[string]any)["items"].([]any)) != 1 {
		t.Fatalf("listed: %v", got)
	}
	if got := call(t, tools[4], `{"id": "01929a1e-4c2b-7a10-9b3e-1f2a3b4c5d01", "expand": ["supplier", "purchase_order"]}`); got["status"] != float64(404) {
		t.Fatalf("expanded: %v", got)
	}
	tools[0].Guard(func(in map[string]any) error { return errors.New("only rehearsals") })
	for _, bad := range []struct {
		tool  *apitools.Tool
		input string
	}{{tools[1], `{}`}, {tools[1], `[1]`}, {tools[0], `{"body": {}}`}} {
		if _, err := bad.tool.Execute(ctx, json.RawMessage(bad.input)); err == nil {
			t.Errorf("%s %s accepted", bad.tool.Name(), bad.input)
		}
	}
	if got := call(t, tools[3], `null`); got["status"] != float64(200) {
		t.Fatalf("null input: %v", got)
	}
	calls, _ := e.api.Must(200, "GET", "/v1/agent-sessions/"+s.ID+"/tool-calls", "", "")
	items := calls["items"].([]any)
	if len(items) != 9 || items[0].(map[string]any)["tool"] != "createConnector" || items[3].(map[string]any)["is_error"] != true ||
		items[6].(map[string]any)["result_summary"] != "missing path parameter in /v1/connectors/{id}" {
		t.Fatalf("tool calls: %v", items)
	}
	if err := s.End(ctx, "completed", anthropic.BetaUsage{InputTokens: 3, OutputTokens: 2}); err != nil {
		t.Fatal(err)
	}
	if err := s.End(ctx, "completed", anthropic.BetaUsage{}); err == nil {
		t.Fatal("ended twice")
	}
	if _, err := tools[1].Execute(ctx, json.RawMessage(`{"id": "`+id+`"}`)); err == nil {
		t.Fatal("recorded into an ended session")
	}
	// Transport failures surface as errors.
	down := &apitools.Client{BaseURL: "http://127.0.0.1:1", Token: "x"}
	if _, err := apitools.StartSession(ctx, down, "p", "m"); err == nil {
		t.Fatal("started without a server")
	}
	offline, _ := apitools.Tools(down, nil, "listConnectors")
	if _, err := offline[0].Execute(ctx, json.RawMessage(`{}`)); err == nil {
		t.Fatal("called without a server")
	}
	if _, err := down.Do(ctx, "GET", "/x", nil, make(chan int), ""); err == nil {
		t.Fatal("unencodable body accepted")
	}
	if _, err := down.Do(ctx, "BAD METHOD", "/x", nil, nil, ""); err == nil {
		t.Fatal("bad method accepted")
	}
}

func TestRun(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	run := func(steps ...llmtest.Step) (*apitools.Session, error) {
		t.Helper()
		srv := llmtest.New(t, steps...)
		s, err := apitools.StartSession(ctx, e.agent, "Run", "claude-opus-5-5")
		if err != nil {
			t.Fatal(err)
		}
		tools, _ := apitools.Tools(e.agent, s, "listConnectors")
		r := srv.Runner()
		_, err = apitools.Run(ctx, r, s, tools, r.Params("system", anthropic.NewBetaTextBlock("go")), 5)
		return s, err
	}
	session := func(s *apitools.Session) map[string]any {
		got, _ := e.api.Must(200, "GET", "/v1/agent-sessions/"+s.ID, "", "")
		return got
	}
	s, err := run(llmtest.Tool("listConnectors", map[string]any{}), llmtest.Final("done"))
	if got := session(s); err != nil || got["status"] != "completed" || got["input_tokens"] != float64(20) || got["output_tokens"] != float64(10) {
		t.Fatalf("completed: %v %v", got, err)
	}
	s, err = run(llmtest.Step{Text: "no", StopReason: "refusal"})
	if got := session(s); err == nil || got["status"] != "refused" {
		t.Fatalf("refused: %v %v", got, err)
	}
	s, err = run()
	if got := session(s); err == nil || got["status"] != "failed" {
		t.Fatalf("failed: %v %v", got, err)
	}
	// A session that cannot be ended is an error even after a good run.
	srv := llmtest.New(t, llmtest.Final("done"))
	s, _ = apitools.StartSession(ctx, e.agent, "Run", "m")
	if err := s.End(ctx, "completed", anthropic.BetaUsage{}); err != nil {
		t.Fatalf("end the session: %v", err)
	}
	r := srv.Runner()
	if _, err := apitools.Run(ctx, r, s, nil, r.Params("system", anthropic.NewBetaTextBlock("go")), 2); err == nil {
		t.Fatal("ended an ended session")
	}
}
