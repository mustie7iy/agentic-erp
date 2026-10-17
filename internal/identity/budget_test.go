package identity_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/money"
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
	pgtest.Exec(t, `INSERT INTO identity.agent_profile (tenant_id, principal_id, autonomy_level, model, monthly_budget, budget_currency, description, created_by, updated_by)
		VALUES ($1, $2, 1, 'erp-default', 5000, 'USD', 'test', $4, $4), ($1, $3, 1, 'erp-default', NULL, NULL, 'test', $4, $4)`, ten.ID, ten.Agent.ID, other, ten.Human.ID)
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
