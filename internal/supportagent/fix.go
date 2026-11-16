package supportagent

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

// The support agent's third capability (ADR-0072 As built, T108c): for
// a root cause of the class configuration that a user accepted, it
// drafts the configuration change request that fixes it and has it
// simulated, for a person to submit and approve. The draft changes one
// setting, one the accepted diagnosis cites as `path` = `value`; it is
// drafted, kept and simulated as the regulatory agent drafts
// (changedraft), and the run recommends on it ready_to_submit or
// simulation_failed by the simulation's outcome. It never submits,
// withdraws or activates a change request, decides an approval or
// changes master data or a document: its only writes are the draft's.

// Configuration is the class of a root cause the agent drafts a fix of.
const Configuration = "configuration"

// FixReads are the operations the agent reads with when it drafts a fix
// (T108c); its writes are a drafting's (changedraft.Writes), and it never
// calls changedraft.Never.
var FixReads = []string{"getRecommendation", "listConfigVersions", "getConfigVersion", "getChangeRequest", "getOperation"}

const fixSystem = `You draft the configuration change request that fixes the root cause of a stuck document or a refused
request, a cause in the configuration that you diagnosed and a user accepted, for a person to submit and approve.

The accepted diagnosis is given to you between the lines <diagnosis> and </diagnosis>, with the settings it cites. Read the
active configuration version (getConfigVersion with the id you are given) and build the complete proposed document: the
active version's document with exactly one of the settings the diagnosis cites changed, to the value that removes the
cause, and nothing else. Then createChangeRequest once, with base_version the active version's number, a title and, as
rationale, the cause and the events the diagnosis cites. Fix it with updateChangeRequest if a tool refuses it. Then
simulateChangeRequest on it (if_match its version): the simulation validates the draft as submission would and replays
recent transactions under it; read its operation (getOperation) for the outcome.

Then recommend once, on the change request: ready_to_submit when the simulation of its last version passed, or
simulation_failed when it failed or found transactions it could not evaluate, saying what failed. The content's first line
is "Change: " and the setting the draft changes, its value in the active version and its value in the draft, each in
backticks: ` + "Change: `path` from `old` to `new`" + `. Then why, citing any other setting as ` + "`path` = `value`" + `
as the active version holds it. Write no other number than one a tool returned.

You never submit, withdraw or activate a change request, decide an approval or change master data or a document: a person
submits the draft, the configuration's approval decides it, and only then does it take effect.`

// The refusals before a fix's session and of its check, beside the
// pattern's and the drafting's (changedraft).
var (
	// ErrNotConfiguration refuses a recommendation that is not an
	// accepted root cause of the class configuration citing a setting.
	ErrNotConfiguration = errors.New("supportagent: a fix is drafted only from an accepted root_cause of the class configuration that cites a setting as `path` = `value`")
	// ErrFixStale refuses a diagnosis citing a setting the active
	// configuration no longer holds with that value: it is diagnosed
	// again.
	ErrFixStale = errors.New("supportagent: a setting the diagnosis cites no longer has that value in the active configuration; diagnose the document again")
	// ErrChange refuses a fix whose first line is not the draft's one
	// change as "Change: `path` from `old` to `new`".
	ErrChange = errors.New("supportagent: the first line is \"Change: `path` from `old` to `new`\", the one setting the draft changes, as it changes it")
)

// diagnosed is a recommendation as the fix reads it.
type diagnosed struct {
	ID         string `json:"id"`
	ObjectType string `json:"object_type"`
	Kind       string `json:"kind"`
	Content    string `json:"content"`
	Outcome    string `json:"outcome"`
}

// Fix runs one session on a diagnosis a user accepted (T108c): it reads
// the recommendation and the active configuration version before the
// session, and refuses one that is not an accepted root_cause of the class
// configuration on a document citing a setting, and one citing a setting
// the active version no longer holds with that value; the session drafts
// the change request that changes one setting the diagnosis cites, has it
// simulated and recommends on it, and Fix reports what it recommended,
// also when the run fails after recommending.
func (a *Agent) Fix(ctx context.Context, recommendation string) (recommend.Result, error) {
	var rec diagnosed
	resp, err := a.API.Do(ctx, http.MethodGet, "/v1/recommendations/"+url.PathEscape(recommendation), nil, nil, "")
	if err == nil {
		err = resp.Decode(&rec)
	}
	if err != nil {
		return recommend.Result{}, fmt.Errorf("supportagent: recommendation %s: %w", recommendation, err)
	}
	class, _ := classOf(rec.Content)
	cited := citationRe.FindAllStringSubmatch(rec.Content, -1)
	if _, ok := Documents[rec.ObjectType]; !ok || rec.Kind != RootCause || rec.Outcome != "accepted" || class != Configuration || len(cited) == 0 {
		return recommend.Result{}, fmt.Errorf("%w: recommendation %s is %s on %s of class %q, outcome %q, citing %d settings",
			ErrNotConfiguration, recommendation, rec.Kind, rec.ObjectType, class, rec.Outcome, len(cited))
	}
	v, err := apitools.ActiveConfig(ctx, a.API)
	if err != nil {
		return recommend.Result{}, err
	}
	active := config.Readable(v.Document)
	var paths []string
	for _, m := range cited {
		if value, ok := config.Setting(active, m[1]); !ok || value != m[2] {
			return recommend.Result{}, fmt.Errorf("%w: %s = %s, version %d is active", ErrFixStale, m[1], m[2], v.Number)
		}
		paths = append(paths, m[1])
	}
	f := &fixing{active: active, draft: (&changedraft.Draft{API: a.API, Poll: a.Poll, Patience: a.Patience, Base: v.Number,
		Active: active, Scope: oneOf(paths), Objects: map[string]string{}}).Start(ctx)}
	task := recommend.Task{
		Purpose: fmt.Sprintf("Draft the fix in configuration version %d of a root cause a user accepted", v.Number),
		Prompt: fmt.Sprintf("The tenant's active configuration is version %d (id %s). The accepted diagnosis %s cites the settings %s:\n<diagnosis>\n%s\n</diagnosis>\n"+
			"Once you have created the change request, you may recommend on object_type %s, object_id its id, with one of the kinds: %s, %s.",
			v.Number, v.ID, rec.ID, strings.Join(paths, ", "), rec.Content, changedraft.Object, changedraft.ReadyToSubmit, changedraft.SimulationFailed),
		Objects: f.draft.Objects,
		Guards:  f.draft.Guards(),
		Observe: f.draft.Observers(),
		Check:   f.check,
	}
	return fixer(a.MaxIterations).Run(ctx, a.LLM, a.API, task)
}

// oneOf is the fix's scope: the draft changes one setting, one of the
// paths the diagnosis cites.
func oneOf(paths []string) func([]config.Change) error {
	return func(changes []config.Change) error {
		if len(changes) != 1 || !slices.Contains(paths, changes[0].Path) {
			changed := make([]string, len(changes))
			for i, ch := range changes {
				changed[i] = ch.Path
			}
			return fmt.Errorf("%w: one of %s, not %s", changedraft.ErrOutside, strings.Join(paths, ", "), strings.Join(changed, ", "))
		}
		return nil
	}
}

// fixer is the fix as the pattern runs it.
func fixer(maxIterations int) recommend.Agent {
	return changedraft.Agent(Type, FixReads, fixSystem, maxIterations)
}

// FixTools are the operations the agent may call when it drafts a fix: its
// reads, the draft's writes and the one that recommends.
func FixTools() []string { return fixer(0).Tools() }

// fixing is what one fix's check knows: the active configuration and the
// draft.
type fixing struct {
	active *config.Config
	draft  *changedraft.Draft
}

// check is the fix's rule on a recommendation the pattern admits
// (T108c): its kind is the outcome of the simulation of the draft's last
// version, its first line is the draft's change, every other setting it
// cites is the active configuration's and every other number is one the
// run read.
func (f *fixing) check(p recommend.Proposed, figures *recommend.Figures) error {
	if err := f.draft.Outcome(p.Kind); err != nil {
		return err
	}
	ch := f.draft.Changes()[0] // a draft whose simulation finished was created, with the one change its scope admits
	first, rest, _ := strings.Cut(p.Content, "\n")
	if strings.TrimSpace(first) != fmt.Sprintf("Change: `%s` from `%s` to `%s`", ch.Path, ch.Old, ch.New) {
		return fmt.Errorf("%w: Change: `%s` from `%s` to `%s`", ErrChange, ch.Path, ch.Old, ch.New)
	}
	for _, m := range citationRe.FindAllStringSubmatch(rest, -1) {
		if v, ok := config.Setting(f.active, m[1]); !ok || v != m[2] {
			return fmt.Errorf("%w: not %s = %s", ErrSetting, m[1], m[2])
		}
	}
	if unread := figures.Unread(citationRe.ReplaceAllString(rest, " ")); len(unread) > 0 {
		return fmt.Errorf("%w: not %s", ErrDiagnosisNumber, strings.Join(unread, ", "))
	}
	return nil
}
