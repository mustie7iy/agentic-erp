package agents

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Policy is a tenant's AI policy (agents.ai_policy, ADR-0036). A tenant
// that never changed it has the defaults, version 1.
type Policy struct {
	Enabled           bool    // the tenant's administrators' hand on the kill switch
	Suspended         bool    // the operators' hand, which wins
	SuspendedReason   *string // why, while suspended
	MonthlyCap        *money.Minor
	Routes            map[string]string // alias → model, over the platform's routes
	AllowedModels     []string          // empty: any priced model
	DocumentsToModels bool
	UpdatedAt         time.Time
	UpdatedBy         *uuid.UUID
	Version           int32
}

// Active reports whether the tenant's AI runs: enabled and not suspended.
func (p Policy) Active() bool { return p.Enabled && !p.Suspended }

// Route is the model alias routes to under the policy, over routes, the
// platform's; ok false for an alias neither routes.
func (p Policy) Route(alias string, routes map[string]string) (model string, ok bool) {
	if m, ok := p.Routes[alias]; ok {
		return m, true
	}
	m, ok := routes[alias]
	return m, ok
}

// Allows reports whether the policy lets its aliases route to model.
func (p Policy) Allows(model string) bool {
	return len(p.AllowedModels) == 0 || slices.Contains(p.AllowedModels, model)
}

// defaultPolicy is the policy of a tenant that never changed its own.
func defaultPolicy() Policy {
	return Policy{Enabled: true, DocumentsToModels: true, Routes: map[string]string{}, AllowedModels: []string{}, Version: 1}
}

const policyColumns = `enabled, suspended, suspended_reason, monthly_cap, routes, allowed_models, documents_to_models, updated_at, updated_by, version`

// ReadPolicy reads the tenant's policy, the defaults when it has none.
func ReadPolicy(ctx context.Context, tx pgx.Tx) (Policy, error) {
	return readPolicy(ctx, tx, false)
}

func readPolicy(ctx context.Context, tx pgx.Tx, lock bool) (Policy, error) {
	sql := `SELECT ` + policyColumns + ` FROM agents.ai_policy`
	if lock {
		sql += ` FOR UPDATE`
	}
	p, err := scanPolicy(tx.QueryRow(ctx, sql))
	if errors.Is(err, pgx.ErrNoRows) {
		return defaultPolicy(), nil
	}
	if err != nil {
		return p, fmt.Errorf("agents: read the AI policy: %w", err)
	}
	return p, nil
}

func scanPolicy(row pgx.Row) (Policy, error) {
	var p Policy
	err := row.Scan(&p.Enabled, &p.Suspended, &p.SuspendedReason, &p.MonthlyCap, &p.Routes, &p.AllowedModels, &p.DocumentsToModels,
		&p.UpdatedAt, &p.UpdatedBy, &p.Version)
	return p, err
}

// PolicyChange is a merge patch of the policy: nil fields are unchanged.
type PolicyChange struct {
	Enabled           *bool
	MonthlyCap        **money.Minor // set to a nil pointer: no cap
	Routes            *map[string]string
	AllowedModels     *[]string
	DocumentsToModels *bool
}

// Fields names the fields the change sets, sorted.
func (ch PolicyChange) Fields() []string {
	var out []string
	for name, set := range map[string]bool{"enabled": ch.Enabled != nil, "monthly_cap": ch.MonthlyCap != nil, "routes": ch.Routes != nil,
		"allowed_models": ch.AllowedModels != nil, "documents_to_models": ch.DocumentsToModels != nil} {
		if set {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// UpdatePolicy changes the tenant's policy, guarded by ifMatch, its row
// held FOR UPDATE. Routes name only the platform's aliases and models with
// a recorded price, within the allowed models.
func UpdatePolicy(ctx context.Context, tx pgx.Tx, actor uuid.UUID, ifMatch func(int32) error, ch PolicyChange) (Policy, error) {
	p, err := lockPolicy(ctx, tx, ifMatch)
	if err != nil {
		return p, err
	}
	p = ch.apply(p)
	if err := validRouting(ctx, tx, p); err != nil {
		return p, err
	}
	return writePolicy(ctx, tx, actor, p)
}

// Suspend switches the tenant's AI off on the operators' hand, whatever
// the tenant's administrators set, until an operator resumes it.
func Suspend(ctx context.Context, tx pgx.Tx, actor uuid.UUID, reason string) (Policy, error) {
	if len(reason) == 0 || len(reason) > 500 {
		return Policy{}, invalidPolicy("reason", "invalid_length", "A suspension needs its reason, 1 to 500 characters.")
	}
	p, err := lockPolicy(ctx, tx, func(int32) error { return nil })
	if err != nil {
		return p, err
	}
	p.Suspended, p.SuspendedReason = true, &reason
	return writePolicy(ctx, tx, actor, p)
}

// Resume lifts the operators' suspension; the tenant's own setting stands.
func Resume(ctx context.Context, tx pgx.Tx, actor uuid.UUID) (Policy, error) {
	p, err := lockPolicy(ctx, tx, func(int32) error { return nil })
	if err != nil {
		return p, err
	}
	p.Suspended, p.SuspendedReason = false, nil
	return writePolicy(ctx, tx, actor, p)
}

// lockPolicy creates the tenant's row with the defaults when it has none,
// so there is a row to lock, and reads it FOR UPDATE.
func lockPolicy(ctx context.Context, tx pgx.Tx, ifMatch func(int32) error) (Policy, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO agents.ai_policy (tenant_id) VALUES (platform.current_tenant()) ON CONFLICT DO NOTHING`); err != nil {
		return Policy{}, fmt.Errorf("agents: create the AI policy: %w", err)
	}
	p, err := readPolicy(ctx, tx, true)
	if err != nil {
		return p, err
	}
	return p, ifMatch(p.Version)
}

func writePolicy(ctx context.Context, tx pgx.Tx, actor uuid.UUID, p Policy) (Policy, error) {
	var currency *string
	if p.MonthlyCap != nil {
		c := PriceCurrency
		currency = &c
	}
	out, err := scanPolicy(tx.QueryRow(ctx, `UPDATE agents.ai_policy SET enabled = $1, suspended = $2, suspended_reason = $3, monthly_cap = $4,
		cap_currency = $5, routes = $6, allowed_models = $7, documents_to_models = $8, updated_by = $9, version = version + 1 RETURNING `+policyColumns,
		p.Enabled, p.Suspended, p.SuspendedReason, p.MonthlyCap, currency, p.Routes, p.AllowedModels, p.DocumentsToModels, actor))
	if err != nil {
		return out, fmt.Errorf("agents: update the AI policy: %w", err)
	}
	return out, nil
}

func (ch PolicyChange) apply(p Policy) Policy {
	if ch.Enabled != nil {
		p.Enabled = *ch.Enabled
	}
	if ch.MonthlyCap != nil {
		p.MonthlyCap = *ch.MonthlyCap
	}
	if ch.Routes != nil {
		p.Routes = *ch.Routes
	}
	if ch.AllowedModels != nil {
		p.AllowedModels = *ch.AllowedModels
	}
	if ch.DocumentsToModels != nil {
		p.DocumentsToModels = *ch.DocumentsToModels
	}
	return p
}

// validRouting checks that every alias routes, under the policy, to a
// model it allows and that has a recorded price: a route the gateway would
// refuse on every call is refused when it is set.
func validRouting(ctx context.Context, tx pgx.Tx, p Policy) error {
	for alias := range p.Routes {
		if _, ok := DefaultRoutes[alias]; !ok {
			return invalidPolicy("routes", "unknown_alias", "The platform routes no model alias "+alias+".")
		}
	}
	for _, m := range p.AllowedModels {
		if !modelID.MatchString(m) {
			return invalidPolicy("allowed_models", "invalid", "Not a model id: "+m+".")
		}
	}
	aliases := make([]string, 0, len(DefaultRoutes))
	for alias := range DefaultRoutes {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	for _, alias := range aliases {
		model, _ := p.Route(alias, DefaultRoutes) // every alias has a platform route
		if !modelID.MatchString(model) {
			return invalidPolicy("routes", "invalid", "Not a model id: "+model+".")
		}
		if !p.Allows(model) {
			return invalidPolicy("routes", "not_allowed", "The alias "+alias+" routes to "+model+", which the allowed models leave out.")
		}
		var priced bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM ref.model_price WHERE model = $1)`, model).Scan(&priced); err != nil {
			return fmt.Errorf("agents: read the price of %s: %w", model, err)
		}
		if !priced {
			return invalidPolicy("routes", "unpriced", "The alias "+alias+" routes to "+model+", which has no recorded price.")
		}
	}
	return nil
}

func invalidPolicy(field, code, detail string) error {
	return problem.Unprocessable("agents.invalid_policy", detail, problem.FieldError{Field: field, Code: code, Message: detail})
}

// PolicyEvent is the policy's AiPolicyChanged event: the fields changed,
// by the tenant's administrators or the operators, and the settings after
// the change; the suspension's reason is the operators' words and stays
// out of it.
func PolicyEvent(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, p Policy, fields []string) (outbox.Event, error) {
	payload := map[string]any{"enabled": p.Enabled, "suspended": p.Suspended, "active": p.Active(), "routes": p.Routes,
		"allowed_models": p.AllowedModels, "documents_to_models": p.DocumentsToModels, "monthly_cap": nil}
	if p.MonthlyCap != nil {
		exp, err := currency.Exponent(ctx, tx, PriceCurrency, nil)
		if err != nil {
			return outbox.Event{}, fmt.Errorf("agents: AI policy event: %w", err)
		}
		payload["monthly_cap"] = money.EventAmount(*p.MonthlyCap, PriceCurrency, exp)
	}
	payload["changed_fields"] = fields
	return outbox.Event{Context: "agents", Name: "ai_policy_changed", AggregateType: "agents.ai_policy", AggregateID: tenant, Payload: payload}, nil
}
