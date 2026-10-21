package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/mustie7iy/agentic-erp/api/gen"
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
