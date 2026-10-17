package uow

import (
	"context"
	"net/http"

	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Authenticated serves next with the request's principal but no
// transaction, for work that must not hold one while it waits (the model
// gateway's calls to the provider, ADR-0036). The principal is resolved,
// with the unit of work's checks, in a short read-only transaction of its
// own, in which check authorizes the request; next opens the transactions
// its work needs.
func (m *Middleware) Authenticated(check func(ctx context.Context) error, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := m.authenticate(r.Context(), check)
		if err != nil {
			problem.Write(w, r, problem.From(err))
			return
		}
		info := reqctx.InfoFrom(r.Context())
		info.TenantID, info.PrincipalID = p.TenantID, p.ID
		next.ServeHTTP(w, r.WithContext(reqctx.WithPrincipal(r.Context(), p)))
	})
}

func (m *Middleware) authenticate(ctx context.Context, check func(ctx context.Context) error) (reqctx.Principal, error) {
	claims, ok := reqctx.ClaimsFrom(ctx)
	if !ok {
		return reqctx.Principal{}, problem.Unauthorized("A bearer token is required.")
	}
	if err := m.admit(ctx, claims); err != nil {
		return reqctx.Principal{}, err
	}
	tx, err := m.begin(ctx, database.Scope{Role: m.Role, TenantID: claims.TenantID, ReadOnly: true})
	if err != nil {
		return reqctx.Principal{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) // read-only: nothing to commit
	p, err := resolve(ctx, tx, claims)
	if err != nil {
		return p, err
	}
	if err := m.authorize(ctx, claims, p); err != nil {
		return p, err
	}
	return p, check(reqctx.WithPrincipal(reqctx.WithTx(ctx, tx), p))
}
