package identity

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/autonomy"
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

// AgentKind is what metrics call the principal (ADR-0043): its agent type,
// autonomy.Untyped for an agent without a known one, or autonomy.NotAgent
// for a person or service. The model gateway and the approval decisions
// label their metrics with it, reading it through this function rather
// than identity's tables.
func AgentKind(ctx context.Context, tx pgx.Tx, principal uuid.UUID) (string, error) {
	var kind string
	var typ *string
	err := tx.QueryRow(ctx, `SELECT p.kind, ap.agent_type FROM identity.principal p
		LEFT JOIN identity.agent_profile ap ON ap.tenant_id = p.tenant_id AND ap.principal_id = p.id WHERE p.id = $1`, principal).Scan(&kind, &typ)
	if err != nil {
		return "", fmt.Errorf("identity: read the agent kind of %s: %w", principal, err)
	}
	if kind != "agent" {
		return autonomy.NotAgent, nil
	}
	if typ == nil {
		return autonomy.Untyped, nil
	}
	return autonomy.Kind(*typ), nil
}
