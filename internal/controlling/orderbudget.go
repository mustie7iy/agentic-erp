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
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// The budget of an internal order against its actual costs (ADR-0064 §3,
// T60c): one overall amount per order in its company's currency. The
// read shows the budget, the actual and what remains, and says when the
// actual exceeds it. Since T60h the budget may carry an availability
// control, a warning and a refusal limit in percent of it, which the
// ledger's postings to the order are checked against (Consume), and
// every change is one version kept in the budget's history
// (budgetchanges.go).

// orderBudget is an order's budget row. Version 0 is an order whose
// budget was never set: it has no row, and its first PATCH names version 0.
type orderBudget struct {
	Amount       money.Minor
	Warn, Refuse *int32
	Version      int32
	UpdatedAt    time.Time
	UpdatedBy    uuid.UUID
}

const budgetColumns = `amount, warn_percent, refuse_percent, version, updated_at, updated_by`

func scanBudget(r pgx.Row) (orderBudget, error) {
	var b orderBudget
	err := r.Scan(&b.Amount, &b.Warn, &b.Refuse, &b.Version, &b.UpdatedAt, &b.UpdatedBy)
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

// budgetInvalid is the refusal of a budget's field.
func budgetInvalid(field, message string) error {
	return problem.Unprocessable("controlling.order_budget_invalid", "The budget is not an amount the order can be given.",
		problem.FieldError{Field: field, Code: "invalid", Message: message})
}

// budgetAmount reads an amount a request gives on field: in the company's
// currency with its digits, zero or more, or above zero when positive.
func budgetAmount(field string, in gen.Money, currency string, exp int, positive bool) (money.Minor, error) {
	if in.Currency != currency {
		return 0, budgetInvalid(field+".currency", "the legal entity's local currency, "+currency)
	}
	amount, err := money.ParseAmount(in.Amount, exp)
	if positive && (err != nil || amount <= 0) {
		return 0, budgetInvalid(field+".amount", fmt.Sprintf("an amount above zero with exactly %d decimals", exp))
	}
	if err != nil || amount < 0 {
		return 0, budgetInvalid(field+".amount", fmt.Sprintf("an amount of zero or more with exactly %d decimals", exp))
	}
	return amount, nil
}

// Availability states (T60h): where an order's consumption stands against
// its budget's limits.
const (
	availabilityWithin       = "within"
	availabilityAboveWarning = "above_warning"
	availabilityAboveRefusal = "above_refusal"
)

// availabilityOf is where consumption stands against the limits of a
// budget of amount: above the refusal limit, above the warning limit, or
// within them.
func availabilityOf(consumption, amount money.Minor, warn, refuse *int32) string {
	switch {
	case refuse != nil && money.AbovePercent(consumption, amount, int64(*refuse)):
		return availabilityAboveRefusal
	case warn != nil && money.AbovePercent(consumption, amount, int64(*warn)):
		return availabilityAboveWarning
	}
	return availabilityWithin
}

// controlToAPI is a budget's availability control, absent without one.
func controlToAPI(warn, refuse *int32) gen.OptInternalOrderAvailabilityControl {
	if warn == nil && refuse == nil {
		return gen.OptInternalOrderAvailabilityControl{}
	}
	return gen.NewOptInternalOrderAvailabilityControl(gen.InternalOrderAvailabilityControl{WarnPercent: apitypes.OptInt32(warn),
		RefusePercent: apitypes.OptInt32(refuse)})
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
	if b.Amount != 0 {
		out.ConsumedPercent.SetTo(money.PercentOf(actual, b.Amount))
	}
	if out.AvailabilityControl = controlToAPI(b.Warn, b.Refuse); out.AvailabilityControl.Set {
		out.Availability.SetTo(availabilityOf(actual, b.Amount, b.Warn, b.Refuse))
	}
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
	next, err := patchedBudget(req, before, le.LocalCurrency, exp)
	if err != nil {
		return nil, err
	}
	after, _, err := writeBudget(ctx, tx, p, budgetWrite{order: o, currency: le.LocalCurrency, exp: exp, kind: changeSet, before: before, next: next})
	if err != nil {
		return nil, err
	}
	res, err := e.budgetAgainstActual(ctx, o, le, exp, after)
	if err != nil {
		return nil, err
	}
	return &gen.UpdateInternalOrderBudgetOKHeaders{ETag: gen.NewOptString(etag.Format(after.Version)), Response: res}, nil
}

// patchedBudget is the budget a merge patch leaves: the amount and the
// availability control each replaced when named and kept when not. A
// patch naming neither, or one without an amount for an order that has
// no budget yet, is refused; so is a warning limit not below the refusal
// limit (the database's order_budget_warn_below_refuse besides).
func patchedBudget(req *gen.InternalOrderBudgetUpdate, before orderBudget, currency string, exp int) (orderBudget, error) {
	next := before
	if !req.Budget.Set && !req.AvailabilityControl.Set {
		return orderBudget{}, budgetInvalid("budget", "a budget, an availability control or both")
	}
	if !req.Budget.Set && before.Version == 0 {
		return orderBudget{}, budgetInvalid("budget", "the order's budget, which it does not have yet")
	}
	if b, ok := req.Budget.Get(); ok {
		amount, err := budgetAmount("budget", b, currency, exp, false)
		if err != nil {
			return orderBudget{}, err
		}
		next.Amount = amount
	}
	if c := req.AvailabilityControl; c.Set {
		// A null control has no value: both limits are removed.
		next.Warn, next.Refuse = apitypes.Ptr[int32](c.Value.WarnPercent), apitypes.Ptr[int32](c.Value.RefusePercent)
	}
	if next.Warn != nil && next.Refuse != nil && *next.Warn >= *next.Refuse {
		return orderBudget{}, budgetInvalid("availability_control.warn_percent", fmt.Sprintf("a warning limit below the refusal limit, %d", *next.Refuse))
	}
	return next, nil
}
