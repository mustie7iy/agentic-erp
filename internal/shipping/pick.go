package shipping

import (
	"context"
	"fmt"
	"math/big"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// PickInput is a quantity picked of a delivery's item ItemNo, in its unit.
type PickInput struct {
	ItemNo         int32
	Quantity, Unit string
}

// Pick records what is picked of a delivery's items (ADR-0083 §5): the
// delivery read under its row lock (ADR-0008) at the version the caller
// read, while its goods issue is not posted (409 shipping.goods_issued);
// each item named once, in its unit, at most its delivery quantity, its
// picked quantity replaced. With the delivery's other items of the
// material, what is picked of it is at most what the storage location
// holds now in the base unit (422 shipping.insufficient_stock); nothing
// is reserved. The version moves; the audit record and
// OutboundDeliveryChanged are written in the request's transaction.
func (e *Endpoints) Pick(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, in []PickInput) (Delivery, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Delivery{}, err
	}
	d, err := open(ctx, tx, id, ifMatch)
	if err != nil {
		return Delivery{}, err
	}
	if err := picks(d, in); err != nil {
		return Delivery{}, err
	}
	if err := e.inStock(ctx, d, in); err != nil {
		return Delivery{}, err
	}
	for _, it := range in {
		if _, err := tx.Exec(ctx, `UPDATE shipping.outbound_delivery_item SET picked_quantity = $3::numeric
			WHERE outbound_delivery_id = $1 AND item_no = $2`, d.ID, it.ItemNo, it.Quantity); err != nil {
			return Delivery{}, fmt.Errorf("shipping: pick item %d of outbound delivery %s: %w", it.ItemNo, d.Number, err)
		}
	}
	after, err := touch(ctx, tx, p, d)
	if err != nil {
		return Delivery{}, err
	}
	return after, e.record(ctx, "shipping.outbound_delivery.pick", after, map[string]any{"picked_items": len(in)})
}

// open reads a delivery of a company the caller may act in under its row
// lock at the version the caller read, refusing one whose goods issue is
// posted (409 shipping.goods_issued) or reversed
// (shipping.goods_issue_reversed).
func open(ctx context.Context, tx pgx.Tx, id uuid.UUID, ifMatch func(int32) error) (Delivery, error) {
	d, err := accessible(ctx, tx, id, true)
	if err != nil {
		return Delivery{}, err
	}
	if err := ifMatch(d.Version); err != nil {
		return Delivery{}, err
	}
	switch d.Status {
	case GoodsIssued:
		return Delivery{}, problem.Conflict("shipping.goods_issued", "Outbound delivery "+d.Number+"'s goods issue is posted ("+
			*d.MaterialDocumentNumber+"); it changes no more.")
	case GoodsIssueReversed:
		return Delivery{}, reversedAlready(d)
	}
	return d, nil
}

// touch moves the delivery's version and reads it back.
func touch(ctx context.Context, tx pgx.Tx, p reqctx.Principal, d Delivery) (Delivery, error) {
	if _, err := tx.Exec(ctx, `UPDATE shipping.outbound_delivery SET version = version + 1, updated_by = $2 WHERE id = $1`, d.ID, p.ID); err != nil {
		return Delivery{}, fmt.Errorf("shipping: outbound delivery %s: %w", d.Number, err)
	}
	return load(ctx, tx, d.ID, false)
}

// picks checks the quantities picked against the delivery's items: each
// item once (422 shipping.item_repeated) and one the delivery has
// (shipping.unknown_item), naming items[i].item_no; a plain decimal of
// zero or more (shipping.invalid_quantity) in the item's unit
// (shipping.unit_mismatch) of at most its delivery quantity
// (shipping.exceeds_delivery_quantity).
func picks(d Delivery, in []PickInput) error {
	named := map[int32]bool{}
	for i, it := range in {
		field := fmt.Sprintf("items[%d]", i)
		switch {
		case named[it.ItemNo]:
			return problem.Invalid("shipping.item_repeated", field+".item_no", fmt.Sprintf("Item %d is named more than once.", it.ItemNo))
		case it.ItemNo < 1 || int(it.ItemNo) > len(d.Items):
			return problem.Invalid("shipping.unknown_item", field+".item_no", fmt.Sprintf("Outbound delivery %s has no item %d.", d.Number, it.ItemNo))
		}
		named[it.ItemNo] = true
		item := d.Items[it.ItemNo-1] // items are numbered from 1 in order (insert)
		switch {
		case money.QuantityOrZero(it.Quantity) != nil:
			return problem.Invalid("shipping.invalid_quantity", field+".quantity.value", fmt.Sprintf(
				"A quantity picked is 0 or a plain decimal more than zero of at most %d integer digits and %d decimals.", money.QuantityIntDigits,
				money.QuantityDecimals))
		case it.Unit != item.Unit:
			return problem.Invalid("shipping.unit_mismatch", field+".quantity.unit", fmt.Sprintf("Item %d of outbound delivery %s is in %s; it is picked in that unit.",
				it.ItemNo, d.Number, item.Unit))
		case money.ParseRat(it.Quantity).Cmp(money.ParseRat(item.Quantity)) > 0:
			return problem.Invalid("shipping.exceeds_delivery_quantity", field+".quantity.value", fmt.Sprintf(
				"Item %d of outbound delivery %s is %s %s; %s is more.", it.ItemNo, d.Number, item.Quantity, item.Unit, it.Quantity))
		}
	}
	return nil
}

// inStock checks that what the delivery picks of each material, its
// items' picked quantities after the request converted to the base unit,
// is at most what its storage location holds now (422
// shipping.insufficient_stock naming the first item of the material the
// request names). A read, not a reservation: the goods issue takes the
// stock under inventory's locks, refusing it then if it has gone.
func (e *Endpoints) inStock(ctx context.Context, d Delivery, in []PickInput) error {
	picked, at := map[int32]string{}, map[int32]int{}
	for _, it := range d.Items {
		picked[it.ItemNo] = it.Picked
	}
	for i, it := range in {
		picked[it.ItemNo], at[it.ItemNo] = it.Quantity, i
	}
	ids := make([]uuid.UUID, len(d.Items))
	for i, it := range d.Items {
		ids[i] = it.MaterialID
	}
	ms, err := e.Materials.Materials(ctx, ids)
	if err != nil {
		return fmt.Errorf("shipping: materials of outbound delivery %s: %w", d.Number, err)
	}
	need := map[uuid.UUID]*big.Rat{}
	for _, it := range d.Items {
		m, q := ms[it.MaterialID], money.ParseRat(picked[it.ItemNo])
		if q.Sign() == 0 {
			continue
		}
		base, err := m.Convert(q, it.Unit)
		if err != nil { // only a quantity the request gives can fail: the others converted when they were picked
			return pickInexact(d, it, m, picked[it.ItemNo], at[it.ItemNo])
		}
		if need[it.MaterialID] == nil {
			need[it.MaterialID] = new(big.Rat)
		}
		need[it.MaterialID].Add(need[it.MaterialID], base)
	}
	for i, it := range in {
		item := d.Items[it.ItemNo-1]
		if need[item.MaterialID] == nil { // nothing picked of the material
			continue
		}
		if err := e.held(ctx, d, item, ms[item.MaterialID], need[item.MaterialID], fmt.Sprintf("items[%d].quantity.value", i)); err != nil {
			return err
		}
	}
	return nil
}

// held refuses picking more of the material than the location holds.
func (e *Endpoints) held(ctx context.Context, d Delivery, it Item, m materials.Material, need *big.Rat, field string) error {
	have, err := e.Stock.Held(ctx, it.MaterialID, d.PlantID, d.StorageLocationID)
	if err != nil {
		return fmt.Errorf("shipping: stock of material %s: %w", m.Number, err)
	}
	if need.Cmp(money.ParseRat(have)) <= 0 {
		return nil
	}
	return problem.Invalid("shipping.insufficient_stock", field, fmt.Sprintf("Outbound delivery %s would pick %s %s of material %s; its storage location holds %s %s.",
		d.Number, money.PlainQuantity(need), m.BaseUnit, m.Number, have, m.BaseUnit))
}

// pickInexact is the refusal of the quantity picked of request item i
// that does not convert to the material's base unit exactly (422
// shipping.inexact_quantity): its unit is the order item's, which sales
// checked is one of the material's (S1f1), so only the quantity can be at
// fault.
func pickInexact(d Delivery, it Item, m materials.Material, quantity string, i int) error {
	return problem.Invalid("shipping.inexact_quantity", fmt.Sprintf("items[%d].quantity.value", i), fmt.Sprintf("Item %d of outbound delivery %s: "+
		"%s %s of material %s is no exact quantity of its base unit %s in six decimals; it is not rounded.", it.ItemNo, d.Number, quantity, it.Unit,
		m.Number, m.BaseUnit))
}
