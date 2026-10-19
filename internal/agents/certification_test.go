package agents_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/agents"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/platform/autonomy"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestRecordCertification: an operator loads a report's certification of
// a known agent type for level 1 or 2 on a model, once (ADR-0039).
func TestRecordCertification(t *testing.T) {
	const model = "test-record-1" // a run before may have loaded it: the table keeps what is loaded
	hash := strings.Repeat("ab", 32)
	record := func(c agents.Certification) error {
		return pgtest.Pool(t).InTx(context.Background(), database.Scope{}, func(ctx context.Context, tx pgx.Tx) error {
			return agents.RecordCertification(ctx, tx, "ops", c)
		})
	}
	good := agents.Certification{AgentType: "test", Model: model, Level: 2, SuiteVersion: 1, ReportSHA256: hash}
	for name, c := range map[string]agents.Certification{
		"capture is no agent type": {AgentType: "capture", Model: model, Level: 1, SuiteVersion: 1, ReportSHA256: hash},
		"not a model id":           {AgentType: "test", Model: "Opus 5", Level: 1, SuiteVersion: 1, ReportSHA256: hash},
		"level 0":                  {AgentType: "test", Model: model, Level: 0, SuiteVersion: 1, ReportSHA256: hash},
		"level 3":                  {AgentType: "test", Model: model, Level: 3, SuiteVersion: 1, ReportSHA256: hash},
		"not a hash":               {AgentType: "test", Model: model, Level: 1, SuiteVersion: 1, ReportSHA256: "abc"},
		"no suite version":         {AgentType: "test", Model: model, Level: 1, SuiteVersion: 0, ReportSHA256: hash},
	} {
		t.Run(name, func(t *testing.T) {
			if err := record(c); err == nil {
				t.Error("got nil error, want the certification refused")
			}
		})
	}
	if err := record(good); err != nil && !strings.Contains(err.Error(), "loaded already") {
		t.Fatal(err)
	}
	if err := record(good); err == nil || !strings.Contains(err.Error(), "loaded already") {
		t.Errorf("the same report again: got %v, want loaded already", err)
	}
	var level int16
	var typeCertified bool
	if err := pgtest.Pool(t).InTx(context.Background(), database.Scope{}, func(ctx context.Context, tx pgx.Tx) (err error) {
		level, typeCertified, err = agents.CertifiedLevel(ctx, tx, "test", model)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if level != 2 || !typeCertified {
		t.Errorf("certified: got level %d (type certified %v), want 2 (true)", level, typeCertified)
	}
}

// TestUncertifiedRoute: a route may not move an alias to a model on which
// one of the tenant's agents on it, above level 0, is not certified for
// its level; an agent with no type, from before certification, holds no
// route back; a cell that runs evaluations waives it (ADR-0039).
func TestUncertifiedRoute(t *testing.T) {
	two, one := pgtest.Certify(t, "ap_exceptions", 2), pgtest.Certify(t, "ap_exceptions", 1)
	ten := pgtest.NewTenant(t)
	pgtest.Route(t, ten.ID, "erp-default", two)
	typed := func(level int) {
		pgtest.Exec(t, `UPDATE identity.agent_profile SET agent_type = 'ap_exceptions', autonomy_level = $2, version = version + 1 WHERE principal_id = $1`,
			ten.Agent.ID, level)
	}
	typed(2)
	// An agent from before, at level 2 with no type, on the same alias.
	untyped := uuid.New()
	pgtest.Exec(t, `INSERT INTO identity.principal (tenant_id, id, kind, display_name, status, created_by, updated_by) VALUES ($1, $2, 'agent', 'Old', 'active', $3, $3)`,
		ten.ID, untyped, ten.Human.ID)
	pgtest.Exec(t, `INSERT INTO identity.agent_profile (tenant_id, principal_id, autonomy_level, model, description, created_by, updated_by)
		VALUES ($1, $2, 2, 'erp-default', 'Old', $3, $3)`, ten.ID, untyped, ten.Human.ID)
	move := func(c agents.Certifications, model string) error {
		routes := map[string]string{"erp-default": model}
		return inTenant(t, ten, func(ctx context.Context, tx pgx.Tx) error {
			_, err := agents.UpdatePolicy(ctx, tx, ten.Human.ID, etag.IfMatch("*"), agents.PolicyChange{Routes: &routes}, c)
			return err
		})
	}
	gate := agents.Certifications{Agents: identity.AutonomousAgents}
	if got := codeOf(move(gate, one)); got != "agents.uncertified_route/uncertified" {
		t.Errorf("level 2 onto a level-1 model: got %s, want agents.uncertified_route/uncertified", got)
	}
	enabled := true
	if err := inTenant(t, ten, func(ctx context.Context, tx pgx.Tx) error {
		_, err := agents.UpdatePolicy(ctx, tx, ten.Human.ID, etag.IfMatch("*"), agents.PolicyChange{Enabled: &enabled}, gate)
		return err
	}); err != nil {
		t.Errorf("a change that moves no route: got %v, want none", err)
	}
	if err := move(agents.Certifications{Waived: true}, one); err != nil {
		t.Errorf("on an evaluation cell: got %v, want the route moved", err)
	}
	typed(1)
	if err := move(gate, two); err != nil {
		t.Errorf("level 1 onto a level-2 model: got %v, want the route moved", err)
	}
	failing := agents.Certifications{Agents: func(context.Context, pgx.Tx) ([]autonomy.Agent, error) { return nil, errors.New("down") }}
	if err := move(failing, one); err == nil || !strings.Contains(err.Error(), "down") {
		t.Errorf("the agents unread: got %v, want their error", err)
	}
}
