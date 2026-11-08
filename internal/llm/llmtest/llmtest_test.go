package llmtest

import (
	"context"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func TestScriptControls(t *testing.T) {
	srv := New(t, Step{Text: "x", Expect: "needle"}, Final("second"))
	if srv.ToolNames() != nil {
		t.Fatal("no requests yet")
	}
	r := srv.Runner()
	params := r.Params("s", anthropic.NewBetaTextBlock("haystack"))
	_, err := r.Client.Beta.Messages.New(context.Background(), params)
	pgtest.WantErr(t, err, "expected")
	if msg, err := r.Client.Beta.Messages.New(context.Background(), params); err != nil || msg.Content[0].Text != "second" {
		t.Fatalf("next step: %v", err)
	}
	_, err = r.Client.Beta.Messages.New(context.Background(), params)
	pgtest.WantErr(t, err, "exhausted")
	if srv.Calls() != 3 {
		t.Fatalf("calls %d", srv.Calls())
	}
	fn := ToolFn("t", func(body string) any { return map[string]any{"len": len(body)} })
	if fn.Tool != "t" {
		t.Errorf("ToolFn tool: got %q, want %q", fn.Tool, "t")
	}
	if got := fn.Input("abc").(map[string]any)["len"]; got != 3 {
		t.Errorf("ToolFn input len: got %v, want 3", got)
	}
}

// TestTogether: a turn of several tool calls answers with each, in order.
func TestTogether(t *testing.T) {
	srv := New(t, Together(Tool("a", map[string]any{"n": 1}), Tool("b", map[string]any{"n": 2})))
	r := srv.Runner()
	msg, err := r.Client.Beta.Messages.New(context.Background(), r.Params("s", anthropic.NewBetaTextBlock("go")))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range msg.Content {
		names = append(names, c.Name)
	}
	if got := strings.Join(names, " "); got != "a b" {
		t.Errorf("tool calls: got %q, want %q", got, "a b")
	}
}

func TestLastToolResultSkipsOtherContent(t *testing.T) {
	body := `{"messages":[` +
		`{"role":"user","content":[{"type":"tool_result","content":[{"type":"text","text":"wanted"}]},{"type":"text","text":"note"}]},` +
		`{"role":"assistant","content":"plain string content"}]}`
	if got := LastToolResult(body); got != "wanted" {
		t.Fatalf("got %q", got)
	}
}
