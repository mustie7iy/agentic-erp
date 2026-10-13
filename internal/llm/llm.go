// Package llm holds what every Claude agent in the platform shares: client
// setup, request defaults, tool result helpers and stop-reason handling.
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
)

// DefaultModel is used unless ERP_AGENT_MODEL says otherwise.
const DefaultModel = "claude-opus-5-5"

type Runner struct {
	Client anthropic.Client
	Model  string
}

// New uses the SDK's standard credential resolution (ANTHROPIC_API_KEY or
// an `ant auth login` profile).
func New() *Runner {
	model := os.Getenv("ERP_AGENT_MODEL")
	if model == "" {
		model = DefaultModel
	}
	return &Runner{Client: anthropic.NewClient(), Model: model}
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
