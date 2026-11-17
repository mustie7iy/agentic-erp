package controlling

import (
	"context"
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
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// The versions of an order's budget (ADR-0064 §3, T60h). Every change of
// a budget, a PATCH (set), a supplement or a return, is one version: the
// budget row is written at the version its reader saw, the order held
// FOR UPDATE, and the change is kept in controlling.order_budget_change
// with the budget before and after it, audited and published.

// The kinds of a budget's change.
const (
	changeSet        = "set"
	changeSupplement = "supplement"
	changeReturn     = "return"
)

// budgetChange is a row of controlling.order_budget_change.
type budgetChange struct {
	ID, OrderID  uuid.UUID
	Version      int32
	Kind         string
	Before       *money.Minor
	After        money.Minor
	Warn, Refuse *int32
	Text         *string
	CreatedAt    time.Time
	CreatedBy    uuid.UUID
}

const changeColumns = `c.id, c.internal_order_id, c.budget_version, c.kind, c.budget_before, c.budget_after, c.warn_percent, c.refuse_percent, c.text,
	c.created_at, c.created_by`

func scanChange(r pgx.Row) (budgetChange, error) {
	var c budgetChange
	err := r.Scan(&c.ID, &c.OrderID, &c.Version, &c.Kind, &c.Before, &c.After, &c.Warn, &c.Refuse, &c.Text, &c.CreatedAt, &c.CreatedBy)
	return c, errs.Wrapf(err, "controlling: scan order budget change")
}

func changeToAPI(currency string, exp int) func(budgetChange) gen.InternalOrderBudgetChange {
	return func(c budgetChange) gen.InternalOrderBudgetChange {
		moved := c.After
		if c.Before != nil {
			// Both are budgets of zero or more in one bigint, so the
			// difference cannot overflow.
			moved = c.After - *c.Before
		}
		out := gen.InternalOrderBudgetChange{ID: c.ID, InternalOrderID: c.OrderID, Version: c.Version, Kind: c.Kind,
			Budget: apitypes.Money(c.After, currency, exp), Change: apitypes.Money(moved, currency, exp), AvailabilityControl: controlToAPI(c.Warn, c.Refuse),
			Text: apitypes.OptString(c.Text), CreatedAt: c.CreatedAt, CreatedBy: c.CreatedBy}
		if c.Before != nil {
			out.BudgetBefore.SetTo(apitypes.Money(*c.Before, currency, exp))
		}
		return out
	}
}

// budgetWrite is one change of an order's budget: the order, its
// company's currency, how it changes, the budget the caller's version
// named and the budget after.
type budgetWrite struct {
	order        Order
	currency     string
	exp          int
	kind         string
	text         *string
	before, next orderBudget
}

// writeBudget writes the budget at the version before was read at, keeps
// the change as the version it made, audits and publishes it. The caller
// holds the order FOR UPDATE.
func writeBudget(ctx context.Context, tx pgx.Tx, p reqctx.Principal, w budgetWrite) (orderBudget, budgetChange, error) {
	o := w.order
	after, err := scanBudget(tx.QueryRow(ctx, `INSERT INTO controlling.order_budget AS b (tenant_id, internal_order_id, amount, warn_percent, refuse_percent,
			created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $6)
		ON CONFLICT (tenant_id, internal_order_id) DO UPDATE SET amount = EXCLUDED.amount, warn_percent = EXCLUDED.warn_percent,
			refuse_percent = EXCLUDED.refuse_percent, version = b.version + 1, updated_by = EXCLUDED.updated_by
			WHERE b.version = $7
		RETURNING `+budgetColumns, p.TenantID, o.ID, w.next.Amount, w.next.Warn, w.next.Refuse, p.ID, w.before.Version))
	if err != nil {
		return orderBudget{}, budgetChange{}, fmt.Errorf("controlling: set budget of internal order %s: %w", o.ID, err)
	}
	var before *money.Minor
	if w.before.Version > 0 {
		before = &w.before.Amount
	}
	c, err := scanChange(tx.QueryRow(ctx, `INSERT INTO controlling.order_budget_change AS c (tenant_id, id, internal_order_id, budget_version, kind, budget_before,
			budget_after, warn_percent, refuse_percent, text, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) RETURNING `+changeColumns, p.TenantID, uuid.Must(uuid.NewV7()), o.ID, after.Version, w.kind,
		before, after.Amount, after.Warn, after.Refuse, w.text, p.ID))
	if err != nil {
		return orderBudget{}, budgetChange{}, fmt.Errorf("controlling: keep the change of the budget of internal order %s: %w", o.ID, err)
	}
	return after, c, recordBudget(ctx, w, after)
}

// recordBudget audits a budget's change with the amounts and limits
// before and after and publishes it.
func recordBudget(ctx context.Context, w budgetWrite, after orderBudget) error {
	o, exp := w.order, w.exp
	detail := map[string]any{"legal_entity_id": o.LegalEntityID, "code": o.Code, "currency": w.currency, "kind": w.kind, "budget_after": after.Amount.Plain(exp),
		"warn_percent_after": after.Warn, "refuse_percent_after": after.Refuse}
	payload := map[string]any{"internal_order_id": o.ID, "legal_entity_id": o.LegalEntityID, "code": o.Code, "name": o.Name,
		"budget": money.EventAmount(after.Amount, w.currency, exp), "version": after.Version, "kind": w.kind}
	if w.before.Version > 0 {
		detail["budget_before"] = w.before.Amount.Plain(exp)
		detail["warn_percent_before"], detail["refuse_percent_before"] = w.before.Warn, w.before.Refuse
		payload["previous_budget"] = money.EventAmount(w.before.Amount, w.currency, exp)
	}
	for k, v := range map[string]*int32{"warn_percent": after.Warn, "refuse_percent": after.Refuse} {
		if v != nil {
			payload[k] = *v
		}
	}
	if w.text != nil {
		detail["text"], payload["text"] = *w.text, *w.text
	}
	action := map[string]string{changeSet: "set_budget", changeSupplement: "supplement_budget", changeReturn: "return_budget"}[w.kind]
	return audit.Record(ctx, "controlling.internal_order."+action, "controlling.internal_order", o.ID, detail,
		outbox.Event{Context: "controlling", Name: "internal_order_budget_changed", AggregateType: "controlling.internal_order", AggregateID: o.ID, Payload: payload})
}

// CreateInternalOrderBudgetChange implements POST
// /v1/internal-orders/{id}/budget-changes: a supplement or a return of the
// order's budget at the version the caller read, the order held FOR
// UPDATE (ADR-0008).
func (e *Endpoints) CreateInternalOrderBudgetChange(ctx context.Context, req *gen.InternalOrderBudgetChangeCreate,
	params gen.CreateInternalOrderBudgetChangeParams) (gen.CreateInternalOrderBudgetChangeRes, error) {
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
	if req.Kind != changeSupplement && req.Kind != changeReturn {
		return nil, problem.Unprocessable("controlling.order_budget_invalid", "The budget is not changed that way.",
			problem.FieldError{Field: "kind", Code: "unsupported", Message: changeSupplement + " or " + changeReturn})
	}
	if before.Version == 0 {
		return nil, problem.Conflict("controlling.order_budget_missing", "The internal order has no budget to supplement or return; set its budget first.")
	}
	amount, err := budgetAmount("amount", req.Amount, le.LocalCurrency, exp, true)
	if err != nil {
		return nil, err
	}
	next := before
	if req.Kind == changeSupplement {
		next.Amount, err = money.Sum(before.Amount, amount)
		if err != nil {
			return nil, budgetInvalid("amount.amount", "an amount the budget can still hold")
		}
	} else if next.Amount, err = e.returned(ctx, o, le.ID, exp, le.LocalCurrency, before.Amount, amount); err != nil {
		return nil, err
	}
	_, c, err := writeBudget(ctx, tx, p, budgetWrite{order: o, currency: le.LocalCurrency, exp: exp, kind: req.Kind, text: apitypes.Ptr[string](req.Text),
		before: before, next: next})
	if err != nil {
		return nil, err
	}
	return &gen.InternalOrderBudgetChangeHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Response: changeToAPI(le.LocalCurrency, exp)(c)}, nil
}

// returned is the budget left after amount is returned: never below the
// order's actual costs, which are used, nor below zero (SAP returns only
// budget that is not used; T60h).
func (e *Endpoints) returned(ctx context.Context, o Order, le uuid.UUID, exp int, currency string, budget, amount money.Minor) (money.Minor, error) {
	actual, err := e.Ledger.OrderBalance(ctx, le, o.ID)
	if err != nil {
		return 0, fmt.Errorf("controlling: actual costs of internal order %s: %w", o.ID, err)
	}
	floor := max(actual, 0)
	if amount > budget-floor { // budget and floor are zero or more, so the difference cannot overflow
		return 0, problem.Conflict("controlling.order_budget_return_too_large", fmt.Sprintf("At most %s %s of the budget may be returned:"+
			" the budget less the order's actual costs.", max(budget-floor, 0).Plain(exp), currency))
	}
	return budget - amount, nil
}

var changeSort = pagination.Sort{Default: "version", DefaultDesc: true,
	Fields: []pagination.Field{{Name: "version", Column: "c.budget_version", Type: "integer"}}}

// ListInternalOrderBudgetChanges implements GET
// /v1/internal-orders/{id}/budget-changes.
func (e *Endpoints) ListInternalOrderBudgetChanges(ctx context.Context, params gen.ListInternalOrderBudgetChangesParams) (gen.ListInternalOrderBudgetChangesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	ks, err := e.Pages.Keyset(changeSort, fmt.Sprintf("listInternalOrderBudgetChanges|%s", params.ID),
		apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	o, le, exp, err := e.orderInCompany(ctx, tx, params.ID, false, orderNotFound)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(`SELECT `+changeColumns+` FROM controlling.order_budget_change c WHERE c.internal_order_id = $1`, []any{o.ID})
	rows, _ := tx.Query(ctx, q, args...)
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (budgetChange, error) { return scanChange(r) })
	if err != nil {
		return nil, fmt.Errorf("controlling: list the budget changes of internal order %s: %w", o.ID, err)
	}
	items, next := apitypes.PageOf(ks, list, func(c budgetChange) (string, uuid.UUID) { return fmt.Sprint(c.Version), c.ID }, changeToAPI(le.LocalCurrency, exp))
	return &gen.InternalOrderBudgetChangePage{Items: items, NextPageToken: next}, nil
}
