package rules

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/extensions"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Source reads a tenant's active rules (configuration context).
type Source interface {
	// Rules returns the entity's rules in the configuration version the
	// request's transaction reads, with that version's custom field schema
	// of the entity and its number; a tenant without rules has none.
	Rules(ctx context.Context, entity string) (Active, error)
}

// Active is an entity's rules in one configuration version. Rules have no
// version of their own: the configuration version names them (ADR-0046).
type Active struct {
	ConfigVersion int32
	Schema        *extensions.Schema // nil when the entity has no custom fields
	Rules         map[string]Rule
}

// Writes evaluates an entity's active rules on its writes (ADR-0046).
type Writes struct {
	Entity string
	Source Source
}

// Write is one write the rules judge: the action its audit names, the
// object (its type, when not the action's own, and its id, uuid.Nil for a
// create), and the row's header fields and extensions as the write leaves
// them, in the inputs' shapes (a Money for an amount the write path
// parsed).
type Write struct {
	Action     string
	ObjectType string // "": audit.ObjectTypeOf(Action)
	ID         uuid.UUID
	Row        map[string]any
	Extensions map[string]any
}

// Check evaluates the entity's active rules on a write whose fields and
// custom fields passed their own checks. A refusal is 422
// request.rule_refused with one field error per refusing rule
// (rules.<name>, the rule's message), and each refusal is audited with
// source rule, the rule's name and the configuration version. The write
// rolls back; the audit is kept (audit.Refuse).
func (w Writes) Check(ctx context.Context, wr Write) error {
	vs, version, err := w.violations(ctx, wr)
	if u := (*UnevaluableError)(nil); errors.As(err, &u) {
		return unevaluableProblem(u)
	}
	if err != nil || len(vs) == 0 {
		return err
	}
	if err := record(ctx, audit.Refuse, wr, "refused", vs, version); err != nil {
		return err
	}
	errs := make([]problem.FieldError, len(vs))
	for i, v := range vs {
		errs[i] = problem.FieldError{Field: "rules." + v.Rule, Code: "rule_refused", Message: v.Message}
	}
	return problem.Unprocessable("request.rule_refused", "A validation rule of the tenant refuses the write.", errs...)
}

// Hold evaluates the rules on a write that lands held rather than refused,
// such as a captured document a person completes later. It returns the
// hold reason naming the refusing rules, "" when none refuses, and audits
// each refusal as held, in the write's transaction. A rule that cannot be
// evaluated holds the write too, naming the rule, rather than refuse it:
// a captured document is never lost (coordinator, 2026-10-20); it is
// logged and counted as on any write.
func (w Writes) Hold(ctx context.Context, wr Write) (string, error) {
	vs, version, err := w.violations(ctx, wr)
	if u := (*UnevaluableError)(nil); errors.As(err, &u) {
		vs, err = []Violation{{Rule: u.Rule, Message: "the rule " + failure(u.Err) + "; have the rule corrected", failure: failure(u.Err)}}, nil
	}
	if err != nil || len(vs) == 0 {
		return "", err
	}
	if err := record(ctx, audit.Add, wr, "held", vs, version); err != nil {
		return "", err
	}
	reasons := make([]string, len(vs))
	for i, v := range vs {
		verb := "refuses it"
		if v.failure != "" {
			verb = "cannot be evaluated on it"
		}
		reasons[i] = fmt.Sprintf("Rule %s %s: %s", v.Rule, verb, strings.TrimSuffix(v.Message, "."))
	}
	return strings.Join(reasons, ". ") + ".", nil
}

func record(ctx context.Context, add func(context.Context, audit.Entry) error, wr Write, outcome string, vs []Violation, version int32) error {
	for _, v := range vs {
		detail := map[string]any{"source": "rule", "rule": v.Rule, "config_version": version}
		if v.failure != "" {
			detail["failure"] = v.failure
		}
		if err := add(ctx, audit.Entry{Action: wr.Action, ObjectType: cmp.Or(wr.ObjectType, audit.ObjectTypeOf(wr.Action)), ObjectID: wr.ID, Outcome: outcome,
			Detail: detail}); err != nil {
			return fmt.Errorf("rules: audit %s refused by %s: %w", wr.Action, v.Rule, err)
		}
	}
	return nil
}

// violations returns the rules that refuse the write and the version of
// the configuration that holds them; a rule that cannot be evaluated is an
// *UnevaluableError, logged and counted here, with the version.
func (w Writes) violations(ctx context.Context, wr Write) ([]Violation, int32, error) {
	if w.Source == nil { // fails closed: wiring that forgot the reader must not skip the rules
		return nil, 0, fmt.Errorf("rules: no rule reader for %s", w.Entity)
	}
	_, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, 0, err
	}
	a, err := w.Source.Rules(ctx, w.Entity)
	if err != nil {
		return nil, 0, fmt.Errorf("rules: read the %s rules: %w", w.Entity, err)
	}
	if len(a.Rules) == 0 {
		return nil, 0, nil
	}
	s, err := compiledSet(setKey{p.TenantID, a.ConfigVersion, w.Entity}, a)
	if err != nil {
		return nil, 0, err
	}
	vs, err := s.Check(ctx, wr.Row, wr.Extensions)
	if u := (*UnevaluableError)(nil); errors.As(err, &u) {
		unevaluable(ctx, p.TenantID, a.ConfigVersion, u)
		return nil, a.ConfigVersion, u
	}
	if err != nil {
		return nil, 0, fmt.Errorf("rules: configuration version %d: %w", a.ConfigVersion, err)
	}
	return vs, a.ConfigVersion, nil
}

// Unevaluable counts the writes refused or held because a rule could not
// be evaluated, by entity; the process registers it with its metrics. The
// rule and the tenant are in the log line, not labels, so the series stay
// few however many rules tenants write.
var Unevaluable = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "erp_rules_unevaluable_total", Help: "Writes refused or held because a tenant's validation rule could not be evaluated, by entity.",
}, []string{"entity"})

// unevaluable logs and counts a rule that failed or exceeded a limit on a
// write's input (ADR-0046), so that operations see it.
func unevaluable(ctx context.Context, tenant uuid.UUID, version int32, u *UnevaluableError) {
	Unevaluable.WithLabelValues(u.Entity).Inc()
	slog.WarnContext(ctx, "a validation rule could not be evaluated on a write", "tenant_id", tenant,
		"entity", u.Entity, "rule", u.Rule, "config_version", version, "failure", failure(u.Err))
}

// unevaluableProblem fails a write closed on a rule that cannot be
// evaluated: neither a pass nor a 500, but 422 request.rule_unevaluable
// naming the rule, so the caller knows which rule to have fixed.
func unevaluableProblem(u *UnevaluableError) error {
	return problem.Unprocessable("request.rule_unevaluable", "A validation rule of the tenant cannot be evaluated on the write.",
		problem.FieldError{Field: "rules." + u.Rule, Code: "rule_unevaluable", Message: "The rule " + failure(u.Err) + "; have the rule corrected."})
}

// setKey names an entity's rules in one tenant's configuration version.
// Versions are immutable, so a set compiled for a key is its set for good.
type setKey struct {
	tenant  uuid.UUID
	version int32
	entity  string
}

// maxCached bounds the compiled sets kept; past it the cache starts over,
// which costs a compilation per key, never a wrong set.
const maxCached = 1024

var cache = struct {
	sync.Mutex
	sets map[setKey]*Set
}{sets: map[setKey]*Set{}}

// compiledSet compiles an entity's active rules once per configuration
// version: compiling runs every rule's cases, which a write need not
// repeat.
func compiledSet(k setKey, a Active) (*Set, error) {
	cache.Lock()
	s, ok := cache.sets[k]
	cache.Unlock()
	if ok {
		return s, nil
	}
	s, _, err := compileActive(k.entity, a)
	if err != nil {
		return nil, err
	}
	cache.Lock()
	defer cache.Unlock()
	if len(cache.sets) >= maxCached {
		clear(cache.sets)
	}
	cache.sets[k] = s
	return s, nil
}

// compileActive compiles an entity's rules in one configuration version
// with the custom field schema they read, returned too (nil without one).
// The configuration context refuses rules that do not compile, or whose
// cases fail, before they activate, so a failure here is an error.
func compileActive(entity string, a Active) (*Set, *extensions.Compiled, error) {
	var schema *extensions.Compiled
	if a.Schema != nil {
		var issues []extensions.Issue
		if schema, issues = extensions.Compile(entity, *a.Schema); len(issues) > 0 {
			return nil, nil, fmt.Errorf("rules: the %s schema of configuration version %d does not compile: %v", entity, a.ConfigVersion, issues)
		}
	}
	s, issues := Compile(entity, schema, a.Rules)
	if len(issues) > 0 {
		return nil, nil, fmt.Errorf("rules: the %s rules of configuration version %d do not compile: %v", entity, a.ConfigVersion, issues)
	}
	return s, schema, nil
}

// Text, ID and Date are optional inputs as a write path holds them: absent
// when nil (a typed nil in an any would read as present), else the input's
// string.
func Text(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// ID is an optional id input; see Text.
func ID(id *uuid.UUID) any {
	if id == nil {
		return nil
	}
	return id.String()
}

// Date is an optional date input, YYYY-MM-DD; see Text.
func Date(d *time.Time) any {
	if d == nil {
		return nil
	}
	return d.Format(time.DateOnly)
}
