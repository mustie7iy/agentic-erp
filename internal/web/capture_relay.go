package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
)

// The relay of a captured document's bytes (ADR-0052 §7, PO 2026-11-01):
// erp-web streams them to the API's own signed upload path (createUpload
// with via api, ADR-0020 As built 2026-11-01), through the router as
// every call goes, never to a presigned bucket URL, so its egress stays the
// router alone and no bucket is on the workspace's CSP or CORS. It holds
// at most a buffer of the document at a time, and stops at the upload's
// limit.

// relay is the API's base URL and the client that reaches it, which
// follows no redirect: a relayed body goes to the path the API named and
// nowhere else.
type relay struct {
	base *url.URL
	hc   *http.Client
}

// newRelay is the relay to the API at base through hc's transport, each
// call bounded by relayTimeout rather than hc's 30 seconds: a document
// streams through it for as long as the cell takes it (PO 2026-11-02).
func newRelay(base *url.URL, hc *http.Client) relay {
	c := *hc
	c.Timeout = relayTimeout
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return relay{base: base, hc: &c}
}

// errNotAPIPath is an upload whose URL is not the API's own signed path
// for it: erp-web sends the document there alone.
var errNotAPIPath = errors.New("web: the upload's URL is not the API's signed path for it")

// errTooLarge is a document that passes its upload's limit while it is
// relayed.
var errTooLarge = errors.New("web: the document is larger than its upload takes")

// target is where the upload's bytes go: the API's signed path, resolved
// against erp-web's own address of the API, whatever host the API's
// public URL names; anything else is refused before a byte is sent.
func (rl relay) target(up gen.Upload) (string, error) {
	u, err := url.Parse(up.URL)
	if err != nil || up.Via != "api" || up.Method != http.MethodPut || u.Path != "/v1/uploads/"+up.ID.String()+"/content" || u.RawQuery == "" {
		return "", errNotAPIPath
	}
	t := *rl.base
	t.Path = strings.TrimSuffix(t.Path, "/") + u.Path
	t.RawQuery = u.RawQuery
	return t.String(), nil
}

// put streams body to the upload's signed path as the person: the
// upload's headers, exactly, and the person's bearer token, which the
// router routes by (the API checks the path's signature). The body is
// sent as it is read, cut at the upload's limit. A nil problem is the
// bytes received; 409 payables.upload_closed is an upload that already
// holds its document (the form sent again).
func (rl relay) put(ctx context.Context, up gen.Upload, body io.Reader) (*gen.Problem, error) {
	target, err := rl.target(up)
	if err != nil {
		return nil, err
	}
	token, _ := ctx.Value(tokenKey{}).(string)
	if token == "" {
		return nil, ErrNoSession
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, &capped{r: body, left: up.MaxBytes})
	if err != nil {
		return nil, fmt.Errorf("web: upload %s: %w", up.ID, err)
	}
	for k, v := range up.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := rl.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("web: upload %s: %w", up.ID, err)
	}
	defer func() { _ = resp.Body.Close() }() // only read from: closing frees the connection and cannot change the answer
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	var prob gen.Problem
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&prob); err != nil || prob.Code == "" {
		return nil, fmt.Errorf("web: upload %s: the API answered %s without a problem", up.ID, resp.Status)
	}
	return &prob, nil
}

// capped reads r and fails once more than left bytes have come.
type capped struct {
	r    io.Reader
	left int64
}

func (c *capped) Read(p []byte) (int, error) {
	if int64(len(p)) > c.left+1 {
		p = p[:c.left+1] // one byte past the limit is enough to know
	}
	n, err := c.r.Read(p)
	if c.left -= int64(n); c.left < 0 {
		return 0, errTooLarge
	}
	return n, err //nolint:wrapcheck // the document's reader's own io.EOF, which the request's transport must see as it is
}

// uploadKey is the idempotency key of the form's createUpload, derived
// from the form's own, which its capture sends: a form sent again creates
// no second upload.
func uploadKey(form uuid.UUID) string {
	return uuid.NewSHA1(form, []byte("createUpload")).String()
}
