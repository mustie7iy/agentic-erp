package procurement

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// init registers the problem of the order item's constraints, each by its
// own call, so the contract's test can tell each constraint's table (api's
// TestProblemCodes). The application writes what the draft was checked
// against; receipts posted since, or an item number removed before,
// refuse it here.
func init() {
	problem.RegisterConstraint("purchase_order_item_received", invalidChange)
	problem.RegisterConstraint("purchase_order_item_identity", invalidChange)
	problem.RegisterConstraint("purchase_order_item_valued", invalidChange)
}

// invalidChange is the problem of a change that no longer fits its order.
func invalidChange() *problem.Problem {
	return problem.Unprocessable("procurement.invalid_change", "The order change no longer fits the order: an item keeps its account, assignment, "+
		"unit, material and contract item, and stays at or above what was received.",
		problem.FieldError{Field: "items", Code: "invalid", Message: "items that keep what the order's items keep"})
}

// submitChange submits a draft change of a released order, checked again
// against the order as it reads now: receipts may have been posted since
// it was drafted. A change that does not raise the order's total is
// applied at once; one that does waits for an order_change approval
// request by amount tier on its new total, unless the tiers ask for none.
// A contract order's increase must be covered by what remains of its
// contract before approval is asked.
func (e *Endpoints) submitChange(ctx context.Context, order, id uuid.UUID, ifMatch func(int32) error) (PurchaseOrderChange, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return PurchaseOrderChange{}, err
	}
	o, err := getOrder(ctx, tx, order, true)
	if err != nil {
		return PurchaseOrderChange{}, err
	}
	ch, err := loadChange(ctx, tx, order, id, ifMatch, "submit")
	if err != nil {
		return PurchaseOrderChange{}, err
	}
	// A draft change's order is released (cancelOpenChange). The check is
	// for validation only: it reproduces the stored proposal, which it
	// built from inputs that cannot drift (a contract changes only as a
	// draft), and which is frozen from submission on
	// (purchase_order_change_frozen).
	if _, _, err := e.proposal(ctx, tx, o, ch.Proposed, ChangeInput{Items: proposedItems(ch)}); err != nil {
		return PurchaseOrderChange{}, err
	}
	// The items it adds are released by it: checked before approval is
	// asked, and again as it is approved (OrderChanges).
	if err := e.checkAssignments(ctx, o.LegalEntityID, ch.Proposed.OrderDate, assignments(ch.Proposed.Items, len(o.Items))); err != nil {
		return PurchaseOrderChange{}, err
	}
	delta, err := changeDelta(o, ch)
	if err != nil {
		return PurchaseOrderChange{}, err
	}
	if err := coversIncrease(ctx, tx, o, ch, delta); err != nil {
		return PurchaseOrderChange{}, err
	}
	if delta <= 0 {
		return e.apply(ctx, tx, p, o, ch, delta)
	}
	request, needed, err := e.Approvals.Open(ctx, approvals.NewRequest{ObjectType: approvals.OrderChange, ObjectID: id,
		Title: "Change of purchase order " + o.DocumentNumber, Amount: &ch.TotalAfter, Currency: &o.Currency, LegalEntityID: o.LegalEntityID,
		Date: ch.Proposed.OrderDate})
	if err != nil {
		return PurchaseOrderChange{}, err
	}
	if !needed {
		return e.apply(ctx, tx, p, o, ch, delta)
	}
	if _, err := tx.Exec(ctx, `UPDATE procurement.purchase_order_change SET status = 'pending_approval', approval_request_id = $2, version = version + 1,
		updated_by = $3 WHERE id = $1`, id, request, p.ID); err != nil {
		return PurchaseOrderChange{}, fmt.Errorf("procurement: submit order change %s: %w", id, err)
	}
	submitted, err := readChange(ctx, tx, order, id, false)
	if err != nil {
		return PurchaseOrderChange{}, err
	}
	return submitted, audit.Record(ctx, "procurement.purchase_order_change.submit", "procurement.purchase_order_change", id,
		map[string]any{"purchase_order_id": order, "total_after": ch.TotalAfter, "approval_request_id": request})
}

// changeDelta is what a change adds to the order's total as it reads now.
// Both totals are in range and not negative, so their difference is too.
func changeDelta(o PurchaseOrder, ch PurchaseOrderChange) (money.Minor, error) {
	before, err := documentTotal(o.Items)
	return ch.TotalAfter - before, err
}

// coversIncrease requires what remains of a contract order's contract to
// cover the change's increase, on the order date as changed.
func coversIncrease(ctx context.Context, tx pgx.Tx, o PurchaseOrder, ch PurchaseOrderChange, delta money.Minor) error {
	if o.ContractID == nil || delta <= 0 {
		return nil
	}
	dated := o
	dated.OrderDate = ch.Proposed.OrderDate
	_, err := coveringContract(ctx, tx, dated, delta)
	return err
}

// apply applies a change to its order, both locked, in the order the
// triggers of 000065 require: the order names the change, its items are
// written (items it adds valued at their price), the change records what
// the order read and the revision it makes, and the order takes the
// change's header fields and moves to that revision as the name is
// cleared. A contract order consumes delta, the difference of its total, and
// PurchaseOrderChanged is published. Reads are without the caller's
// company filter: the caller may change the order, or approved the change.
func (e *Endpoints) apply(ctx context.Context, tx pgx.Tx, p reqctx.Principal, o PurchaseOrder, ch PurchaseOrderChange,
	delta money.Minor) (PurchaseOrderChange, error) {
	if err := writeChange(ctx, tx, p, o, ch); err != nil {
		return PurchaseOrderChange{}, err
	}
	if err := consumeDelta(ctx, tx, p, o, ch, delta); err != nil {
		return PurchaseOrderChange{}, err
	}
	changed, err := readOrder(ctx, tx, o.ID, false)
	if err != nil {
		return PurchaseOrderChange{}, err
	}
	applied, err := readChange(ctx, tx, o.ID, ch.ID, false)
	if err != nil {
		return PurchaseOrderChange{}, err
	}
	payload := orderEvent(changed, map[string]any{"change_id": ch.ID, "revision": changed.Revision,
		"reason": ch.Reason, "order_date": changed.OrderDate.Format(time.DateOnly), "payment_term_code": changed.PaymentTermCode,
		"contract_id": changed.ContractID, "total_before": money.EventAmount(ch.TotalBefore, o.Currency, o.Exponent),
		"total_after": money.EventAmount(ch.TotalAfter, o.Currency, o.Exponent), "items": releasedItems(changed)})
	if err := e.extensions().Event(ctx, payload, changed.Extensions, changed.ExtensionsSchema); err != nil {
		return PurchaseOrderChange{}, err
	}
	return applied, audit.Record(ctx, "procurement.purchase_order_change.apply", "procurement.purchase_order_change", ch.ID,
		map[string]any{"purchase_order_id": o.ID, "revision": changed.Revision, "total_before": ch.TotalBefore, "total_after": ch.TotalAfter},
		event("purchase_order_changed", "purchase_order", o.ID, payload))
}

// writeChange makes the order read as the change proposes.
func writeChange(ctx context.Context, tx pgx.Tx, p reqctx.Principal, o PurchaseOrder, ch PurchaseOrderChange) error {
	if _, err := tx.Exec(ctx, `UPDATE procurement.purchase_order SET applying_change_id = $2, version = version + 1, updated_by = $3 WHERE id = $1`,
		o.ID, ch.ID, p.ID); err != nil {
		return fmt.Errorf("procurement: apply order change %s: name it on purchase order %s: %w", ch.ID, o.ID, err)
	}
	if err := writeOrderItems(ctx, tx, p.TenantID, o.ID, ch.Proposed.Items, true); err != nil {
		return err
	}
	previous := ChangeProposal{OrderDate: o.OrderDate, PaymentTermCode: o.PaymentTermCode, Extensions: o.Extensions, Items: o.Items}
	if _, err := tx.Exec(ctx, `UPDATE procurement.purchase_order_change SET status = 'applied', revision = $2, previous = $3, version = version + 1,
		updated_by = $4 WHERE id = $1`, ch.ID, o.Revision+1, previous, p.ID); err != nil {
		return fmt.Errorf("procurement: apply order change %s: %w", ch.ID, err)
	}
	// coalesce: a change proposed before T58c carries no schema version and
	// keeps the order's; schemas are never removed, so no other proposal has none.
	if _, err := tx.Exec(ctx, `UPDATE procurement.purchase_order SET applying_change_id = NULL, revision = revision + 1, order_date = $2,
		payment_term_code = $3, extensions = $4, extensions_schema = coalesce($6, extensions_schema), version = version + 1, updated_by = $5 WHERE id = $1`,
		o.ID, ch.Proposed.OrderDate, ch.Proposed.PaymentTermCode, ch.Proposed.Extensions, p.ID, ch.Proposed.ExtensionsSchema); err != nil {
		return fmt.Errorf("procurement: apply order change %s to purchase order %s: %w", ch.ID, o.ID, err)
	}
	return nil
}

// consumeDelta records the change of a contract order's consumption, the
// difference of its total, negative for a decrease; the increase was
// checked against the contract before.
func consumeDelta(ctx context.Context, tx pgx.Tx, p reqctx.Principal, o PurchaseOrder, ch PurchaseOrderChange, delta money.Minor) error {
	if o.ContractID == nil || delta == 0 {
		return nil
	}
	if err := coversIncrease(ctx, tx, o, ch, delta); err != nil { // the contract is locked, and checked again on an approval
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO procurement.contract_release (tenant_id, contract_id, purchase_order_id, value, released_at, released_by)
		VALUES ($1, $2, $3, $4, clock_timestamp(), $5)`, p.TenantID, *o.ContractID, o.ID, delta, p.ID); err != nil {
		return fmt.Errorf("procurement: apply order change %s: consume contract %s: %w", ch.ID, *o.ContractID, err)
	}
	return nil
}

// cancelOpenChange cancels the open change of an order being closed or
// cancelled, withdrawing the request it waits for.
func (e *Endpoints) cancelOpenChange(ctx context.Context, tx pgx.Tx, p reqctx.Principal, order uuid.UUID, why string) error {
	var id uuid.UUID
	var status string
	err := tx.QueryRow(ctx, `SELECT id, status FROM procurement.purchase_order_change WHERE purchase_order_id = $1
		AND status IN ('draft', 'pending_approval') FOR UPDATE`, order).Scan(&id, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("procurement: find the open change of purchase order %s: %w", order, err)
	}
	if status == PendingApproval {
		if err := e.Approvals.Withdraw(ctx, approvals.OrderChange, id, why); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE procurement.purchase_order_change SET status = 'cancelled', version = version + 1, updated_by = $2
		WHERE id = $1`, id, p.ID); err != nil {
		return fmt.Errorf("procurement: cancel order change %s: %w", id, err)
	}
	return audit.Record(ctx, "procurement.purchase_order_change.cancel", "procurement.purchase_order_change", id,
		map[string]any{"purchase_order_id": order, "from_status": status, "comment": why})
}

// OrderChanges moves an order change on when its order_change approval
// request completes (approvals.Handler, T47f), in the deciding
// transaction. Each acts only while the change waits for that request.
// Endpoints is procurement as the API wires it.
type OrderChanges struct {
	Endpoints *Endpoints
}

// Approved applies the change. Receipts posted while it waited that it
// would fall below refuse the decision, and the request stays pending.
func (h OrderChanges) Approved(ctx context.Context, change, request uuid.UUID) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	o, ch, err := waitingChange(ctx, tx, change, request)
	if err != nil {
		return err
	}
	if err := h.Endpoints.supplier(ctx, o.SupplierID, o.LegalEntityID); err != nil {
		return err
	}
	if err := h.Endpoints.checkAssignments(ctx, o.LegalEntityID, ch.Proposed.OrderDate, assignments(ch.Proposed.Items, len(o.Items))); err != nil {
		return err
	}
	delta, err := changeDelta(o, ch)
	if err != nil {
		return err
	}
	_, err = h.Endpoints.apply(ctx, tx, p, o, ch, delta)
	return err
}

// Rejected ends the change; the order stays as it was.
func (OrderChanges) Rejected(ctx context.Context, change, request uuid.UUID, comment string) error {
	return endChange(ctx, change, request, "status = 'rejected'", "reject", comment)
}

// Cancelled returns the change to draft: its requester withdrew the
// request.
func (OrderChanges) Cancelled(ctx context.Context, change, request uuid.UUID, comment string) error {
	return endChange(ctx, change, request, "status = 'draft', approval_request_id = NULL", "withdraw", comment)
}

// endChange ends a change's wait; set is one of the two constant SET
// clauses above.
func endChange(ctx context.Context, change, request uuid.UUID, set, action, comment string) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	o, _, err := waitingChange(ctx, tx, change, request)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE procurement.purchase_order_change SET `+set+`, version = version + 1, updated_by = $3
		WHERE id = $1 AND approval_request_id = $2`, change, request, p.ID); err != nil {
		return fmt.Errorf("procurement: %s order change %s: %w", action, change, err)
	}
	return audit.Record(ctx, "procurement.purchase_order_change."+action, "procurement.purchase_order_change", change,
		map[string]any{"purchase_order_id": o.ID, "approval_request_id": request, "comment": comment})
}

// waitingChange locks the change's order, then the change, and returns
// them if the change waits for the request. The decider may not see the
// order's company: the request is what authorises the change.
func waitingChange(ctx context.Context, tx pgx.Tx, change, request uuid.UUID) (PurchaseOrder, PurchaseOrderChange, error) {
	var order uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT purchase_order_id FROM procurement.purchase_order_change WHERE id = $1`, change).Scan(&order); err != nil {
		return PurchaseOrder{}, PurchaseOrderChange{}, fmt.Errorf("procurement: find the order of change %s: %w", change, err)
	}
	o, err := readOrder(ctx, tx, order, true)
	if err != nil {
		return PurchaseOrder{}, PurchaseOrderChange{}, err
	}
	ch, err := readChange(ctx, tx, order, change, true)
	if err != nil {
		return PurchaseOrder{}, PurchaseOrderChange{}, err
	}
	if ch.Status != PendingApproval || ch.ApprovalRequestID == nil || *ch.ApprovalRequestID != request {
		return PurchaseOrder{}, PurchaseOrderChange{}, fmt.Errorf("procurement: order change %s (%s) does not wait for request %s", change, ch.Status, request)
	}
	return o, ch, nil
}

// Resources is where the API reads each change (approvals.Locator):
// through its order, /v1/purchase-orders/{id}/changes/{change_id}.
func (OrderChanges) Resources(ctx context.Context, changes []uuid.UUID) (map[uuid.UUID]string, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT id, purchase_order_id FROM procurement.purchase_order_change WHERE id = ANY($1)`, changes)
	out := map[uuid.UUID]string{}
	var change, order uuid.UUID
	_, err = pgx.ForEachRow(rows, []any{&change, &order}, func() error {
		out[change] = "/v1/purchase-orders/" + order.String() + "/changes/" + change.String()
		return nil
	})
	if err != nil {
		return out, fmt.Errorf("procurement: orders of %d changes: %w", len(changes), err)
	}
	return out, nil
}
