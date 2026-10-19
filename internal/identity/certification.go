package identity

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/autonomy"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Certifications is what identity needs to know of the agents context's
// certifications (agents.Certifications, ADR-0039), answered in the
// request's transaction.
type Certifications interface {
	// Certified is the level agentType is certified for on the model
	// alias routes to under the tenant's policy, and whether the type has
	// any certification at all.
	Certified(ctx context.Context, tx pgx.Tx, agentType, alias string) (level int16, typeCertified bool, err error)
}

// certified refuses a profile ap, replacing old (nil: a new agent), whose
// level its type is not certified for on the model its alias routes to.
// Only a change can be refused: a raise, or a new type or alias for an
// agent above level 0. An agent whose type has never been certified keeps
// the level it has (ADR-0036's level 2, given before certification) until
// its type's first run, but is raised by none; a type cleared or changed
// keeps nothing.
func certified(ctx context.Context, tx pgx.Tx, cert Certifications, old *AgentProfile, ap AgentProfile) error {
	if ap.AutonomyLevel == 0 || old != nil && ap.AutonomyLevel <= old.AutonomyLevel && same(old.AgentType, ap.AgentType) && old.Model == ap.Model {
		return nil
	}
	var agentType string
	if ap.AgentType != nil {
		agentType = *ap.AgentType
	}
	level, typeCertified, err := cert.Certified(ctx, tx, agentType, ap.Model)
	if err != nil {
		return fmt.Errorf("identity: certification of %q: %w", agentType, err)
	}
	// Kept only by the agent's own type, or by an agent that had none:
	// clearing or changing a type would otherwise shed the bound.
	kept := old != nil && ap.AutonomyLevel <= old.AutonomyLevel && !typeCertified && (old.AgentType == nil || same(old.AgentType, ap.AgentType))
	if ap.AutonomyLevel <= level || kept {
		return nil
	}
	detail := fmt.Sprintf("The agent's type is certified for level %d on the model its alias %s routes to, below level %d; an agent above level 0 needs a type certified for its level (ADR-0039).",
		level, ap.Model, ap.AutonomyLevel)
	if agentType == "" {
		detail = "An agent above level 0 needs a type (agent_type) certified for its level on the model its alias routes to (ADR-0039)."
	}
	return problem.Unprocessable("identity.autonomy_uncertified", detail,
		problem.FieldError{Field: "agent_profile.autonomy_level", Code: "uncertified", Message: detail})
}

// AutonomousAgents lists the tenant's agents above level 0 with their type
// and alias (agents.AutonomousAgents): a change of the AI policy's routes
// keeps them on models their type is certified on.
func AutonomousAgents(ctx context.Context, tx pgx.Tx) ([]autonomy.Agent, error) {
	rows, _ := tx.Query(ctx, `SELECT principal_id, coalesce(agent_type, ''), model, autonomy_level FROM identity.agent_profile WHERE autonomy_level > 0 ORDER BY principal_id`)
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[autonomy.Agent])
	if err != nil {
		return nil, fmt.Errorf("identity: agents above level 0: %w", err)
	}
	return out, nil
}
