// Package migrationagent is the migration consultant agent: it rehearses a
// migration scope through API tools, reads the quality findings and the
// reconciliation report, and explains what blocks the cutover and how to
// clean it up. It only rehearses: a rehearsal is rolled back after its
// report, and the tools refuse a run that is not one.
package migrationagent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	anthropic "github.com/anthropics/anthropic-sdk-go"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/llm"
)

// Tools are the operations the agent may call.
var Tools = []string{"getConnector", "createMigrationRun", "startMigrationRun", "getMigrationRun", "listMigrationRuns", "getOperation"}

const system = `You prepare SAP migrations. A migration run reads one scope (legal entities and a fiscal year) from a
connector, checks its quality, loads it and reconciles it with SAP. You only rehearse: create the run with
is_rehearsal true, start it, and read its report with getMigrationRun.

The report has extract counts, quality findings (errors block the load; warnings need a business decision), load
refusals, reconciliation checks with the differences behind failed ones, and SAP documents missing on the platform.

Finish with an assessment for the migration lead: whether the scope can be cut over, each blocking finding with its
likely cause and the cleansing action (who must do what, in SAP or in the configuration), and the warnings that need a
decision. Do not invent figures; quote the report.`

// Agent assesses migration scopes.
type Agent struct {
	LLM *llm.Runner
	API *apitools.Client
}

var errNotRehearsal = errors.New("this agent only rehearses: create the run with is_rehearsal true")

// rehearsalsOnly refuses runs that would stay loaded.
func rehearsalsOnly(input map[string]any) error {
	body, _ := input["body"].(map[string]any)
	if body["is_rehearsal"] != true {
		return errNotRehearsal
	}
	return nil
}

// Assess rehearses the scope and returns the agent's assessment.
func (a *Agent) Assess(ctx context.Context, connector string, legalEntities []string, fiscalYear int, mode string) (string, error) {
	s, err := apitools.StartSession(ctx, a.API, "Migration rehearsal", a.LLM.Model)
	if err != nil {
		return "", err
	}
	tools, _ := apitools.Tools(a.API, s, Tools...) // the operations are in the contract and open to agents
	tools[1].Guard(rehearsalsOnly)
	prompt := fmt.Sprintf("Rehearse the migration from connector %s of legal entities %s for fiscal year %d in %s mode, and assess it.",
		connector, strings.Join(legalEntities, ", "), fiscalYear, mode)
	msg, err := apitools.Run(ctx, a.LLM, s, tools, a.LLM.Params(system, anthropic.NewBetaTextBlock(prompt)), 15)
	if err != nil {
		return "", err
	}
	return llm.Text(msg), nil
}
