package sales

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Items added and removed and the payment term changed in a change of a
// sales order (T78e; ADR-0067, As built T78e), as SAP's VA02 adds an
// item to an open order, deletes one without follow-on documents and
// changes the header's terms of payment. A removed item stays on the
// order, marked with its change, since the application deletes no
// business data (000011).

// What a change did to an item (ItemChange.Kind).
const (
	ItemChanged = "changed"
	ItemAdded   = "added"
	ItemRemoved = "removed"
)

// MaxOrderItems is the most items an order holds, removed ones included:
// as many as a new order takes (createSalesOrder's items).
const MaxOrderItems = 999

func init() {
	// The handlers check under the order's row lock; the rule is the
	// database's and answers alike (a delivery of a removed item).
	problem.RegisterConstraint("sales_order_item_removed_undelivered", func() *problem.Problem {
		return problem.Unprocessable("sales.order_item_removed", "The sales order's item is removed; it takes no delivery.")
	})
}

// itemRemoved refuses something asked of a removed item, naming the
// field that names it (422 sales.order_item_removed).
func itemRemoved(field string, o Order, it OrderItem) *problem.Problem {
	return problem.Invalid("sales.order_item_removed", field, fmt.Sprintf("Item %d of sales order %s was removed in change %d; it changes no more and is not delivered.",
		it.ItemNo, o.DocumentNumber, *it.RemovedInChange))
}

// changedTerm is the order's payment term after the change: the one
// given, which must be configured (paymentTerm), else the order's. A new
// term is refused once a billing document of the order stands that is
// not cancelled (422 sales.order_billed naming payment_term_code): a
// billing document reads its order's term. The order is read under its
// row lock, which a billing document's creation also takes, so the check
// holds.
func (e *Endpoints) changedTerm(ctx context.Context, tx pgx.Tx, o Order, given *string) (string, error) {
	if given == nil || *given == o.PaymentTermCode {
		return o.PaymentTermCode, nil
	}
	term, err := e.paymentTerm(ctx, given, partner.Customer{}) // a term is given, so the customer's is not read
	if err != nil {
		return "", err
	}
	var billed bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM sales.billing_document WHERE sales_order_id = $1 AND status <> $2)`, o.ID,
		Cancelled).Scan(&billed); err != nil {
		return "", fmt.Errorf("sales: billing documents of sales order %s: %w", o.ID, err)
	}
	if billed {
		return "", problem.Invalid("sales.order_billed", "payment_term_code", "Sales order "+o.DocumentNumber+
			" has a billing document, which is billed with the order's payment term; it stays "+o.PaymentTermCode+". Cancel the billing documents first.")
	}
	return term, nil
}

// changeCheck is the item check a change prices its items with: the
// order's document on the order's date, with the units and materials of
// the order's items and of the items added known.
func (e *Endpoints) changeCheck(ctx context.Context, tx pgx.Tx, o Order, le tenancy.Entity, added []ItemInput) (itemCheck, error) {
	doc, err := e.document(ctx, tx, le, o.CustomerID, o.Currency)
	if err != nil {
		return itemCheck{}, err
	}
	doc.date = o.OrderDate
	units := map[string]bool{}
	if len(added) > 0 {
		named := make([]string, len(added))
		for i, it := range added {
			named[i] = it.Unit
		}
		if units, err = knownUnits(ctx, tx, named); err != nil {
			return itemCheck{}, err
		}
	}
	named := append([]ItemInput(nil), added...)
	for _, it := range o.Items {
		units[it.Unit] = true
		named = append(named, ItemInput{MaterialID: it.MaterialID})
	}
	ms, err := e.itemMaterials(ctx, named)
	if err != nil {
		return itemCheck{}, err
	}
	return itemCheck{e: e, tx: tx, doc: doc, units: units, materials: ms, accounts: map[uuid.UUID]bool{}, codes: map[string]bool{}}, nil
}

// removeItems removes the items the request names (T78e), each once and
// not also changed (422 sales.item_repeated naming removed_items[i]), an
// item of the order (sales.unknown_order_item) that is not removed
// already (sales.order_item_removed) and of which nothing is delivered
// or held by an outbound delivery (sales.order_item_delivered; only what
// is delivered is billed; ADR-0083 §4). A
// removed item is marked with the change and is no longer ordered: its
// change has nothing after it.
func removeItems(o Order, items []OrderItem, changes []ItemChange, in OrderChangeInput) ([]OrderItem, []ItemChange, error) {
	byNo := make(map[int32]int, len(o.Items))
	for i, it := range o.Items {
		byNo[it.ItemNo] = i
	}
	named := make(map[int32]bool, len(in.Items)+len(in.Removed))
	for _, ch := range in.Items {
		named[ch.ItemNo] = true
	}
	no := o.Changes + 1
	for i, n := range in.Removed {
		field := fmt.Sprintf("removed_items[%d]", i)
		at, ok := byNo[n]
		switch {
		case !ok:
			return nil, nil, problem.Invalid("sales.unknown_order_item", field, fmt.Sprintf("Sales order %s has no item %d.", o.DocumentNumber, n))
		case named[n]:
			return nil, nil, problem.Invalid("sales.item_repeated", field, fmt.Sprintf("A change names an item once; item %d is named more than once.", n))
		case o.Items[at].RemovedInChange != nil:
			return nil, nil, itemRemoved(field, o, o.Items[at])
		case o.Items[at].taken() != "":
			return nil, nil, problem.Invalid("sales.order_item_delivered", field, fmt.Sprintf(
				"Item %d of sales order %s has %s, so it is not removed.", n, o.DocumentNumber, o.Items[at].taken()))
		}
		named[n] = true
		it := o.Items[at]
		items[at].RemovedInChange = &no
		changes = append(changes, ItemChange{ItemNo: n, Kind: ItemRemoved, Unit: it.Unit, QuantityBefore: it.Quantity, QuantityAfter: "0",
			UnitPriceBefore: it.UnitPrice, UnitPriceAfter: it.UnitPrice, PriceUnitBefore: it.PriceUnit, PriceUnitAfter: it.PriceUnit,
			NetBefore: it.Net})
	}
	return items, changes, nil
}

// addItems checks and prices the items the request adds (T78e) as a new
// order's items are, on the order's date, and numbers them after the
// order's highest; the order holds at most MaxOrderItems (422
// sales.too_many_items naming new_items). A refusal names the request's
// new_items[i]. An added item had nothing before it.
func addItems(ctx context.Context, check itemCheck, o Order, items []OrderItem, changes []ItemChange, in []ItemInput) ([]OrderItem, []ItemChange, error) {
	if len(o.Items)+len(in) > MaxOrderItems {
		return nil, nil, problem.Invalid("sales.too_many_items", "new_items", fmt.Sprintf("Sales order %s has %d items; an order holds at most %d.",
			o.DocumentNumber, len(o.Items), MaxOrderItems))
	}
	next := int32(len(o.Items)) // the highest number: items are numbered from 1 without a gap and never deleted
	for i, input := range in {
		it, err := check.item(ctx, i, input)
		if err != nil {
			return nil, nil, asNewItem(err)
		}
		next++
		it.ItemNo = next
		items = append(items, OrderItem{Item: it, Delivered: "0", Billed: "0", Returned: "0"})
		changes = append(changes, ItemChange{ItemNo: it.ItemNo, Kind: ItemAdded, Unit: it.Unit, QuantityBefore: "0", QuantityAfter: it.Quantity,
			UnitPriceAfter: it.UnitPrice, PriceUnitBefore: it.PriceUnit, PriceUnitAfter: it.PriceUnit, NetAfter: it.Net})
	}
	if len(standing(items)) == 0 {
		return nil, nil, problem.Invalid("sales.order_needs_item", "removed_items", "Sales order "+o.DocumentNumber+
			" keeps at least one item; cancel the order instead (cancelSalesOrder).")
	}
	return items, changes, nil
}

// asNewItem names, in a refusal of an item the request adds, the
// request's new_items[i] where the item check names items[i]. The problem
// is copied, never changed: its maker may keep it (as asPosting does).
func asNewItem(err error) error {
	var p *problem.Problem
	if !errors.As(err, &p) {
		return err
	}
	refused := *p
	refused.Errors = slices.Clone(p.Errors)
	for i, f := range refused.Errors {
		if rest, ok := strings.CutPrefix(f.Field, "items["); ok {
			refused.Errors[i].Field = "new_items[" + rest
		}
	}
	return &refused
}

// writeItemsAddedRemoved marks the items the change removes with its
// number and writes the items it adds, after the change is kept: the
// items' rules find it (sales_order_item_removed_kept,
// sales_order_item_added_kept).
func writeItemsAddedRemoved(ctx context.Context, tx pgx.Tx, p reqctx.Principal, o Order, c changed, no int32) error {
	var removed []int32
	var added []Item
	for _, ch := range c.changes {
		switch ch.Kind {
		case ItemRemoved:
			removed = append(removed, ch.ItemNo)
		case ItemAdded:
			added = append(added, c.items[ch.ItemNo-1].Item)
		}
	}
	if len(removed) > 0 {
		if _, err := tx.Exec(ctx, `UPDATE sales.sales_order_item SET removed_in_change = $3 WHERE sales_order_id = $1 AND item_no = ANY($2)`,
			o.ID, removed, no); err != nil {
			return fmt.Errorf("sales: remove items of sales order %s: %w", o.ID, err)
		}
	}
	if len(added) > 0 {
		return insertItems(ctx, tx, "sales_order_item", "sales_order_id", p.TenantID, o.ID, added)
	}
	return nil
}
