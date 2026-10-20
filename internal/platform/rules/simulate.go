package rules

import (
	"context"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/extensions"
)

// The bounds of a simulation (ADR-0046, T58h): the rows of the last
// SampleWindow, newest first, at most SampleLimit per entity, so that a
// simulation's cost does not grow with the tenant's history: at most
// SampleLimit × MaxRules × 2 evaluations per entity, each within its cost
// and time limits.
const (
	SampleWindow = 30 * 24 * time.Hour
	SampleLimit  = 200
	// examples is how many rows of each kind a rule's report lists; the
	// counts are complete.
	examples = 20
)

// Sample is a stored row of an entity as its rules read it: its header
// fields in the inputs' shapes and its custom fields, with the id and the
// reference (document or partner number) a report names it by.
type Sample struct {
	ID         uuid.UUID
	Reference  string
	Row        map[string]any
	Extensions map[string]any
}

// Sampler reads an entity's recent rows for a simulation, in the
// request's transaction: those written since the time given, newest first,
// at most limit (the context that owns the entity implements it).
type Sampler interface {
	Sample(ctx context.Context, since time.Time, limit int) ([]Sample, error)
}

// Replay is what an entity's proposed rules would do to its recent rows,
// compared with its active rules: per rule, the rows it would newly refuse
// and those it would newly pass, and the rows it cannot evaluate. Passed
// is false when any proposed rule cannot be evaluated on a row: such a
// rule would refuse writes with request.rule_unevaluable, so the change
// may not activate.
//
// A row whose stored custom fields the proposed schema does not accept
// (a key written before T58c, a value of a narrowed field) is skipped:
// its next write is refused by the custom field check before any rule
// reads it. Skipped counts them, with examples.
type Replay struct {
	Entity      string       `json:"entity"`
	Rows        int          `json:"rows"`
	Passed      bool         `json:"passed"`
	Rules       []RuleReplay `json:"rules"`
	Skipped     int          `json:"skipped"`
	SkippedRows []SampleRef  `json:"skipped_rows,omitempty"`
}

// RuleReplay is one rule's outcome on the sample; only rules whose outcome
// changes on some row, or that fail on one, are listed.
type RuleReplay struct {
	Rule          string      `json:"rule"`
	NewlyRefused  int         `json:"newly_refused"`
	NewlyPassing  int         `json:"newly_passing"`
	Unevaluable   int         `json:"unevaluable"`
	RefusedRows   []SampleRef `json:"refused_rows,omitempty"`
	PassingRows   []SampleRef `json:"passing_rows,omitempty"`
	ErrorRows     []SampleRef `json:"error_rows,omitempty"`
	changed, fail bool
}

// SampleRef names a row in a report: its id and reference, and its custom
// fields with every value not declared `x-pii: none` masked, since the
// report is shown to approvers. Header fields are left out: a partner's
// name may be personal data. Error is why the rule could not be evaluated.
type SampleRef struct {
	ID         uuid.UUID      `json:"id"`
	Reference  string         `json:"reference"`
	Extensions map[string]any `json:"extensions"`
	Error      string         `json:"error,omitempty"`
}

// outcome is a rule's verdict on one row: refused, or failed with err.
type outcome struct {
	refused bool
	err     error
}

// Simulate replays an entity's sample under its active (base) and its
// proposed rules. A base rule that fails on a row counts as accepting it:
// it is the proposal that is judged.
func Simulate(ctx context.Context, entity string, base, proposed Active, samples []Sample) (Replay, error) {
	before, _, err := compileActive(entity, base)
	if err != nil {
		return Replay{}, err
	}
	after, schema, err := compileActive(entity, proposed)
	if err != nil {
		return Replay{}, err
	}
	reports := map[string]*RuleReplay{}
	for _, name := range slices.Concat(slices.Collect(maps.Keys(base.Rules)), slices.Collect(maps.Keys(proposed.Rules))) {
		reports[name] = &RuleReplay{Rule: name}
	}
	out := Replay{Entity: entity, Rows: len(samples), Passed: true, Rules: []RuleReplay{}}
	for _, sm := range samples {
		ref := SampleRef{ID: sm.ID, Reference: sm.Reference, Extensions: mask(schema, sm.Extensions)}
		is, err := after.verdicts(ctx, sm)
		if err != nil {
			out.Skipped++
			ref.Error = err.Error()
			out.SkippedRows = appendExample(out.SkippedRows, ref)
			continue
		}
		was, _ := before.verdicts(ctx, sm) // a row the base cannot read was refused by nothing it can show: every base verdict is the zero, accepting one
		for name, r := range reports {
			r.add(was[name], is[name], ref)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(reports)) {
		r := reports[name]
		out.Passed = out.Passed && !r.fail
		if r.changed || r.fail {
			out.Rules = append(out.Rules, *r)
		}
	}
	return out, nil
}

// verdicts evaluates every rule of the set on a row, each on its own: a
// failing rule does not hide the others. A row whose inputs do not bind
// is an error.
func (s *Set) verdicts(ctx context.Context, sm Sample) (map[string]outcome, error) {
	act, err := s.bind(sm.Row, sm.Extensions)
	if err != nil {
		return nil, err
	}
	out := make(map[string]outcome, len(s.rules))
	for _, r := range s.rules {
		ok, err := r.eval(ctx, act)
		out[r.name] = outcome{refused: !ok, err: err}
	}
	return out, nil
}

// add records a row's verdicts by the base and the proposed rule of one
// name (the zero outcome, accepting, where a side has no such rule).
func (r *RuleReplay) add(was, is outcome, ref SampleRef) {
	switch {
	case is.err != nil:
		r.fail = true
		r.Unevaluable++
		ref.Error = is.err.Error()
		r.ErrorRows = appendExample(r.ErrorRows, ref)
	case is.refused && (!was.refused || was.err != nil):
		r.changed = true
		r.NewlyRefused++
		r.RefusedRows = appendExample(r.RefusedRows, ref)
	case !is.refused && was.refused && was.err == nil:
		r.changed = true
		r.NewlyPassing++
		r.PassingRows = appendExample(r.PassingRows, ref)
	}
}

func appendExample(rows []SampleRef, ref SampleRef) []SampleRef {
	if len(rows) < examples {
		rows = append(rows, ref)
	}
	return rows
}

// mask masks a row's custom fields per the schema's x-pii; without a
// schema every key is unknown, so every value is masked.
func mask(schema *extensions.Compiled, ext map[string]any) map[string]any {
	if schema == nil {
		schema = &extensions.Compiled{}
	}
	return schema.Mask(ext)
}
