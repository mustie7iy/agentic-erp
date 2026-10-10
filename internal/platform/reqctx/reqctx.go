// Package reqctx carries what the request pipeline establishes (ADR-0014)
// through context: the authenticated claims, the resolved principal, the
// request's database transaction and its trace id. Handlers read these; only
// the pipeline sets them.
package reqctx

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Principal kinds, as in identity.principal.kind.
const (
	KindHuman   = "human"
	KindAgent   = "agent"
	KindService = "service"
	KindSystem  = "system" // platform jobs such as the outbox relay; never a token holder
)

// Claims are the verified facts from the bearer token.
type Claims struct {
	Issuer   string
	Subject  string
	TenantID uuid.UUID
}

// Principal is the authenticated actor resolved in the tenant's identity data.
type Principal struct {
	ID       uuid.UUID
	Kind     string
	TenantID uuid.UUID
}

type key int

const (
	claimsKey key = iota
	principalKey
	txKey
	traceKey
)

func WithClaims(ctx context.Context, c Claims) context.Context {
	return context.WithValue(ctx, claimsKey, c)
}

func ClaimsFrom(ctx context.Context) (Claims, bool) {
	c, ok := ctx.Value(claimsKey).(Claims)
	return c, ok
}

func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey, p)
}

func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey).(Principal)
	return p, ok
}

// WithTx attaches the request's transaction. Everything a handler reads or
// writes goes through it, so tenant isolation and atomicity hold.
func WithTx(ctx context.Context, tx pgx.Tx) context.Context {
	return context.WithValue(ctx, txKey, tx)
}

func TxFrom(ctx context.Context) (pgx.Tx, bool) {
	tx, ok := ctx.Value(txKey).(pgx.Tx)
	return tx, ok
}

func WithTraceID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, traceKey, id)
}

// TraceID returns the request's trace id, or "" outside a request.
func TraceID(ctx context.Context) string {
	id, _ := ctx.Value(traceKey).(string)
	return id
}

// Info is filled in by the pipeline's inner layers so the outermost layer
// can log and measure the request with its tenant, principal and operation.
type Info struct {
	TenantID    uuid.UUID
	PrincipalID uuid.UUID
	Operation   string
}

type infoKey struct{}

// WithInfo attaches an empty Info that inner layers fill.
func WithInfo(ctx context.Context) (context.Context, *Info) {
	i := &Info{}
	return context.WithValue(ctx, infoKey{}, i), i
}

// InfoFrom returns the request's Info, or a throwaway one outside a request.
func InfoFrom(ctx context.Context) *Info {
	if i, ok := ctx.Value(infoKey{}).(*Info); ok {
		return i
	}
	return &Info{}
}

// ErrOutsideRequest means a handler or context function ran without the
// transaction and principal the unit of work provides.
var ErrOutsideRequest = errors.New("reqctx: no transaction and principal in context (outside a unit of work)")

// Request returns the request's transaction and principal.
func Request(ctx context.Context) (pgx.Tx, Principal, error) {
	tx, ok := TxFrom(ctx)
	p, ok2 := PrincipalFrom(ctx)
	if !ok || !ok2 {
		return nil, Principal{}, ErrOutsideRequest
	}
	return tx, p, nil
}
