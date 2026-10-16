package uow_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn/authntest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
)

// TestCertificateBoundTokens: a token bound to a certificate (RFC 8705,
// cnf.x5t#S256) is accepted only with that certificate; with
// RequireBoundAgents (production) an agent's token is accepted only from a
// provider that binds tokens, bound, with its certificate. People and
// services are not asked, and each refusal is logged (ADR-0033).
func TestCertificateBoundTokens(t *testing.T) {
	is := authntest.NewSeparate(t)
	ten := pgtest.NewTenant(t)
	pgtest.Exec(t, `UPDATE identity.principal SET external_issuer = $2, version = version + 1 WHERE tenant_id = $1`, ten.ID, is.URL)
	const thumb, other = "s5-thumbprint-of-the-agents-certificate", "another-certificate"
	bound := map[string]any{"cnf": map[string]any{"x5t#S256": thumb}}
	for _, c := range []struct {
		name      string
		binds     bool // the provider binds its tokens
		require   bool // production: agents' tokens must be bound
		subject   string
		claims    map[string]any
		presented string // the client certificate's thumbprint the router forwarded
		status    int
	}{
		{"a bound agent with its certificate", true, true, ten.Agent.Subject, bound, thumb, 200},
		{"a bound agent with another certificate", true, true, ten.Agent.Subject, bound, other, 401},
		{"a bound agent without a certificate", true, true, ten.Agent.Subject, bound, "", 401},
		{"an unbound agent of a binding provider", true, true, ten.Agent.Subject, nil, thumb, 401},
		{"a bound agent of a provider that does not bind", false, true, ten.Agent.Subject, bound, thumb, 401},
		{"an unbound agent outside production", false, false, ten.Agent.Subject, nil, "", 200},
		{"a bound token outside production still needs its certificate", false, false, ten.Agent.Subject, bound, other, 401},
		{"a person, unbound", true, true, ten.Human.Subject, nil, "", 200},
		{"a service, unbound", true, true, ten.Service.Subject, nil, "", 200},
	} {
		t.Run(c.name, func(t *testing.T) {
			reg := authn.Registration{TenantID: ten.ID, Issuer: is.URL, Audience: authntest.Audience, CertificateBound: c.binds}
			v, err := authn.NewRegistered(context.Background(), authn.Development, oneProvider{reg}, &authn.Providers{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			mw := &uow.Middleware{Pool: pgtest.Pool(t), Role: "erp_app", Log: slog.New(slog.NewTextHandler(&logs, nil)), Now: time.Now, RequireBoundAgents: c.require}
			handler := v.Middleware(mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })))
			r := httptest.NewRequest("GET", "/read", nil)
			r.Header.Set("Authorization", "Bearer "+is.Token(t, uuid.Nil, c.subject, c.claims))
			if c.presented != "" {
				r = r.WithContext(reqctx.WithClientCert(r.Context(), c.presented))
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, r)
			if rec.Code != c.status {
				t.Fatalf("status: got %d %s, want %d", rec.Code, rec.Body, c.status)
			}
			if c.status == 401 && !strings.Contains(logs.String(), "certificate binding refused") {
				t.Errorf("logs: got %s, want the refusal logged", logs.String())
			}
		})
	}
}
