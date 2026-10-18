package uow_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn/authntest"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
)

// TestAuthenticated: a handler that must not hold a transaction while it
// works (the model gateway, ADR-0036) gets the request's principal from a
// short transaction of its own, in which check authorizes it; its own
// work then runs with the principal and no transaction.
func TestAuthenticated(t *testing.T) {
	is := authntest.NewSeparate(t)
	ten := pgtest.NewTenant(t)
	pgtest.Exec(t, `UPDATE identity.principal SET external_issuer = $2, version = version + 1 WHERE tenant_id = $1`, ten.ID, is.URL)
	reg := authn.Registration{TenantID: ten.ID, Issuer: is.URL, Audience: authntest.Audience}
	v, err := authn.NewRegistered(context.Background(), authn.Development, oneProvider{reg}, &authn.Providers{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	mw := &uow.Middleware{Pool: pgtest.Pool(t), Role: "erp_app", Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: time.Now}
	var seen reqctx.Principal
	var hadTx bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = reqctx.PrincipalFrom(r.Context())
		_, _, err := reqctx.Request(r.Context())
		hadTx = err == nil
		w.WriteHeader(http.StatusNoContent)
	})
	for _, c := range []struct {
		name    string
		subject string
		token   bool
		check   func(context.Context) error
		status  int
	}{
		{"an agent, authorized", ten.Agent.Subject, true, func(ctx context.Context) error {
			if _, _, err := reqctx.Request(ctx); err != nil {
				return errors.New("check ran without the transaction")
			}
			return nil
		}, http.StatusNoContent},
		{"refused by the check", ten.Agent.Subject, true, func(context.Context) error { return problem.Forbidden("authz.denied", "no") }, http.StatusForbidden},
		{"an unknown subject", "nobody", true, func(context.Context) error { return nil }, http.StatusForbidden},
		{"no token", "", false, func(context.Context) error { return nil }, http.StatusUnauthorized},
		{"a bound token without its certificate", ten.Agent.Subject, true, func(context.Context) error { return nil }, http.StatusUnauthorized},
	} {
		t.Run(c.name, func(t *testing.T) {
			seen, hadTx = reqctx.Principal{}, true
			r := httptest.NewRequest(http.MethodPost, "/v1/model/v1/messages", nil)
			if c.token {
				var claims map[string]any
				if c.name == "a bound token without its certificate" {
					claims = map[string]any{"cnf": map[string]any{"x5t#S256": "t1"}}
				}
				r.Header.Set("Authorization", "Bearer "+is.Token(t, uuid.Nil, c.subject, claims))
			}
			rec := httptest.NewRecorder()
			v.Middleware(mw.Authenticated(c.check, inner)).ServeHTTP(rec, r)
			if rec.Code != c.status {
				t.Fatalf("status: got %d %s, want %d", rec.Code, rec.Body, c.status)
			}
			if c.status != http.StatusNoContent {
				return
			}
			if seen.ID != ten.Agent.ID {
				t.Errorf("principal: got %v, want the agent %s", seen.ID, ten.Agent.ID)
			}
			if hadTx {
				t.Error("the handler ran inside a transaction, want none")
			}
		})
	}
}

// TestAuthenticatedWithoutTheDatabase: a transaction that cannot begin
// fails the request, the handler not reached.
func TestAuthenticatedWithoutTheDatabase(t *testing.T) {
	is := authntest.NewSeparate(t)
	reg := authn.Registration{TenantID: uuid.New(), Issuer: is.URL, Audience: authntest.Audience}
	v, err := authn.NewRegistered(context.Background(), authn.Development, oneProvider{reg}, &authn.Providers{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	mw := &uow.Middleware{Pool: pgtest.Pool(t), Role: "erp_app", Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: time.Now,
		Begin: func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errors.New("database down") }}
	reached := false
	r := httptest.NewRequest(http.MethodPost, "/v1/model/v1/messages", nil)
	r.Header.Set("Authorization", "Bearer "+is.Token(t, uuid.Nil, "ada", nil))
	rec := httptest.NewRecorder()
	v.Middleware(mw.Authenticated(func(context.Context) error { return nil }, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))).ServeHTTP(rec, r)
	if rec.Code != http.StatusInternalServerError || reached {
		t.Errorf("got %d (handler reached %v), want 500 and not reached", rec.Code, reached)
	}
}

// TestAuthenticatedWithoutClaims: outside the verifier no request is
// authenticated.
func TestAuthenticatedWithoutClaims(t *testing.T) {
	mw := &uow.Middleware{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	rec := httptest.NewRecorder()
	mw.Authenticated(func(context.Context) error { return nil }, http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/model/v1/messages", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("got %d, want 401", rec.Code)
	}
}

// TestAuthenticatedRequiresBoundAgents: where agents must hold
// certificate-bound tokens, an agent's unbound token is refused, as the
// unit of work refuses it (ADR-0033), and the handler is not reached.
func TestAuthenticatedRequiresBoundAgents(t *testing.T) {
	is := authntest.NewSeparate(t)
	ten := pgtest.NewTenant(t)
	pgtest.Exec(t, `UPDATE identity.principal SET external_issuer = $2, version = version + 1 WHERE tenant_id = $1`, ten.ID, is.URL)
	reg := authn.Registration{TenantID: ten.ID, Issuer: is.URL, Audience: authntest.Audience}
	v, err := authn.NewRegistered(context.Background(), authn.Development, oneProvider{reg}, &authn.Providers{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	mw := &uow.Middleware{Pool: pgtest.Pool(t), Role: "erp_app", Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: time.Now, RequireBoundAgents: true}
	reached := false
	r := httptest.NewRequest(http.MethodPost, "/v1/model/v1/messages", nil)
	r.Header.Set("Authorization", "Bearer "+is.Token(t, uuid.Nil, ten.Agent.Subject, nil))
	rec := httptest.NewRecorder()
	v.Middleware(mw.Authenticated(func(context.Context) error { return nil }, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))).ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d %s, want 401", rec.Code, rec.Body)
	}
	if reached {
		t.Error("handler reached, want not")
	}
}
