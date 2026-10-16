package procurement

import (
	"context"
	"fmt"
	"math/big"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/extensions"
	"github.com/mustie7iy/agentic-erp/internal/platform/mergepatch"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Statuses of an order change (T47e) besides Draft, PendingApproval,
// Rejected and Cancelled.
const Applied = "applied"

func init() {
	problem.RegisterConstraint("purchase_order_one_open_change", func() *problem.Problem {
		return problem.Conflict("procurement.change_open", "The purchase order has an open change; change or cancel that one.")
	})
}

// PurchaseOrderChange is a row of procurement.purchase_order_change: a
// version of a released order, drafted, decided and applied (T47e/T47f).
type PurchaseOrderChange struct {
	ID                uuid.UUID
	PurchaseOrderID   uuid.UUID
	LegalEntityID     uuid.UUID // the order's, for the caller's company access
	Currency          string    // the order's, which a change keeps
	Exponent          int
	Status            string
	Reason            string
	BaseRevision      int32
	Revision          *int32
	Proposed          ChangeProposal
	TotalBefore       money.Minor
	TotalAfter        money.Minor
	ApprovalRequestID *uuid.UUID
	Version           int32
	CreatedAt         time.Time
	CreatedBy         uuid.UUID
	UpdatedAt         time.Time
	UpdatedBy         uuid.UUID
}

// ChangeProposal is what an order change makes of the order: the header
// fields it may change and the complete items, stored as the change's
// proposed jsonb.
type ChangeProposal struct {
	OrderDate       time.Time      `json:"order_date"`
	PaymentTermCode string         `json:"payment_term_code"`
	Extensions      map[string]any `json:"extensions"`
	Items           []Item         `json:"items"`
}

// ChangeInput is a new order change or a merge patch of a draft one; nil
// fields keep what the change (or, for a new one, the order) reads.
type ChangeInput struct {
	Reason          *string
	OrderDate       *time.Time
	PaymentTermCode *string
	Items           []ItemInput
	Extensions      map[string]any
}

const changeSelect = `SELECT ch.id, ch.purchase_order_id, o.legal_entity_id, o.currency, c.exponent, ch.status, ch.reason, ch.base_revision, ch.revision,
		ch.proposed, ch.total_before, ch.total_after, ch.approval_request_id, ch.version, ch.created_at, ch.created_by, ch.updated_at, ch.updated_by
	FROM procurement.purchase_order_change ch
	JOIN procurement.purchase_order o ON o.tenant_id = ch.tenant_id AND o.id = ch.purchase_order_id
	JOIN ref.currency c ON c.code = o.currency`

func scanChange(row pgx.Row) (PurchaseOrderChange, error) {
	var ch PurchaseOrderChange
	if err := row.Scan(&ch.ID, &ch.PurchaseOrderID, &ch.LegalEntityID, &ch.Currency, &ch.Exponent, &ch.Status, &ch.Reason, &ch.BaseRevision,
		&ch.Revision, &ch.Proposed, &ch.TotalBefore, &ch.TotalAfter, &ch.ApprovalRequestID, &ch.Version, &ch.CreatedAt, &ch.CreatedBy,
		&ch.UpdatedAt, &ch.UpdatedBy); err != nil {
		return PurchaseOrderChange{}, fmt.Errorf("scan purchase order change: %w", err)
	}
	return ch, nil
}

// getChange reads a change of the order the caller can see; lock takes
// the change row only, not its order.
func getChange(ctx context.Context, tx pgx.Tx, order, id uuid.UUID, lock bool) (PurchaseOrderChange, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return PurchaseOrderChange{}, err
	}
	q := changeSelect + ` WHERE ch.purchase_order_id = $1 AND ch.id = $2`
	if lock {
		q += ` FOR UPDATE OF ch`
	}
	ch, err := scanChange(tx.QueryRow(ctx, q, order, id))
	if err = authz.Visible(err, access, ch.LegalEntityID, problem.NotFoundOf("order change"), "procurement: get order change %s", id); err != nil {
		return PurchaseOrderChange{}, err
	}
	return ch, nil
}

// GetOrderChange returns a change of a purchase order.
func GetOrderChange(ctx context.Context, order, id uuid.UUID) (PurchaseOrderChange, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return PurchaseOrderChange{}, err
	}
	return getChange(ctx, tx, order, id, false)
}

// createChange drafts a change of a released order. The order is locked
// so that the change is checked against what it reads; nothing on the
// order changes until the change is applied (T47f).
func (e *Endpoints) createChange(ctx context.Context, order uuid.UUID, in ChangeInput) (PurchaseOrderChange, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return PurchaseOrderChange{}, err
	}
	o, err := getOrder(ctx, tx, order, true)
	if err != nil {
		return PurchaseOrderChange{}, err
	}
	if o.Status != Released {
		return PurchaseOrderChange{}, statusConflict("purchase order", o.Status, "an order change", Released)
	}
	base := ChangeProposal{OrderDate: o.OrderDate, PaymentTermCode: o.PaymentTermCode, Extensions: o.Extensions}
	if in.Items == nil {
		in.Items = currentItems(o)
	}
	proposed, after, err := e.proposal(ctx, tx, o, base, in)
	if err != nil {
		return PurchaseOrderChange{}, err
	}
	before, err := documentTotal(o.Items)
	if err != nil {
		return PurchaseOrderChange{}, err
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO procurement.purchase_order_change (tenant_id, id, purchase_order_id, reason, base_revision, proposed,
			total_before, total_after, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9)`,
		p.TenantID, id, order, *in.Reason, o.Revision, proposed, before, after, p.ID); err != nil {
		return PurchaseOrderChange{}, fmt.Errorf("procurement: draft change of purchase order %s: %w", order, err)
	}
	ch, err := getChange(ctx, tx, order, id, false)
	if err != nil {
		return PurchaseOrderChange{}, err
	}
	return ch, audit.Record(ctx, "procurement.purchase_order_change.create", "procurement.purchase_order_change", id,
		map[string]any{"purchase_order_id": order, "base_revision": o.Revision, "items": len(proposed.Items)})
}

// updateChange changes a draft change and checks it again against the
// order as it reads now: receipts may have been posted since.
func (e *Endpoints) updateChange(ctx context.Context, order, id uuid.UUID, ifMatch func(int32) error, in ChangeInput) (PurchaseOrderChange, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return PurchaseOrderChange{}, err
	}
	o, err := getOrder(ctx, tx, order, true)
	if err != nil {
		return PurchaseOrderChange{}, err
	}
	ch, err := loadChange(ctx, tx, order, id, ifMatch, "update")
	if err != nil {
		return PurchaseOrderChange{}, err
	}
	if o.Status != Released {
		return PurchaseOrderChange{}, statusConflict("purchase order", o.Status, "an order change", Released)
	}
	if in.Items == nil {
		in.Items = proposedItems(ch)
	}
	if in.Reason == nil {
		in.Reason = &ch.Reason
	}
	proposed, after, err := e.proposal(ctx, tx, o, ch.Proposed, in)
	if err != nil {
		return PurchaseOrderChange{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE procurement.purchase_order_change SET reason = $2, proposed = $3, total_after = $4, version = version + 1,
		updated_by = $5 WHERE id = $1`, id, *in.Reason, proposed, after, p.ID); err != nil {
		return PurchaseOrderChange{}, fmt.Errorf("procurement: update order change %s: %w", id, err)
	}
	updated, err := getChange(ctx, tx, order, id, false)
	if err != nil {
		return PurchaseOrderChange{}, err
	}
	return updated, audit.Record(ctx, "procurement.purchase_order_change.update", "procurement.purchase_order_change", id,
		map[string]any{"purchase_order_id": order, "items": len(proposed.Items)})
}

// cancelChange cancels a draft change for good; the order is unchanged.
func cancelChange(ctx context.Context, order, id uuid.UUID, ifMatch func(int32) error) (PurchaseOrderChange, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return PurchaseOrderChange{}, err
	}
	// The order first, as every write of a change takes the two locks.
	if _, err := getOrder(ctx, tx, order, true); err != nil {
		return PurchaseOrderChange{}, err
	}
	if _, err := loadChange(ctx, tx, order, id, ifMatch, "cancel"); err != nil {
		return PurchaseOrderChange{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE procurement.purchase_order_change SET status = 'cancelled', version = version + 1, updated_by = $2
		WHERE id = $1`, id, p.ID); err != nil {
		return PurchaseOrderChange{}, fmt.Errorf("procurement: cancel order change %s: %w", id, err)
	}
	cancelled, err := getChange(ctx, tx, order, id, false)
	if err != nil {
		return PurchaseOrderChange{}, err
	}
	return cancelled, audit.Record(ctx, "procurement.purchase_order_change.cancel", "procurement.purchase_order_change", id,
		map[string]any{"purchase_order_id": order})
}

// loadChange locks a draft change at the version the caller read.
func loadChange(ctx context.Context, tx pgx.Tx, order, id uuid.UUID, ifMatch func(int32) error, action string) (PurchaseOrderChange, error) {
	ch, err := getChange(ctx, tx, order, id, true)
	if err != nil {
		return PurchaseOrderChange{}, err
	}
	if err := ifMatch(ch.Version); err != nil {
		return PurchaseOrderChange{}, err
	}
	if ch.Status != Draft {
		return PurchaseOrderChange{}, statusConflict("order change", ch.Status, action, Draft)
	}
	return ch, nil
}

// proposal checks a change against its order: the header fields given
// replace base's, the items are checked as an order's are under the
// order's supplier, currency and contract, and keep what a released
// order's items keep. It returns the proposal and its total.
func (e *Endpoints) proposal(ctx context.Context, tx pgx.Tx, o PurchaseOrder, base ChangeProposal, in ChangeInput) (ChangeProposal, money.Minor, error) {
	pr := base
	if in.OrderDate != nil {
		pr.OrderDate = *in.OrderDate
	}
	if in.Extensions != nil {
		pr.Extensions = mergepatch.Apply(pr.Extensions, in.Extensions)
		if err := extensions.Check(pr.Extensions); err != nil {
			return ChangeProposal{}, 0, err
		}
	}
	le, err := e.legalEntity(ctx, o.LegalEntityID)
	if err != nil {
		return ChangeProposal{}, 0, err
	}
	if in.PaymentTermCode != nil {
		if pr.PaymentTermCode, err = e.paymentTerm(ctx, in.PaymentTermCode, o.SupplierID, le.ID); err != nil {
			return ChangeProposal{}, 0, err
		}
	}
	h, err := e.header(ctx, tx, le, o.SupplierID, o.Currency, pr.OrderDate, o.ContractID, o.PurchasingOrgID)
	if err != nil {
		return ChangeProposal{}, 0, err
	}
	if pr.Items, err = e.orderItems(ctx, h, keptTolerance(o.Items, in.Items)); err != nil {
		return ChangeProposal{}, 0, err
	}
	if err := keptItems(o.Items, pr.Items); err != nil {
		return ChangeProposal{}, 0, err
	}
	after, err := documentTotal(pr.Items)
	return pr, after, err
}

// keptTolerance gives an item the change keeps the tolerance it has when
// the change leaves it out, rather than the company default a new item
// takes: a change of price alone does not move the tolerance, nor refuse
// an item over-delivered within it.
func keptTolerance(current []Item, in []ItemInput) []ItemInput {
	out := slices.Clone(in)
	for i := range out {
		if i < len(current) && out[i].OverDelivery == nil {
			out[i].OverDelivery = &current[i].OverDelivery
		}
	}
	return out
}

// keptItems refuses changed items that do not keep what a released
// order's items keep (check_order_item_editable, 000065): its account,
// unit, material and contract item, and the quantity received, allowing
// the item's over-delivery tolerance; an item with receipts stays.
func keptItems(current, changed []Item) error {
	for i, cur := range current {
		field := fmt.Sprintf("items[%d]", i)
		if i >= len(changed) {
			if hasReceipts(cur) {
				return invalid("procurement.invalid_change", "items", fmt.Sprintf("item %d has receipts and stays", cur.No))
			}
			continue
		}
		it := changed[i]
		if it.AccountID != cur.AccountID || it.Unit != cur.Unit || !samePtr(it.MaterialID, cur.MaterialID) || !samePtr(it.ContractItemNo, cur.ContractItemNo) {
			return invalid("procurement.invalid_change", field, "an item keeps its account, unit, material and contract item")
		}
		if belowReceived(it, cur.Received) {
			return invalid("procurement.invalid_change", field+".quantity", "at least the "+cur.Received+" received, allowing the over-delivery tolerance")
		}
	}
	return nil
}

// hasReceipts reports whether an order item's received quantity, a plain
// decimal from the order select, is not zero; one that does not parse
// counts as received, so the check fails closed.
func hasReceipts(it Item) bool {
	got, ok := new(big.Rat).SetString(it.Received)
	return !ok || got.Sign() != 0
}

// belowReceived reports whether quantity × (1 + tolerance/100) is below
// what was received, as the database checks it; values that do not parse
// fail closed.
func belowReceived(it Item, received string) bool {
	q, okQ := new(big.Rat).SetString(it.Quantity)
	tol, okT := new(big.Rat).SetString(it.OverDelivery)
	got, okR := new(big.Rat).SetString(received)
	if !okQ || !okT || !okR {
		return true
	}
	limit := new(big.Rat).Mul(q, new(big.Rat).Add(big.NewRat(1, 1), new(big.Rat).Quo(tol, big.NewRat(100, 1))))
	return limit.Cmp(got) < 0
}

func samePtr[T comparable](a, b *T) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

// proposedItems are a change's items as input, so that a patch that
// leaves them out checks them again.
func proposedItems(ch PurchaseOrderChange) []ItemInput {
	return currentItems(PurchaseOrder{Currency: ch.Currency, Exponent: ch.Exponent, Items: ch.Proposed.Items})
}
