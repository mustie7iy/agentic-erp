package identity_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/autonomy"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestAgentBudget: an agent's monthly budget is its profile's, in its
// currency; an agent without one, or without a profile, has none (the
// model gateway's caps, ADR-0036).
func TestAgentBudget(t *testing.T) {
	ten := pgtest.NewTenant(t)
	pool := pgtest.Pool(t)
	other := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO identity.principal (tenant_id, id, kind, display_name, status, created_by, updated_by) VALUES ($1, $2, 'agent', 'Unbudgeted', 'active', $3, $3)`,
		ten.ID, other, ten.Human.ID)
	pgtest.Exec(t, `UPDATE identity.agent_profile SET monthly_budget = 5000, budget_currency = 'USD', version = version + 1 WHERE tenant_id = $1 AND principal_id = $2`,
		ten.ID, ten.Agent.ID)
	pgtest.Exec(t, `INSERT INTO identity.agent_profile (tenant_id, principal_id, autonomy_level, model, description, created_by, updated_by)
		VALUES ($1, $2, 1, 'erp-default', 'test', $3, $3)`, ten.ID, other, ten.Human.ID)
	read := func(t *testing.T, agent uuid.UUID) (money.Minor, string, bool, error) {
		t.Helper()
		ctx := context.Background()
		tx, err := pool.Begin(ctx, database.Scope{Role: "erp_app", TenantID: ten.ID, ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }() // read-only
		return identity.AgentBudget(ctx, tx, agent)
	}
	t.Run("an agent with a budget", func(t *testing.T) {
		amount, cur, ok, err := read(t, ten.Agent.ID)
		if err != nil || !ok {
			t.Fatalf("got ok %v, err %v; want a budget", ok, err)
		}
		if amount != 5000 || cur != "USD" {
			t.Errorf("budget: got %d %s, want 5000 USD", amount, cur)
		}
	})
	for name, agent := range map[string]uuid.UUID{"a profile without a budget": other, "no profile": ten.Service.ID} {
		t.Run(name, func(t *testing.T) {
			if _, _, ok, err := read(t, agent); err != nil || ok {
				t.Errorf("got ok %v, err %v; want no budget", ok, err)
			}
		})
	}
	t.Run("the database failing", func(t *testing.T) {
		ctx := context.Background()
		tx, err := pool.Begin(ctx, database.Scope{Role: "erp_app", TenantID: ten.ID, ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }() // read-only
		var ftx pgx.Tx = &pgtest.FailingTx{Tx: tx, FailAt: 1}
		if _, _, _, err := identity.AgentBudget(ctx, ftx, ten.Agent.ID); !errors.Is(err, pgtest.ErrInjected) {
			t.Errorf("got %v, want the injected error", err)
		}
	})
}

// TestAgentKind: an agent's kind is its type, untyped without one, and a
// person or service is not an agent (metric labels, ADR-0043).
func TestAgentKind(t *testing.T) {
	ten := pgtest.NewTenant(t)
	pool := pgtest.Pool(t)
	typed := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO identity.principal (tenant_id, id, kind, display_name, status, created_by, updated_by) VALUES ($1, $2, 'agent', 'Typed', 'active', $3, $3)`,
		ten.ID, typed, ten.Human.ID)
	pgtest.Exec(t, `INSERT INTO identity.agent_profile (tenant_id, principal_id, autonomy_level, agent_type, model, description, created_by, updated_by)
		VALUES ($1, $2, 1, 'ap_exceptions', 'erp-default', 'test', $3, $3)`, ten.ID, typed, ten.Human.ID)
	pgtest.Exec(t, `UPDATE identity.agent_profile SET agent_type = NULL, version = version + 1 WHERE tenant_id = $1 AND principal_id = $2`, ten.ID, ten.Agent.ID)
	ctx := context.Background()
	tx, err := pool.Begin(ctx, database.Scope{Role: "erp_app", TenantID: ten.ID, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // read-only
	for name, c := range map[string]struct {
		id   uuid.UUID
		want string
	}{"typed": {typed, "ap_exceptions"}, "untyped": {ten.Agent.ID, autonomy.Untyped}, "a person": {ten.Human.ID, autonomy.NotAgent}} {
		t.Run(name, func(t *testing.T) {
			if got, err := identity.AgentKind(ctx, tx, c.id); err != nil || got != c.want {
				t.Errorf("kind: got %q, %v; want %q", got, err, c.want)
			}
		})
	}
	t.Run("unknown principal", func(t *testing.T) {
		if _, err := identity.AgentKind(ctx, tx, uuid.Must(uuid.NewV7())); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("got %v, want no rows", err)
		}
	})
}
