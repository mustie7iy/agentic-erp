// Package llmtest is a scripted stand-in for the Claude Messages API, so
// agent loops can be tested deterministically without network access.
package llmtest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/mustie7iy/agentic-erp/internal/llm"
)

// Step is one model turn. Input may depend on the request body (for example
// an id returned by an earlier tool).
type Step struct {
	Tool       string
	Input      func(body string) any
	Text       string
	StopReason string
	// Expect, if set, must appear in the request that this step answers.
	Expect string
}

// Tool calls a tool with fixed input.
func Tool(name string, input any) Step {
	return Step{Tool: name, Input: func(string) any { return input }}
}

// ToolFn calls a tool with input built from the request body.
func ToolFn(name string, input func(body string) any) Step { return Step{Tool: name, Input: input} }

// Final ends the conversation with text.
func Final(text string) Step { return Step{Text: text} }

// Server serves the script and records every request.
type Server struct {
	*httptest.Server
	t        *testing.T
	mu       sync.Mutex
	steps    []Step
	Requests []string
	Headers  []http.Header
}

func New(t *testing.T, steps ...Step) *Server {
	t.Helper()
	s := &Server{t: t, steps: steps}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	s.Requests = append(s.Requests, string(body))
	s.Headers = append(s.Headers, r.Header.Clone())
	n := len(s.Requests) - 1
	if n >= len(s.steps) {
		http.Error(w, fmt.Sprintf("script exhausted after %d steps", len(s.steps)), http.StatusInternalServerError)
		return
	}
	st := s.steps[n]
	if st.Expect != "" && !strings.Contains(string(body), st.Expect) {
		http.Error(w, fmt.Sprintf("step %d expected %q in the request", n+1, st.Expect), http.StatusBadRequest)
		return
	}
	var content []map[string]any
	stop := "end_turn"
	switch {
	case st.Tool != "":
		content = []map[string]any{{"type": "tool_use", "id": fmt.Sprintf("toolu_%d", n), "name": st.Tool, "input": st.Input(string(body))}}
		stop = "tool_use"
	default:
		content = []map[string]any{{"type": "text", "text": st.Text}}
	}
	if st.StopReason != "" {
		stop = st.StopReason
	}
	resp := map[string]any{
		"id": fmt.Sprintf("msg_%d", n), "type": "message", "role": "assistant", "model": llm.DefaultModel,
		"content": content, "stop_reason": stop, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": 10, "output_tokens": 5},
	}
	if stop == "refusal" {
		resp["stop_details"] = map[string]any{"type": "refusal", "category": "cyber", "explanation": "declined in test"}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp) // a failed response write means the client is gone: no one is left to tell
}

// Runner returns an llm.Runner pointed at the fake server.
func (s *Server) Runner() *llm.Runner {
	return &llm.Runner{
		Client: anthropic.NewClient(option.WithBaseURL(s.URL), option.WithAPIKey("test"), option.WithMaxRetries(0)),
		Model:  llm.DefaultModel,
	}
}

// Calls is the number of requests received.
func (s *Server) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Requests)
}

// ToolNames lists the tools offered in the first request.
func (s *Server) ToolNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.Requests) == 0 {
		return nil
	}
	var req struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	_ = json.Unmarshal([]byte(s.Requests[0]), &req) // the SDK's own JSON; an unreadable body offers no tools, which the caller's check reports
	names := make([]string, len(req.Tools))
	for i, t := range req.Tools {
		names[i] = t.Name
	}
	return names
}

// LastToolResult returns the text of the last tool_result in a request body.
func LastToolResult(body string) string {
	var req struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal([]byte(body), &req) // an unreadable body has no tool result: "", which the caller's check reports
	for i := len(req.Messages) - 1; i >= 0; i-- {
		var blocks []struct {
			Type    string          `json:"type"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(req.Messages[i].Content, &blocks) != nil {
			continue
		}
		for j := len(blocks) - 1; j >= 0; j-- {
			if blocks[j].Type != "tool_result" {
				continue
			}
			var parts []struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(blocks[j].Content, &parts) == nil && len(parts) > 0 {
				return parts[0].Text
			}
			var s string
			_ = json.Unmarshal(blocks[j].Content, &s) // content that is neither text parts nor a string has no text: ""
			return s
		}
	}
	return ""
}
