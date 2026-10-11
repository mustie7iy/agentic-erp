// Package configagent is the functional consultant agent: it turns a
// request in plain language (and, when given, a draft generated from SAP
// customizing) into configuration change requests through API tools. It
// proposes, submits and starts simulations; it cannot approve or activate
// (activation is a person's act, and approvals are refused to agents).
package configagent

import (
	"context"
	"encoding/json"

	anthropic "github.com/anthropics/anthropic-sdk-go"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/llm"
)

// Tools are the operations the agent may call.
var Tools = []string{"listConfigVersions", "getConfigVersion", "listChangeRequests", "getChangeRequest", "createChangeRequest",
	"updateChangeRequest", "submitChangeRequest", "simulateChangeRequest", "withdrawChangeRequest", "getOperation"}

const system = `You are a functional consultant for an ERP system whose configuration is one versioned document
(companies with their chart of accounts, account determination, tax codes, tolerances and house bank; payment terms;
approval tiers; payment settings; number ranges).

To change it: read the active version (listConfigVersions with is_active true, then getConfigVersion), build the complete
proposed document, createChangeRequest with base_version set to the active version's number, then submitChangeRequest.
Submission validates the document and computes its items and risk tier; if it is refused, read the field errors, fix the
draft with updateChangeRequest and submit again. Tier 3 and 4 changes need simulation evidence: start it with
simulateChangeRequest. Keep every change minimal and explain it in the rationale. You cannot approve or activate:
people do. Never weaken segregation of duties or approval tiers unless the request explicitly asks for it.

Finish with a short summary for the person who asked: what you changed, the change request number, its tier and status,
and what happens next.`

// Agent handles configuration requests.
type Agent struct {
	LLM *llm.Runner
	API *apitools.Client
}

// Result is the agent's reply and the change requests it created.
type Result struct {
	Reply          string
	ChangeRequests []string // ids
}

// Handle works on one request; draft, when not empty, is a configuration
// generated from SAP customizing for the agent to start from.
func (a *Agent) Handle(ctx context.Context, request string, draft []byte) (Result, error) {
	s, err := apitools.StartSession(ctx, a.API, "Configuration request", a.LLM.Model)
	if err != nil {
		return Result{}, err
	}
	tools, _ := apitools.Tools(a.API, s, Tools...) // the operations are in the contract and open to agents
	var res Result
	tools[4].Observe(func(r apitools.Response) {
		var cr struct{ ID string }
		if r.OK() && json.Unmarshal(r.Body, &cr) == nil {
			res.ChangeRequests = append(res.ChangeRequests, cr.ID)
		}
	})
	text := request
	if len(draft) > 0 {
		text += "\n\nDraft generated from the client's SAP customizing (YAML), to review and adapt:\n" + string(draft)
	}
	msg, err := apitools.Run(ctx, a.LLM, s, tools, a.LLM.Params(system, anthropic.NewBetaTextBlock(text)), 15)
	if err != nil {
		return res, err
	}
	res.Reply = llm.Text(msg)
	return res, nil
}
