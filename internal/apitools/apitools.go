// Package apitools gives agents the public API as their tools (04-api-design
// §4.13): one tool per operation of the contract, named after its
// operationId, with the operation's parameters and request body as the
// tool's input schema. An agent can therefore do nothing the API does not
// let its principal do, and every call it makes carries the API's
// permission, company and segregation-of-duties checks and its audit
// record. Operations marked x-agent: false are never tools.
//
// Agents work in sessions recorded through the API (the agents context):
// every tool call is recorded in order, and the session ends with the
// tokens it used.
package apitools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
)

// Client calls the API as one principal.
type Client struct {
	BaseURL string // e.g. https://erp.example.com
	Token   string // bearer token of the principal
	HTTP    *http.Client
}

// Response is an API answer.
type Response struct {
	Status int
	ETag   string
	Body   json.RawMessage
}

// OK reports a 2xx answer.
func (r Response) OK() bool { return r.Status >= 200 && r.Status < 300 }

// Error is a non-2xx answer as a Go error, with the problem's code.
type Error struct {
	Status       int
	Code, Detail string
}

func (e *Error) Error() string { return fmt.Sprintf("API %d %s: %s", e.Status, e.Code, e.Detail) }

// Err is nil for a 2xx answer, else the problem it carries.
func (r Response) Err() error {
	if r.OK() {
		return nil
	}
	var p struct{ Code, Detail string }
	_ = json.Unmarshal(r.Body, &p) // problems are JSON; anything else leaves the code empty
	return &Error{Status: r.Status, Code: p.Code, Detail: p.Detail}
}

// Decode unmarshals a 2xx answer's body, or returns its problem.
func (r Response) Decode(into any) error {
	if err := r.Err(); err != nil {
		return err
	}
	return errs.Wrapf(json.Unmarshal(r.Body, into), "decode response body")
}

// Do performs a call. Unsafe calls get a fresh Idempotency-Key; ifMatch,
// when not empty, is sent as If-Match.
func (c *Client) Do(ctx context.Context, method, path string, query url.Values, body any, ifMatch string) (Response, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return Response{}, fmt.Errorf("apitools: body: %w", err)
		}
		reader = bytes.NewReader(b)
	}
	u := strings.TrimRight(c.BaseURL, "/") + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return Response{}, fmt.Errorf("apitools: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		if method == http.MethodPatch {
			req.Header.Set("Content-Type", "application/merge-patch+json")
		}
	}
	if method != http.MethodGet {
		req.Header.Set("Idempotency-Key", uuid.NewString())
	}
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	h := c.HTTP
	if h == nil {
		h = http.DefaultClient
	}
	resp, err := h.Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("apitools: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, fmt.Errorf("apitools: %s %s: %w", method, path, err)
	}
	return Response{Status: resp.StatusCode, ETag: resp.Header.Get("ETag"), Body: b}, nil
}
