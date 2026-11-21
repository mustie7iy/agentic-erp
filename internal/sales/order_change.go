package sales

import (
	"cmp"
	"context"
	"fmt"
	"math/big"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// OrderChangeInput is a change of a sales order as a client sends it
// (T78d): a reason, the requested delivery date when DeliveryDateSet
// (DeliveryDate nil clears it) and the items that change; from T78e the
// items added (NewItems, as a new order's), the items removed by number
// and the payment term (nil: it stays).
type OrderChangeInput struct {
	Reason          *string
	DeliveryDateSet bool
	DeliveryDate    *time.Time
	Items           []ItemChangeInput
	NewItems        []ItemInput
	Removed         []int32
	PaymentTerm     *string
}

// ItemChangeInput is a change of one item of a sales order: its new
// quantity (Quantity and Unit, nil when it stays) and its new unit price
// per PriceUnit (nil when it stays; PriceUnit 0: per 1).
type ItemChangeInput struct {
	ItemNo         int32
	Quantity, Unit *string
	UnitPrice      *Amount
	PriceUnit      int32
}

// ItemChange is what a change did to one item of an order: its quantity
// (decimal strings in Unit), unit price per price unit and net value
// before and after, amounts in minor units of the order's currency. Kind
// says what the change did (T78e): ItemChanged, ItemAdded (nothing
// before: a quantity and a net value of zero) or ItemRemoved (nothing
// after); a change kept before T78e has none and is ItemChanged.
type ItemChange struct {
	ItemNo          int32       `json:"item_no"`
	Kind            string      `json:"change,omitempty"`
	Unit            string      `json:"unit"`
	QuantityBefore  string      `json:"quantity_before"`
	QuantityAfter   string      `json:"quantity_after"`
	UnitPriceBefore money.Minor `json:"unit_price_before"`
	UnitPriceAfter  money.Minor `json:"unit_price_after"`
	PriceUnitBefore int32       `json:"price_unit_before"`
	PriceUnitAfter  int32       `json:"price_unit_after"`
	NetBefore       money.Minor `json:"net_amount_before"`
	NetAfter        money.Minor `json:"net_amount_after"`
}

// OrderChange is a row of sales.sales_order_change: a change of an order,
// numbered within it, with the order's version after it and what it
// changed before and after. Amounts are minor units of Currency, whose
// minor-unit digits are Exponent. TermBefore and TermAfter are the payment
// term a change changed (T78e), nil for one that kept it.
type OrderChange struct {
	ID                            uuid.UUID
	No                            int32
	OrderID                       uuid.UUID
	Version                       int32
	Reason                        *string
	Currency                      string
	Exponent                      int
	NetBefore, NetAfter           money.Minor
	DeliveryBefore, DeliveryAfter *time.Time
	BlockBefore, BlockAfter       string
	TermBefore, TermAfter         *string
	Items                         []ItemChange
	CreatedAt                     time.Time
	CreatedBy                     uuid.UUID
}

// changeSelect reads an order's changes with the order's currency. It
// reads the currency by a subquery, not a join, for the reason orderSelect
// names the quotation by one: the keyset of the list orders by an
// unqualified id.
const changeSelect = `SELECT h.id, h.change_no, h.sales_order_id, h.order_version, h.reason, x.currency, x.exponent, h.net_amount_before,
		h.net_amount_after, h.requested_delivery_date_before, h.requested_delivery_date_after, h.credit_block_before, h.credit_block_after,
		h.payment_term_code_before, h.payment_term_code_after, h.items, h.created_at, h.created_by
	FROM sales.sales_order_change h CROSS JOIN LATERAL (SELECT o.currency, c.exponent FROM sales.sales_order o JOIN ref.currency c ON c.code = o.currency
		WHERE o.tenant_id = h.tenant_id AND o.id = h.sales_order_id) x`

func scanChange(row pgx.Row) (OrderChange, error) {
	var h OrderChange
	err := row.Scan(&h.ID, &h.No, &h.OrderID, &h.Version, &h.Reason, &h.Currency, &h.Exponent, &h.NetBefore, &h.NetAfter, &h.DeliveryBefore,
		&h.DeliveryAfter, &h.BlockBefore, &h.BlockAfter, &h.TermBefore, &h.TermAfter, &h.Items, &h.CreatedAt, &h.CreatedBy)
	for i := range h.Items {
		if h.Items[i].Kind == "" {
			h.Items[i].Kind = ItemChanged // kept before T78e, which changed items alone
		}
	}
	return h, errs.Wrapf(err, "scan sales order change")
}

// deliveryDateFrom refuses a requested delivery date before the order's
// date (422 sales.invalid_requested_delivery_date): delivery is asked for
// on the order's date or later (sales_order_requested_delivery).
func deliveryDateFrom(given *time.Time, orderDate time.Time) error {
	if given == nil || !given.Before(orderDate) {
		return nil
	}
	return problem.Invalid("sales.invalid_requested_delivery_date", "requested_delivery_date",
		"The requested delivery date is the order's date, "+orderDate.Format(time.DateOnly)+", or later.")
}

// changeableOrder reads the order under its row lock (ADR-0008) at the version
// the caller read, and refuses a cancelled one (409 sales.invalid_status
// saying what was asked).
func changeableOrder(ctx context.Context, tx pgx.Tx, id uuid.UUID, ifMatch func(int32) error, what string) (Order, error) {
	o, err := order(ctx, tx, id, true)
	if err != nil {
		return Order{}, err
	}
	if err := ifMatch(o.Version); err != nil {
		return Order{}, err
	}
	if o.Status == Cancelled {
		return Order{}, problem.Conflict("sales.invalid_status", "Sales order "+o.DocumentNumber+" is cancelled; "+what)
	}
	return o, nil
}

// changed is what a change makes of an order: its items after it (a
// removed one marked, an added one numbered after the highest), the
// changes of those that changed, its net value, requested delivery date
// and payment term after it, and the credit check of an increase (nil
// when none was made).
type changed struct {
	items    []OrderItem
	changes  []ItemChange
	net      money.Minor
	delivery *time.Time
	term     string
	check    *checked
}

// ChangeOrder changes a draft or confirmed order in place (ADR-0067, As
// built T78d): items' quantities and prices and the requested delivery
// date. The order is read under its row lock at the version the caller
// read; a cancelled one is refused (409 sales.invalid_status). Each item
// changed is checked and priced again (changeItems); a request that
// changes nothing is refused (422 sales.nothing_changed). A confirmed
// order whose value rises is checked for credit on the increase
// (increaseCheck) and blocked again over the limit. The change is kept
// with its values before and after, the order's change count and version
// move, and its audit record and SalesOrderChanged (with
// SalesOrderCreditBlocked for an order blocked again) are written in the
// request's transaction. A confirmed order with a plant has the items the
// change added or changed the quantity of checked for availability again
// (quantityChanged; S2j). Nothing is posted.
func (e *Endpoints) ChangeOrder(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, in OrderChangeInput) (Order, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Order{}, err
	}
	o, err := changeableOrder(ctx, tx, id, ifMatch, "a cancelled order is not changed.")
	if err != nil {
		return Order{}, err
	}
	le, _, err := e.LegalEntities.Entity(ctx, o.LegalEntityID) // a stored order's legal entity exists
	if err != nil {
		return Order{}, fmt.Errorf("sales: company of sales order %s: %w", id, err)
	}
	c, err := e.change(ctx, tx, o, le, in)
	if err != nil {
		return Order{}, err
	}
	block := o.CreditBlock
	if c.check != nil && c.check.block == Blocked {
		block = Blocked
	}
	no, err := writeChange(ctx, tx, p, o, c, block, in.Reason)
	if err != nil {
		return Order{}, err
	}
	var promises []promise
	if o.Status == Confirmed {
		if promises, err = e.availability(ctx, tx, o, c.items, quantityChanged(c.changes)); err != nil {
			return Order{}, err
		}
	}
	after, err := loadOrder(ctx, tx, id, false)
	if err != nil {
		return Order{}, err
	}
	names, err := e.Partners.Names(ctx, []uuid.UUID{o.CustomerID})
	if err != nil {
		return Order{}, fmt.Errorf("sales: customer of sales order %s: %w", id, err)
	}
	extra := map[string]any{"change_no": no, "reason": in.Reason, "items_changed": len(c.changes), "net_amount_before": money.EventAmount(o.Net, o.Currency, o.Exponent)}
	if checked := promised(promises, o.PlantID); checked != nil {
		extra["availability"] = checked
	}
	return after, recordOrder(ctx, "sales.sales_order.change", after, le, names[o.CustomerID].Number, &o.Status, c.check, extra)
}

// quantityChanged reports whether a change added the item or changed its
// quantity: what a confirmed order's availability check confirms again
// (S2j), as far as there is stock, keeping what the item had confirmed up
// to its new quantity.
func quantityChanged(changes []ItemChange) func(OrderItem) bool {
	return func(it OrderItem) bool {
		i := slices.IndexFunc(changes, func(c ItemChange) bool { return c.ItemNo == it.ItemNo })
		return i >= 0 && (changes[i].Kind == ItemAdded ||
			changes[i].Kind == ItemChanged && money.ParseRat(changes[i].QuantityBefore).Cmp(money.ParseRat(changes[i].QuantityAfter)) != 0)
	}
}

// change works out what the request makes of the order: its items
// (changeItems), its net value, the items that stand added up, its
// requested delivery date, its payment term (changedTerm) and, for a
// confirmed order whose value rises, the credit check of the increase.
func (e *Endpoints) change(ctx context.Context, tx pgx.Tx, o Order, le tenancy.Entity, in OrderChangeInput) (changed, error) {
	c := changed{delivery: o.DeliveryDate}
	if in.DeliveryDateSet {
		if err := deliveryDateFrom(in.DeliveryDate, o.OrderDate); err != nil {
			return changed{}, err
		}
		c.delivery = in.DeliveryDate
	}
	var err error
	if c.term, err = e.changedTerm(ctx, tx, o, in.PaymentTerm); err != nil {
		return changed{}, err
	}
	if c.items, c.changes, err = e.changeItems(ctx, tx, o, le, in); err != nil {
		return changed{}, err
	}
	if len(c.changes) == 0 && clock.SameDay(c.delivery, o.DeliveryDate) && c.term == o.PaymentTermCode {
		return changed{}, problem.Unprocessable("sales.nothing_changed", "The request changes nothing of sales order "+o.DocumentNumber+".")
	}
	kept := standing(c.items)
	nets := make([]money.Minor, len(kept))
	for i, it := range kept {
		nets[i] = it.Net
	}
	if c.net, err = money.Sum(nets...); err != nil { // money.Sum's only error is the range
		return changed{}, errTooLarge()
	}
	if o.Status == Confirmed && o.CreditBlock != Blocked && c.net > o.Net {
		check, err := e.increaseCheck(ctx, tx, o, c.items, le)
		if err != nil {
			return changed{}, err
		}
		c.check = &check
	}
	return c, nil
}

// itemPlan is how one change of the request applies to its item: the
// request's place, the order's item and what the item is checked and
// priced from.
type itemPlan struct {
	i     int
	item  OrderItem
	input ItemInput
}

// changeItems checks the request's item changes, removals and additions
// and prices each changed and added item, returning every item of the
// order as it is after the change and the changes, by item number. An
// item is named by its number, once (422 sales.unknown_order_item,
// sales.item_repeated naming items[i].item_no), in its own unit
// (sales.unit_mismatch); an item of which something is billed keeps its
// quantity and price (422 sales.order_item_billed naming items[i]); a
// removed one changes no more (422 sales.order_item_removed); a new
// quantity is never below what is delivered (422
// sales.quantity_below_delivered). The item is checked and priced as an
// item given directly is (itemCheck.item, ADR-0067 §3), on the order's
// date: a new price is entered; without one, an entered price stays
// entered and a price-list price is determined again. An item that comes
// out as it was is no change. Removals and additions are removed and
// added (T78e).
func (e *Endpoints) changeItems(ctx context.Context, tx pgx.Tx, o Order, le tenancy.Entity, in OrderChangeInput) ([]OrderItem, []ItemChange, error) {
	if len(in.Items) == 0 && len(in.NewItems) == 0 && len(in.Removed) == 0 {
		return o.Items, nil, nil
	}
	plans, err := planItems(o, in.Items)
	if err != nil {
		return nil, nil, err
	}
	check, err := e.changeCheck(ctx, tx, o, le, in.NewItems)
	if err != nil {
		return nil, nil, err
	}
	items := append([]OrderItem(nil), o.Items...)
	var changes []ItemChange
	for _, plan := range plans {
		after, err := check.item(ctx, plan.i, plan.input)
		if err != nil {
			return nil, nil, err
		}
		if err := notBelowDelivered(o, plan.item, after.Quantity, fmt.Sprintf("items[%d].quantity.value", plan.i)); err != nil {
			return nil, nil, err
		}
		after.ItemNo = plan.item.ItemNo
		if samePricing(plan.item.Item, after) {
			continue
		}
		items[plan.item.ItemNo-1].Item = after // items are numbered from 1 in order (insertItems), and added after the highest (T78e)
		changes = append(changes, ItemChange{ItemNo: after.ItemNo, Kind: ItemChanged, Unit: after.Unit, QuantityBefore: plan.item.Quantity,
			QuantityAfter: after.Quantity, UnitPriceBefore: plan.item.UnitPrice, UnitPriceAfter: after.UnitPrice, PriceUnitBefore: plan.item.PriceUnit,
			PriceUnitAfter: after.PriceUnit, NetBefore: plan.item.Net, NetAfter: after.Net})
	}
	if items, changes, err = removeItems(o, items, changes, in); err != nil {
		return nil, nil, err
	}
	if items, changes, err = addItems(ctx, check, o, items, changes, in.NewItems); err != nil {
		return nil, nil, err
	}
	slices.SortFunc(changes, func(a, b ItemChange) int { return cmp.Compare(a.ItemNo, b.ItemNo) }) // kept by item number, as the contract says
	return items, changes, nil
}

// planItems matches the request's item changes to the order's items and
// refuses what no pricing could take: an unknown or repeated item, a
// quantity in another unit, a removed item, an item of which something is
// billed.
func planItems(o Order, in []ItemChangeInput) ([]itemPlan, error) {
	byNo := make(map[int32]OrderItem, len(o.Items))
	for _, it := range o.Items {
		byNo[it.ItemNo] = it
	}
	named := make(map[int32]bool, len(in))
	plans := make([]itemPlan, len(in))
	for i, ch := range in {
		field := fmt.Sprintf("items[%d]", i)
		it, ok := byNo[ch.ItemNo]
		switch {
		case !ok:
			return nil, problem.Invalid("sales.unknown_order_item", field+".item_no", fmt.Sprintf("Sales order %s has no item %d.", o.DocumentNumber, ch.ItemNo))
		case named[ch.ItemNo]:
			return nil, problem.Invalid("sales.item_repeated", field+".item_no", fmt.Sprintf("A change names an item once; item %d is named more than once.", ch.ItemNo))
		case ch.Unit != nil && *ch.Unit != it.Unit:
			return nil, problem.Invalid("sales.unit_mismatch", field+".quantity.unit", fmt.Sprintf("Item %d is ordered in %s.", ch.ItemNo, it.Unit))
		case it.RemovedInChange != nil:
			return nil, itemRemoved(field+".item_no", o, it)
		case money.ParseRat(it.Billed).Sign() != 0:
			return nil, problem.Invalid("sales.order_item_billed", field, fmt.Sprintf(
				"Item %d of sales order %s has %s %s billed, so its quantity and price stay; cancel its billing documents first.", ch.ItemNo, o.DocumentNumber, it.Billed, it.Unit))
		}
		named[ch.ItemNo] = true
		plans[i] = itemPlan{i: i, item: it, input: changedInput(it.Item, ch, o.Currency, o.Exponent)}
	}
	return plans, nil
}

// changedInput is the item as it is to be checked and priced again: what
// it names, its unit, account, tax code and jurisdiction as they are, the quantity
// given else its own, and the price given, else its own entered price,
// else none, so the price records give it again.
func changedInput(it Item, ch ItemChangeInput, currency string, exp int) ItemInput {
	in := ItemInput{ItemKey: it.ItemKey, MaterialID: it.MaterialID, Description: it.Description, Quantity: it.Quantity, Unit: it.Unit, AccountID: it.AccountID,
		TaxCode: it.TaxCode, TaxJurisdiction: it.TaxJurisdiction, Kept: true}
	if ch.Quantity != nil {
		in.Quantity = *ch.Quantity
	}
	switch {
	case ch.UnitPrice != nil:
		in.UnitPrice, in.PriceUnit = ch.UnitPrice, ch.PriceUnit
	case ch.PriceUnit != 0:
		in.PriceUnit = ch.PriceUnit // refused by unitPrice: a price unit comes with a unit price
	case it.PriceSource == Manual:
		in.UnitPrice, in.PriceUnit = &Amount{Value: it.UnitPrice.Plain(exp), Currency: currency}, it.PriceUnit
	}
	return in
}

// samePricing says whether an item priced again comes out as it was.
func samePricing(before, after Item) bool {
	return money.ParseRat(before.Quantity).Cmp(money.ParseRat(after.Quantity)) == 0 && before.UnitPrice == after.UnitPrice &&
		before.PriceUnit == after.PriceUnit && before.Net == after.Net && before.PriceSource == after.PriceSource &&
		reflect.DeepEqual(conditionsOf(before), conditionsOf(after))
}

// conditionsOf is an item's conditions with none as an empty list.
func conditionsOf(it Item) []Condition {
	if it.Conditions == nil {
		return []Condition{}
	}
	return it.Conditions
}

// writeChange keeps the change as the order's next and writes the
// changed, removed and added items and the order's net value, requested
// delivery date, payment term, change count, credit block and version. The change is written first:
// the order's change rule finds it (sales_order_confirmed_once). It
// returns the change's number.
func writeChange(ctx context.Context, tx pgx.Tx, p reqctx.Principal, o Order, c changed, block string, reason *string) (int32, error) {
	no := o.Changes + 1
	if c.changes == nil {
		c.changes = []ItemChange{} // stored as an empty array, not null
	}
	var termBefore, termAfter *string
	if c.term != o.PaymentTermCode {
		termBefore, termAfter = &o.PaymentTermCode, &c.term
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sales.sales_order_change (tenant_id, id, sales_order_id, change_no, order_version, reason, net_amount_before,
			net_amount_after, requested_delivery_date_before, requested_delivery_date_after, credit_block_before, credit_block_after,
			payment_term_code_before, payment_term_code_after, items, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
		p.TenantID, uuid.Must(uuid.NewV7()), o.ID, no, o.Version+1, reason, o.Net, c.net, o.DeliveryDate, c.delivery, o.CreditBlock, block,
		termBefore, termAfter, c.changes, p.ID); err != nil {
		return 0, fmt.Errorf("sales: keep change %d of sales order %s: %w", no, o.ID, err)
	}
	if err := updateItems(ctx, tx, o, c); err != nil {
		return 0, err
	}
	if err := writeItemsAddedRemoved(ctx, tx, p, o, c, no); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE sales.sales_order SET net_amount = $2, requested_delivery_date = $3, changes = $4, credit_block = $5,
			credit_released_by = CASE WHEN $5 = 'released' THEN credit_released_by END,
			credit_released_by_name = CASE WHEN $5 = 'released' THEN credit_released_by_name END,
			credit_released_at = CASE WHEN $5 = 'released' THEN credit_released_at END,
			payment_term_code = $8, version = version + 1, updated_by = $6
		WHERE id = $1 AND changes = $7`, o.ID, c.net, c.delivery, no, block, p.ID, o.Changes, c.term); err != nil {
		return 0, fmt.Errorf("sales: write change %d of sales order %s: %w", no, o.ID, err)
	}
	return no, nil
}

// updateItems writes the changed items' quantity and pricing.
func updateItems(ctx context.Context, tx pgx.Tx, o Order, c changed) error {
	var priced []ItemChange
	for _, ch := range c.changes {
		if ch.Kind == ItemChanged {
			priced = append(priced, ch)
		}
	}
	if len(priced) == 0 {
		return nil
	}
	n := len(priced)
	nos, quantities, prices, units, nets := make([]int32, n), make([]string, n), make([]money.Minor, n), make([]int32, n), make([]money.Minor, n)
	sources, conditions := make([]string, n), make([][]Condition, n)
	for i, ch := range priced {
		it := c.items[ch.ItemNo-1].Item
		nos[i], quantities[i], prices[i], units[i], nets[i], sources[i], conditions[i] = it.ItemNo, it.Quantity, it.UnitPrice, it.PriceUnit, it.Net,
			it.PriceSource, conditionsOf(it)
	}
	if _, err := tx.Exec(ctx, `UPDATE sales.sales_order_item t SET quantity = x.q::numeric, unit_price = x.p, price_unit = x.pu, net_amount = x.na,
			price_source = x.ps, conditions = $8::jsonb -> (x.ord::int - 1)
		FROM unnest($2::int[], $3::text[], $4::bigint[], $5::int[], $6::bigint[], $7::text[]) WITH ORDINALITY AS x(n, q, p, pu, na, ps, ord)
		WHERE t.sales_order_id = $1 AND t.item_no = x.n`,
		o.ID, nos, quantities, prices, units, nets, sources, conditions); err != nil {
		return fmt.Errorf("sales: write the changed items of sales order %s: %w", o.ID, err)
	}
	return nil
}

// CancelOrder cancels a draft or confirmed order of which nothing is
// delivered, once, with a reason (ADR-0067, As built T78d). The order is
// read under its row lock at the version the caller read; one already
// cancelled is refused (409 sales.invalid_status), as is one with a
// quantity delivered (409 sales.order_delivered): only what is delivered
// is billed, so nothing of it is billed either; an order an outbound
// delivery holds is refused alike (ADR-0083 §4). Its reason, who cancelled
// it and when, its audit record and SalesOrderChanged are written in the
// request's transaction. Nothing is posted.
func (e *Endpoints) CancelOrder(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, reason string) (Order, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Order{}, err
	}
	if err := problem.RequireText("reason", reason); err != nil {
		return Order{}, fmt.Errorf("sales: reason of a cancellation: %w", err)
	}
	o, err := changeableOrder(ctx, tx, id, ifMatch, "it is cancelled once.")
	if err != nil {
		return Order{}, err
	}
	for _, it := range o.Items {
		if taken := it.taken(); taken != "" {
			return Order{}, problem.Conflict("sales.order_delivered", fmt.Sprintf(
				"Item %d of sales order %s has %s; an order is cancelled while nothing of it is delivered or in an outbound delivery.", it.ItemNo,
				o.DocumentNumber, taken))
		}
	}
	le, _, err := e.LegalEntities.Entity(ctx, o.LegalEntityID) // a stored order's legal entity exists
	if err != nil {
		return Order{}, fmt.Errorf("sales: company of sales order %s: %w", id, err)
	}
	names, err := e.Partners.Names(ctx, []uuid.UUID{o.CustomerID})
	if err != nil {
		return Order{}, fmt.Errorf("sales: customer of sales order %s: %w", id, err)
	}
	people, err := e.People.PersonNames(ctx, []uuid.UUID{p.ID})
	if err != nil {
		return Order{}, fmt.Errorf("sales: name of the person cancelling sales order %s: %w", id, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE sales.sales_order SET status = $2, cancellation_reason = $3, cancelled_at = $4, cancelled_by = $5,
			cancelled_by_name = $7, version = version + 1, updated_by = $5 WHERE id = $1 AND status = $6`,
		id, Cancelled, reason, clock.Now(e.Now), p.ID, o.Status, people[p.ID]); err != nil {
		return Order{}, fmt.Errorf("sales: cancel sales order %s: %w", id, err)
	}
	cancelled, err := loadOrder(ctx, tx, id, false)
	if err != nil {
		return Order{}, err
	}
	return cancelled, recordOrder(ctx, "sales.sales_order.cancel", cancelled, le, names[o.CustomerID].Number, &o.Status, nil,
		map[string]any{"reason": reason})
}

// notBelowDelivered refuses an item's new quantity below what is
// delivered of it and what outbound deliveries hold (422
// sales.quantity_below_delivered naming field; ADR-0083 §4).
func notBelowDelivered(o Order, it OrderItem, quantity, field string) error {
	taken := new(big.Rat).Add(money.ParseRat(it.Delivered), money.ParseRat(it.InDelivery))
	if money.ParseRat(quantity).Cmp(taken) >= 0 {
		return nil
	}
	return problem.Invalid("sales.quantity_below_delivered", field, fmt.Sprintf("Item %d of sales order %s has %s; its quantity is not below that.",
		it.ItemNo, o.DocumentNumber, it.taken()))
}

// taken words what is delivered of the item and what outbound deliveries
// hold of it ("5 EA delivered", "5 EA delivered and 3 EA in outbound
// deliveries"), "" when there is neither.
func (it OrderItem) taken() string {
	var parts []string
	if money.ParseRat(it.Delivered).Sign() != 0 {
		parts = append(parts, it.Delivered+" "+it.Unit+" delivered")
	}
	if money.ParseRat(it.InDelivery).Sign() != 0 {
		parts = append(parts, it.InDelivery+" "+it.Unit+" in outbound deliveries")
	}
	return strings.Join(parts, " and ")
}
