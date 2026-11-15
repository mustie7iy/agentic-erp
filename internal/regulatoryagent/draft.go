package regulatoryagent

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/recommend"
)

// The drafting's object type and kinds (T107b): a recommendation on the
// change request the run drafted.
const (
	ChangeRequestObject = "change_request"
	ReadyToSubmit       = "ready_to_submit"
	SimulationFailed    = "simulation_failed"
)

// DraftReads are the operations the drafting reads with (T107b).
var DraftReads = []string{"getRecommendation", "listConfigVersions", "getConfigVersion", "listFiscalPeriods", "getChangeRequest", "getOperation"}

// DraftWrites are the drafting's writes, each held by its run's guard: a
// draft of the paths the accepted recommendation named, changed and
// previewed by the run that drafted it.
var DraftWrites = []string{"createChangeRequest", "updateChangeRequest", "simulateChangeRequest"}

// DraftNever are the operations the drafting never calls: every other
// write of the configuration, a decision on its approval and on a
// recommendation. A person submits the draft, the configuration's own
// approval decides it and a person activates it.
var DraftNever = []string{"submitChangeRequest", "withdrawChangeRequest", "activateChangeRequest", "applyCountryPack", "createRegressionSuite",
	"approveApprovalRequest", "rejectApprovalRequest", "decideRecommendation"}

const draftSystem = `You draft the configuration change request for a regulatory recommendation a tax accountant accepted, for a
person to submit and approve.

The accepted recommendation is given to you: its first line names the paths of the configuration the notice affects, its
second when the change takes effect in the notice's words, and it quotes the notice. Read the active configuration version
(getConfigVersion) and build the complete proposed document: the active version's document with only the settings under
those paths changed, as the quoted passages say, from the date they say (a tax code's rate from a date is a component of the
code with dated rates: the rate in force until the day before, and the new rate from that day; a code of one undated rate has
had it since the first day of its company's first fiscal period, listFiscalPeriods). Then createChangeRequest once,
with base_version the active version's number, a title and, as rationale, the passages the change rests on. Fix it with
updateChangeRequest if a tool refuses it. Then simulateChangeRequest on it (if_match its version): the simulation validates
the draft as submission would and replays recent transactions under it; read its operation (getOperation) for the outcome.

Then recommend once, on the change request: ready_to_submit when the simulation of its last version passed, or
simulation_failed when it failed or found transactions it could not evaluate, saying what failed. Quote the notice's
passages exactly as the accepted recommendation quotes them, in double quotes, and write no other number that a tool did
not return. End with the sentence: ` + Caveat + `

You never submit, withdraw or activate a change request and never decide an approval: a person submits it, the
configuration's approval decides it, and only then does it take effect.`

// The drafting's refusals beside the pattern's.
var (
	// ErrNotAccepted refuses a recommendation that is not an accepted
	// change_needed on a configuration version, the regulatory agent's.
	ErrNotAccepted = errors.New("regulatoryagent: a change is drafted only from an accepted change_needed recommendation on a configuration version")
	// ErrStale refuses a recommendation made on a version no longer
	// active: the notice is read again against the active one.
	ErrStale = errors.New("regulatoryagent: the recommendation was made on a configuration version that is no longer active; read the notice again")
	// ErrBase refuses a draft not based on the active version.
	ErrBase = errors.New("regulatoryagent: the draft's base_version is the active version's number")
	// ErrOutside refuses a draft that changes nothing, or a setting
	// outside the paths the recommendation named.
	ErrOutside = errors.New("regulatoryagent: the draft changes settings under the paths the accepted recommendation named, and nothing else")
	// ErrNotTheDraft refuses a second draft of a run, or a change or
	// simulation of a change request the run did not draft.
	ErrNotTheDraft = errors.New("regulatoryagent: a run drafts one change request and changes and simulates only that one")
	// ErrNotSimulated refuses ready_to_submit unless the simulation of
	// the draft's last version passed, and simulation_failed unless it
	// failed.
	ErrNotSimulated = errors.New("regulatoryagent: ready_to_submit needs a passed simulation of the draft's last version, simulation_failed a failed one")
)

// accepted is a recommendation as the drafting reads it.
type accepted struct {
	ID         string `json:"id"`
	ObjectType string `json:"object_type"`
	ObjectID   string `json:"object_id"`
	Kind       string `json:"kind"`
	Content    string `json:"content"`
	Outcome    string `json:"outcome"`
}

// Draft runs one session on a recommendation a person accepted (T107b):
// it reads the recommendation and the active configuration version before
// the session, and refuses a recommendation that is not an accepted
// change_needed of the regulatory agent's kind, one made on a version no
// longer active, and one whose paths the version does not hold; the
// session drafts the change request, has it simulated and recommends on
// it, and Draft reports what it recommended, also when the run fails
// after recommending.
func (a *Agent) Draft(ctx context.Context, recommendation string) (recommend.Result, error) {
	var rec accepted
	resp, err := a.API.Do(ctx, http.MethodGet, "/v1/recommendations/"+url.PathEscape(recommendation), nil, nil, "")
	if err == nil {
		err = resp.Decode(&rec)
	}
	if err != nil {
		return recommend.Result{}, fmt.Errorf("regulatoryagent: recommendation %s: %w", recommendation, err)
	}
	if rec.ObjectType != VersionObject || rec.Kind != ChangeNeeded || rec.Outcome != "accepted" {
		return recommend.Result{}, fmt.Errorf("%w: recommendation %s is %s on %s, %s", ErrNotAccepted, recommendation, rec.Kind, rec.ObjectType, outcome(rec.Outcome))
	}
	v, err := apitools.ActiveConfig(ctx, a.API)
	if err != nil {
		return recommend.Result{}, err
	}
	if v.ID != rec.ObjectID {
		return recommend.Result{}, fmt.Errorf("%w: it was made on %s, version %d is active", ErrStale, rec.ObjectID, v.Number)
	}
	d := &drafting{agent: a, ctx: ctx, base: v.Number, active: config.Readable(v.Document), accepted: rec.Content, objects: map[string]string{}}
	if d.paths, err = recommended(rec.Content, d.active); err != nil {
		return recommend.Result{}, err
	}
	task := recommend.Task{
		Purpose: fmt.Sprintf("Draft the change of configuration version %d a regulatory recommendation names", v.Number),
		Prompt: fmt.Sprintf("The tenant's active configuration is version %d (id %s). The accepted recommendation %s:\n<recommendation>\n%s\n</recommendation>\n"+
			"Once you have created the change request, you may recommend on object_type %s, object_id its id, with one of the kinds: %s, %s.",
			v.Number, v.ID, rec.ID, rec.Content, ChangeRequestObject, ReadyToSubmit, SimulationFailed),
		Objects: d.objects,
		Guards: map[string]func(map[string]any) error{"createChangeRequest": d.create, "updateChangeRequest": d.update,
			"simulateChangeRequest": d.simulate},
		Observe: map[string]func(apitools.Response){"createChangeRequest": d.created, "updateChangeRequest": d.updated,
			"simulateChangeRequest": d.simulated},
		Check: d.check,
	}
	return drafter(a.MaxIterations).Run(ctx, a.LLM, a.API, task)
}

// outcome names a recommendation's outcome, open while it has none.
func outcome(o string) string {
	if o == "" {
		return "open"
	}
	return o
}

// recommended are the paths an accepted recommendation's first line
// names, each one the active version holds.
func recommended(content string, active *config.Config) ([]string, error) {
	first, _, _ := strings.Cut(content, "\n")
	list, ok := strings.CutPrefix(first, "Paths: ")
	var paths []string
	for _, p := range strings.Split(list, ",") {
		if p = strings.TrimSpace(p); config.Holds(active, p) {
			paths = append(paths, p)
			continue
		}
		ok = false
	}
	if !ok {
		return nil, fmt.Errorf("%w: its paths, %q, are not all the active version's", ErrNotAccepted, first)
	}
	return paths, nil
}

// drafter is the drafting as the pattern runs it.
func drafter(maxIterations int) recommend.Agent {
	return recommend.Agent{Type: Type, Reads: DraftReads, Writes: DraftWrites, System: draftSystem, MaxIterations: maxIterations,
		Kinds: map[string][]string{ChangeRequestObject: {ReadyToSubmit, SimulationFailed}}}
}

// DraftTools are the operations the drafting may call: its reads, its
// writes and the one that recommends.
func DraftTools() []string { return drafter(0).Tools() }

// drafting is what one drafting run's guards and check know: the active
// version, the paths the accepted recommendation named and its content,
// the change request the run drafted, its last version and the outcome of
// the simulation of a version of it. The tools run one call at a time
// (recommend), so none of it needs a lock of its own.
type drafting struct {
	agent    *Agent
	ctx      context.Context // the run's: a simulation is waited for in it
	base     int
	active   *config.Config
	paths    []string
	accepted string
	objects  map[string]string // the run's, to which the draft is added

	draft   string // the change request's id, once created
	version int    // its last version
	preview *preview
}

// preview is a finished simulation of a version of the draft.
type preview struct {
	version int
	passed  bool // succeeded and passed
}

// create admits the run's one draft: based on the active version and
// changing only what the recommendation named.
func (d *drafting) create(in map[string]any) error {
	if d.draft != "" {
		return ErrNotTheDraft
	}
	b, _ := in["body"].(map[string]any)                      // no body: refused below
	if fmt.Sprint(b["base_version"]) != fmt.Sprint(d.base) { // a JSON number prints as the integer it is
		return fmt.Errorf("%w: %d", ErrBase, d.base)
	}
	return d.within(b["proposed"])
}

// update admits a change of the run's draft that keeps it on the active
// version and whose proposal, when it changes it, still changes only what
// the recommendation named.
func (d *drafting) update(in map[string]any) error {
	if d.draft == "" || in["id"] != d.draft {
		return ErrNotTheDraft
	}
	b, _ := in["body"].(map[string]any) // no body: the API refuses it
	if base, ok := b["base_version"]; ok && fmt.Sprint(base) != fmt.Sprint(d.base) {
		return fmt.Errorf("%w: %d", ErrBase, d.base)
	}
	if proposed, ok := b["proposed"]; ok {
		return d.within(proposed)
	}
	return nil
}

// simulate admits a simulation of the run's draft.
func (d *drafting) simulate(in map[string]any) error {
	if d.draft == "" || in["id"] != d.draft {
		return ErrNotTheDraft
	}
	return nil
}

// within refuses a proposal that is no configuration, changes nothing or
// changes a setting outside the recommendation's paths.
func (d *drafting) within(proposed any) error {
	b, _ := json.Marshal(proposed) // decoded from JSON, so it encodes again
	c, _, err := config.ParseActive(b)
	if err != nil {
		return fmt.Errorf("%w: the proposal is no configuration: %w", ErrOutside, err)
	}
	changes, _ := config.Diff(d.active, c) // never fails
	if len(changes) == 0 {
		return fmt.Errorf("%w: it changes nothing", ErrOutside)
	}
	for _, ch := range changes {
		if !slices.ContainsFunc(d.paths, func(p string) bool { return under(ch.Path, p) }) {
			return fmt.Errorf("%w: not %s", ErrOutside, ch.Path)
		}
	}
	return nil
}

// under reports whether a setting's path is the path or inside it.
func under(setting, path string) bool {
	return setting == path || strings.HasPrefix(setting, path+".") || strings.HasPrefix(setting, path+"[")
}

// created keeps the draft the API recorded and lets the run recommend on
// it.
func (d *drafting) created(r apitools.Response) {
	var cr struct {
		ID      string `json:"id"`
		Version int    `json:"version"`
	}
	if r.OK() && r.Decode(&cr) == nil && cr.ID != "" {
		d.draft, d.version = cr.ID, cr.Version
		d.objects[cr.ID] = ChangeRequestObject
	}
}

// updated keeps the draft's last version.
func (d *drafting) updated(r apitools.Response) {
	var cr struct{ Version int }
	if r.OK() && r.Decode(&cr) == nil {
		d.version = cr.Version
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
// its outcome: the check holds the recommendation to it. A simulation
// that does not finish within the agent's patience is kept as none.
func (d *drafting) simulated(r apitools.Response) {
	var op operation
	if r.Status != http.StatusAccepted || r.Decode(&op) != nil || op.ID == "" {
		return
	}
	if op, ok := d.agent.settle(d.ctx, op.ID); ok {
		d.preview = &preview{version: op.Result.Version, passed: op.Status == "succeeded" && op.Result.Passed}
		if op.Status != "succeeded" {
			d.preview.version = d.version // a failed simulation names no version: it is of the draft as it stands
		}
	}
}

// settle reads the operation every Poll (default one second) until it
// finishes, at most Patience (default two minutes), and reports it.
func (a *Agent) settle(ctx context.Context, id string) (operation, bool) {
	poll, patience := cmp.Or(max(a.Poll, 0), time.Second), cmp.Or(max(a.Patience, 0), 2*time.Minute)
	ctx, cancel := context.WithTimeout(ctx, patience)
	defer cancel()
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		var op operation
		resp, err := a.API.Do(ctx, http.MethodGet, "/v1/operations/"+url.PathEscape(id), nil, nil, "")
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

// check is the drafting's rule on a recommendation the pattern admits
// (T107b): its kind is the outcome of the simulation of the draft's last
// version, every quotation is one the accepted recommendation quotes, the
// caveat is said and every number outside the quotations is one the run
// read.
func (d *drafting) check(p recommend.Proposed, f *recommend.Figures) error {
	ran := d.preview != nil && d.preview.version == d.version
	if !ran || (p.Kind == ReadyToSubmit) != d.preview.passed {
		return ErrNotSimulated
	}
	quoted, rest := quotations(p.Content)
	for _, q := range quoted {
		if !inNotice(d.accepted, q) {
			return fmt.Errorf("%w: not %q", ErrNotQuoted, q)
		}
	}
	if !strings.Contains(p.Content, Caveat) {
		return ErrNoCaveat
	}
	if unread := f.Unread(rest); len(unread) > 0 {
		return fmt.Errorf("%w: not %s", ErrUnread, strings.Join(unread, ", "))
	}
	return nil
}
