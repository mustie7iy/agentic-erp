// Package llm holds what every Claude agent in the platform shares: client
// setup, request defaults, tool result helpers and stop-reason handling. The
// agents are API clients whose tools are the API's operations (04-api-design
// §4.13).
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
)

// DefaultModel is the model alias agents name unless ERP_AGENT_MODEL says
// otherwise; the cell's gateway routes it to a model (ADR-0036).
const DefaultModel = "erp-default"

type Runner struct {
	Client anthropic.Client
	Model  string
}

// Gateway is an agent's runner: its calls go to the model gateway of the
// API at apiURL (/v1/model, api/model.yaml) with the agent's own token,
// never a provider key, which agents do not hold. client nil is the SDK's
// default.
func Gateway(apiURL, token string, client *http.Client) *Runner {
	model := os.Getenv("ERP_AGENT_MODEL")
	if model == "" {
		model = DefaultModel
	}
	opts := []option.RequestOption{option.WithBaseURL(strings.TrimRight(apiURL, "/") + "/v1/model"), option.WithAuthToken(token),
		option.WithHeaderDel("X-Api-Key")}
	if client != nil {
		opts = append(opts, option.WithHTTPClient(client))
	}
	return &Runner{Client: anthropic.NewClient(opts...), Model: model}
}

// InProcess is the server's own runner: its calls go to the gateway's
// in-process transport (agents.Gateway.Transport), as the request's
// principal, naming alias.
func InProcess(transport http.RoundTripper, alias string) *Runner {
	return &Runner{Client: anthropic.NewClient(option.WithBaseURL("http://model-gateway.internal"), option.WithAPIKey("in-process"),
		option.WithHTTPClient(&http.Client{Transport: transport}), option.WithMaxRetries(0)), Model: alias}
}

// Params builds a request with the platform defaults: cached system prompt,
// medium effort, and server-side fallback if a request is declined.
func (r *Runner) Params(system string, user ...anthropic.BetaContentBlockParamUnion) anthropic.BetaMessageNewParams {
	return anthropic.BetaMessageNewParams{
		Model:        anthropic.Model(r.Model),
		MaxTokens:    16000,
		System:       []anthropic.BetaTextBlockParam{{Text: system, CacheControl: anthropic.NewBetaCacheControlEphemeralParam()}},
		OutputConfig: anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffortMedium},
		Fallbacks:    anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()},
		Betas:        []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01},
		Messages:     []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(user...)},
	}
}

// RunTools runs an agent loop over the given tools and returns the final message.
func (r *Runner) RunTools(ctx context.Context, tools []anthropic.BetaTool, params anthropic.BetaMessageNewParams, maxIterations int) (*anthropic.BetaMessage, error) {
	runner := r.Client.Beta.Messages.NewToolRunner(tools, anthropic.BetaToolRunnerParams{BetaMessageNewParams: params, MaxIterations: maxIterations})
	msg, err := runner.RunToCompletion(ctx)
	if err != nil {
		return nil, fmt.Errorf("llm: run tools: %w", err)
	}
	return msg, Check(msg)
}

// Check turns a declined request into an error.
func Check(msg *anthropic.BetaMessage) error {
	if msg.StopReason == anthropic.BetaStopReasonRefusal {
		return fmt.Errorf("model declined: %s", msg.StopDetails.Explanation)
	}
	return nil
}

// Text returns the text content of a message.
func Text(msg *anthropic.BetaMessage) string {
	var b strings.Builder
	for _, block := range msg.Content {
		if t, ok := block.AsAny().(anthropic.BetaTextBlock); ok {
			b.WriteString(t.Text)
		}
	}
	return strings.TrimSpace(b.String())
}

// Result wraps a value as a tool result (strings as-is, anything else as JSON).
func Result(v any) (anthropic.BetaToolResultBlockParamContentUnion, error) {
	text, ok := v.(string)
	if !ok {
		b, err := json.Marshal(v)
		if err != nil {
			return anthropic.BetaToolResultBlockParamContentUnion{}, fmt.Errorf("llm: encode tool result: %w", err)
		}
		text = string(b)
	}
	return anthropic.BetaToolResultBlockParamContentUnion{OfText: &anthropic.BetaTextBlockParam{Text: text}}, nil
}

// Error reports a tool failure back to the model so it can correct course.
func Error(err error) (anthropic.BetaToolResultBlockParamContentUnion, error) {
	return Result("error: " + err.Error())
}
