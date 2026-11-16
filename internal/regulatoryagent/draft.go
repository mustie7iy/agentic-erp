package regulatoryagent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/changedraft"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/recommend"
)

// DraftReads are the operations the drafting reads with (T107b); its
// writes are a drafting's (changedraft.Writes), and it never calls
// changedraft.Never.
var DraftReads = []string{"getRecommendation", "listConfigVersions", "getConfigVersion", "listFiscalPeriods", "getChangeRequest", "getOperation"}

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

// The drafting's refusals before its session, beside the pattern's and
// the drafting's own (changedraft).
var (
	// ErrNotAccepted refuses a recommendation that is not an accepted
	// change_needed on a configuration version, the regulatory agent's.
	ErrNotAccepted = errors.New("regulatoryagent: a change is drafted only from an accepted change_needed recommendation on a configuration version")
	// ErrStale refuses a recommendation made on a version no longer
	// active: the notice is read again against the active one.
	ErrStale = errors.New("regulatoryagent: the recommendation was made on a configuration version that is no longer active; read the notice again")
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
	active := config.Readable(v.Document)
	paths, err := recommended(rec.Content, active)
	if err != nil {
		return recommend.Result{}, err
	}
	d := &drafting{accepted: rec.Content, draft: (&changedraft.Draft{API: a.API, Poll: a.Poll, Patience: a.Patience, Base: v.Number,
		Active: active, Scope: under(paths), Objects: map[string]string{}}).Start(ctx)}
	task := recommend.Task{
		Purpose: fmt.Sprintf("Draft the change of configuration version %d a regulatory recommendation names", v.Number),
		Prompt: fmt.Sprintf("The tenant's active configuration is version %d (id %s). The accepted recommendation %s:\n<recommendation>\n%s\n</recommendation>\n"+
			"Once you have created the change request, you may recommend on object_type %s, object_id its id, with one of the kinds: %s, %s.",
			v.Number, v.ID, rec.ID, rec.Content, changedraft.Object, changedraft.ReadyToSubmit, changedraft.SimulationFailed),
		Objects: d.draft.Objects,
		Guards:  d.draft.Guards(),
		Observe: d.draft.Observers(),
		Check:   d.check,
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

// under is the drafting's scope: every setting it changes is at or under
// one of the paths the recommendation named.
func under(paths []string) func([]config.Change) error {
	return func(changes []config.Change) error {
		for _, ch := range changes {
			if !slices.ContainsFunc(paths, func(p string) bool {
				return ch.Path == p || strings.HasPrefix(ch.Path, p+".") || strings.HasPrefix(ch.Path, p+"[")
			}) {
				return fmt.Errorf("%w: not %s", changedraft.ErrOutside, ch.Path)
			}
		}
		return nil
	}
}

// drafter is the drafting as the pattern runs it.
func drafter(maxIterations int) recommend.Agent {
	return changedraft.Agent(Type, DraftReads, draftSystem, maxIterations)
}

// DraftTools are the operations the drafting may call: its reads, its
// writes and the one that recommends.
func DraftTools() []string { return drafter(0).Tools() }

// drafting is what one drafting run's check knows beside the draft: the
// accepted recommendation's content, whose quotations it repeats.
type drafting struct {
	accepted string
	draft    *changedraft.Draft
}

// check is the drafting's rule on a recommendation the pattern admits
// (T107b): its kind is the outcome of the simulation of the draft's last
// version, every quotation is one the accepted recommendation quotes, the
// caveat is said and every number outside the quotations is one the run
// read.
func (d *drafting) check(p recommend.Proposed, f *recommend.Figures) error {
	if err := d.draft.Outcome(p.Kind); err != nil {
		return err
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
