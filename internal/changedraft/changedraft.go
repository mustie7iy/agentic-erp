// Package changedraft is the drafting of a configuration change request
// by a recommending agent (ADR-0072 As built, T107b; moved here for
// T108c, so the regulatory and the support agent share it rather than
// copy it): a run drafts one change request based on the active version
// (createChangeRequest), may fix it (updateChangeRequest) and has it
// simulated as a preview (simulateChangeRequest), and recommends on it
// ready_to_submit or simulation_failed by the outcome of the simulation
// of its last version. Its guards hold the run to one draft, kept on the
// active version, whose proposal is a configuration that changes
// something and only what the agent's scope admits; a change or
// simulation of another change request is refused. A person submits the
// draft, the configuration's own approval decides it and a person
// activates it: the drafting never calls Never.
package changedraft

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/recommend"
)

// The object type a drafting run recommends on and its kinds: a
// recommendation on the change request the run drafted.
const (
	Object           = "change_request"
	ReadyToSubmit    = "ready_to_submit"
	SimulationFailed = "simulation_failed"
)

// Kinds are the kinds a drafting run recommends, by its object type.
func Kinds() map[string][]string {
	return map[string][]string{Object: {ReadyToSubmit, SimulationFailed}}
}

// Writes are a drafting run's writes, each held by its guard (Guards).
var Writes = []string{"createChangeRequest", "updateChangeRequest", "simulateChangeRequest"}

// Never are the operations a drafting run never calls: every other write
// of the configuration, a decision on its approval and on a
// recommendation. A person submits the draft, the configuration's own
// approval decides it and a person activates it.
var Never = []string{"submitChangeRequest", "withdrawChangeRequest", "activateChangeRequest", "applyCountryPack", "createRegressionSuite",
	"approveApprovalRequest", "rejectApprovalRequest", "decideRecommendation"}

// The drafting's refusals.
var (
	// ErrBase refuses a draft not based on the active version.
	ErrBase = errors.New("changedraft: the draft's base_version is the active version's number")
	// ErrOutside refuses a draft that is no configuration, changes
	// nothing, or changes what the agent's scope does not admit.
	ErrOutside = errors.New("changedraft: the draft changes what the accepted recommendation names, and nothing else")
	// ErrNotTheDraft refuses a second draft of a run, or a change or
	// simulation of a change request the run did not draft.
	ErrNotTheDraft = errors.New("changedraft: a run drafts one change request and changes and simulates only that one")
	// ErrNotSimulated refuses ready_to_submit unless the simulation of
	// the draft's last version passed, and simulation_failed unless it
	// failed.
	ErrNotSimulated = errors.New("changedraft: ready_to_submit needs a passed simulation of the draft's last version, simulation_failed a failed one")
)

// Draft is what one drafting run's guards and check know: the active
// version, the agent's scope, the change request the run drafted, its
// last version and changes, and the outcome of the simulation of a
// version of it. The pattern runs the tools one call at a time
// (recommend), so none of it needs a lock of its own.
type Draft struct {
	// API reads the simulation's operation.
	API *apitools.Client
	// Poll is how often a simulation is read while the run waits for it,
	// and Patience how long it waits; defaults one second and two
	// minutes.
	Poll, Patience time.Duration
	// Base is the active version's number, Active its configuration.
	Base   int
	Active *config.Config
	// Scope admits the changes a proposal makes to the active version (at
	// least one): an error, which wraps ErrOutside, refuses it.
	Scope func([]config.Change) error
	// Objects are the run's (recommend.Task.Objects), to which the draft
	// is added once created.
	Objects map[string]string

	ctx     context.Context // the run's: a simulation is waited for in it
	draft   string          // the change request's id, once created
	version int             // its last version
	changes []config.Change // what its last proposal changes
	pending []config.Change // what the proposal of the call now running changes
	preview *preview
}

// preview is a finished simulation of a version of the draft.
type preview struct {
	version int
	passed  bool // succeeded and passed
}

// Start readies the drafting for a run in ctx, in which the simulations
// are waited for, and returns it.
func (d *Draft) Start(ctx context.Context) *Draft {
	d.ctx = ctx
	return d
}

// Guards are the drafting's guards, by write (recommend.Task.Guards).
func (d *Draft) Guards() map[string]func(map[string]any) error {
	return map[string]func(map[string]any) error{"createChangeRequest": d.create, "updateChangeRequest": d.update, "simulateChangeRequest": d.simulate}
}

// Observers are the drafting's observers, by write (recommend.Task.Observe).
func (d *Draft) Observers() map[string]func(apitools.Response) {
	return map[string]func(apitools.Response){"createChangeRequest": d.created, "updateChangeRequest": d.updated, "simulateChangeRequest": d.simulated}
}

// Changes are what the draft's last proposal the API accepted changes in
// the active version; none before a draft is created.
func (d *Draft) Changes() []config.Change { return d.changes }

// Outcome refuses a recommendation of the kind unless the simulation of
// the draft's last version finished: ready_to_submit when it passed,
// simulation_failed when it failed or did not pass.
func (d *Draft) Outcome(kind string) error {
	if d.preview == nil || d.preview.version != d.version || (kind == ReadyToSubmit) != d.preview.passed {
		return ErrNotSimulated
	}
	return nil
}

// create admits the run's one draft: based on the active version and
// changing only what the scope admits.
func (d *Draft) create(in map[string]any) error {
	if d.draft != "" {
		return ErrNotTheDraft
	}
	b, _ := in["body"].(map[string]any)                      // no body: refused below
	if fmt.Sprint(b["base_version"]) != fmt.Sprint(d.Base) { // a JSON number prints as the integer it is
		return fmt.Errorf("%w: %d", ErrBase, d.Base)
	}
	return d.within(b["proposed"])
}

// update admits a change of the run's draft that keeps it on the active
// version and whose proposal, when it changes it, still changes only what
// the scope admits.
func (d *Draft) update(in map[string]any) error {
	if d.draft == "" || in["id"] != d.draft {
		return ErrNotTheDraft
	}
	b, _ := in["body"].(map[string]any) // no body: the API refuses it
	if base, ok := b["base_version"]; ok && fmt.Sprint(base) != fmt.Sprint(d.Base) {
		return fmt.Errorf("%w: %d", ErrBase, d.Base)
	}
	d.pending = d.changes
	if proposed, ok := b["proposed"]; ok {
		return d.within(proposed)
	}
	return nil
}

// simulate admits a simulation of the run's draft.
func (d *Draft) simulate(in map[string]any) error {
	if d.draft == "" || in["id"] != d.draft {
		return ErrNotTheDraft
	}
	return nil
}

// within refuses a proposal that is no configuration, changes nothing or
// changes what the scope does not admit, and keeps what it changes for
// the API's answer.
func (d *Draft) within(proposed any) error {
	b, _ := json.Marshal(proposed) // decoded from JSON, so it encodes again
	c, _, err := config.ParseActive(b)
	if err != nil {
		return fmt.Errorf("%w: the proposal is no configuration: %w", ErrOutside, err)
	}
	changes, _ := config.Diff(d.Active, c) // never fails
	if len(changes) == 0 {
		return fmt.Errorf("%w: it changes nothing", ErrOutside)
	}
	if err := d.Scope(changes); err != nil {
		return err
	}
	d.pending = changes
	return nil
}

// created keeps the draft the API recorded and lets the run recommend on
// it.
func (d *Draft) created(r apitools.Response) {
	var cr struct {
		ID      string `json:"id"`
		Version int    `json:"version"`
	}
	if r.OK() && r.Decode(&cr) == nil && cr.ID != "" {
		d.draft, d.version, d.changes = cr.ID, cr.Version, d.pending
		d.Objects[cr.ID] = Object
	}
}

// updated keeps the draft's last version and what it changes.
func (d *Draft) updated(r apitools.Response) {
	var cr struct{ Version int }
	if r.OK() && r.Decode(&cr) == nil {
		d.version, d.changes = cr.Version, d.pending
	}
}

// operation is a long-running operation as the drafting reads it.
type operation struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Result struct {
		Passed  bool `json:"passed"`
		Version int  `json:"version"`
	} `json:"result"`
}

// simulated waits for the simulation the API accepted to finish and keeps
// its outcome: Outcome holds the recommendation to it. A simulation that
// does not finish within the patience is kept as none.
func (d *Draft) simulated(r apitools.Response) {
	var op operation
	if r.Status != http.StatusAccepted || r.Decode(&op) != nil || op.ID == "" {
		return
	}
	if op, ok := d.settle(op.ID); ok {
		d.preview = &preview{version: op.Result.Version, passed: op.Status == "succeeded" && op.Result.Passed}
		if op.Status != "succeeded" {
			d.preview.version = d.version // a failed simulation names no version: it is of the draft as it stands
		}
	}
}

// settle reads the operation every Poll until it finishes, at most
// Patience, and reports it.
func (d *Draft) settle(id string) (operation, bool) {
	poll, patience := cmp.Or(max(d.Poll, 0), time.Second), cmp.Or(max(d.Patience, 0), 2*time.Minute)
	ctx, cancel := context.WithTimeout(cmp.Or(d.ctx, context.Background()), patience)
	defer cancel()
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		var op operation
		resp, err := d.API.Do(ctx, http.MethodGet, "/v1/operations/"+url.PathEscape(id), nil, nil, "")
		if err == nil && resp.Decode(&op) == nil && slices.Contains([]string{"succeeded", "failed", "cancelled"}, op.Status) {
			return op, true
		}
		select {
		case <-ctx.Done():
			return operation{}, false
		case <-tick.C:
		}
	}
}

// Agent is the drafting as the pattern runs it, for an agent of the type
// that reads with reads.
func Agent(agentType string, reads []string, system string, maxIterations int) recommend.Agent {
	return recommend.Agent{Type: agentType, Reads: reads, Writes: Writes, System: system, MaxIterations: maxIterations, Kinds: Kinds()}
}
