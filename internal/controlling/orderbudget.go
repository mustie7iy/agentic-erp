package controlling

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// The budget of an internal order against its actual costs (ADR-0064 §3,
// T60c): one overall amount per order in its company's currency. Nothing
// is refused for a budget (plan default 10); the read shows the budget,
// the actual and what remains, and says when the actual exceeds it.

// orderBudget is an order's budget row. Version 0 is an order whose
// budget was never set: it has no row, and its first PATCH names version 0.
type orderBudget struct {
	Amount    money.Minor
	Version   int32
	UpdatedAt time.Time
	UpdatedBy uuid.UUID
}

const budgetColumns = `amount, version, updated_at, updated_by`

func scanBudget(r pgx.Row) (orderBudget, error) {
	var b orderBudget
	err := r.Scan(&b.Amount, &b.Version, &b.UpdatedAt, &b.UpdatedBy)
	return b, errs.Wrapf(err, "controlling: scan order budget")
}

// budgetOf reads the order's budget, the zero budget (version 0) when it
// was never set. A caller that changes it holds the order FOR UPDATE, so
// the row needs no lock of its own.
func budgetOf(ctx context.Context, tx pgx.Tx, order uuid.UUID) (orderBudget, error) {
	b, err := scanBudget(tx.QueryRow(ctx, `SELECT `+budgetColumns+` FROM controlling.order_budget WHERE internal_order_id = $1`, order))
	if errors.Is(err, pgx.ErrNoRows) {
		return orderBudget{}, nil
	}
	if err != nil {
		return orderBudget{}, fmt.Errorf("controlling: read budget of internal order %s: %w", order, err)
	}
	return b, nil
}

// orderInCompany reads an order and its company with the digits of the
// company's currency, FOR UPDATE when the order or what hangs on it is to
// change. An order that does not exist, or of a company the caller may
// not act in, is answered as missing (04 §4.9).
func (e *Endpoints) orderInCompany(ctx context.Context, tx pgx.Tx, id uuid.UUID, forUpdate bool, missing func() error) (Order, tenancy.Entity, int, error) {
	lock := ``
	if forUpdate {
		lock = ` FOR UPDATE`
	}
	o, err := scanOrder(tx.QueryRow(ctx, `SELECT `+orderColumns+` FROM controlling.internal_order WHERE id = $1`+lock, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, tenancy.Entity{}, 0, missing()
	}
	if err != nil {
		return Order{}, tenancy.Entity{}, 0, fmt.Errorf("controlling: read internal order %s: %w", id, err)
	}
	le, exp, ok, err := e.company(ctx, tx, o.LegalEntityID)
	if err != nil {
		return Order{}, tenancy.Entity{}, 0, err
	}
	if !ok {
		return Order{}, tenancy.Entity{}, 0, missing()
	}
	return o, le, exp, nil
}

func orderNotFound() error { return problem.NotFoundOf("internal order") }

// budgetAmount reads the budget a request gives: zero or more, in the
// company's currency with its digits.
func budgetAmount(in gen.Money, currency string, exp int) (money.Minor, error) {
	invalid := func(field, message string) error {
		return problem.Unprocessable("controlling.order_budget_invalid", "The budget is not an amount the order can be given.",
			problem.FieldError{Field: field, Code: "invalid", Message: message})
	}
	if in.Currency != currency {
		return 0, invalid("budget.currency", "the legal entity's local currency, "+currency)
	}
	amount, err := money.ParseAmount(in.Amount, exp)
	if err != nil || amount < 0 {
		return 0, invalid("budget.amount", fmt.Sprintf("an amount of zero or more with exactly %d decimals", exp))
	}
	return amount, nil
}

// budgetAgainstActual is the order's budget beside its actual costs, the
// ledger's balance of the order over every year, and what remains.
func (e *Endpoints) budgetAgainstActual(ctx context.Context, o Order, le tenancy.Entity, exp int, b orderBudget) (gen.InternalOrderBudget, error) {
	actual, err := e.Ledger.OrderBalance(ctx, le.ID, o.ID)
	if err != nil {
		return gen.InternalOrderBudget{}, fmt.Errorf("controlling: actual costs of internal order %s: %w", o.ID, err)
	}
	out := gen.InternalOrderBudget{InternalOrderID: o.ID, InternalOrderCode: o.Code, InternalOrderName: o.Name, LegalEntityID: le.ID,
		Version: b.Version, Actual: apitypes.Money(actual, le.LocalCurrency, exp)}
	if b.Version == 0 {
		return out, nil
	}
	remaining, err := money.Diff(b.Amount, actual)
	if err != nil {
		return gen.InternalOrderBudget{}, fmt.Errorf("controlling: what remains of the budget of internal order %s: %w", o.ID, err)
	}
	out.Budget.SetTo(apitypes.Money(b.Amount, le.LocalCurrency, exp))
	out.Remaining.SetTo(apitypes.Money(remaining, le.LocalCurrency, exp))
	out.Exceeded = actual > b.Amount
	out.UpdatedAt.SetTo(b.UpdatedAt)
	out.UpdatedBy.SetTo(b.UpdatedBy)
	return out, nil
}

// GetInternalOrderBudget implements GET /v1/internal-orders/{id}/budget.
func (e *Endpoints) GetInternalOrderBudget(ctx context.Context, params gen.GetInternalOrderBudgetParams) (gen.GetInternalOrderBudgetRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	o, le, exp, err := e.orderInCompany(ctx, tx, params.ID, false, orderNotFound)
	if err != nil {
		return nil, err
	}
	b, err := budgetOf(ctx, tx, o.ID)
	if err != nil {
		return nil, err
	}
	res, err := e.budgetAgainstActual(ctx, o, le, exp, b)
	if err != nil {
		return nil, err
	}
	return &gen.InternalOrderBudgetHeaders{ETag: gen.NewOptString(etag.Format(b.Version)), Response: res}, nil
}

// UpdateInternalOrderBudget implements PATCH /v1/internal-orders/{id}/budget:
// the order is held FOR UPDATE, so of two changes at the version both
// read the second finds the budget moved and is refused; the write is
// guarded by that version besides (ADR-0008).
func (e *Endpoints) UpdateInternalOrderBudget(ctx context.Context, req *gen.InternalOrderBudgetUpdate, params gen.UpdateInternalOrderBudgetParams) (gen.UpdateInternalOrderBudgetRes, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	o, le, exp, err := e.orderInCompany(ctx, tx, params.ID, true, orderNotFound)
	if err != nil {
		return nil, err
	}
	before, err := budgetOf(ctx, tx, o.ID)
	if err != nil {
		return nil, err
	}
	if err := etag.IfMatch(params.IfMatch)(before.Version); err != nil {
		return nil, err
	}
	amount, err := budgetAmount(req.Budget, le.LocalCurrency, exp)
	if err != nil {
		return nil, err
	}
	after, err := scanBudget(tx.QueryRow(ctx, `INSERT INTO controlling.order_budget AS b (tenant_id, internal_order_id, amount, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $4)
		ON CONFLICT (tenant_id, internal_order_id) DO UPDATE SET amount = EXCLUDED.amount, version = b.version + 1, updated_by = EXCLUDED.updated_by
			WHERE b.version = $5
		RETURNING `+budgetColumns, p.TenantID, o.ID, amount, p.ID, before.Version))
	if err != nil {
		return nil, fmt.Errorf("controlling: set budget of internal order %s: %w", o.ID, err)
	}
	if err := recordBudget(ctx, o, le.LocalCurrency, exp, before, after); err != nil {
		return nil, err
	}
	res, err := e.budgetAgainstActual(ctx, o, le, exp, after)
	if err != nil {
		return nil, err
	}
	return &gen.UpdateInternalOrderBudgetOKHeaders{ETag: gen.NewOptString(etag.Format(after.Version)), Response: res}, nil
}

// recordBudget audits a budget's change with the amounts before and after
// and publishes it.
func recordBudget(ctx context.Context, o Order, currency string, exp int, before, after orderBudget) error {
	detail := map[string]any{"legal_entity_id": o.LegalEntityID, "code": o.Code, "currency": currency, "budget_after": after.Amount.Plain(exp)}
	payload := map[string]any{"internal_order_id": o.ID, "legal_entity_id": o.LegalEntityID, "code": o.Code, "name": o.Name,
		"budget": money.EventAmount(after.Amount, currency, exp), "version": after.Version}
	if before.Version > 0 {
		detail["budget_before"] = before.Amount.Plain(exp)
		payload["previous_budget"] = money.EventAmount(before.Amount, currency, exp)
	}
	return audit.Record(ctx, "controlling.internal_order.set_budget", "controlling.internal_order", o.ID, detail,
		outbox.Event{Context: "controlling", Name: "internal_order_budget_changed", AggregateType: "controlling.internal_order", AggregateID: o.ID, Payload: payload})
}
