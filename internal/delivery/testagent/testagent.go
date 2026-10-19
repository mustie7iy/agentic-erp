// Package testagent is the test consultant agent: it reviews a configuration
// change request through API tools (04-api-design §4.13: its items, tier,
// evidence and the version it changes), leaves one recommendation on it for
// the approvers, and writes the user acceptance test scripts the change
// needs. It changes nothing.
package testagent

import (
	"context"
	"fmt"

	anthropic "github.com/anthropics/anthropic-sdk-go"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/llm"
)

// Tools are the operations the agent may call.
var Tools = []string{"getChangeRequest", "getConfigVersion", "listConfigVersions", "listRegressionSuites", "createRegressionSuite", "getOperation", "createRecommendation"}

const system = `You are a test consultant. Review the configuration change request you are given: read it with
getChangeRequest (its computed items, risk tier, status and evidence) and the version it is based on.

A change of tier 3 or 4 needs simulation and regression evidence before approval. If its evidence says no regression
suite is captured on its base version and createRegressionSuite is among your tools, capture one (listRegressionSuites
shows the existing ones); either way, say in your recommendation that the simulation must be run again with one.

Then call createRecommendation exactly once with object_type "change_request", the change request id as object_id,
the session id you are given, and kind approve, reject or needs_evidence, with the reasons in content: what the change
affects, the risks, and missing evidence.

Finish with user acceptance test scripts in Markdown: for each affected process, numbered steps with the data to use
and the expected result, including at least one negative case.`

// Agent reviews change requests.
type Agent struct {
	LLM *llm.Runner
	API *apitools.Client
}

// Review is the agent's recommendation and its test scripts.
type Review struct {
	Recommended bool   // a recommendation was recorded
	Scripts     string // UAT scripts, Markdown
}

// Review reviews a change request.
func (a *Agent) Review(ctx context.Context, changeRequest string) (Review, error) {
	s, err := apitools.StartSession(ctx, a.API, "Change request review", a.LLM.Model)
	if err != nil {
		return Review{}, err
	}
	tools, _ := apitools.Tools(a.API, s, Tools...) // the operations are in the contract and open to agents
	var rev Review
	tools[len(tools)-1].Observe(func(r apitools.Response) { rev.Recommended = rev.Recommended || r.OK() })
	prompt := fmt.Sprintf("Review change request %s. Your session id is %s.", changeRequest, s.ID)
	msg, err := apitools.Run(ctx, a.LLM, s, tools, a.LLM.Params(system, anthropic.NewBetaTextBlock(prompt)), 12)
	if err != nil {
		return rev, err
	}
	rev.Scripts = llm.Text(msg)
	return rev, nil
}
