package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
)

// Client is a tenant's web client at its identity provider, found by its
// login code (ADR-0052 G8): what erp-web needs to start a sign-in before
// it knows anyone.
type Client struct {
	LoginCode     string
	Issuer        string
	Audience      string // the access token's audience: the registration's
	TenantID      uuid.UUID
	ClientID      string
	AuthMethod    string // private_key_jwt or client_secret_basic
	CredentialRef string
	Scope         string
}

// ErrUnknownCode means no active provider's web client holds the login
// code.
var ErrUnknownCode = errors.New("session: no web client holds the login code")

// The client cache's bounds: an answer, a client or an unknown code, is
// kept CacheTTL, as the router keeps the directory's (ADR-0052 §5), and at
// most MaxCached, so codes anyone can type do not grow it without end.
const (
	CacheTTL  = 30 * time.Second
	MaxCached = 10000
)

// Clients finds web clients by login code in the directory's projection
// (web.login_client), remembering each answer for CacheTTL. A change or
// removal at the control plane reaches sign-ins within a minute and a
// half: the cell's next round (a minute at most, immediately for a
// registration or change) and this cache.
type Clients struct {
	Store *Store

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	client Client
	err    error
	until  time.Time
}

// Find returns the web client holding the login code; ErrUnknownCode when
// none does. Failures to read are not remembered.
func (c *Clients) Find(ctx context.Context, code string) (Client, error) {
	now := clock.Now(c.Store.Now)
	c.mu.Lock()
	hit, ok := c.cache[code]
	c.mu.Unlock()
	if ok && now.Before(hit.until) {
		return hit.client, hit.err
	}
	var out Client
	err := c.Store.inTx(ctx, true, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT login_code, issuer, audience, tenant_id, client_id, auth_method, credential_ref, scope
			FROM web.login_client WHERE login_code = $1`, code).Scan(
			&out.LoginCode, &out.Issuer, &out.Audience, &out.TenantID, &out.ClientID, &out.AuthMethod, &out.CredentialRef, &out.Scope)
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		err = ErrUnknownCode
	case err != nil:
		return Client{}, fmt.Errorf("session: find the web client of login code %s: %w", code, err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cache == nil || len(c.cache) >= MaxCached {
		c.cache = map[string]cached{} // a full cache starts afresh: its answers are a lookup away
	}
	c.cache[code] = cached{client: out, err: err, until: now.Add(CacheTTL)}
	return out, err
}
