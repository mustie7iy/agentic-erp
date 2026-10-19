package apitools

import (
	"context"
	"fmt"
	"net/http"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/mustie7iy/agentic-erp/internal/llm"
)

// Session is an agent session recorded through the API.
type Session struct {
	ID string
	// Level is the agent's autonomy level when the session started, as the
	// API answered it (0 when it did not say): Run offers the model only
	// the tools of that level (ADR-0036).
	Level  int
	client *Client
}

// StartSession opens a session for the client's agent.
func StartSession(ctx context.Context, client *Client, purpose, model string) (*Session, error) {
	resp, err := client.Do(ctx, http.MethodPost, "/v1/agent-sessions", nil, map[string]string{"purpose": purpose, "model": model}, "")
	if err != nil {
		return nil, err
	}
	var s struct {
		ID    string `json:"id"`
		Level int    `json:"autonomy_level"`
	}
	if err := resp.Decode(&s); err != nil {
		return nil, fmt.Errorf("apitools: start session: %w", err)
	}
	return &Session{ID: s.ID, Level: s.Level, client: client}, nil
}

func (s *Session) record(ctx context.Context, tool string, input map[string]any, summary string, isError bool) error {
	resp, err := s.client.Do(ctx, http.MethodPost, "/v1/agent-sessions/"+s.ID+"/tool-calls", nil,
		map[string]any{"tool": tool, "input": input, "result_summary": summary, "is_error": isError}, "")
	if err == nil {
		err = resp.Err()
	}
	if err != nil {
		return fmt.Errorf("apitools: record %s: %w", tool, err)
	}
	return nil
}

// End ends the session with its outcome, whatever its version: each model
// call through the gateway makes a new one, and the session is the
// agent's own. The usage sent is the contract's, which the server ignores:
// the session's tokens and cost are its calls' (ADR-0036).
func (s *Session) End(ctx context.Context, status string, usage anthropic.BetaUsage) error {
	resp, err := s.client.Do(ctx, http.MethodPost, "/v1/agent-sessions/"+s.ID+":end", nil,
		map[string]any{"status": status, "input_tokens": usage.InputTokens, "output_tokens": usage.OutputTokens}, "*")
	if err == nil {
		err = resp.Err()
	}
	if err != nil {
		return fmt.Errorf("apitools: end session: %w", err)
	}
	return nil
}

// SessionHeader names the session a model call belongs to, for the cell's
// model gateway (api/model.yaml) to add the call to it.
const SessionHeader = "Erp-Agent-Session"

// Run runs the model over the tools the session's autonomy level performs,
// at most maxIterations requests, and ends the session: completed, refused
// when the model declined, failed on an error. It returns the final
// message and the tokens the run used. A tool above the level is not
// offered: the model would only be refused it (403
// identity.autonomy_insufficient, which stays the backstop).
func Run(ctx context.Context, r *llm.Runner, s *Session, tools []*Tool, params anthropic.BetaMessageNewParams, maxIterations int) (*anthropic.BetaMessage, error) {
	runner := r.Client.Beta.Messages.NewToolRunner(AsBeta(offered(tools, s.Level)), anthropic.BetaToolRunnerParams{BetaMessageNewParams: params, MaxIterations: maxIterations},
		option.WithHeader(SessionHeader, s.ID))
	var usage anthropic.BetaUsage
	var last *anthropic.BetaMessage
	var runErr error
	for msg, err := range runner.All(ctx) {
		if err != nil {
			runErr = err
			break
		}
		usage.InputTokens += msg.Usage.InputTokens
		usage.OutputTokens += msg.Usage.OutputTokens
		last = msg
	}
	status := "completed"
	switch {
	case runErr != nil:
		status = "failed"
	default: // the runner yields a message or an error on every turn
		if runErr = llm.Check(last); runErr != nil {
			status = "refused"
		}
	}
	if err := s.End(ctx, status, usage); err != nil {
		return last, err
	}
	return last, runErr
}

// offered is the tools an agent of level performs.
func offered(tools []*Tool, level int) []*Tool {
	out := make([]*Tool, 0, len(tools))
	for _, t := range tools {
		if t.level <= level {
			out = append(out, t)
		}
	}
	return out
}
