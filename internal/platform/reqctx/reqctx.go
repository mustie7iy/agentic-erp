// Package reqctx carries what the request pipeline establishes (ADR-0014)
// through context: the authenticated claims, the resolved principal, the
// request's database transaction and its trace id. Handlers read these; only
// the pipeline sets them.
package reqctx

import (
	"context"
	"errors"
	"net/netip"
	"time"

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
	// Roles are an operator token's roles (ADR-0027): nil when the token
	// carries no roles claim, which the control planes treat as every role.
	Roles []string
	// ProviderID is the registered identity provider whose registration
	// decided the tenant (ADR-0031); zero for a token of a static issuer.
	ProviderID uuid.UUID
	// The token's facts multi-factor and sessions are decided on: when the
	// person authenticated at the provider (auth_time; zero when absent),
	// when the token was issued, how (acr, amr) and in which provider
	// session (sid). Absent or malformed claims leave them empty.
	AuthTime  time.Time
	IssuedAt  time.Time
	Expiry    time.Time
	ACR       string
	AMR       []string
	SessionID string
	// MFA is the registered provider's multi-factor rule (ADR-0031), which
	// the unit of work applies once it knows the principal is a person.
	MFA MFA
	// Session is the tenant's session rule for people; zero for a token
	// of a deployment's own issuer, which no tenant's policy covers.
	Session SessionRule
	// CertThumbprint is the token's cnf.x5t#S256: the SHA-256 thumbprint
	// of the client certificate it is bound to (RFC 8705); empty when the
	// token is not bound.
	CertThumbprint string
	// CertificateBound says the token's registered provider binds its
	// tokens to certificates (ADR-0033).
	CertificateBound bool
}

// SessionRule is how long a person's session lasts from the
// authentication at the provider, and without a request.
type SessionRule struct {
	MaxAge time.Duration
	Idle   time.Duration
}

// MFA is whether the tenant requires multi-factor authentication of
// people, and the acr and amr values by which their provider proves it.
type MFA struct {
	Required  bool
	ACRValues []string
	AMRValues []string
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
	clientCertKey
	clientAddressKey
)

// WithClientCert records the thumbprint (RFC 8705 x5t#S256) of the client
// certificate the request was made with, as the router forwarded it.
func WithClientCert(ctx context.Context, thumbprint string) context.Context {
	return context.WithValue(ctx, clientCertKey, thumbprint)
}

// ClientCertFrom is the request's client certificate thumbprint, or "".
func ClientCertFrom(ctx context.Context) string {
	t, _ := ctx.Value(clientCertKey).(string)
	return t
}

// WithClientAddress records the IP address the request came from, as the
// cell established it (httpapi.ClientAddress): the person's in the web
// workspace, otherwise the connection's. The audit trail records it.
func WithClientAddress(ctx context.Context, addr netip.Addr) context.Context {
	return context.WithValue(ctx, clientAddressKey, addr)
}

// ClientAddressFrom is the request's client address; ok is false when none
// is known (work no request made).
func ClientAddressFrom(ctx context.Context) (addr netip.Addr, ok bool) {
	addr, ok = ctx.Value(clientAddressKey).(netip.Addr)
	return addr, ok && addr.IsValid()
}

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

// Masked reports whether personal data only people may see is withheld
// from the caller, such as client addresses in the audit trail (T73c,
// product owner 2026-10-22: people only). It is an allow-list: false for a
// person alone, true for agents, service principals, any other kind and a
// caller the request does not name, so a new kind fails closed.
func Masked(ctx context.Context) bool {
	p, ok := PrincipalFrom(ctx)
	return !ok || p.Kind != KindHuman
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

type readOnlyKey struct{}

// WithReadOnly marks the request's transaction read-only, so that an error
// it raises is translated as one (problem.FromReadOnly, ADR-0040).
func WithReadOnly(ctx context.Context) context.Context {
	return context.WithValue(ctx, readOnlyKey{}, true)
}

// ReadOnly reports whether the request's transaction is read-only.
func ReadOnly(ctx context.Context) bool {
	return ctx.Value(readOnlyKey{}) != nil
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

// Commit holds what runs once the unit of work's transaction has
// committed: effects the outbox does not carry that must not happen for a
// request rolled back, such as counting a decision in a metric (ADR-0043).
// A request adds to it from its own goroutine; it is not safe for
// concurrent use.
type Commit struct{ fns []func() }

type commitKey struct{}

// WithCommit attaches an empty Commit, which the unit of work runs after
// its commit.
func WithCommit(ctx context.Context) (context.Context, *Commit) {
	c := &Commit{}
	return context.WithValue(ctx, commitKey{}, c), c
}

// OnCommit adds fn to the request's Commit. Work outside an API request's
// unit of work (a worker's uow.Act, whose caller commits) has no Commit,
// and fn is not run there: such work is not observed this way.
func OnCommit(ctx context.Context, fn func()) {
	if c, ok := ctx.Value(commitKey{}).(*Commit); ok {
		c.fns = append(c.fns, fn)
	}
}

// Run runs what was added, once.
func (c *Commit) Run() {
	fns := c.fns
	c.fns = nil
	for _, fn := range fns {
		fn()
	}
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
