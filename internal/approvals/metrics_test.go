package approvals_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/telemetry"
)

// modifier is an owning context whose objects a person changed while they
// waited, or whose answer fails.
type modifier struct {
	recorder
	err error
}

func (m modifier) Modified(context.Context, uuid.UUID, uuid.UUID) (bool, error) {
	return m.err == nil, m.err
}

// TestAgentProposalsDecided: a person's decision on an agent's request is
// counted once it commits, by the agent's kind: approved, rejected, or
// modified when its object changed before the approval; a person's own
// requests are not counted (the override rate, ADR-0043).
func TestAgentProposalsDecided(t *testing.T) {
	f := setup(t)
	var calls []string
	metrics := telemetry.NewMetrics()
	plain := approvals.Decider{Roles: identity.Directory{}, AgentKind: identity.AgentKind, Metrics: metrics,
		Handlers: map[string]approvals.Handler{approvals.Requisition: recorder{&calls}}}
	changed := plain
	changed.Handlers = map[string]approvals.Handler{approvals.Requisition: modifier{recorder: recorder{&calls}}}
	open := func(t *testing.T, as pgtest.Actor) uuid.UUID {
		t.Helper()
		var id uuid.UUID
		eur, amount := "EUR", money.Minor(100)
		if err := apitest.Committed(t, f.Tenant, as, func(ctx context.Context) (err error) {
			id, _, err = fixture.Approvals().Open(ctx, approvals.NewRequest{ObjectType: approvals.Requisition, ObjectID: uuid.New(), Title: "PR",
				Amount: &amount, Currency: &eur, LegalEntityID: f.DE})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	anyVersion := func(int32) error { return nil }
	// decide runs as the approver and then, as the unit of work would, what
	// the decision left for after its commit.
	decide := func(t *testing.T, fn func(ctx context.Context) error) error {
		t.Helper()
		var c *reqctx.Commit
		err := apitest.Committed(t, f.Tenant, f.Approver, func(ctx context.Context) error {
			ctx, c = reqctx.WithCommit(ctx)
			return fn(ctx)
		})
		if err == nil {
			c.Run()
		}
		return err
	}
	for name, fn := range map[string]func(ctx context.Context, id uuid.UUID) error{
		"approved": func(ctx context.Context, id uuid.UUID) error {
			_, err := plain.Approve(ctx, id, anyVersion, "")
			return err
		},
		"rejected": func(ctx context.Context, id uuid.UUID) error {
			_, err := plain.Reject(ctx, id, anyVersion, "no")
			return err
		},
		"modified": func(ctx context.Context, id uuid.UUID) error {
			_, err := changed.Approve(ctx, id, anyVersion, "")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			id := open(t, f.Tenant.Agent)
			if err := decide(t, func(ctx context.Context) error { return fn(ctx, id) }); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("a person's request", func(t *testing.T) {
		id := open(t, f.Tenant.Human)
		if err := decide(t, func(ctx context.Context) error { _, err := plain.Approve(ctx, id, anyVersion, ""); return err }); err != nil {
			t.Fatal(err)
		}
	})
	rec := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, want := range []string{
		`erp_agent_proposals_decided_total{agent_kind="untyped",decision="approved"} 1`,
		`erp_agent_proposals_decided_total{agent_kind="untyped",decision="rejected"} 1`,
		`erp_agent_proposals_decided_total{agent_kind="untyped",decision="modified"} 1`,
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("metrics: got %s, want %s", rec.Body, want)
		}
	}
	if strings.Contains(rec.Body.String(), `agent_kind="none"`) {
		t.Errorf("metrics: got %s, want no person's request counted", rec.Body)
	}

	kindFails, modifiedFails := plain, plain
	kindFails.AgentKind = func(context.Context, pgx.Tx, uuid.UUID) (string, error) { return "", pgtest.ErrInjected }
	modifiedFails.Handlers = map[string]approvals.Handler{approvals.Requisition: modifier{recorder: recorder{&calls}, err: pgtest.ErrInjected}}
	for name, fn := range map[string]func(ctx context.Context, id uuid.UUID) error{
		"approve, failing to read the requester's kind": func(ctx context.Context, id uuid.UUID) error {
			_, err := kindFails.Approve(ctx, id, anyVersion, "")
			return err
		},
		"reject, failing to read the requester's kind": func(ctx context.Context, id uuid.UUID) error {
			_, err := kindFails.Reject(ctx, id, anyVersion, "no")
			return err
		},
		"approve, failing to read whether it was modified": func(ctx context.Context, id uuid.UUID) error {
			_, err := modifiedFails.Approve(ctx, id, anyVersion, "")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			id := open(t, f.Tenant.Agent)
			if err := decide(t, func(ctx context.Context) error { return fn(ctx, id) }); !errors.Is(err, pgtest.ErrInjected) {
				t.Errorf("decision: got %v, want the injected error", err)
			}
		})
	}
}
