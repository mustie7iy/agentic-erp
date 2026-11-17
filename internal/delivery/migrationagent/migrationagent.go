// Package migrationagent is the migration consultant agent: it rehearses a
// migration scope through API tools (04-api-design §4.13), reads the quality
// findings and the reconciliation report, and explains what blocks the
// cutover and how to clean it up. It only rehearses: a rehearsal is rolled
// back after its report; its guards refuse to create a run that is not
// one and to start any run but a rehearsal its session created, and the
// API refuses an agent a run that is not a rehearsal (ADR-0010). Its
// session and autonomy are ADR-0036's, its evaluation suite evals/migration
// (ADR-0039).
package migrationagent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	anthropic "github.com/anthropics/anthropic-sdk-go"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/llm"
)

// Tools are the operations the agent may call.
var Tools = []string{"getConnector", Creator, Starter, "getMigrationRun", "listMigrationRuns", "getOperation"}

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

// The operations of Tools that write: a run's creation and its start.
const (
	Creator = "createMigrationRun"
	Starter = "startMigrationRun"
)

var (
	errNotRehearsal = errors.New("this agent only rehearses: create the run with is_rehearsal true")
	// errNotCreated refuses a start of a run the session did not create as
	// a rehearsal: a run created otherwise would stay loaded.
	errNotCreated = errors.New("this agent starts only the rehearsals it created in this session")
)

// run is what one session's guards hold its writes to: the rehearsals it
// created, which alone it may start. Its writes run each call whole under
// mu (apitools Exclusive): the runner executes one turn's tool calls
// concurrently.
type run struct {
	mu         sync.Mutex
	rehearsals map[string]bool // ids, in lower case
}

// hold puts the run's guards on the writes and its observer on the
// creation.
func (r *run) hold(tools []*apitools.Tool) {
	for _, t := range tools {
		switch t.Name() {
		case Creator:
			t.Guard(rehearsalsOnly)
			t.Observe(r.created)
			t.Exclusive(&r.mu)
		case Starter:
			t.Guard(r.start)
			t.Exclusive(&r.mu)
		}
	}
}

// rehearsalsOnly refuses runs that would stay loaded.
func rehearsalsOnly(input map[string]any) error {
	body, _ := input["body"].(map[string]any) // no body: refused below
	if body["is_rehearsal"] != true {
		return errNotRehearsal
	}
	return nil
}

// created keeps a rehearsal the API created.
func (r *run) created(resp apitools.Response) {
	var mr struct {
		ID          string `json:"id"`
		IsRehearsal bool   `json:"is_rehearsal"`
	}
	if resp.OK() && resp.Decode(&mr) == nil && mr.IsRehearsal {
		r.rehearsals[strings.ToLower(mr.ID)] = true
	}
}

// start refuses a start of a run that is not a rehearsal the session
// created.
func (r *run) start(input map[string]any) error {
	if !r.rehearsals[strings.ToLower(fmt.Sprint(input["id"]))] {
		return errNotCreated
	}
	return nil
}

// Assess rehearses the scope and returns the agent's assessment.
func (a *Agent) Assess(ctx context.Context, connector string, legalEntities []string, fiscalYear int, mode string) (string, error) {
	s, err := apitools.StartSession(ctx, a.API, "Migration rehearsal", a.LLM.Model)
	if err != nil {
		return "", err
	}
	tools, err := s.Tools(ctx, Tools...)
	if err != nil {
		return "", err
	}
	(&run{rehearsals: map[string]bool{}}).hold(tools)
	prompt := fmt.Sprintf("Rehearse the migration from connector %s of legal entities %s for fiscal year %d in %s mode, and assess it.",
		connector, strings.Join(legalEntities, ", "), fiscalYear, mode)
	msg, err := apitools.Run(ctx, a.LLM, s, tools, a.LLM.Params(system, anthropic.NewBetaTextBlock(prompt)), 15)
	if err != nil {
		return "", err
	}
	return llm.Text(msg), nil
}
