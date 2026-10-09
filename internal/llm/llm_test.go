package llm_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/mustie7iy/agentic-erp/internal/llm"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
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
			t.Errorf("request lacks %s", frag)
		}
	}
	if !strings.Contains(srv.Headers[0].Get("anthropic-beta"), "server-side-fallback-2026-07-01") {
		t.Fatal("fallback beta header")
	}
	refuse := llmtest.New(t, llmtest.Step{StopReason: "refusal"})
	r2 := refuse.Runner()
	if _, err := r2.RunTools(context.Background(), nil, r2.Params("s", anthropic.NewBetaTextBlock("x")), 2); err == nil || !strings.Contains(err.Error(), "declined in test") {
		t.Fatalf("refusal: %v", err)
	}
	down := llmtest.New(t)
	r3 := down.Runner()
	if _, err := r3.RunTools(context.Background(), nil, r3.Params("s", anthropic.NewBetaTextBlock("x")), 2); err == nil {
		t.Fatal("server errors propagate")
	}
	if down.Calls() != 1 || len(down.ToolNames()) != 0 {
		t.Fatal("fake server bookkeeping")
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

func TestMust(t *testing.T) {
	if llm.Must(5, nil) != 5 {
		t.Fatal("value passes through")
	}
	defer func() {
		if r := recover(); r == nil || !strings.Contains(r.(string), "boom") {
			t.Fatalf("must panic with the error: %v", r)
		}
	}()
	llm.Must(0, errors.New("boom"))
}
