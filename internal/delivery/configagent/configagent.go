// Package configagent is the functional consultant agent: it turns a request
// in plain language (and, when given, a draft generated from SAP
// customizing) into configuration change requests through API tools
// (04-api-design §4.13). It proposes, submits and starts simulations; it
// cannot approve or activate (activation is a person's act, and approvals
// are refused to agents, ADR-0010), and its guards hold its changes to the
// change requests its session created, never a person's. Its session and
// autonomy are ADR-0036's, its evaluation suite evals/configuration
// (ADR-0039).
package configagent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	anthropic "github.com/anthropics/anthropic-sdk-go"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/llm"
)

// Tools are the operations the agent may call.
var Tools = []string{"listConfigVersions", "getConfigVersion", "listChangeRequests", "getChangeRequest", Creator,
	"updateChangeRequest", "submitChangeRequest", "simulateChangeRequest", "withdrawChangeRequest", "getOperation"}

const system = `You are a functional consultant for an ERP system whose configuration is one versioned document
(companies with their chart of accounts, account determination, tax codes, tolerances and house bank; payment terms;
approval tiers; payment settings; number ranges).

To change it: read the active version (listConfigVersions with is_active true, then getConfigVersion), build the complete
proposed document, createChangeRequest with base_version set to the active version's number, then submitChangeRequest.
Submission validates the document and computes its items and risk tier; if it is refused, read the field errors, fix the
draft with updateChangeRequest and submit again. Tier 3 and 4 changes need simulation evidence: start it with
simulateChangeRequest. Keep every change minimal and explain it in the rationale. You change, submit, simulate or
withdraw only the change requests you create in this session, never a person's. You cannot approve or activate: people
do. Never weaken segregation of duties or approval tiers unless the request explicitly asks for it.
Every company needs its time zone, the IANA name such as America/Toronto: when the request does not give a company's
zone, ask the person for it rather than guess it from the country, and file nothing until they answer.

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

// Creator creates the run's change requests; Drafting are the writes that
// act on one, held to those the run created.
const Creator = "createChangeRequest"

// Drafting are the operations that change a change request: the agent's
// session may use them only on a change request it created itself, never
// on a person's (withdrawing one cancels its approval).
var Drafting = []string{"updateChangeRequest", "submitChangeRequest", "simulateChangeRequest", "withdrawChangeRequest"}

// ErrNotCreated refuses a change to a change request this run did not
// create.
var ErrNotCreated = errors.New("configagent: this session changes only the change requests it created")

// Handle works on one request; draft, when not empty, is a configuration
// generated from SAP customizing for the agent to start from.
func (a *Agent) Handle(ctx context.Context, request string, draft []byte) (Result, error) {
	s, err := apitools.StartSession(ctx, a.API, "Configuration request", a.LLM.Model)
	if err != nil {
		return Result{}, err
	}
	tools, err := s.Tools(ctx, Tools...)
	if err != nil {
		return Result{}, err
	}
	var res Result
	newRun(&res).hold(tools)
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

// run is what one session's guards hold its writes to: the change
// requests it created. Its tools run each call whole under mu (apitools
// Exclusive): the runner executes one turn's tool calls concurrently.
type run struct {
	mu      sync.Mutex
	res     *Result
	created map[string]bool // ids, in lower case
}

func newRun(res *Result) *run { return &run{res: res, created: map[string]bool{}} }

// hold puts the run's observer on the creating tool and its guard on the
// drafting ones.
func (r *run) hold(tools []*apitools.Tool) {
	for _, t := range tools {
		switch {
		case t.Name() == Creator:
			t.Observe(r.made)
			t.Exclusive(&r.mu)
		case slices.Contains(Drafting, t.Name()):
			t.Guard(r.own)
			t.Exclusive(&r.mu)
		}
	}
}

// made keeps a change request the API created.
func (r *run) made(resp apitools.Response) {
	var cr struct{ ID string }
	if resp.OK() && resp.Decode(&cr) == nil && cr.ID != "" {
		r.res.ChangeRequests = append(r.res.ChangeRequests, cr.ID)
		r.created[strings.ToLower(cr.ID)] = true
	}
}

// own refuses a change to a change request the run did not create.
func (r *run) own(in map[string]any) error {
	if !r.created[strings.ToLower(fmt.Sprint(in["id"]))] {
		return ErrNotCreated
	}
	return nil
}
