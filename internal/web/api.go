package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/proxyproto"
)

// ErrNoSession is the API client's answer for a call made without the
// signed-in person's token: erp-web has no token of its own (ADR-0052 §7).
var ErrNoSession = errors.New("web: no signed-in person: an API call carries the person's own token")

type tokenKey struct{}

// WithToken attaches the signed-in person's access token, which the API
// client sends unchanged as the request's bearer token (T73b's sessions
// hold it).
func WithToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, tokenKey{}, token)
}

type addressKey struct{}

// WithClientAddress attaches the signed-in person's address, as erp-web's
// load balancer named it, which the API client forwards to the router for
// the API's audit trail (ADR-0011 as amended by T73c).
func WithClientAddress(ctx context.Context, addr netip.Addr) context.Context {
	return context.WithValue(ctx, addressKey{}, addr)
}

// forwardAddress sends the person's address (WithClientAddress) with each
// API call as Erp-Client-Address, and none when the context has none, so
// a call never carries another person's.
type forwardAddress struct{ next http.RoundTripper }

func (f forwardAddress) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context()) // a RoundTripper must not change the caller's request
	r.Header.Del(proxyproto.ClientAddressHeader)
	if addr, ok := r.Context().Value(addressKey{}).(netip.Addr); ok && addr.IsValid() {
		r.Header.Set(proxyproto.ClientAddressHeader, addr.String())
	}
	return f.next.RoundTrip(r) //nolint:wrapcheck // the transport's errors reach the API client as they are, as without this wrapper
}

// personToken is the client's security source: the token WithToken
// attached, or ErrNoSession before any request is sent.
type personToken struct{}

func (personToken) Oidc(ctx context.Context, _ gen.OperationName) (gen.Oidc, error) {
	token, _ := ctx.Value(tokenKey{}).(string)
	if token == "" {
		return gen.Oidc{}, ErrNoSession
	}
	return gen.Oidc{Token: token}, nil
}

// NewAPI is the public API's typed client (api/gen) at baseURL, the
// router's public address, through hc. A change to api/openapi.yaml that a
// screen's call no longer fits breaks the build (ADR-0052 §4).
func NewAPI(baseURL string, hc *http.Client) (*gen.Client, error) {
	c, err := gen.NewClient(baseURL, personToken{}, gen.WithClient(hc))
	if err != nil {
		return nil, fmt.Errorf("web: API client for %s: %w", baseURL, err)
	}
	return c, nil
}
