// Package testagent is the test consultant agent: it reviews a configuration
// change request through API tools (04-api-design §4.13: its items, tier,
// evidence and the version it changes), leaves one recommendation on it for
// the approvers, and writes the user acceptance test scripts the change
// needs. It changes nothing: its guards hold its one recommendation to the
// change request it was given and its writes to one regression suite.
// Its session and autonomy are ADR-0036's, its evaluation suite evals/test
// (ADR-0039).
package testagent

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
var Tools = []string{"getChangeRequest", "getConfigVersion", "listConfigVersions", "listRegressionSuites", Capturer, "getOperation", Proposer}

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

// The operations of Tools that write: the one recommendation, and the
// regression suite's capture.
const (
	Proposer = "createRecommendation"
	Capturer = "createRegressionSuite"
)

// Kinds are the kinds the agent recommends on a change request.
var Kinds = []string{"approve", "reject", "needs_evidence"}

var (
	// ErrOtherChange refuses a recommendation that is not the run's one on
	// its change request: another session, object or kind, or a second.
	ErrOtherChange = errors.New("testagent: a recommendation is this run's one on its change request (session, object_type change_request, its id, one of Kinds)")
	// ErrSecondCapture refuses a second capture in a run: one suite is
	// what the change's simulation replays.
	ErrSecondCapture = errors.New("testagent: a run captures at most one regression suite")
)

// Review reviews a change request.
func (a *Agent) Review(ctx context.Context, changeRequest string) (Review, error) {
	s, err := apitools.StartSession(ctx, a.API, "Change request review", a.LLM.Model)
	if err != nil {
		return Review{}, err
	}
	tools, err := s.Tools(ctx, Tools...)
	if err != nil {
		return Review{}, err
	}
	var rev Review
	r := &run{rev: &rev, session: s.ID, change: strings.ToLower(changeRequest)}
	r.hold(tools)
	prompt := fmt.Sprintf("Review change request %s. Your session id is %s.", changeRequest, s.ID)
	msg, err := apitools.Run(ctx, a.LLM, s, tools, a.LLM.Params(system, anthropic.NewBetaTextBlock(prompt)), 12)
	if err != nil {
		return rev, err
	}
	rev.Scripts = llm.Text(msg)
	return rev, nil
}

// run is what one session's guards hold its writes to: its session and
// change request, whether it has recommended and whether it has captured
// a suite. Its writes run each call whole under mu (apitools Exclusive):
// the runner executes one turn's tool calls concurrently.
type run struct {
	mu       sync.Mutex
	rev      *Review
	session  string
	change   string // the change request's id, in lower case
	captured bool
}

// hold puts the run's guards and observers on the writes.
func (r *run) hold(tools []*apitools.Tool) {
	for _, t := range tools {
		switch t.Name() {
		case Proposer:
			t.Guard(r.recommendation)
			t.Observe(func(resp apitools.Response) { r.rev.Recommended = r.rev.Recommended || resp.OK() })
			t.Exclusive(&r.mu)
		case Capturer:
			t.Guard(r.capture)
			t.Observe(func(resp apitools.Response) { r.captured = r.captured || resp.OK() })
			t.Exclusive(&r.mu)
		}
	}
}

// recommendation refuses a recommendation that is not the run's one on
// its change request.
func (r *run) recommendation(in map[string]any) error {
	b, _ := in["body"].(map[string]any) // no body: refused below, as the API would
	if r.rev.Recommended || fmt.Sprint(b["session_id"]) != r.session || fmt.Sprint(b["object_type"]) != "change_request" ||
		strings.ToLower(fmt.Sprint(b["object_id"])) != r.change || !slices.Contains(Kinds, fmt.Sprint(b["kind"])) {
		return ErrOtherChange
	}
	return nil
}

// capture refuses a second suite in the run.
func (r *run) capture(map[string]any) error {
	if r.captured {
		return ErrSecondCapture
	}
	return nil
}
