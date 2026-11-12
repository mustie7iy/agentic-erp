package controlling

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// The settlement rule of an internal order (ADR-0064 §3, T60d): one
// receiver, a cost center of the order's company, taking the whole. The
// settlement that uses it is T60e's.

// settlementRule is an order's rule row; Set is false for an order that
// has none. ReceiverCode is named by the ledger's port when the rule is
// shown.
type settlementRule struct {
	Set          bool
	ReceiverID   uuid.UUID
	ReceiverCode string
	UpdatedAt    time.Time
	UpdatedBy    uuid.UUID
}

const ruleColumns = `receiver_cost_center_id, updated_at, updated_by`

func scanRule(r pgx.Row) (settlementRule, error) {
	rule := settlementRule{Set: true}
	err := r.Scan(&rule.ReceiverID, &rule.UpdatedAt, &rule.UpdatedBy)
	return rule, errs.Wrapf(err, "controlling: scan settlement rule")
}

// ruleOf reads the order's rule with its receiver's code, the zero rule
// when the order has none. A caller that changes it holds the order FOR
// UPDATE, so the row needs no lock of its own.
func (e *Endpoints) ruleOf(ctx context.Context, tx pgx.Tx, order uuid.UUID) (settlementRule, error) {
	rule, err := scanRule(tx.QueryRow(ctx, `SELECT `+ruleColumns+` FROM controlling.settlement_rule WHERE internal_order_id = $1`, order))
	if errors.Is(err, pgx.ErrNoRows) {
		return settlementRule{}, nil
	}
	if err != nil {
		return settlementRule{}, fmt.Errorf("controlling: read settlement rule of internal order %s: %w", order, err)
	}
	refs, err := e.Ledger.CostCenterRefs(ctx, []uuid.UUID{rule.ReceiverID})
	if err != nil {
		return settlementRule{}, fmt.Errorf("controlling: receiver of internal order %s: %w", order, err)
	}
	rule.ReceiverCode = refs[rule.ReceiverID].Code
	return rule, nil
}

// receiver checks the cost center a rule names: an active cost center of
// the order's company that is valid today, the company's date in its
// time zone (422 naming why otherwise), and returns its code.
func (e *Endpoints) receiver(ctx context.Context, le tenancy.Entity, costCenter uuid.UUID) (string, error) {
	refs, err := e.Ledger.CostCenterRefs(ctx, []uuid.UUID{costCenter})
	if err != nil {
		return "", fmt.Errorf("controlling: receiving cost center %s: %w", costCenter, err)
	}
	invalid := func(reason, message string) error {
		return problem.Unprocessable("controlling.settlement_rule_receiver_invalid", "The receiving cost center "+message+".",
			problem.FieldError{Field: "receiver_cost_center_id", Code: reason, Message: "an active cost center of the order's legal entity, valid today"})
	}
	ref, known := refs[costCenter]
	switch {
	case !known:
		return "", invalid("unknown", "is not known")
	case ref.LegalEntityID != le.ID:
		return "", invalid("not_in_company", "is not one of the order's legal entity's")
	case !ref.Active:
		return "", invalid("not_active", "is not active")
	}
	today, err := config.DocumentDay(ctx, e.Settings, le.Code, clock.Now(e.Now))
	if err != nil {
		return "", fmt.Errorf("controlling: date of legal entity %s: %w", le.ID, err)
	}
	valid, err := e.Ledger.CostCenterValidOn(ctx, costCenter, today.Date)
	if err != nil {
		return "", fmt.Errorf("controlling: receiving cost center %s: %w", costCenter, err)
	}
	if !valid {
		return "", invalid("not_valid_today", "is not valid on "+today.Date.Format(time.DateOnly)+", the legal entity's date in "+today.Zone.String())
	}
	return ref.Code, nil
}

func ruleToAPI(o Order, rule settlementRule) gen.InternalOrderSettlementRule {
	out := gen.InternalOrderSettlementRule{InternalOrderID: o.ID, InternalOrderCode: o.Code, InternalOrderName: o.Name, LegalEntityID: o.LegalEntityID,
		Version: o.Version}
	if rule.Set {
		out.ReceiverCostCenterID.SetTo(rule.ReceiverID)
		out.ReceiverCostCenterCode.SetTo(rule.ReceiverCode)
		out.UpdatedAt.SetTo(rule.UpdatedAt)
		out.UpdatedBy.SetTo(rule.UpdatedBy)
	}
	return out
}

// GetInternalOrderSettlementRule implements GET /v1/internal-orders/{id}/settlement-rule.
func (e *Endpoints) GetInternalOrderSettlementRule(ctx context.Context, params gen.GetInternalOrderSettlementRuleParams) (gen.GetInternalOrderSettlementRuleRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	o, err := getOrder(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	rule, err := e.ruleOf(ctx, tx, o.ID)
	if err != nil {
		return nil, err
	}
	return &gen.InternalOrderSettlementRuleHeaders{ETag: gen.NewOptString(etag.Format(o.Version)), Response: ruleToAPI(o, rule)}, nil
}

// UpdateInternalOrderSettlementRule implements PATCH
// /v1/internal-orders/{id}/settlement-rule: the order is read FOR UPDATE
// at the version the caller read and its version moves with the rule, so
// of two changes at one version the second is refused (ADR-0008).
func (e *Endpoints) UpdateInternalOrderSettlementRule(ctx context.Context, req *gen.InternalOrderSettlementRuleUpdate, params gen.UpdateInternalOrderSettlementRuleParams) (gen.UpdateInternalOrderSettlementRuleRes, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	o, le, _, err := e.orderInCompany(ctx, tx, params.ID, true, orderNotFound)
	if err != nil {
		return nil, err
	}
	if err := etag.IfMatch(params.IfMatch)(o.Version); err != nil {
		return nil, err
	}
	code, err := e.receiver(ctx, le, req.ReceiverCostCenterID)
	if err != nil {
		return nil, err
	}
	before, err := e.ruleOf(ctx, tx, o.ID)
	if err != nil {
		return nil, err
	}
	after, err := scanRule(tx.QueryRow(ctx, `INSERT INTO controlling.settlement_rule AS r (tenant_id, internal_order_id, receiver_cost_center_id, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $4)
		ON CONFLICT (tenant_id, internal_order_id) DO UPDATE SET receiver_cost_center_id = EXCLUDED.receiver_cost_center_id, version = r.version + 1,
			updated_by = EXCLUDED.updated_by
		RETURNING `+ruleColumns, p.TenantID, o.ID, req.ReceiverCostCenterID, p.ID))
	if err != nil {
		return nil, fmt.Errorf("controlling: set settlement rule of internal order %s: %w", o.ID, err)
	}
	after.ReceiverCode = code
	if o, err = scanOrder(tx.QueryRow(ctx, `UPDATE controlling.internal_order SET version = version + 1, updated_by = $2 WHERE id = $1 AND version = $3
		RETURNING `+orderColumns, o.ID, p.ID, o.Version)); err != nil {
		return nil, fmt.Errorf("controlling: move the version of internal order %s with its settlement rule: %w", params.ID, err)
	}
	// The audit record is the request's last step and publishes nothing,
	// so its error is the handler's: the unit of work rolls back on it.
	return &gen.UpdateInternalOrderSettlementRuleOKHeaders{ETag: gen.NewOptString(etag.Format(o.Version)), Response: ruleToAPI(o, after)},
		recordRule(ctx, o, before, after)
}

// recordRule audits a rule's change with the receivers before and after.
func recordRule(ctx context.Context, o Order, before, after settlementRule) error {
	detail := map[string]any{"legal_entity_id": o.LegalEntityID, "code": o.Code, "receiver_cost_center_id_after": after.ReceiverID,
		"receiver_cost_center_code_after": after.ReceiverCode}
	if before.Set {
		detail["receiver_cost_center_id_before"] = before.ReceiverID
		detail["receiver_cost_center_code_before"] = before.ReceiverCode
	}
	return audit.Record(ctx, "controlling.internal_order.set_settlement_rule", "controlling.internal_order", o.ID, detail)
}
