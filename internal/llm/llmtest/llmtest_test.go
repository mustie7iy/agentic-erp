package llmtest

import (
	"context"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

func TestScriptControls(t *testing.T) {
	srv := New(t, Step{Text: "x", Expect: "needle"}, Final("second"))
	if srv.ToolNames() != nil {
		t.Fatal("no requests yet")
	}
	r := srv.Runner()
	params := r.Params("s", anthropic.NewBetaTextBlock("haystack"))
	if _, err := r.Client.Beta.Messages.New(context.Background(), params); err == nil || !strings.Contains(err.Error(), "expected") {
		t.Fatalf("expectation mismatch must fail: %v", err)
	}
	if msg, err := r.Client.Beta.Messages.New(context.Background(), params); err != nil || msg.Content[0].Text != "second" {
		t.Fatalf("next step: %v", err)
	}
	if _, err := r.Client.Beta.Messages.New(context.Background(), params); err == nil || !strings.Contains(err.Error(), "exhausted") {
		t.Fatalf("exhausted script must fail: %v", err)
	}
	if srv.Calls() != 3 {
		t.Fatalf("calls %d", srv.Calls())
	}
	fn := ToolFn("t", func(body string) any { return map[string]any{"len": len(body)} })
	if fn.Tool != "t" || fn.Input("abc").(map[string]any)["len"] != 3 {
		t.Fatal("ToolFn")
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
