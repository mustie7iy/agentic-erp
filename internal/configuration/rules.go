package configuration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/rules"
)

// activeRules is an entity's rules in a configuration document, with the
// entity's custom field schema they read (rules.Active).
func activeRules(c *config.Config, version int32, entity string) rules.Active {
	a := rules.Active{ConfigVersion: version, Rules: c.Rules[entity]}
	if s, ok := c.Extensions[entity]; ok {
		a.Schema = &s
	}
	return a
}

// ruleEntities are the entities whose rules the proposal changes, or whose
// custom field schema it changes while rules read it.
func ruleEntities(base, proposed *config.Config) []string {
	var out []string
	for _, e := range rules.Entities() {
		b, p := activeRules(base, 0, e), activeRules(proposed, 0, e)
		hasRules := len(b.Rules) > 0 || len(p.Rules) > 0
		if hasRules && (!reflect.DeepEqual(b.Rules, p.Rules) || !reflect.DeepEqual(b.Schema, p.Schema)) {
			out = append(out, e)
		}
	}
	return out
}

// replayRules replays the proposal's rule changes on each changed entity's
// recent rows (ADR-0046, T58h): what each rule would newly refuse or pass,
// and where it cannot be evaluated, which fails the simulation. It adds
// the replays to the simulation with the hash of the proposal they judged,
// which activation checks.
func (w *SimulationWorker) replayRules(ctx context.Context, c ChangeRequest, base, proposed *config.Config, sim Simulation) (Simulation, error) {
	sim.Detail = maps.Clone(sim.Detail)
	if sim.Detail == nil {
		sim.Detail = map[string]any{}
	}
	sim.Detail["proposal_sha256"] = proposalHash(c.Proposed)
	entities := ruleEntities(base, proposed)
	if len(entities) == 0 {
		return sim, nil
	}
	since := clock.Now(w.Now).Add(-rules.SampleWindow)
	replays := make([]rules.Replay, 0, len(entities))
	for _, e := range entities {
		r, err := w.replayEntity(ctx, e, since, activeRules(base, c.BaseVersion, e), activeRules(proposed, 0, e))
		if err != nil {
			return Simulation{}, err
		}
		replays = append(replays, r)
		sim.Passed = sim.Passed && r.Passed
		sim.Summary += " " + replaySummary(r)
	}
	sim.Detail["rules"] = replays
	return sim, nil
}

func (w *SimulationWorker) replayEntity(ctx context.Context, entity string, since time.Time, base, proposed rules.Active) (rules.Replay, error) {
	s, ok := w.Dependencies.RuleSamplers[entity]
	if !ok { // fails closed: wiring that forgot a reader must not pass a rule unsimulated
		return rules.Replay{}, fmt.Errorf("configuration: no rule sampler for %s", entity)
	}
	samples, err := s.Sample(ctx, since, rules.SampleLimit)
	if err != nil {
		return rules.Replay{}, fmt.Errorf("configuration: sample %s rows: %w", entity, err)
	}
	r, err := rules.Simulate(ctx, entity, base, proposed, samples)
	return r, errs.Wrapf(err, "configuration: simulate the %s rules", entity)
}

// replaySummary is one line on an entity's replay for approvers.
func replaySummary(r rules.Replay) string {
	var refused, passing, failing int
	for _, x := range r.Rules {
		refused, passing, failing = refused+x.NewlyRefused, passing+x.NewlyPassing, failing+x.Unevaluable
	}
	s := fmt.Sprintf("Rules on %s, %d recent rows: %d newly refused, %d newly passing.", r.Entity, r.Rows, refused, passing)
	if failing > 0 {
		s += fmt.Sprintf(" %d evaluations failed or exceeded a limit: the rules cannot activate.", failing)
	}
	return s
}

// proposalHash names a proposal exactly: the evidence of a simulation
// carries it, so that it vouches for this proposal and no other.
func proposalHash(proposed json.RawMessage) string {
	h := sha256.Sum256(proposed)
	return hex.EncodeToString(h[:])
}

// requireRulesSimulated refuses to activate a change of rules without a
// passed simulation of this exact proposal (ADR-0046, T58h). A change
// request's proposal is fixed once submitted, so this holds by its
// lifecycle; checking it here keeps it so should the lifecycle change.
func requireRulesSimulated(c ChangeRequest) error {
	if !slices.ContainsFunc(c.Items, func(i Item) bool { return strings.HasPrefix(i.Path, "rules.") }) {
		return nil
	}
	want := proposalHash(c.Proposed)
	for _, e := range c.Evidence {
		var d struct {
			ProposalSHA256 string `json:"proposal_sha256"`
		}
		// A detail that does not decode names no proposal: no evidence.
		if err := json.Unmarshal(e.Detail, &d); err == nil && e.Kind == "simulation" && e.Passed && d.ProposalSHA256 == want {
			return nil
		}
	}
	return problem.Conflict("config.rules_not_simulated",
		"The change adds or changes validation rules and has no passed simulation of this proposal; simulate it before it activates.")
}
