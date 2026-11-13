package apitools_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	anthropic "github.com/anthropics/anthropic-sdk-go"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
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
	if got := tools[0].Name(); got != "getSupplierInvoice" {
		t.Fatalf("name: got %q, want %q (the checks below read its schema)", got, "getSupplierInvoice")
	}
	if got := tools[0].Description(); !strings.HasPrefix(got, "Get a supplier invoice. ") {
		t.Errorf("description: got %q, want the operation summary first", got)
	}
	if got := props["id"].(map[string]any)["format"]; got != "uuid" {
		t.Errorf("getSupplierInvoice id format: got %v, want uuid", got)
	}
	if got := props["expand"].(map[string]any)["type"]; got != "array" {
		t.Errorf("getSupplierInvoice expand type: got %v, want array", got)
	}
	if got := len(get["required"].([]any)); got != 1 {
		t.Errorf("getSupplierInvoice required: got %d, want 1 in %v", got, get)
	}
	if got := len(update["required"].([]any)); got != 3 {
		t.Errorf("updateConnector required: got %d, want 3 in %v", got, update)
	}
	if update["properties"].(map[string]any)["if_match"] == nil {
		t.Errorf("updateConnector: got no if_match property, want one in %v", update)
	}
	body := create["properties"].(map[string]any)["body"].(map[string]any)
	kind := body["properties"].(map[string]any)["kind"].(map[string]any)
	if got := kind["description"].(string); !strings.Contains(got, "Known values: sap_odata") {
		t.Errorf("createConnector kind description: got %q, want the known values", got)
	}
	if got := body["examples"]; got != nil {
		t.Errorf("createConnector body examples: got %v, want none", got)
	}
	_, err = apitools.Tools(nil, nil, "approveApprovalRequest")
	pgtest.WantErr(t, err, "x-agent")
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
	if got := rec["agent_id"]; got != nil {
		t.Errorf("recommendation body agent_id: got %v, want it left out (read-only)", got)
	}
	if rec["session_id"] == nil {
		t.Errorf("recommendation body: got no session_id, want one in %v", rec)
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
		t.Fatalf("observed: got %v, want [201]", seen)
	}
	body := created["body"].(map[string]any)
	if got := created["status"]; got != float64(201) {
		t.Errorf("created status: got %v, want 201", got)
	}
	if got := created["etag"]; got != `"1"` {
		t.Errorf("created etag: got %v, want %q", got, `"1"`)
	}
	if got := body["name"]; got != "SAP" {
		t.Errorf("created name: got %v, want SAP", got)
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
	// A tool run under a mutex its caller shares takes it for the call.
	var mu sync.Mutex
	tools[3].Exclusive(&mu)
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
		t.Run(bad.tool.Name()+" "+bad.input, func(t *testing.T) {
			if _, err := bad.tool.Execute(ctx, json.RawMessage(bad.input)); err == nil {
				t.Errorf("%s %s: got no error, want one", bad.tool.Name(), bad.input)
			}
		})
	}
	if !mu.TryLock() {
		t.Error("the mutex is still held after the call")
	}
	mu.Unlock()
	if got := call(t, tools[3], `null`); got["status"] != float64(200) {
		t.Fatalf("null input: %v", got)
	}
	calls, _ := e.api.Must(200, "GET", "/v1/agent-sessions/"+s.ID+"/tool-calls", "", "")
	items := calls["items"].([]any)
	if len(items) != 9 {
		t.Fatalf("tool calls: got %d, want 9: %v", len(items), items)
	}
	if got := items[0].(map[string]any)["tool"]; got != "createConnector" {
		t.Errorf("first tool call: got %v, want createConnector", got)
	}
	if got := items[3].(map[string]any)["is_error"]; got != true {
		t.Errorf("fourth tool call is_error: got %v, want true", got)
	}
	if got := items[6].(map[string]any)["result_summary"]; got != "missing path parameter in /v1/connectors/{id}" {
		t.Errorf("seventh tool call summary: got %v, want the missing path parameter", got)
	}
	// The gateway adds each model call to the session, a new version
	// each time: ending it takes the session's current one (ADR-0036).
	pgtest.Exec(t, `UPDATE agents.session SET cost_micro = cost_micro + 1, version = version + 1 WHERE id = $1`, s.ID)
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
	servers := map[string]*llmtest.Server{}
	srvOf := func(s *apitools.Session) *llmtest.Server { return servers[s.ID] }
	run := func(steps ...llmtest.Step) (*apitools.Session, error) {
		t.Helper()
		srv := llmtest.New(t, steps...)
		s, err := apitools.StartSession(ctx, e.agent, "Run", "claude-opus-5-5")
		if err != nil {
			t.Fatal(err)
		}
		servers[s.ID] = srv
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
	if err != nil {
		t.Fatalf("completed run: %v", err)
	}
	if got := session(s)["status"]; got != "completed" {
		t.Errorf("completed session status: got %v, want completed", got)
	}
	// Every model call names the session, for the gateway to add it there
	// (this fake model is no gateway, so the session counts nothing).
	if h := srvOf(s).Headers; len(h) == 0 || h[0].Get(apitools.SessionHeader) != s.ID {
		t.Errorf("model calls: got headers %v, want %s: %s", h, apitools.SessionHeader, s.ID)
	}
	s, err = run(llmtest.Step{Text: "no", StopReason: "refusal"})
	if err == nil {
		t.Errorf("refused run: got no error, want one")
	}
	if got := session(s)["status"]; got != "refused" {
		t.Errorf("refused session status: got %v, want refused", got)
	}
	s, err = run()
	if err == nil {
		t.Errorf("failed run: got no error, want one")
	}
	if got := session(s)["status"]; got != "failed" {
		t.Errorf("failed session status: got %v, want failed", got)
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

// TestRunOffersTheToolsOfTheLevel: an agent's model is offered only the
// operations its autonomy level performs (ADR-0036): reads at 0, proposals
// from 1, other writes from 2. The session says the level the API gave.
func TestRunOffersTheToolsOfTheLevel(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	for _, c := range []struct {
		level int
		want  []string
	}{
		{0, []string{"listConnectors"}},
		{1, []string{"listConnectors", "createRecommendation"}},
		{2, []string{"listConnectors", "createRecommendation", "createConnector"}},
		{3, []string{"listConnectors", "createRecommendation", "createConnector"}},
	} {
		t.Run(fmt.Sprint("level ", c.level), func(t *testing.T) {
			s, err := apitools.StartSession(ctx, e.agent, "Run", "claude-opus-5-5")
			if err != nil {
				t.Fatal(err)
			}
			if s.Level != 2 {
				t.Fatalf("session level: got %d, want the test agent's 2", s.Level)
			}
			s.Level = c.level
			srv := llmtest.New(t, llmtest.Final("done"))
			tools, _ := apitools.Tools(e.agent, s, "listConnectors", "createRecommendation", "createConnector")
			r := srv.Runner()
			if _, err := apitools.Run(ctx, r, s, tools, r.Params("system", anthropic.NewBetaTextBlock("go")), 2); err != nil {
				t.Fatal(err)
			}
			if got := srv.ToolNames(); !slices.Equal(got, c.want) {
				t.Errorf("offered: got %v, want %v", got, c.want)
			}
		})
	}
}

// TestForm: a form posts its values as a multipart form, with the token
// and a fresh Idempotency-Key; a server that cannot be reached is an error.
func TestForm(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer t" || r.Header.Get("Idempotency-Key") == "" ||
			!strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			w.WriteHeader(400)
			return
		}
		w.Header().Set("ETag", `"1"`)
		w.WriteHeader(201)
		_, _ = fmt.Fprintf(w, `{"format": %q, "file": %q}`, r.FormValue("format"), r.FormValue("file"))
	}))
	t.Cleanup(srv.Close)
	resp, err := (&apitools.Client{BaseURL: srv.URL + "/", Token: "t"}).Form(context.Background(), "/v1/uploads", map[string]string{"format": "mt940", "file": ":20:X\n"})
	if err != nil || resp.Status != 201 || resp.ETag != `"1"` || string(resp.Body) != `{"format": "mt940", "file": ":20:X\n"}` {
		t.Errorf("form: got %+v, %v", resp, err)
	}
	if _, err := (&apitools.Client{BaseURL: "http://127.0.0.1:1", Token: "t"}).Form(context.Background(), "/v1/uploads", nil); err == nil {
		t.Error("an unreachable server: got no error")
	}
}

// TestLegalEntity: a company is found by its code among the legal entities
// the caller may read; one it may not read, or one the list does not
// have, is none, and a list that cannot be read fails.
func TestLegalEntity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items": [{"id": "le-1000", "code": "1000"}, {"id": "le-3000", "code": "3000"}]}`)) // a test's answer
	}))
	t.Cleanup(srv.Close)
	c := &apitools.Client{BaseURL: srv.URL, Token: "t"}
	if got, err := apitools.LegalEntity(context.Background(), c, "3000"); err != nil || got != "le-3000" {
		t.Errorf("company 3000: got %q, %v, want le-3000", got, err)
	}
	if _, err := apitools.LegalEntity(context.Background(), c, "9999"); err == nil || !strings.Contains(err.Error(), "no company 9999") {
		t.Errorf("a company not in the list: got %v, want it named as none", err)
	}
	if _, err := apitools.LegalEntity(context.Background(), &apitools.Client{BaseURL: "http://127.0.0.1:1", Token: "t"}, "3000"); err == nil {
		t.Error("an unreachable server: got no error")
	}
}

// TestEach: a list is shown item by item across its pages, in order, and
// reading stops where the visitor says so, without asking for the page
// after.
func TestEach(t *testing.T) {
	pages := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		if r.URL.Query().Get("page_token") == "" {
			_, _ = w.Write([]byte(`{"items": [1, 2], "next_page_token": "p2"}`)) // a test's answer
			return
		}
		_, _ = w.Write([]byte(`{"items": [3, 4], "next_page_token": "p3"}`)) // a test's answer
	}))
	t.Cleanup(srv.Close)
	c := &apitools.Client{BaseURL: srv.URL, Token: "t"}
	var seen []int
	err := apitools.Each(context.Background(), c, "/v1/things", nil, func(n int) bool {
		seen = append(seen, n)
		return n < 3
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(seen, []int{1, 2, 3}) {
		t.Errorf("items shown: got %v, want 1 2 3: the visitor stopped at 3", seen)
	}
	if pages != 2 {
		t.Errorf("pages read: got %d, want 2", pages)
	}
}
