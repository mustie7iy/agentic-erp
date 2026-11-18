package revenue

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/sales"
)

// A contract follows its sales order (T81e; ADR-0068, As built T81e).
// The sales context changes an order in place and cancels it (T78d) and
// publishes SalesOrderChanged; the contract consumer reads it and brings
// the order's contract to the order as it is then, as SAP's Revenue
// Accounting takes an order's changes from sales after the fact. Reading
// the order as it is, not the event's figures, makes a follow idempotent
// per state: an event read twice, out of order, or for a change that
// leaves the order's values as they were, finds nothing to do.

// What made a follow, as revenue.contract_change keeps it.
const (
	byOrderChange       = "sales_order_changed"
	byOrderCancellation = "sales_order_cancelled"
)

// ObligationFollow is what a follow did to one obligation: its order
// value and its allocated amount before and after (allocated nil while
// the contract is a draft), whether it ended and whether the follow added
// it (nothing before). Amounts are minor units of the contract's
// currency.
type ObligationFollow struct {
	No              int32        `json:"obligation_no"`
	NetBefore       money.Minor  `json:"net_amount_before"`
	NetAfter        money.Minor  `json:"net_amount_after"`
	AllocatedBefore *money.Minor `json:"allocated_amount_before"`
	AllocatedAfter  *money.Minor `json:"allocated_amount_after"`
	Ended           bool         `json:"ended"`
	Added           bool         `json:"added"`
}

// followed is what the order as it is makes of its contract: the
// obligations after the follow, those of them it adds, what it did to
// each that it changed or added, the transaction price after it, and
// whether it ends the contract.
type followed struct {
	obligations []Obligation
	added       []Obligation
	changes     []ObligationFollow
	price       money.Minor
	ends        bool
}

// followOf works out what the order makes of the contract (ADR-0068, As
// built T81e), and says whether that is anything: each obligation that
// has not ended takes its order item's net value as its order value, and
// ends when the order is cancelled or its item removed (T78e); an item
// added to the order (T78e) gets an obligation as at the contract's
// creation, numbered after the highest, satisfied at a point in time with
// its net value as its stand-alone selling price; the transaction price
// is the order values of the obligations that stand added up, the order's
// net value; an active contract's shares are that price allocated by the
// stand-alone selling prices (allocate), an ended obligation weighing
// nothing. An ended obligation stays as it is. Nothing a person judged
// changes.
func followOf(k Contract, o sales.Order) (followed, bool) {
	cancelled := o.Status == sales.Cancelled
	after, ended := orderValues(k, o, cancelled)
	f := followed{ends: cancelled}
	if !cancelled {
		f.added = newObligations(k, o)
		after = append(after, f.added...)
		ended = append(ended, make([]bool, len(f.added))...)
	}
	nets, weights := make([]money.Minor, 0, len(after)), make([]money.Minor, len(after))
	for i, ob := range after {
		if ob.EndedAt == nil && !ended[i] {
			weights[i] = ob.StandalonePrice
			nets = append(nets, ob.Net)
		}
	}
	// The order values are the net values of the order's items that stand, which add up to its net value, an amount: cannot fail.
	f.price, _ = money.Sum(nets...)
	if k.Status == Active {
		for i, share := range allocate(f.price, weights) {
			after[i].Allocated = &share
		}
	}
	for i := range after {
		if ended[i] && after[i].Allocated == nil { // a draft's ended obligation takes nothing when it is activated, and is not changed then
			after[i].Allocated = new(money.Minor)
		}
	}
	f.obligations, f.added = after, after[len(k.Obligations):] // the added ones with their shares
	for i, ob := range after {
		before := Obligation{}
		if i < len(k.Obligations) {
			before = k.Obligations[i]
		}
		added := i >= len(k.Obligations)
		if ob.EndedAt != nil || (!added && !ended[i] && ob.Net == before.Net && sameAmount(ob.Allocated, before.Allocated)) {
			continue
		}
		f.changes = append(f.changes, ObligationFollow{No: ob.No, NetBefore: before.Net, NetAfter: ob.Net, AllocatedBefore: before.Allocated,
			AllocatedAfter: ob.Allocated, Ended: ended[i], Added: added})
	}
	return f, len(f.changes) > 0 || f.price != k.TransactionPrice || cancelled
}

// orderValues are the contract's obligations with their order items' net
// values, and which of them the order's cancellation or their item's
// removal ends; an obligation that has ended stays as it is.
func orderValues(k Contract, o sales.Order, cancelled bool) ([]Obligation, []bool) {
	items := make(map[int32]sales.OrderItem, len(o.Items))
	for _, it := range o.Items {
		items[it.ItemNo] = it
	}
	after, ended := append([]Obligation(nil), k.Obligations...), make([]bool, len(k.Obligations))
	for i, ob := range after {
		if ob.EndedAt != nil {
			continue
		}
		item, ok := items[ob.OrderItemNo]
		if cancelled || !ok || item.RemovedInChange != nil {
			ended[i] = true
			continue
		}
		after[i].Net = item.Net
	}
	return after, ended
}

// newObligations are the obligations of the order's items that stand and
// have none, in the items' order, numbered after the contract's highest:
// each starts as a contract's do (ADR-0068 §3).
func newObligations(k Contract, o sales.Order) []Obligation {
	has := make(map[int32]bool, len(k.Obligations))
	var no int32
	for _, ob := range k.Obligations {
		has[ob.OrderItemNo], no = true, max(no, ob.No)
	}
	var out []Obligation
	for _, it := range o.Items {
		if has[it.ItemNo] || it.RemovedInChange != nil {
			continue
		}
		no++
		out = append(out, Obligation{No: no, OrderItemNo: it.ItemNo, ItemKey: it.ItemKey, Description: it.Description, Net: it.Net, Timing: PointInTime,
			StandalonePrice: it.Net})
	}
	return out
}

// sameAmount says whether two optional amounts are the same.
func sameAmount(a, b *money.Minor) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// lockedContractOf reads the sales order's contract under its row lock
// (ADR-0008), or says it has none. The lock is taken by a statement of
// its own, so the obligations read after it are as the transaction that
// held it left them.
func lockedContractOf(ctx context.Context, tx pgx.Tx, order uuid.UUID) (Contract, bool, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM revenue.contract WHERE sales_order_id = $1 FOR UPDATE`, order).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Contract{}, false, nil
	}
	if err != nil {
		return Contract{}, false, fmt.Errorf("revenue: lock the contract of sales order %s: %w", order, err)
	}
	k, err := load(ctx, tx, id)
	return k, err == nil, err
}

// follow brings the contract to its order as it is (followOf), as p, in
// tx, and returns the contract after it: the follow is kept as the
// contract's next change with its values
// before and after, the obligations and the contract are written, and
// the audit record and RevenueContractChanged go in the same transaction.
// A contract that ended, or whose order changed nothing of it, is left as
// it is. Nothing is posted: the next recognition run reads the contract
// as it now is (ADR-0068 §9).
func (e *Endpoints) follow(ctx context.Context, tx pgx.Tx, p reqctx.Principal, k Contract, o sales.Order) (Contract, error) {
	if k.EndedAt != nil {
		return k, nil
	}
	f, changed := followOf(k, o)
	if !changed {
		return k, nil
	}
	no, cause := k.Changes+1, byOrderChange
	if f.ends {
		cause = byOrderCancellation
	}
	if _, err := tx.Exec(ctx, `INSERT INTO revenue.contract_change (tenant_id, id, contract_id, change_no, cause, sales_order_version, sales_order_changes,
			transaction_price_before, transaction_price_after, obligations, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		p.TenantID, uuid.Must(uuid.NewV7()), k.ID, no, cause, o.Version, o.Changes, k.TransactionPrice, f.price, f.changes, p.ID); err != nil {
		return Contract{}, fmt.Errorf("revenue: keep follow %d of contract %s: %w", no, k.ID, err)
	}
	if err := insertObligations(ctx, tx, p.TenantID, k.ID, f.added); err != nil {
		return Contract{}, err
	}
	now := clock.Now(e.Now)
	if err := writeFollow(ctx, tx, k, f, now); err != nil {
		return Contract{}, err
	}
	var end *time.Time
	if f.ends {
		end = &now
	}
	// The contract's row is locked (lockedContractOf), so it is as k read it.
	if _, err := tx.Exec(ctx, `UPDATE revenue.contract SET transaction_price = $2, changes = $3, ended_at = $4, version = version + 1, updated_by = $5
		WHERE id = $1`, k.ID, f.price, no, end, p.ID); err != nil {
		return Contract{}, fmt.Errorf("revenue: write follow %d of contract %s: %w", no, k.ID, err)
	}
	after, err := load(ctx, tx, k.ID)
	if err != nil {
		return Contract{}, err
	}
	return after, e.recordFollow(ctx, after, k, f, no, cause)
}

// writeFollow writes the obligations the follow changed and did not add:
// their order value, allocated amount and end.
func writeFollow(ctx context.Context, tx pgx.Tx, k Contract, f followed, now time.Time) error {
	var changed []ObligationFollow
	for _, ch := range f.changes {
		if !ch.Added {
			changed = append(changed, ch)
		}
	}
	if len(changed) == 0 {
		return nil
	}
	n := len(changed)
	numbers, nets, allocated, ended := make([]int32, n), make([]int64, n), make([]*int64, n), make([]*time.Time, n)
	for i, ch := range changed {
		numbers[i], nets[i] = ch.No, int64(ch.NetAfter)
		if ch.AllocatedAfter != nil {
			a := int64(*ch.AllocatedAfter)
			allocated[i] = &a
		}
		if ch.Ended {
			ended[i] = &now
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE revenue.performance_obligation o SET net_amount = n.net, allocated_amount = n.allocated, ended_at = n.ended
		FROM unnest($2::int[], $3::bigint[], $4::bigint[], $5::timestamptz[]) AS n (obligation_no, net, allocated, ended)
		WHERE o.contract_id = $1 AND o.obligation_no = n.obligation_no`, k.ID, numbers, nets, allocated, ended); err != nil {
		return fmt.Errorf("revenue: write the obligations of contract %s followed: %w", k.ID, err)
	}
	return nil
}

// recordFollow writes the follow's audit record, with its number, cause,
// transaction price before and what it did to each obligation, and
// RevenueContractChanged, the status kept.
func (e *Endpoints) recordFollow(ctx context.Context, after, before Contract, f followed, no int32, cause string) error {
	le, _, err := e.LegalEntities.Entity(ctx, after.LegalEntityID) // a stored contract's legal entity exists
	if err != nil {
		return fmt.Errorf("revenue: company of contract %s: %w", after.ID, err)
	}
	names, err := e.Partners.Names(ctx, []uuid.UUID{after.CustomerID})
	if err != nil {
		return fmt.Errorf("revenue: customer of contract %s: %w", after.ID, err)
	}
	extra := map[string]any{"change_no": no, "transaction_price_before": money.EventAmount(before.TransactionPrice, before.Currency, before.Exponent),
		"obligations_changed": f.changes}
	return record(ctx, "revenue.contract.follow", cause, after, le, names[after.CustomerID].Number, &before.Status, extra)
}

// current is the active contract k of a recognition run brought to its
// order first (T81e, review round 1): the run reads the order as it is
// now, so a contract whose order's change the consumer has not followed
// yet is followed here, in the run's transaction, the order under its
// share lock and then the contract under its row lock, the order the
// consumer takes them in. A follow is idempotent, so the consumer finds
// nothing left to do.
func (e *Endpoints) current(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, k Contract) (Contract, sales.Order, error) {
	order, _, err := e.SalesOrders.Shared(ctx, k.SalesOrderID) // a stored contract's sales order exists
	if err != nil {
		return Contract{}, sales.Order{}, fmt.Errorf("revenue: sales order of contract %s: %w", k.ID, err)
	}
	locked, _, err := lockedContractOf(ctx, tx, k.SalesOrderID) // the contract is there: the run read it
	if err != nil {
		return Contract{}, sales.Order{}, err
	}
	after, err := e.follow(ctx, tx, reqctx.Principal{ID: BuilderActor, Kind: reqctx.KindSystem, TenantID: tenant}, locked, order)
	return after, order, err
}

// asOrdered is the contract as a follow of its order makes it, which a
// read of its schedule takes before the consumer has followed the
// order's change (T81e, review round 1): a read writes nothing, so it
// computes what the follow will write.
func asOrdered(k Contract, o sales.Order) Contract {
	if k.EndedAt != nil {
		return k
	}
	if f, changed := followOf(k, o); changed {
		k.Obligations, k.TransactionPrice = f.obligations, f.price
	}
	return k
}
