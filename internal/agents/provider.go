package agents

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Anthropic is the provider behind the gateway: its Messages API, reached
// with the cell's key (ERP_MODEL_API_KEY, ADR-0036).
type Anthropic struct {
	Endpoint string       // https://api.anthropic.com
	Key      string       // the API key; agents never hold it
	Client   *http.Client // the deployment's TLS client (ADR-0033)
}

// APIVersion is the Messages API version the gateway speaks.
const APIVersion = "2023-06-01"

// Messages implements Provider.
func (a Anthropic) Messages(ctx context.Context, body []byte, betas []string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.Endpoint, "/")+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("agents: model request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", a.Key)
	req.Header.Set("Anthropic-Version", APIVersion)
	if len(betas) > 0 {
		req.Header.Set("Anthropic-Beta", strings.Join(betas, ","))
	}
	resp, err := a.Client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("agents: model provider: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() // only read from: closing frees the connection and cannot change the answer
	answer, err := io.ReadAll(io.LimitReader(resp.Body, MaxModelBody))
	if err != nil {
		return 0, nil, fmt.Errorf("agents: model provider's answer: %w", err)
	}
	return resp.StatusCode, answer, nil
}
