package identity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Principal kinds and statuses.
const (
	Human   = "human"
	Agent   = "agent"
	Service = "service"

	Active        = "active"
	Blocked       = "blocked"
	Deprovisioned = "deprovisioned"
)

// AgentProfile is the settings row of an agent.
type AgentProfile struct {
	AutonomyLevel  int16
	Model          string
	Description    string
	MonthlyBudget  *int64 // minor units of BudgetCurrency; nil: unlimited
	BudgetCurrency *string
}

// Budget is a monthly budget as a decimal amount and currency.
type Budget struct{ Amount, Currency string }

// Principal is a row of identity.principal with the agent's profile.
type Principal struct {
	ID              uuid.UUID
	Kind            string
	ExternalSubject *string
	DisplayName     string
	Email           *string
	AllCompanies    bool
	Status          string
	Profile         *AgentProfile
	BudgetDecimal   *Budget // the profile's budget formatted for the API
	Version         int32
	CreatedAt       time.Time
	CreatedBy       uuid.UUID
	UpdatedAt       time.Time
	UpdatedBy       uuid.UUID
}

const principalSelect = `SELECT p.id, p.kind, p.external_subject, p.display_name, p.email, p.all_companies, p.status, p.version,
		p.created_at, p.created_by, p.updated_at, p.updated_by,
		ap.principal_id IS NOT NULL, coalesce(ap.autonomy_level, 0), coalesce(ap.model, ''), coalesce(ap.description, ''),
		ap.monthly_budget, ap.budget_currency, coalesce(c.exponent, 0)
	FROM identity.principal p
	LEFT JOIN identity.agent_profile ap ON ap.tenant_id = p.tenant_id AND ap.principal_id = p.id
	LEFT JOIN ref.currency c ON c.code = ap.budget_currency`

func scanPrincipal(row pgx.Row) (Principal, error) {
	var p Principal
	var hasProfile bool
	var ap AgentProfile
	var exponent int16
	err := row.Scan(&p.ID, &p.Kind, &p.ExternalSubject, &p.DisplayName, &p.Email, &p.AllCompanies, &p.Status, &p.Version,
		&p.CreatedAt, &p.CreatedBy, &p.UpdatedAt, &p.UpdatedBy,
		&hasProfile, &ap.AutonomyLevel, &ap.Model, &ap.Description, &ap.MonthlyBudget, &ap.BudgetCurrency, &exponent)
	if hasProfile {
		p.Profile = &ap
		if ap.MonthlyBudget != nil {
			p.BudgetDecimal = &Budget{Amount: money.Minor(*ap.MonthlyBudget).Plain(int(exponent)), Currency: *ap.BudgetCurrency}
		}
	}
	return p, errs.Wrapf(err, "scan principal")
}

func getPrincipal(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (Principal, error) {
	q := principalSelect + ` WHERE p.id = $1`
	if lock {
		q += ` FOR UPDATE OF p`
	}
	p, err := scanPrincipal(tx.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return p, notFound("principal")
	}
	return p, errs.Wrapf(err, "identity: read principal %s", id)
}

// ProfileInput is an agent profile as the API sends it.
type ProfileInput struct {
	AutonomyLevel int16
	Model         string
	Description   string
	Budget        *Budget
}

// profile converts the input, parsing the budget with the currency's
// minor-unit digits.
func profile(ctx context.Context, tx pgx.Tx, in ProfileInput) (AgentProfile, error) {
	ap := AgentProfile{AutonomyLevel: in.AutonomyLevel, Model: in.Model, Description: in.Description}
	if in.Budget == nil {
		return ap, nil
	}
	var exponent int16
	err := tx.QueryRow(ctx, `SELECT exponent FROM ref.currency WHERE code = $1`, in.Budget.Currency).Scan(&exponent)
	if errors.Is(err, pgx.ErrNoRows) {
		return ap, problem.Unprocessable("identity.unknown_currency", "The budget currency is not known.",
			problem.FieldError{Field: "agent_profile.monthly_budget.currency", Code: "unknown", Message: "not an ISO 4217 currency in the reference data"})
	}
	if err != nil {
		return ap, fmt.Errorf("identity: read exponent of currency %s: %w", in.Budget.Currency, err)
	}
	amount, err := money.ParseDecimal(in.Budget.Amount, int(exponent))
	if err != nil || amount < 0 {
		return ap, problem.Unprocessable("request.invalid_amount", "The budget amount is not a valid non-negative amount in this currency.",
			problem.FieldError{Field: "agent_profile.monthly_budget.amount", Code: "invalid", Message: "decimal string with the currency's minor-unit digits"})
	}
	v := int64(amount)
	ap.MonthlyBudget, ap.BudgetCurrency = &v, &in.Budget.Currency
	return ap, nil
}

// NewPrincipal is the input of CreatePrincipal.
type NewPrincipal struct {
	Kind            string
	ExternalSubject *string
	DisplayName     string
	Email           *string
	AllCompanies    bool
	Profile         *ProfileInput
}

func invalidKind() error {
	return problem.Unprocessable("identity.unknown_principal_kind", "Unknown principal kind.",
		problem.FieldError{Field: "kind", Code: "unknown", Message: "human, agent or service"})
}

// CreatePrincipal inserts an active principal; agents get their profile.
func CreatePrincipal(ctx context.Context, in NewPrincipal) (Principal, error) {
	tx, actor, err := reqctx.Request(ctx)
	if err != nil {
		return Principal{}, err
	}
	switch {
	case in.Kind != Human && in.Kind != Agent && in.Kind != Service:
		return Principal{}, invalidKind()
	case in.Kind != Agent && in.ExternalSubject == nil:
		return Principal{}, problem.Unprocessable("identity.subject_required", "People and services need their identity provider subject.",
			problem.FieldError{Field: "external_subject", Code: "required", Message: "required unless kind is agent"})
	case (in.Kind == Agent) != (in.Profile != nil):
		return Principal{}, problem.Unprocessable("identity.agent_profile", "An agent needs an agent profile; other principals cannot have one.",
			problem.FieldError{Field: "agent_profile", Code: "agents_only", Message: "required for agents, not allowed otherwise"})
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO identity.principal (tenant_id, id, kind, external_subject, display_name, email, all_companies, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'active', $8, $8)`,
		actor.TenantID, id, in.Kind, in.ExternalSubject, in.DisplayName, in.Email, in.AllCompanies, actor.ID); err != nil {
		return Principal{}, fmt.Errorf("identity: insert principal %s: %w", id, err)
	}
	var events []outbox.Event
	if in.Profile != nil {
		ap, err := profile(ctx, tx, *in.Profile)
		if err != nil {
			return Principal{}, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO identity.agent_profile (tenant_id, principal_id, autonomy_level, model, monthly_budget, budget_currency,
			description, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)`,
			actor.TenantID, id, ap.AutonomyLevel, ap.Model, ap.MonthlyBudget, ap.BudgetCurrency, ap.Description, actor.ID); err != nil {
			return Principal{}, fmt.Errorf("identity: insert agent profile of principal %s: %w", id, err)
		}
		events = append(events, profileEvent(id, "created", []string{"autonomy_level", "model", "description", "monthly_budget"}, in.Profile))
	}
	p, err := getPrincipal(ctx, tx, id, false)
	if err != nil {
		return Principal{}, err
	}
	events = append([]outbox.Event{{Context: "identity", Name: "principal_created", AggregateType: "identity.principal", AggregateID: id,
		Payload: map[string]any{"principal_id": id, "kind": p.Kind, "display_name": p.DisplayName, "all_companies": p.AllCompanies, "status": p.Status}}},
		events...)
	return p, audit.Record(ctx, "identity.principal.create", "identity.principal", id, map[string]any{"kind": p.Kind}, events...)
}

func profileEvent(id uuid.UUID, change string, fields []string, in *ProfileInput) outbox.Event {
	var budget any
	if in.Budget != nil {
		budget = map[string]any{"amount": in.Budget.Amount, "currency": in.Budget.Currency}
	}
	return outbox.Event{Context: "identity", Name: "agent_profile_changed", AggregateType: "identity.agent_profile", AggregateID: id,
		Payload: map[string]any{"principal_id": id, "change": change, "changed_fields": fields, "autonomy_level": in.AutonomyLevel,
			"model": in.Model, "description": in.Description, "monthly_budget": budget}}
}

// PrincipalChange is a merge patch of a principal.
type PrincipalChange struct {
	DisplayName  *string
	Email        **string // set: nil pointer value clears
	AllCompanies *bool
	Profile      *ProfileInput // replaces the whole profile (agents only)
}

// UpdatePrincipal changes a principal and, for agents, its profile.
func UpdatePrincipal(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, ch PrincipalChange) (Principal, error) {
	tx, actor, err := reqctx.Request(ctx)
	if err != nil {
		return Principal{}, err
	}
	p, err := getPrincipal(ctx, tx, id, true)
	if err != nil {
		return Principal{}, err
	}
	if err := ifMatch(p.Version); err != nil {
		return Principal{}, err
	}
	if ch.Profile != nil && p.Kind != Agent {
		return Principal{}, problem.Unprocessable("identity.agent_profile", "Only agents have an agent profile.",
			problem.FieldError{Field: "agent_profile", Code: "agents_only", Message: "not allowed for " + p.Kind + " principals"})
	}
	name, email, all := p.DisplayName, p.Email, p.AllCompanies
	if ch.DisplayName != nil {
		name = *ch.DisplayName
	}
	if ch.Email != nil {
		email = *ch.Email
	}
	if ch.AllCompanies != nil {
		all = *ch.AllCompanies
	}
	if _, err := tx.Exec(ctx, `UPDATE identity.principal SET display_name = $2, email = $3, all_companies = $4, version = version + 1, updated_by = $5
		WHERE id = $1`, id, name, email, all, actor.ID); err != nil {
		return Principal{}, fmt.Errorf("identity: update principal %s: %w", id, err)
	}
	fields := changedFields(p.DisplayName != name, "display_name", !sameString(p.Email, email), "email", p.AllCompanies != all, "all_companies")
	var events []outbox.Event
	if ch.Profile != nil {
		ap, err := profile(ctx, tx, *ch.Profile)
		if err != nil {
			return Principal{}, err
		}
		old := *p.Profile
		profileFields := changedFields(old.AutonomyLevel != ap.AutonomyLevel, "autonomy_level", old.Model != ap.Model, "model",
			old.Description != ap.Description, "description",
			!sameInt(old.MonthlyBudget, ap.MonthlyBudget) || !sameString(old.BudgetCurrency, ap.BudgetCurrency), "monthly_budget")
		if len(profileFields) > 0 {
			if _, err := tx.Exec(ctx, `UPDATE identity.agent_profile SET autonomy_level = $2, model = $3, description = $4, monthly_budget = $5,
				budget_currency = $6, version = version + 1, updated_by = $7 WHERE principal_id = $1`,
				id, ap.AutonomyLevel, ap.Model, ap.Description, ap.MonthlyBudget, ap.BudgetCurrency, actor.ID); err != nil {
				return Principal{}, fmt.Errorf("identity: update agent profile of principal %s: %w", id, err)
			}
			events = append(events, profileEvent(id, "updated", profileFields, ch.Profile))
			fields = append(fields, "agent_profile")
		}
	}
	updated, err := getPrincipal(ctx, tx, id, false)
	if err != nil {
		return Principal{}, err
	}
	return updated, audit.Record(ctx, "identity.principal.update", "identity.principal", id, map[string]any{"changed_fields": fields}, events...)
}

// BlockPrincipal blocks an active principal; the pipeline refuses its
// tokens from then on. Nobody can block themselves, so an administrator
// cannot lock the last administrator out by accident.
func BlockPrincipal(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, reason string) (Principal, error) {
	tx, actor, err := reqctx.Request(ctx)
	if err != nil {
		return Principal{}, err
	}
	p, err := getPrincipal(ctx, tx, id, true)
	if err != nil {
		return Principal{}, err
	}
	if err := ifMatch(p.Version); err != nil {
		return Principal{}, err
	}
	switch {
	case p.Status != Active:
		return Principal{}, problem.Conflict("identity.invalid_status", "The principal is "+p.Status+"; block requires status active.")
	case p.ID == actor.ID:
		return Principal{}, problem.Conflict("identity.cannot_block_self", "You cannot block yourself.")
	}
	if _, err := tx.Exec(ctx, `UPDATE identity.principal SET status = 'blocked', version = version + 1, updated_by = $2 WHERE id = $1`, id, actor.ID); err != nil {
		return Principal{}, fmt.Errorf("identity: block principal %s: %w", id, err)
	}
	updated, err := getPrincipal(ctx, tx, id, false)
	if err != nil {
		return Principal{}, err
	}
	return updated, audit.Record(ctx, "identity.principal.block", "identity.principal", id, map[string]any{"reason": reason},
		outbox.Event{Context: "identity", Name: "principal_blocked", AggregateType: "identity.principal", AggregateID: id,
			Payload: map[string]any{"principal_id": id, "kind": p.Kind, "previous_status": p.Status, "status": Blocked}})
}

func changedFields(pairs ...any) []string {
	fields := []string{}
	for i := 0; i < len(pairs); i += 2 {
		if pairs[i].(bool) {
			fields = append(fields, pairs[i+1].(string)) // #nosec G602 -- pairs come in twos by construction
		}
	}
	return fields
}

func sameString(a, b *string) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
func sameInt(a, b *int64) bool     { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
