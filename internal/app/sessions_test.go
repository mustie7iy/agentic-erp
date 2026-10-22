package app_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn/authntest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// oneProvider is a registry holding one registration.
type oneProvider struct{ reg authn.Registration }

func (o oneProvider) Resolve(_ context.Context, issuer string, audiences []string) (authn.Registration, error) {
	for _, a := range audiences {
		if issuer == o.reg.Issuer && a == o.reg.Audience {
			return o.reg, nil
		}
	}
	return authn.Registration{}, authn.ErrUnknownIssuer
}

// TestEndCurrentSession is G9 (ADR-0052): a person, without any role,
// ends their own session at sign-out, and every token of that session is
// refused from then on, a refreshed one too; a token whose provider keeps
// no session ends nothing; an agent may not.
func TestEndCurrentSession(t *testing.T) {
	is := authntest.NewSeparate(t)
	ten := pgtest.NewTenant(t, "tenancy.org.read")
	pgtest.Exec(t, `UPDATE identity.principal SET external_issuer = $2, version = version + 1 WHERE tenant_id = $1`, ten.ID, is.URL)
	reg := authn.Registration{TenantID: ten.ID, Issuer: is.URL, Audience: authntest.Audience, SessionMaxAge: time.Hour, IdleTimeout: 30 * time.Minute}
	api := apitest.NewWith(t, func(cfg *app.Config) {
		v, err := authn.NewRegistered(context.Background(), authn.Development, oneProvider{reg}, &authn.Providers{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Verifier = v
	})
	token := func(subject, sid string) http.Header {
		return http.Header{"Authorization": {"Bearer " + is.Token(t, ten.ID, subject, map[string]any{"sid": sid, "auth_time": time.Now().Unix()})}}
	}
	end := func(h http.Header) (int, map[string]any) {
		rec := api.Do(apitest.Req{Method: "POST", Path: "/v1/sessions/current:end", Key: apitest.NewKey(), Header: h})
		return rec.Code, apitest.JSON(t, rec)
	}
	read := func(h http.Header) (int, string) {
		rec := api.Do(apitest.Req{Method: "GET", Path: "/v1/legal-entities", Header: h})
		return rec.Code, rec.Body.String()
	}
	sid := "sid-" + uuid.NewString()
	if code, body := read(token(ten.Human.Subject, sid)); code != 200 {
		t.Fatalf("before: got %d %s, want 200", code, body)
	}
	code, out := end(token(ten.Human.Subject, sid))
	if code != 200 || out["ended"] != true || out["ended_at"] == nil {
		t.Fatalf("end: got %d %v, want 200 ended", code, out)
	}
	if code, body := read(token(ten.Human.Subject, sid)); code != 401 || !strings.Contains(body, "identity.session_revoked") {
		t.Errorf("after, a refreshed token: got %d %s, want 401 identity.session_revoked", code, body)
	}
	if code, body := read(token(ten.Human.Subject, "sid-"+uuid.NewString())); code != 200 {
		t.Errorf("a new sign-in: got %d %s, want 200", code, body)
	}
	t.Run("no session kept", func(t *testing.T) {
		static := apitest.New(t)
		plain := pgtest.NewTenant(t)
		rec := static.Do(apitest.Req{Method: "POST", Path: "/v1/sessions/current:end", Tenant: plain.ID, As: plain.Human, Key: apitest.NewKey()})
		if out := apitest.JSON(t, rec); rec.Code != 200 || out["ended"] != false {
			t.Errorf("got %d %v, want 200 not ended", rec.Code, out)
		}
	})
	t.Run("an agent", func(t *testing.T) {
		if code, out := end(token(ten.Agent.Subject, "")); code != 403 {
			t.Errorf("got %d %v, want 403", code, out)
		}
	})
}
