package authn_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn/authntest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// TestCheckBinding: a bound token needs its certificate; requireBound needs
// a bound token of a binding provider (RFC 8705, ADR-0033).
func TestCheckBinding(t *testing.T) {
	bound := reqctx.Claims{CertThumbprint: "t1", CertificateBound: true}
	for _, c := range []struct {
		name      string
		claims    reqctx.Claims
		presented string
		require   bool
		ok        bool
	}{
		{"bound, its certificate", bound, "t1", true, true},
		{"bound, another certificate", bound, "t2", false, false},
		{"bound, no certificate", bound, "", false, false},
		{"unbound, not required", reqctx.Claims{}, "", false, true},
		{"unbound, required", reqctx.Claims{CertificateBound: true}, "t1", true, false},
		{"bound by a provider that does not bind, required", reqctx.Claims{CertThumbprint: "t1"}, "t1", true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := authn.CheckBinding(c.claims, c.presented, c.require); (err == nil) != c.ok {
				t.Errorf("got %v, want accepted %v", err, c.ok)
			}
		})
	}
}

// TestConfirmationClaims: a token's cnf binds it to a certificate only by
// a non-empty x5t#S256; any other confirmation (DPoP's jkt, a malformed
// one) is refused rather than taken as a bearer token, and an operator's
// token may not be bound at all, its listeners having no certificate to
// check it against (ADR-0033).
func TestConfirmationClaims(t *testing.T) {
	is := authntest.New(t)
	v, err := authn.New(context.Background(), authn.Development, []authn.Issuer{is.Config()})
	if err != nil {
		t.Fatal(err)
	}
	tenant := uuid.New()
	for _, c := range []struct {
		name string
		cnf  any
		want string // the thumbprint; "refused" for a 401
	}{
		{"bound", map[string]any{"x5t#S256": "t1"}, "t1"},
		{"DPoP", map[string]any{"jkt": "k1"}, "refused"},
		{"not a string", map[string]any{"x5t#S256": 1}, "refused"},
		{"empty", map[string]any{"x5t#S256": ""}, "refused"},
		{"not an object", "x", "refused"},
	} {
		t.Run(c.name, func(t *testing.T) {
			claims, err := v.Verify(context.Background(), is.Token(t, tenant, "ada", map[string]any{"cnf": c.cnf}))
			switch {
			case c.want == "refused" && problem.From(err).Status != http.StatusUnauthorized:
				t.Errorf("got %v, want 401", err)
			case c.want != "refused" && (err != nil || claims.CertThumbprint != c.want):
				t.Errorf("got %q, %v, want %q", claims.CertThumbprint, err, c.want)
			}
		})
	}
	t.Run("an operator's bound token", func(t *testing.T) {
		_, err := v.VerifyOperator(context.Background(), is.Token(t, uuid.Nil, "ops|alice", map[string]any{"tenant_id": nil, "cnf": map[string]any{"x5t#S256": "t1"}}))
		if problem.From(err).Status != http.StatusUnauthorized {
			t.Errorf("got %v, want 401", err)
		}
	})
}
