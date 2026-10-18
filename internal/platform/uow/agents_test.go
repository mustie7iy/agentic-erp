package uow_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn/authntest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
)

// TestAgentsAdmitted: Agents decides an agent's requests, in the request's
// transaction and before the handler runs; people and services are not
// asked (ADR-0036).
func TestAgentsAdmitted(t *testing.T) {
	is := authntest.New(t)
	v, err := authn.New(context.Background(), authn.Development, []authn.Issuer{is.Config()})
	if err != nil {
		t.Fatal(err)
	}
	ten := pgtest.NewTenant(t)
	refused := problem.Forbidden("agents.ai_disabled", "off")
	for _, c := range []struct {
		name    string
		subject string
		gate    error
		status  int
	}{
		{"an agent, admitted", ten.Agent.Subject, nil, 200},
		{"an agent, refused", ten.Agent.Subject, refused, 403},
		{"a person, not asked", ten.Human.Subject, refused, 200},
		{"a service, not asked", ten.Service.Subject, refused, 200},
	} {
		t.Run(c.name, func(t *testing.T) {
			var asked, served bool
			mw := &uow.Middleware{Pool: pgtest.Pool(t), Role: "erp_app", Log: slog.New(slog.DiscardHandler), Now: time.Now,
				Agents: func(_ context.Context, tx pgx.Tx) error {
					asked = tx != nil
					return c.gate
				}}
			handler := v.Middleware(mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				served = true
				w.WriteHeader(http.StatusOK)
			})))
			r := httptest.NewRequest("GET", "/read", nil)
			r.Header.Set("Authorization", "Bearer "+is.Token(t, ten.ID, c.subject, nil))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, r)
			if rec.Code != c.status {
				t.Fatalf("status: got %d %s, want %d", rec.Code, rec.Body, c.status)
			}
			if want := c.subject == ten.Agent.Subject; asked != want {
				t.Errorf("asked in a transaction: got %v, want %v", asked, want)
			}
			if want := c.status == 200; served != want {
				t.Errorf("served: got %v, want %v", served, want)
			}
		})
	}
}
