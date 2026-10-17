package llm_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/mustie7iy/agentic-erp/internal/llm"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestGateway: an agent's runner calls the cell's model gateway with the
// agent's own token and no provider key, naming a model alias
// (ERP_AGENT_MODEL, default erp-default; ADR-0036).
func TestGateway(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()
	t.Setenv("ANTHROPIC_API_KEY", "sk-operator") // never sent to the gateway
	t.Setenv("ERP_AGENT_MODEL", "")
	r := llm.Gateway(srv.URL+"/", "agent-token", srv.Client())
	if r.Model != llm.DefaultModel || llm.DefaultModel != "erp-default" {
		t.Errorf("model: got %s, want erp-default", r.Model)
	}
	msg, err := r.Client.Beta.Messages.New(context.Background(), r.Params("system", anthropic.NewBetaTextBlock("hi")))
	if err != nil || llm.Text(msg) != "ok" {
		t.Fatalf("call: %v", err)
	}
	if got.URL.Path != "/v1/model/v1/messages" {
		t.Errorf("path: got %s, want /v1/model/v1/messages", got.URL.Path)
	}
	if got.Header.Get("Authorization") != "Bearer agent-token" || got.Header.Get("X-Api-Key") != "" {
		t.Errorf("credentials: got Authorization %q, X-Api-Key %q, want the agent's token alone", got.Header.Get("Authorization"), got.Header.Get("X-Api-Key"))
	}
	t.Setenv("ERP_AGENT_MODEL", "erp-analytics")
	if r := llm.Gateway(srv.URL, "t", nil); r.Model != "erp-analytics" {
		t.Errorf("model: got %s, want erp-analytics", r.Model)
	}
}

// TestInProcess: the server's own runner calls the gateway's transport,
// naming the alias it is given.
func TestInProcess(t *testing.T) {
	var got string
	r := llm.InProcess(roundTrip(func(req *http.Request) (*http.Response, error) {
		got = req.URL.Path
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(
			`{"type":"message","role":"assistant","model":"m","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))}, nil
	}), "erp-extraction")
	if r.Model != "erp-extraction" {
		t.Errorf("model: got %s, want erp-extraction", r.Model)
	}
	if _, err := r.Client.Beta.Messages.New(context.Background(), r.Params("s", anthropic.NewBetaTextBlock("x"))); err != nil || got != "/v1/messages" {
		t.Errorf("call: got %s, %v, want /v1/messages", got, err)
	}
}

// roundTrip is a transport made of a function.
type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestResultHelpers(t *testing.T) {
	r, err := llm.Result("plain")
	if err != nil || r.OfText.Text != "plain" {
		t.Fatal("string result")
	}
	r, _ = llm.Result(map[string]int{"a": 1})
	if r.OfText.Text != `{"a":1}` {
		t.Fatalf("json result: %s", r.OfText.Text)
	}
	if _, err := llm.Result(make(chan int)); err == nil {
		t.Fatal("unmarshalable value must error")
	}
	e, _ := llm.Error(errors.New("boom"))
	if e.OfText.Text != "error: boom" {
		t.Fatal("error result")
	}
}

func TestRunToolsTextAndRefusal(t *testing.T) {
	srv := llmtest.New(t, llmtest.Final("hello"))
	r := srv.Runner()
	msg, err := r.RunTools(context.Background(), nil, r.Params("system", anthropic.NewBetaTextBlock("hi")), 2)
	if err != nil || llm.Text(msg) != "hello" {
		t.Fatalf("%v %v", err, msg)
	}
	req := srv.Requests[0]
	for _, frag := range []string{`"effort":"medium"`, `"fallbacks":"default"`, `"cache_control"`, `"model":"erp-default"`} {
		if !strings.Contains(req, frag) {
			t.Errorf("request: got %s, want it to contain %s", req, frag)
		}
	}
	if !strings.Contains(srv.Headers[0].Get("anthropic-beta"), "server-side-fallback-2026-07-01") {
		t.Fatal("fallback beta header")
	}
	refuse := llmtest.New(t, llmtest.Step{StopReason: "refusal"})
	r2 := refuse.Runner()
	_, err = r2.RunTools(context.Background(), nil, r2.Params("s", anthropic.NewBetaTextBlock("x")), 2)
	pgtest.WantErr(t, err, "declined in test")
	down := llmtest.New(t)
	r3 := down.Runner()
	if _, err := r3.RunTools(context.Background(), nil, r3.Params("s", anthropic.NewBetaTextBlock("x")), 2); err == nil {
		t.Fatal("server errors propagate")
	}
	if got := down.Calls(); got != 1 {
		t.Errorf("fake server calls: got %d, want 1", got)
	}
	if got := down.ToolNames(); len(got) != 0 {
		t.Errorf("fake server tool names: got %v, want none", got)
	}
}

func TestLastToolResult(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"tool_use"}]},{"role":"user","content":[{"type":"tool_result","content":[{"type":"text","text":"result A"}]}]}]}`
	if got := llmtest.LastToolResult(body); got != "result A" {
		t.Fatalf("got %q", got)
	}
	str := `{"messages":[{"role":"user","content":[{"type":"tool_result","content":"plain"}]}]}`
	if got := llmtest.LastToolResult(str); got != "plain" {
		t.Fatalf("string content: %q", got)
	}
	if llmtest.LastToolResult(`{"messages":[]}`) != "" {
		t.Fatal("no tool result")
	}
}
