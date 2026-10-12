package authn_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
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
	v, err := authn.New(context.Background(), cfg)
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
	if c.TenantID != tenant || c.Subject != "human|1" || c.Issuer != is.URL {
		t.Fatalf("%+v", c)
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
		_, err := v.Verify(context.Background(), raw)
		if p := problem.From(err); err == nil || p.Status != http.StatusUnauthorized {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestNewValidatesConfiguration(t *testing.T) {
	ctx := context.Background()
	if _, err := authn.New(ctx, nil); err == nil {
		t.Fatal("no issuers accepted")
	}
	is := authntest.New(t)
	if _, err := authn.New(ctx, []authn.Issuer{{URL: is.URL}}); err == nil {
		t.Fatal("missing audience accepted")
	}
	if _, err := authn.New(ctx, []authn.Issuer{{URL: "http://127.0.0.1:1", Audience: "a"}}); err == nil {
		t.Fatal("unreachable issuer accepted")
	}
	custom, err := authn.New(ctx, []authn.Issuer{{URL: is.URL, Audience: authntest.Audience, TenantClaim: "https://erp/tenant"}})
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
	if err != nil || c.Subject != "ops|alice" || c.Issuer != is.URL || c.TenantID != uuid.Nil {
		t.Fatalf("%+v %v", c, err)
	}
	for name, raw := range map[string]string{
		"names a tenant": is.Token(t, uuid.New(), "ops|alice", nil),
		"untrusted":      other.Token(t, uuid.Nil, "ops|alice", map[string]any{"tenant_id": nil}),
		"malformed":      "abc",
	} {
		if _, err := v.VerifyOperator(ctx, raw); problem.From(err).Status != http.StatusUnauthorized {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := v.Verify(ctx, operator); problem.From(err).Status != http.StatusUnauthorized {
		t.Fatalf("operator token as a tenant's: %v", err)
	}
	h := v.OperatorMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _ := reqctx.ClaimsFrom(r.Context())
		w.Header().Set("Subject", c.Subject)
		w.WriteHeader(204)
	}))
	for raw, want := range map[string]int{operator: 204, is.Token(t, uuid.New(), "s", nil): 401} {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/control/v1/tenants", nil)
		r.Header.Set("Authorization", "Bearer "+raw)
		h.ServeHTTP(rec, r)
		if rec.Code != want || (want == 204 && rec.Header().Get("Subject") != "ops|alice") {
			t.Errorf("%d: %d", want, rec.Code)
		}
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
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/v1/operations", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		h.ServeHTTP(rec, r)
		if rec.Code != want {
			t.Errorf("%q: %d", header, rec.Code)
		}
		if want == 401 && rec.Header().Get("Content-Type") != "application/problem+json" {
			t.Errorf("%q: not a problem response", header)
		}
	}
	if got.TenantID != tenant {
		t.Fatal("claims not passed on")
	}
}

// TestOperatorRoles: the roles claim as an array or a space-separated
// string, absent roles meaning every role, and the admin role implying all.
func TestOperatorRoles(t *testing.T) {
	is := authntest.New(t)
	v, err := authn.New(context.Background(), []authn.Issuer{is.Config()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for name, c := range map[string]struct {
		claim any
		roles []string
	}{
		"array":   {[]any{"erp:read", "erp:operate"}, []string{"erp:read", "erp:operate"}},
		"scope":   {"erp:read erp:provision", []string{"erp:read", "erp:provision"}},
		"empty":   {[]any{}, []string{}},
		"garbage": {42, nil},
		"mixed":   {[]any{"erp:read", 7, ""}, []string{"erp:read"}},
	} {
		claims, err := v.VerifyOperator(ctx, is.Token(t, uuid.Nil, "ops", map[string]any{"tenant_id": nil, "roles": c.claim}))
		if err != nil || strings.Join(claims.Roles, ",") != strings.Join(c.roles, ",") || (claims.Roles == nil) != (c.roles == nil) {
			t.Errorf("%s: %v %v", name, claims.Roles, err)
		}
	}
	claims, err := v.VerifyOperator(ctx, is.Token(t, uuid.Nil, "ops", map[string]any{"tenant_id": nil}))
	if err != nil || claims.Roles != nil || !authn.OperatorAllowed(claims, authn.RoleOperate) {
		t.Fatalf("no roles claim: %v %v", claims.Roles, err)
	}
	for roles, allowed := range map[string]map[string]bool{
		"":                     {authn.RoleRead: false, authn.RoleOperate: false},
		"erp:read":             {authn.RoleRead: true, authn.RoleOperate: false, authn.RoleProvision: false},
		"erp:provision":        {authn.RoleProvision: true, authn.RoleOperate: false},
		"erp:admin":            {authn.RoleRead: true, authn.RoleProvision: true, authn.RoleOperate: true},
		"erp:read erp:operate": {authn.RoleRead: true, authn.RoleOperate: true, authn.RoleProvision: false},
	} {
		for role, want := range allowed {
			if got := authn.OperatorAllowed(reqctx.Claims{Roles: strings.Fields(roles)}, role); got != want {
				t.Errorf("roles %q, %s: %v", roles, role, got)
			}
		}
	}
	// A custom roles claim name per issuer.
	custom := is.Config()
	custom.RolesClaim = "groups"
	v2, _ := authn.New(ctx, []authn.Issuer{custom})
	if claims, err := v2.VerifyOperator(ctx, is.Token(t, uuid.Nil, "ops", map[string]any{"tenant_id": nil, "groups": []any{"erp:admin"}})); err != nil || len(claims.Roles) != 1 {
		t.Fatalf("custom claim: %v %v", claims.Roles, err)
	}
}
