package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/mustie7iy/agentic-erp/internal/analytics/wire"
	"github.com/mustie7iy/agentic-erp/internal/platform/tlsconf"
)

// maxResult bounds a result's body: groups of one query.
const maxResult = 16 << 20

// Client calls erp-analytics over mTLS (ADR-0033): HTTP is the internal
// client of erp-server's TLS material (tlsconf.Store.InternalClient, with
// its timeout), which accepts the server only when it presents Peer.
type Client struct {
	URL  string // the reader's base URL, https://…
	Peer string // spiffe://<domain>/<cell>/erp-analytics
	HTTP *http.Client
}

// Query posts req and returns its groups. Any answer but 200 is an error,
// which the source answers from the database: a refusal (422), the reader
// busy (503) or failing, a timeout.
func (c *Client) Query(ctx context.Context, req wire.Request) (*wire.Result, error) {
	body, _ := json.Marshal(req) // ids, strings and numbers: it always encodes
	r, err := http.NewRequestWithContext(tlsconf.ExpectPeer(ctx, c.Peer), http.MethodPost, c.URL+wire.QueryPath, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("analytics: query: %w", err)
	}
	r.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(r)
	if err != nil {
		return nil, fmt.Errorf("analytics: query erp-analytics: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // a read body; closing it reports nothing to act on
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResult))
	if err != nil {
		return nil, fmt.Errorf("analytics: read erp-analytics' answer: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("analytics: erp-analytics answered %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	var res wire.Result
	if err := json.Unmarshal(b, &res); err != nil {
		return nil, fmt.Errorf("analytics: decode erp-analytics' answer: %w", err)
	}
	if res.ManifestKey != req.ManifestKey {
		return nil, fmt.Errorf("analytics: erp-analytics answered from %s, asked %s", res.ManifestKey, req.ManifestKey)
	}
	return &res, nil
}
