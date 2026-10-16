package uow_test

import (
	"context"
	"encoding/json"
	"io"
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

// TestMultiFactorForPeople: where the tenant requires multi-factor
// authentication, a person's token must prove it by the provider's acr or
// amr values, or is answered with the RFC 9470 challenge; services and
// agents are not asked (ADR-0031).
func TestMultiFactorForPeople(t *testing.T) {
	is := authntest.NewSeparate(t)
	ten := pgtest.NewTenant(t)
	pgtest.Exec(t, `UPDATE identity.principal SET external_issuer = $2, version = version + 1 WHERE tenant_id = $1`, ten.ID, is.URL)
	reg := authn.Registration{TenantID: ten.ID, Issuer: is.URL, Audience: authntest.Audience, MFARequired: true, ACRValues: []string{"urn:loa:2"}, AMRValues: []string{"mfa"}}
	v, err := authn.NewRegistered(context.Background(), authn.Development, oneProvider{reg}, &authn.Providers{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	mw := &uow.Middleware{Pool: pgtest.Pool(t), Role: "erp_app", Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: time.Now}
	handler := v.Middleware(mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := reqctx.PrincipalFrom(r.Context())
		_ = json.NewEncoder(w).Encode(map[string]string{"kind": p.Kind}) // the status is what the test reads
	})))
	read := func(subject string, extra map[string]any) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/read", nil)
		r.Header.Set("Authorization", "Bearer "+is.Token(t, uuid.Nil, subject, extra))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		return rec
	}
	for _, c := range []struct {
		name    string
		subject string
		extra   map[string]any
		status  int
	}{
		{"a person by acr", ten.Human.Subject, map[string]any{"acr": "urn:loa:2"}, 200},
		{"a person by amr", ten.Human.Subject, map[string]any{"amr": []string{"pwd", "mfa"}}, 200},
		{"a person by password", ten.Human.Subject, map[string]any{"amr": []string{"pwd"}}, 401},
		{"a service", ten.Service.Subject, nil, 200},
		{"an agent", ten.Agent.Subject, nil, 200},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := read(c.subject, c.extra)
			if rec.Code != c.status {
				t.Fatalf("status: got %d %s, want %d", rec.Code, rec.Body, c.status)
			}
			if c.status == 401 {
				if !strings.Contains(rec.Body.String(), "identity.mfa_required") {
					t.Errorf("problem: got %s, want identity.mfa_required", rec.Body)
				}
				if h := rec.Header().Get("WWW-Authenticate"); !strings.Contains(h, "insufficient_user_authentication") {
					t.Errorf("challenge: got %q, want insufficient_user_authentication", h)
				}
			}
		})
	}
}
