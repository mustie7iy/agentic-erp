package authn_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn/authntest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

func verifier(t *testing.T, issuers ...*authntest.Issuer) *authn.Verifier {
	t.Helper()
	var cfg []authn.Issuer
	for _, is := range issuers {
		cfg = append(cfg, is.Config())
	}
	v, err := authn.New(context.Background(), authn.Development, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestVerifyAcceptsValidTokens(t *testing.T) {
	is := authntest.New(t)
	v := verifier(t, is)
	tenant := uuid.New()
	c, err := v.Verify(context.Background(), is.Token(t, tenant, "human|1", nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.TenantID != tenant {
		t.Errorf("tenant: got %v, want %v", c.TenantID, tenant)
	}
	if c.Subject != "human|1" {
		t.Errorf("subject: got %s, want human|1", c.Subject)
	}
	if c.Issuer != is.URL {
		t.Errorf("issuer: got %s, want %s", c.Issuer, is.URL)
	}
}

func TestVerifyRejectsBadTokens(t *testing.T) {
	is, untrusted := authntest.New(t), authntest.New(t)
	v := verifier(t, is)
	tenant := uuid.New()
	valid := is.Token(t, tenant, "s", nil)
	parts := strings.Split(valid, ".")
	cases := map[string]string{
		"not a jwt":         "abc",
		"bad base64":        parts[0] + ".!!!." + parts[2],
		"bad json":          parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte("{")) + "." + parts[2],
		"untrusted issuer":  untrusted.Token(t, tenant, "s", nil),
		"expired":           is.Token(t, tenant, "s", map[string]any{"exp": time.Now().Add(-time.Hour).Unix()}),
		"wrong audience":    is.Token(t, tenant, "s", map[string]any{"aud": "someone-else"}),
		"tampered":          parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"`+is.URL+`","sub":"admin"}`)) + "." + parts[2],
		"no tenant":         is.Token(t, tenant, "s", map[string]any{"tenant_id": nil}),
		"tenant not a uuid": is.Token(t, tenant, "s", map[string]any{"tenant_id": "acme"}),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := v.Verify(context.Background(), raw)
			if p := problem.From(err); err == nil || p.Status != http.StatusUnauthorized {
				t.Errorf("got %v, want a 401 problem", err)
			}
		})
	}
}

func TestNewValidatesConfiguration(t *testing.T) {
	ctx := context.Background()
	if _, err := authn.New(ctx, authn.Development, nil); err == nil {
		t.Fatal("no issuers accepted")
	}
	is := authntest.New(t)
	if _, err := authn.New(ctx, authn.Development, []authn.Issuer{{URL: is.URL}}); err == nil {
		t.Fatal("missing audience accepted")
	}
	if _, err := authn.New(ctx, authn.Development, []authn.Issuer{{URL: "http://127.0.0.1:1", Audience: "a"}}); err == nil {
		t.Fatal("unreachable issuer accepted")
	}
	custom, err := authn.New(ctx, authn.Development, []authn.Issuer{{URL: is.URL, Audience: authntest.Audience, TenantClaim: "https://erp/tenant"}})
	if err != nil {
		t.Fatal(err)
	}
	tenant := uuid.New()
	c, err := custom.Verify(ctx, is.Token(t, uuid.New(), "s", map[string]any{"https://erp/tenant": tenant.String()}))
	if err != nil || c.TenantID != tenant {
		t.Fatal(c, err)
	}
}

func TestOperatorTokens(t *testing.T) {
	is, other := authntest.New(t), authntest.New(t)
	v := verifier(t, is)
	ctx := context.Background()
	// An operator token names no tenant; a tenant's token is not an operator's and the reverse.
	operator := is.Token(t, uuid.Nil, "ops|alice", map[string]any{"tenant_id": nil})
	c, err := v.VerifyOperator(ctx, operator)
	if err != nil {
		t.Fatalf("operator token: %v", err)
	}
	if c.Subject != "ops|alice" {
		t.Errorf("operator subject: got %s, want ops|alice", c.Subject)
	}
	if c.Issuer != is.URL {
		t.Errorf("operator issuer: got %s, want %s", c.Issuer, is.URL)
	}
	if c.TenantID != uuid.Nil {
		t.Errorf("operator tenant: got %v, want none", c.TenantID)
	}
	for name, raw := range map[string]string{
		"names a tenant": is.Token(t, uuid.New(), "ops|alice", nil),
		"untrusted":      other.Token(t, uuid.Nil, "ops|alice", map[string]any{"tenant_id": nil}),
		"malformed":      "abc",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := v.VerifyOperator(ctx, raw); problem.From(err).Status != http.StatusUnauthorized {
				t.Errorf("got %v, want a 401 problem", err)
			}
		})
	}
	if _, err := v.Verify(ctx, operator); problem.From(err).Status != http.StatusUnauthorized {
		t.Fatalf("operator token as a tenant's: %v", err)
	}
	h := v.OperatorMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _ := reqctx.ClaimsFrom(r.Context())
		w.Header().Set("Subject", c.Subject)
		w.WriteHeader(204)
	}))
	for name, c := range map[string]struct {
		raw  string
		want int
	}{
		"operator token": {operator, 204},
		"tenant token":   {is.Token(t, uuid.New(), "s", nil), 401},
	} {
		t.Run("middleware "+name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "/control/v1/tenants", nil)
			r.Header.Set("Authorization", "Bearer "+c.raw)
			h.ServeHTTP(rec, r)
			if rec.Code != c.want {
				t.Errorf("got status %d, want %d", rec.Code, c.want)
			}
			if c.want == 204 && rec.Header().Get("Subject") != "ops|alice" {
				t.Errorf("got subject %q, want ops|alice", rec.Header().Get("Subject"))
			}
		})
	}
}

func TestMiddleware(t *testing.T) {
	is := authntest.New(t)
	v := verifier(t, is)
	tenant := uuid.New()
	var got reqctx.Claims
	h := v.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = reqctx.ClaimsFrom(r.Context())
		w.WriteHeader(204)
	}))
	for header, want := range map[string]int{
		"":               401,
		"Basic abc":      401,
		"Bearer ":        401,
		"Bearer garbage": 401,
		"bearer " + is.Token(t, tenant, "s", nil): 204,
	} {
		t.Run(fmt.Sprintf("%.20q", header), func(t *testing.T) {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "/v1/operations", nil)
			if header != "" {
				r.Header.Set("Authorization", header)
			}
			h.ServeHTTP(rec, r)
			if rec.Code != want {
				t.Errorf("got status %d, want %d", rec.Code, want)
			}
			if ct := rec.Header().Get("Content-Type"); want == 401 && ct != "application/problem+json" {
				t.Errorf("got content type %q, want application/problem+json", ct)
			}
		})
	}
	if got.TenantID != tenant {
		t.Fatal("claims not passed on")
	}
}

// TestOperatorRoles: the roles claim as an array or a space-separated
// string, absent roles meaning every role in development, and the admin
// role implying all.
func TestOperatorRoles(t *testing.T) {
	is := authntest.New(t)
	v, err := authn.New(context.Background(), authn.Development, []authn.Issuer{is.Config()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for name, c := range map[string]struct {
		claim any
		roles []string
	}{
		"array":  {[]any{"erp:read", "erp:operate"}, []string{"erp:read", "erp:operate"}},
		"scope":  {"erp:read erp:provision", []string{"erp:read", "erp:provision"}},
		"empty":  {[]any{}, []string{}},
		"blanks": {[]any{"erp:read", ""}, []string{"erp:read"}},
		// A claim that is present but malformed holds no role: only an
		// absent claim means every role (ADR-0027).
		"number": {42, []string{}},
		"object": {map[string]any{"erp:admin": true}, []string{}},
		"bool":   {true, []string{}},
		"null":   {nil, []string{}},
		"mixed":  {[]any{"erp:read", 7}, []string{}},
		"nested": {[]any{[]any{"erp:admin"}}, []string{}},
	} {
		t.Run(name, func(t *testing.T) {
			claims, err := v.VerifyOperator(ctx, is.Token(t, uuid.Nil, "ops", map[string]any{"tenant_id": nil, "roles": c.claim}))
			if err != nil {
				t.Fatalf("verify: %v", err)
			}
			if got, want := strings.Join(claims.Roles, ","), strings.Join(c.roles, ","); got != want {
				t.Errorf("got roles %s, want %s", got, want)
			}
			if got, want := claims.Roles == nil, c.roles == nil; got != want {
				t.Errorf("got roles nil %v, want nil %v", got, want)
			}
			if got, want := authn.OperatorAllowed(claims, authn.RoleAdmin), slices.Contains(c.roles, authn.RoleAdmin); got != want {
				t.Errorf("got admin allowed %v, want %v", got, want)
			}
		})
	}
	claims, err := v.VerifyOperator(ctx, is.Token(t, uuid.Nil, "ops", map[string]any{"tenant_id": nil}))
	if err != nil {
		t.Fatalf("no roles claim: %v", err)
	}
	if claims.Roles != nil {
		t.Errorf("no roles claim: got roles %v, want nil", claims.Roles)
	}
	if !authn.OperatorAllowed(claims, authn.RoleOperate) {
		t.Errorf("no roles claim: got operate refused, want allowed")
	}
	for roles, allowed := range map[string]map[string]bool{
		"":                     {authn.RoleRead: false, authn.RoleOperate: false},
		"erp:read":             {authn.RoleRead: true, authn.RoleOperate: false, authn.RoleProvision: false},
		"erp:provision":        {authn.RoleProvision: true, authn.RoleOperate: false},
		"erp:admin":            {authn.RoleRead: true, authn.RoleProvision: true, authn.RoleOperate: true},
		"erp:read erp:operate": {authn.RoleRead: true, authn.RoleOperate: true, authn.RoleProvision: false},
	} {
		t.Run("roles="+roles, func(t *testing.T) {
			for role, want := range allowed {
				if got := authn.OperatorAllowed(reqctx.Claims{Roles: strings.Fields(roles)}, role); got != want {
					t.Errorf("%s: got %v, want %v", role, got, want)
				}
			}
		})
	}
	// A custom roles claim name per issuer.
	custom := is.Config()
	custom.RolesClaim = "groups"
	v2, _ := authn.New(ctx, authn.Development, []authn.Issuer{custom})
	if claims, err := v2.VerifyOperator(ctx, is.Token(t, uuid.Nil, "ops", map[string]any{"tenant_id": nil, "groups": []any{"erp:admin"}})); err != nil || len(claims.Roles) != 1 {
		t.Fatalf("custom claim: %v %v", claims.Roles, err)
	}
}

// TestOperatorWithoutRolesClaim: an operator token without a roles claim
// holds every role only in development; in production, and in an
// environment the verifier does not know, it holds none, so every
// control-plane operation is refused (ADR-0027, amended for F7).
func TestOperatorWithoutRolesClaim(t *testing.T) {
	is := authntest.New(t)
	ctx := context.Background()
	for _, c := range []struct {
		env     authn.Environment
		allowed bool
	}{
		{authn.Development, true},
		{authn.Production, false},
		{"", false},
		{"staging", false},
	} {
		t.Run(string(c.env), func(t *testing.T) {
			v, err := authn.New(ctx, c.env, []authn.Issuer{is.Config()})
			if err != nil {
				t.Fatal(err)
			}
			claims, err := v.VerifyOperator(ctx, is.Token(t, uuid.Nil, "ops", map[string]any{"tenant_id": nil}))
			if err != nil {
				t.Fatalf("verify: %v", err)
			}
			for _, role := range []string{authn.RoleRead, authn.RoleProvision, authn.RoleOperate, authn.RoleAdmin} {
				if got := authn.OperatorAllowed(claims, role); got != c.allowed {
					t.Errorf("%s: got allowed %v, want %v", role, got, c.allowed)
				}
			}
		})
	}
}
