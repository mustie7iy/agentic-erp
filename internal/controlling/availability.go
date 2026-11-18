package controlling

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Availability control of an order's budget (ADR-0064 §3, T60h). The
// ledger asks Consume, in the posting's transaction, for every order a
// posting adds to; the order's consumption with the posting is its actual
// costs over every year, the ledger's balance of its lines (commitments
// are not counted yet), plus what the posting adds. Above the refusal
// limit the posting is refused; above the warning limit it goes through
// and InternalOrderBudgetWarned is published with it.
//
// Locks: the posting already holds the order FOR SHARE (Orders with
// hold), so a budget change, which takes the order FOR UPDATE, is not
// under way; Consume then takes the budget rows FOR UPDATE, in order id
// order, before the actual is read, so two postings to one order are
// checked one after the other and the second reads the first's lines.
// It is asked before the posting locks its centers and the number range,
// which a posting takes after it in the same order.

// budgetControl is an order's budget with its availability control, as a
// posting checks it.
type budgetControl struct {
	OrderID      uuid.UUID
	Code, Name   string
	Amount       money.Minor
	Warn, Refuse *int32
}

// Consume implements ledger.CostObjects.Consume: it returns the first
// order, in id order, whose consumption with the posting would be above
// its refusal limit, or uuid.Nil, and publishes a warning for each order
// above its warning limit. An order without limits is not checked.
func (CostObjects) Consume(ctx context.Context, le uuid.UUID, cur string, entry uuid.UUID, posted map[uuid.UUID]money.Minor) (uuid.UUID, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT b.internal_order_id, o.code, o.name, b.amount, b.warn_percent, b.refuse_percent
		FROM controlling.order_budget b JOIN controlling.internal_order o ON o.tenant_id = b.tenant_id AND o.id = b.internal_order_id
		WHERE b.internal_order_id = ANY($1) AND (b.warn_percent IS NOT NULL OR b.refuse_percent IS NOT NULL)
		ORDER BY b.internal_order_id FOR UPDATE OF b`, slices.Collect(maps.Keys(posted)))
	controls, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (budgetControl, error) {
		var c budgetControl
		err := r.Scan(&c.OrderID, &c.Code, &c.Name, &c.Amount, &c.Warn, &c.Refuse)
		return c, errs.Wrapf(err, "controlling: scan order budget control")
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("controlling: hold the budgets of the orders posted to: %w", err)
	}
	for _, c := range controls {
		consumption, err := consumptionOf(ctx, le, c.OrderID, posted[c.OrderID])
		if err != nil {
			return uuid.Nil, err
		}
		switch availabilityOf(consumption, c.Amount, c.Warn, c.Refuse) {
		case availabilityAboveRefusal:
			return c.OrderID, nil
		case availabilityAboveWarning:
			if err := warn(ctx, tx, warning{le: le, currency: cur, entry: entry, control: c, consumption: consumption, posted: posted[c.OrderID]}); err != nil {
				return uuid.Nil, err
			}
		}
	}
	return uuid.Nil, nil
}

// consumptionOf is the order's actual costs read under its budget's lock,
// plus what the posting adds.
func consumptionOf(ctx context.Context, le, order uuid.UUID, posted money.Minor) (money.Minor, error) {
	actual, err := ledger.AssignmentBalances{}.OrderBalance(ctx, le, order)
	if err != nil {
		return 0, fmt.Errorf("controlling: actual costs of internal order %s: %w", order, err)
	}
	sum, err := money.Sum(actual, posted)
	return sum, errs.Wrapf(err, "controlling: consumption of internal order %s", order)
}

// warning is a posting above an order's warning limit: the company and
// its currency, the leading ledger's entry being posted, the order's
// budget, its consumption with the posting and what the posting adds.
type warning struct {
	le                  uuid.UUID
	currency            string
	entry               uuid.UUID
	control             budgetControl
	consumption, posted money.Minor
}

// warn publishes InternalOrderBudgetWarned for a posting that takes the
// order above its warning limit, naming the entry. The posting is audited
// by the ledger; the warning changes nothing of the order's, so it is
// published, not audited.
func warn(ctx context.Context, tx pgx.Tx, w warning) error {
	exp, err := currency.Exponent(ctx, tx, w.currency, nil)
	if err != nil {
		return fmt.Errorf("controlling: digits of %s: %w", w.currency, err)
	}
	c := w.control
	payload := map[string]any{"internal_order_id": c.OrderID, "legal_entity_id": w.le, "code": c.Code, "name": c.Name, "journal_entry_id": w.entry,
		"budget": money.EventAmount(c.Amount, w.currency, exp), "consumption": money.EventAmount(w.consumption, w.currency, exp),
		"posted": money.EventAmount(w.posted, w.currency, exp), "warn_percent": *c.Warn}
	if c.Amount != 0 {
		payload["consumed_percent"] = money.PercentOf(w.consumption, c.Amount)
	}
	_, err = outbox.Add(ctx, outbox.Event{Context: "controlling", Name: "internal_order_budget_warned", AggregateType: "controlling.internal_order",
		AggregateID: c.OrderID, Payload: payload})
	return err
}
