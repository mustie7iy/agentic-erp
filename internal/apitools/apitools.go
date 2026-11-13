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
	"maps"
	"mime/multipart"
	"net/http"
	"net/url"
	"slices"
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

// List reads every page of the list operation at path with the query, 500
// items a page, following next_page_token until a page has none: a caller
// that read only the first page would silently miss the rest.
func List[T any](ctx context.Context, c *Client, path string, query url.Values) ([]T, error) {
	q := url.Values{"page_size": {"500"}}
	for k, v := range query {
		q[k] = v
	}
	var out []T
	for {
		resp, err := c.Do(ctx, http.MethodGet, path, q, nil, "")
		if err != nil {
			return nil, err
		}
		var page struct {
			Items         []T
			NextPageToken string `json:"next_page_token"`
		}
		if err := resp.Decode(&page); err != nil {
			return nil, fmt.Errorf("apitools: list %s: %w", path, err)
		}
		out = append(out, page.Items...)
		if page.NextPageToken == "" {
			return out, nil
		}
		// A server answering the token it was sent would be read forever.
		if page.NextPageToken == q.Get("page_token") {
			return nil, fmt.Errorf("apitools: list %s: page token %q repeated", path, page.NextPageToken)
		}
		q.Set("page_token", page.NextPageToken)
	}
}

// LegalEntity is the id of the company with the code, among the legal
// entities the caller may read: an agent's run is given its company by
// code, and finds no company it may not read.
func LegalEntity(ctx context.Context, c *Client, code string) (string, error) {
	les, err := List[struct {
		ID   string `json:"id"`
		Code string `json:"code"`
	}](ctx, c, "/v1/legal-entities", nil)
	if err != nil {
		return "", err
	}
	for _, le := range les {
		if le.Code == code {
			return le.ID, nil
		}
	}
	return "", fmt.Errorf("apitools: no company %s that the caller may read", code)
}

// Multipart is a request body sent as a multipart form, one part per
// field, in the order of their names: an upload whose file the contract
// takes as a string of bytes, such as a bank statement.
type Multipart map[string]string

// Do performs a call. Unsafe calls get a fresh Idempotency-Key; ifMatch,
// when not empty, is sent as If-Match. A Multipart body is sent as a
// multipart form, any other as JSON.
func (c *Client) Do(ctx context.Context, method, path string, query url.Values, body any, ifMatch string) (Response, error) {
	var reader io.Reader
	contentType := ""
	if form, ok := body.(Multipart); ok {
		reader, contentType = form.encode()
	} else if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return Response{}, fmt.Errorf("apitools: body: %w", err)
		}
		reader, contentType = bytes.NewReader(b), "application/json"
		if method == http.MethodPatch {
			contentType = "application/merge-patch+json"
		}
	}
	u := strings.TrimRight(c.BaseURL, "/") + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return c.send(ctx, method, u, contentType, reader, ifMatch)
}

// Form posts a multipart form of the values to path (Multipart), with a
// fresh Idempotency-Key.
func (c *Client) Form(ctx context.Context, path string, values map[string]string) (Response, error) {
	return c.Do(ctx, http.MethodPost, path, nil, Multipart(values), "")
}

// encode writes the form, its fields in the order of their names, and
// returns it with its content type.
func (m Multipart) encode() (io.Reader, string) {
	var body bytes.Buffer
	// The multipart writer writes to a bytes.Buffer, whose writes never
	// fail, so none of these calls can return an error.
	w := multipart.NewWriter(&body)
	for _, k := range slices.Sorted(maps.Keys(m)) {
		_ = w.WriteField(k, m[k])
	}
	_ = w.Close()
	return &body, w.FormDataContentType()
}

// send makes the request with the client's token.
func (c *Client) send(ctx context.Context, method, u, contentType string, body io.Reader, ifMatch string) (Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return Response{}, fmt.Errorf("apitools: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
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
		return Response{}, fmt.Errorf("apitools: %s %s: %w", method, req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }() // only read from: closing frees the connection and cannot change the answer
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, fmt.Errorf("apitools: %s %s: %w", method, req.URL.Path, err)
	}
	return Response{Status: resp.StatusCode, ETag: resp.Header.Get("ETag"), Body: b}, nil
}
