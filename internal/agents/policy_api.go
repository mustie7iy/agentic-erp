package agents

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// GetAiPolicy implements GET /v1/ai-policy.
func (e *Endpoints) GetAiPolicy(ctx context.Context) (gen.GetAiPolicyRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	p, err := ReadPolicy(ctx, tx)
	if err != nil {
		return nil, err
	}
	out, err := PolicyToAPI(ctx, tx, p)
	if err != nil {
		return nil, err
	}
	return &gen.AiPolicyHeaders{ETag: gen.NewOptString(etag.Format(p.Version)), Response: out}, nil
}

// UpdateAiPolicy implements PATCH /v1/ai-policy: the tenant's
// administrators' change, audited and published as AiPolicyChanged.
func (e *Endpoints) UpdateAiPolicy(ctx context.Context, req *gen.AiPolicyUpdate, params gen.UpdateAiPolicyParams) (gen.UpdateAiPolicyRes, error) {
	tx, actor, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	ch, err := policyChange(ctx, tx, req)
	if err != nil {
		return nil, err
	}
	p, err := UpdatePolicy(ctx, tx, actor.ID, etag.IfMatch(params.IfMatch), ch)
	if err != nil {
		return nil, err
	}
	if err := recordPolicy(ctx, tx, actor.TenantID, "agents.ai_policy.update", p, ch.Fields()); err != nil {
		return nil, err
	}
	out, err := PolicyToAPI(ctx, tx, p)
	if err != nil {
		return nil, err
	}
	return &gen.UpdateAiPolicyOKHeaders{ETag: gen.NewOptString(etag.Format(p.Version)), Response: out}, nil
}

// recordPolicy audits a change of the policy and publishes it. The entry
// names the fields changed and whether AI runs after it: settings, never
// the operators' reason or anyone's words.
func recordPolicy(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, action string, p Policy, fields []string) error {
	ev, err := PolicyEvent(ctx, tx, tenant, p, fields)
	if err != nil {
		return err
	}
	return audit.Record(ctx, action, "agents.ai_policy", tenant, map[string]any{"changed_fields": fields, "active": p.Active(), "version": p.Version}, ev)
}

// policyChange reads the merge patch. The cap is in the prices' currency,
// at its minor-unit digits.
func policyChange(ctx context.Context, tx pgx.Tx, req *gen.AiPolicyUpdate) (PolicyChange, error) {
	ch := PolicyChange{Enabled: apitypes.Ptr[bool](req.Enabled), DocumentsToModels: apitypes.Ptr[bool](req.DocumentsToModels)}
	if req.Routes.Set {
		routes := map[string]string(req.Routes.Value)
		ch.Routes = &routes
	}
	if req.AllowedModels != nil {
		ch.AllowedModels = &req.AllowedModels
	}
	switch {
	case req.MonthlyCap.Null:
		var none *money.Minor
		ch.MonthlyCap = &none
	case req.MonthlyCap.Set:
		limit, err := capOf(ctx, tx, req.MonthlyCap.Value)
		if err != nil {
			return ch, err
		}
		ch.MonthlyCap = &limit
	}
	return ch, nil
}

func capOf(ctx context.Context, tx pgx.Tx, m gen.AiPolicyUpdateMonthlyCap) (*money.Minor, error) {
	if m.Currency != PriceCurrency {
		return nil, invalidPolicy("monthly_cap.currency", "currency", "The cap is in "+PriceCurrency+", the prices' currency.")
	}
	exp, err := currency.Exponent(ctx, tx, PriceCurrency, nil)
	if err != nil {
		return nil, fmt.Errorf("agents: AI policy cap: %w", err)
	}
	limit, err := money.ParseAmount(m.Amount, exp)
	if err != nil {
		return nil, invalidPolicy("monthly_cap.amount", "invalid", "The cap is a non-negative amount with the currency's minor-unit digits.")
	}
	return &limit, nil
}

// PolicyToAPI is the policy as the contract's AiPolicy, the operators'
// and the tenant's API alike.
func PolicyToAPI(ctx context.Context, tx pgx.Tx, p Policy) (gen.AiPolicy, error) {
	effective := gen.AiPolicyEffectiveRoutes{}
	for alias := range DefaultRoutes {
		effective[alias], _ = p.Route(alias, DefaultRoutes) // every alias has a platform route
	}
	out := gen.AiPolicy{Version: p.Version, Enabled: p.Enabled, Suspended: p.Suspended, SuspendedReason: apitypes.OptString(p.SuspendedReason),
		Active: p.Active(), Routes: gen.AiPolicyRoutes(p.Routes), EffectiveRoutes: effective, AllowedModels: p.AllowedModels,
		DocumentsToModels: p.DocumentsToModels, UpdatedBy: apitypes.OptUUID(p.UpdatedBy)}
	if p.UpdatedBy != nil {
		out.UpdatedAt.SetTo(p.UpdatedAt)
	}
	if p.MonthlyCap != nil {
		exp, err := currency.Exponent(ctx, tx, PriceCurrency, nil)
		if err != nil {
			return out, fmt.Errorf("agents: AI policy cap: %w", err)
		}
		out.MonthlyCap.SetTo(apitypes.Money(*p.MonthlyCap, PriceCurrency, exp))
	}
	return out, nil
}
