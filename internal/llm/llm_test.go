package llm_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/mustie7iy/agentic-erp/internal/llm"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func TestNewUsesEnvironmentModel(t *testing.T) {
	t.Setenv("ERP_AGENT_MODEL", "claude-sonnet-5-5")
	if r := llm.New(); r.Model != "claude-sonnet-5-5" {
		t.Fatalf("model %s", r.Model)
	}
	t.Setenv("ERP_AGENT_MODEL", "")
	if r := llm.New(); r.Model != llm.DefaultModel {
		t.Fatalf("default model %s", r.Model)
	}
}

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
	for _, frag := range []string{`"effort":"medium"`, `"fallbacks":"default"`, `"cache_control"`, `"model":"claude-opus-5-5"`} {
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
