package agents

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/internal/platform/autonomy"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Certification is what an evaluation run certified (ADR-0039): an agent
// type's highest autonomy level on a concrete model, with the suite's
// version and the run's report's SHA-256, as an operator loads it.
type Certification struct {
	AgentType    string
	Model        string
	Level        int16
	SuiteVersion int
	ReportSHA256 string
}

var reportHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

// RecordCertification records c in tx with who loaded it. A loaded
// certification stands: a report is loaded once, and the latest load for
// a type and model is its certification. tx runs as a role that may write
// reference data (erp-server certify load).
func RecordCertification(ctx context.Context, tx pgx.Tx, by string, c Certification) error {
	switch {
	case !autonomy.KnownType(c.AgentType):
		return fmt.Errorf("agents: certify %q: not an agent type (%v)", c.AgentType, autonomy.Types)
	case !modelID.MatchString(c.Model):
		return fmt.Errorf("agents: certify %s: %q is not a model id", c.AgentType, c.Model)
	case c.Level < 1 || c.Level > 2:
		return fmt.Errorf("agents: certify %s on %s: level %d is not 1 or 2", c.AgentType, c.Model, c.Level)
	case !reportHash.MatchString(c.ReportSHA256):
		return fmt.Errorf("agents: certify %s on %s: the report's hash is not a SHA-256", c.AgentType, c.Model)
	}
	_, err := tx.Exec(ctx, `INSERT INTO ref.agent_certification (agent_type, model, level, suite_version, report_sha256, certified_by)
		VALUES ($1, $2, $3, $4, $5, $6)`, c.AgentType, c.Model, c.Level, c.SuiteVersion, c.ReportSHA256, by)
	var pg *pgconn.PgError
	switch {
	case errors.As(err, &pg) && pg.Code == "23505":
		return fmt.Errorf("agents: certify %s on %s: this report is loaded already", c.AgentType, c.Model)
	case err != nil:
		return fmt.Errorf("agents: certify %s on %s: %w", c.AgentType, c.Model, err)
	}
	return nil
}

// CertifiedLevel is the level agentType is certified for on model (0:
// none), by its latest load, and whether the type has any certification:
// an agent of a type never certified keeps the level it has until its
// type's first run (ADR-0039). An agent with no type is certified for
// nothing.
func CertifiedLevel(ctx context.Context, tx pgx.Tx, agentType, model string) (level int16, typeCertified bool, err error) {
	if agentType == "" {
		return 0, false, nil
	}
	err = tx.QueryRow(ctx, `SELECT coalesce((SELECT level FROM ref.agent_certification WHERE agent_type = $1 AND model = $2
			ORDER BY certified_at DESC, report_sha256 LIMIT 1), 0),
		EXISTS (SELECT FROM ref.agent_certification WHERE agent_type = $1)`, agentType, model).Scan(&level, &typeCertified)
	if err != nil {
		return 0, false, fmt.Errorf("agents: read the certification of %s on %s: %w", agentType, model, err)
	}
	return level, typeCertified, nil
}

// AutonomousAgents lists the tenant's agents above level 0 in tx
// (identity.AutonomousAgents), which agents reads through this function
// rather than identity's tables.
type AutonomousAgents func(ctx context.Context, tx pgx.Tx) ([]autonomy.Agent, error)

// Certifications bounds agents' autonomy by their type's certification
// on the model their alias routes to (ADR-0039).
type Certifications struct {
	Agents AutonomousAgents
	// Waived lifts the bound on a cell that runs live evaluations
	// (ERP_EVAL_CELL, refused in production): a type's first run needs
	// agents at the levels its suite tests.
	Waived bool
}

// Certified is the level agentType is certified for on the model alias
// routes to under the tenant's policy, and whether the type has any
// certification (identity's Certifications). The policy's row is held FOR
// SHARE, so that a change of its routes, which reads the agents' levels,
// waits for the level being set, or this for the route.
func (c Certifications) Certified(ctx context.Context, tx pgx.Tx, agentType, alias string) (level int16, typeCertified bool, err error) {
	if c.Waived {
		return autonomy.MaxLevel, true, nil
	}
	p, err := heldPolicy(ctx, tx, `FOR SHARE`)
	if err != nil {
		return 0, false, err
	}
	model, _ := p.Route(alias, DefaultRoutes) // identity accepts only the platform's aliases, which all route
	return CertifiedLevel(ctx, tx, agentType, model)
}

// checkRoutes refuses moving an alias to a model on which one of the
// tenant's agents on it, above level 0, is not certified for its level;
// an agent of a type never certified keeps its level until the type's
// first run. Routes that stay leave the agents on them as they are.
func (c Certifications) checkRoutes(ctx context.Context, tx pgx.Tx, before, after Policy) error {
	if c.Waived {
		return nil
	}
	moved := map[string]string{}
	for alias := range DefaultRoutes {
		from, _ := before.Route(alias, DefaultRoutes) // every alias has a platform route
		if to, _ := after.Route(alias, DefaultRoutes); to != from {
			moved[alias] = to
		}
	}
	if len(moved) == 0 {
		return nil
	}
	agents, err := c.Agents(ctx, tx)
	if err != nil {
		return fmt.Errorf("agents: the agents a route change moves: %w", err)
	}
	for _, a := range agents {
		model, ok := moved[a.Alias]
		if !ok {
			continue
		}
		level, typeCertified, err := CertifiedLevel(ctx, tx, a.Type, model)
		if err != nil {
			return err
		}
		if a.Level > level && typeCertified {
			detail := fmt.Sprintf("The alias %s would route to %s, on which agent %s (%s) is certified for level %d, below its level %d: lower the agent's level or certify its type on that model first.",
				a.Alias, model, a.ID, a.Type, level, a.Level)
			return problem.Unprocessable("agents.uncertified_route", detail, problem.FieldError{Field: "routes", Code: "uncertified", Message: detail})
		}
	}
	return nil
}
