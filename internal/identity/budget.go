package identity

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// AgentBudget is an agent's monthly budget, in minor units of its
// currency, as its profile sets it; ok is false for an agent with no
// budget or no profile. The model gateway holds the agent's calls against
// it (ADR-0036), reading it through this function rather than identity's
// tables.
func AgentBudget(ctx context.Context, tx pgx.Tx, agent uuid.UUID) (amount money.Minor, currency string, ok bool, err error) {
	var budget *money.Minor
	var cur *string
	err = tx.QueryRow(ctx, `SELECT monthly_budget, budget_currency FROM identity.agent_profile WHERE principal_id = $1`, agent).Scan(&budget, &cur)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, "", false, nil
	case err != nil:
		return 0, "", false, fmt.Errorf("identity: read the budget of agent %s: %w", agent, err)
	case budget == nil:
		return 0, "", false, nil
	}
	return *budget, *cur, true, nil // the profile's CHECK sets both or neither
}
