package uow_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
)

// TestActing: work in the process reads as the principal it acts as
// (reqctx.WithActing, ADR-0076 §5), resolved by id in its tenant and
// required active, with no token; an agent is still asked the kill
// switch.
func TestActing(t *testing.T) {
	ten := pgtest.NewTenant(t)
	blocked := pgtest.NewTenant(t)
	pgtest.Exec(t, "UPDATE identity.principal SET status = 'blocked', version = version + 1 WHERE id = $1", blocked.Human.ID)
	off := problem.Forbidden("agents.ai_disabled", "off")
	for name, c := range map[string]struct {
		p      reqctx.Principal
		status int
	}{
		"a person":                 {reqctx.Principal{ID: ten.Human.ID, TenantID: ten.ID}, 200},
		"an agent, kill switch on": {reqctx.Principal{ID: ten.Agent.ID, TenantID: ten.ID}, 403},
		"a person blocked":         {reqctx.Principal{ID: blocked.Human.ID, TenantID: blocked.ID}, 403},
		"another tenant's person":  {reqctx.Principal{ID: blocked.Human.ID, TenantID: ten.ID}, 403},
		"an unknown tenant":        {reqctx.Principal{ID: ten.Human.ID, TenantID: uuid.New()}, 401},
	} {
		t.Run(name, func(t *testing.T) {
			var seen reqctx.Principal
			mw := &uow.Middleware{Pool: pgtest.Pool(t), Role: "erp_app", Log: slog.New(slog.DiscardHandler), Now: time.Now,
				Agents: func(context.Context, pgx.Tx) error { return off }}
			h := mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen, _ = reqctx.PrincipalFrom(r.Context())
				w.WriteHeader(http.StatusOK)
			}))
			r := httptest.NewRequest("GET", "/read", nil)
			r = r.WithContext(reqctx.WithActing(r.Context(), c.p))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			if rec.Code != c.status {
				t.Fatalf("status: got %d %s, want %d", rec.Code, rec.Body, c.status)
			}
			if c.status == 200 && (seen.ID != c.p.ID || seen.Kind != reqctx.KindHuman || seen.TenantID != ten.ID) {
				t.Errorf("principal: got %+v, want the person", seen)
			}
		})
	}
	// Neither a token nor a principal acted as: 401.
	mw := &uow.Middleware{Pool: pgtest.Pool(t), Role: "erp_app", Log: slog.New(slog.DiscardHandler), Now: time.Now}
	rec := httptest.NewRecorder()
	mw.Wrap(http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest("GET", "/read", nil))
	if rec.Code != 401 {
		t.Errorf("no caller: got %d, want 401", rec.Code)
	}
}
