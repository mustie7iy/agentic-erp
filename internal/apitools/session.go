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
	ID     string
	client *Client
}

// StartSession opens a session for the client's agent.
func StartSession(ctx context.Context, client *Client, purpose, model string) (*Session, error) {
	resp, err := client.Do(ctx, http.MethodPost, "/v1/agent-sessions", nil, map[string]string{"purpose": purpose, "model": model}, "")
	if err != nil {
		return nil, err
	}
	var s struct {
		ID string `json:"id"`
	}
	if err := resp.Decode(&s); err != nil {
		return nil, fmt.Errorf("apitools: start session: %w", err)
	}
	return &Session{ID: s.ID, client: client}, nil
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

// End ends the session with its outcome. The session's tokens and cost
// are its model calls' through the gateway, each of which made it a new
// version, so the session is read first for its current one; the usage
// sent is the contract's, which the server ignores (ADR-0036).
func (s *Session) End(ctx context.Context, status string, usage anthropic.BetaUsage) error {
	resp, err := s.client.Do(ctx, http.MethodGet, "/v1/agent-sessions/"+s.ID, nil, nil, "")
	if err == nil {
		err = resp.Err()
	}
	if err == nil {
		resp, err = s.client.Do(ctx, http.MethodPost, "/v1/agent-sessions/"+s.ID+":end", nil,
			map[string]any{"status": status, "input_tokens": usage.InputTokens, "output_tokens": usage.OutputTokens}, resp.ETag)
	}
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

// Run runs the model over the tools, at most maxIterations requests, and
// ends the session: completed, refused when the model declined, failed on
// an error. It returns the final message and the tokens the run used.
func Run(ctx context.Context, r *llm.Runner, s *Session, tools []*Tool, params anthropic.BetaMessageNewParams, maxIterations int) (*anthropic.BetaMessage, error) {
	runner := r.Client.Beta.Messages.NewToolRunner(AsBeta(tools), anthropic.BetaToolRunnerParams{BetaMessageNewParams: params, MaxIterations: maxIterations},
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
