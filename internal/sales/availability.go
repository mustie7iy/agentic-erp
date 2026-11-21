package sales

import (
	"bytes"
	"context"
	"fmt"
	"math/big"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Available-to-promise (S2j; ADR-0082 As built S2j). An order may name
// the plant it delivers from. Confirming such an order checks each of its
// items of a material kept in stock against what the plant has available
// of the material: the stock its storage locations hold (the inventory
// context's PlantStock) less what other confirmed orders of the plant
// promised and have not delivered, and confirms what is available, all
// of the item or part of it, possibly nothing. The check is of stock on
// hand: with no planned receipts it confirms the same quantity for the
// order's requested delivery date as for today (checking rules with
// replenishment lead time are Later). The item keeps its confirmed
// quantity, in its own unit, and its outbound deliveries take at most
// that (OrderItem.open). Checks of one material at one plant are
// serialized by a lock of their own, so two orders racing for the last
// quantity confirm it once.

// Stock is what the availability check reads of a plant's stock (the
// inventory context's published API).
type Stock interface {
	PlantStock(ctx context.Context, plant uuid.UUID, materials []uuid.UUID) (map[uuid.UUID]string, error)
}

// kindPlant is the org unit kind of a plant (tenancy context).
const kindPlant = "plant"

// orderPlant checks the plant an order names, when it names one: an
// active plant of the order's company (422 sales.plant_invalid naming
// plant_id).
func (e *Endpoints) orderPlant(ctx context.Context, plant *uuid.UUID, le tenancy.Entity) error {
	if plant == nil {
		return nil
	}
	units, err := e.LegalEntities.OrgUnits(ctx, []uuid.UUID{*plant})
	if err != nil {
		return fmt.Errorf("sales: plant %s of a sales order: %w", *plant, err)
	}
	if u, ok := units[*plant]; !ok || u.Kind != kindPlant || u.Status != tenancy.Active || u.LegalEntityID != le.ID {
		return problem.Invalid("sales.plant_invalid", "plant_id", "No active plant with this id in company "+le.Code+", the order's.")
	}
	return nil
}

// promise is one item's availability check: the item, its quantity, what
// it had confirmed before (nil: nothing) and what it confirms now, in
// its unit; unvalued when its unit is no longer one of the material's, so
// it kept what it had.
type promise struct {
	item      OrderItem
	material  uuid.UUID
	before    *string
	confirmed string
	unvalued  bool
}

// detail is the audit record's account of the check of one item.
func (p promise) detail() map[string]any {
	return map[string]any{"item_no": p.item.ItemNo, "material_id": p.material, "quantity": p.item.Quantity, "unit": p.item.Unit,
		"confirmed_before": p.before, "confirmed_quantity": p.confirmed, "unvalued": p.unvalued}
}

// promised is the audit detail of an order's availability check, nil
// when it checked nothing.
func promised(ps []promise, plant *uuid.UUID) map[string]any {
	if len(ps) == 0 {
		return nil
	}
	items := make([]map[string]any, len(ps))
	for i, p := range ps {
		items[i] = p.detail()
	}
	return map[string]any{"plant_id": plant, "items": items}
}

// availability checks the items of an order with a plant whose row lock
// the caller holds, those check selects (checkPlan), and writes and
// returns what each confirms, in item order. The order of its steps is
// what makes it safe: the materials' locks first (lockMaterials), then
// what is promised (demand) before the stock (Stock.PlantStock), so a
// goods issue committed between the two reads counts out of the stock
// but still in the demand and the check promises less, never more.
func (e *Endpoints) availability(ctx context.Context, tx pgx.Tx, o Order, items []OrderItem, check func(OrderItem) bool) ([]promise, error) {
	if o.PlantID == nil {
		return nil, nil
	}
	plan, err := e.checkPlan(ctx, items, check)
	if err != nil || len(plan.checked) == 0 {
		return nil, err // nothing to check: no lock, no read
	}
	if err := lockMaterials(ctx, tx, *o.PlantID, plan.ids); err != nil {
		return nil, err
	}
	available, err := demand(ctx, tx, o, plan.materials)
	if err != nil {
		return nil, err
	}
	if err := e.addStock(ctx, *o.PlantID, plan.ids, available); err != nil {
		return nil, err
	}
	out := available.allocate(plan)
	return out, writePromises(ctx, tx, o, out)
}

// plan is what an availability check works on: the order's standing items
// of a material kept in stock, in item order, split into those it checks
// and those that keep what they confirmed, with their materials, by id
// and in the order their locks are taken.
type plan struct {
	checked, kept []OrderItem
	materials     map[uuid.UUID]materials.Material
	ids           []uuid.UUID
}

// checkPlan reads the items' materials and plans the check.
func (e *Endpoints) checkPlan(ctx context.Context, items []OrderItem, check func(OrderItem) bool) (plan, error) {
	var ids []uuid.UUID
	for _, it := range standing(items) {
		if it.MaterialID != nil {
			ids = append(ids, *it.MaterialID)
		}
	}
	ms, err := e.Materials.Materials(ctx, ids)
	if err != nil {
		return plan{}, fmt.Errorf("sales: materials of the order's items: %w", err)
	}
	p := plan{materials: ms}
	for _, it := range standing(items) {
		switch {
		case it.MaterialID == nil || !ms[*it.MaterialID].Stocked():
			continue
		case check(it):
			p.checked = append(p.checked, it)
		default:
			p.kept = append(p.kept, it)
		}
		if !slices.Contains(p.ids, *it.MaterialID) {
			p.ids = append(p.ids, *it.MaterialID)
		}
	}
	// As PostgreSQL orders uuids (bytewise): the order lockMaterials takes their locks in.
	slices.SortFunc(p.ids, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	return p, nil
}

// lockMaterials takes, for each material in order, the transaction's
// advisory lock on the material at the plant, so two availability checks
// of it wait for one another and the second counts what the first
// promised (ADR-0008). Each is taken by a statement of its own, in the
// materials' order, so two checks never wait on each other in a cycle;
// an order's row lock, then its customer's credit lock (credit.Check),
// is taken before them by every caller, and nothing that holds one of
// them waits for an order or a customer.
func lockMaterials(ctx context.Context, tx pgx.Tx, plant uuid.UUID, ids []uuid.UUID) error {
	for _, m := range ids {
		if err := database.LockTenant(ctx, tx, "sales.availability", plant.String(), m.String()); err != nil {
			return fmt.Errorf("sales: lock material %s at plant %s: %w", m, plant, err)
		}
	}
	return nil
}

// stockLeft is what is available of each material at the plant, in its
// base unit, as the check counts it down; unknown marks a material of
// which a promise could not be valued (its unit is no longer one of the
// material's), which then has nothing available.
type stockLeft struct {
	left    map[uuid.UUID]*big.Rat
	unknown map[uuid.UUID]bool
}

// take counts q, in the base unit, out of the material's stock; nil: q
// could not be valued.
func (s stockLeft) take(material uuid.UUID, q *big.Rat) {
	if q == nil {
		s.unknown[material] = true
		return
	}
	s.left[material] = new(big.Rat).Sub(s.left[material], q)
}

// free is what is available of the material, never below zero, as a
// value of its own.
func (s stockLeft) free(material uuid.UUID) *big.Rat {
	if s.unknown[material] || s.left[material].Sign() < 0 {
		return new(big.Rat)
	}
	return new(big.Rat).Set(s.left[material])
}

// promisedSQL is what the confirmed orders of the plant ($2) other than
// the one checked ($3) promised of the materials ($1) and have not
// delivered: each standing item's confirmed quantity less what is
// delivered of it; an item delivered as far as it was confirmed promises
// nothing. What an order with a plant has in delivery is inside it.
const promisedSQL = `SELECT i.material_id, i.uom_code, i.confirmed_quantity - i.delivered_quantity AS q
	FROM sales.sales_order_item i JOIN sales.sales_order o ON o.tenant_id = i.tenant_id AND o.id = i.sales_order_id
	WHERE i.material_id = ANY($1) AND i.confirmed_quantity > i.delivered_quantity AND i.removed_in_change IS NULL
		AND o.plant_id = $2 AND o.status = 'confirmed' AND o.id <> $3`

// heldSQL is what outbound deliveries from the plant ($2) hold of the
// materials ($1) for orders that name no plant: their assignments there,
// taken less given back at the goods issue, read only for items something
// is still in delivery of (sales_order_item_in_delivery_held), so a
// plant's delivery history is not read.
const heldSQL = `SELECT i.material_id, i.uom_code, a.quantity
	FROM sales.sales_order_item i
		JOIN sales.delivery_assignment a ON a.tenant_id = i.tenant_id AND a.sales_order_id = i.sales_order_id AND a.order_item_no = i.item_no
		JOIN sales.sales_order o ON o.tenant_id = i.tenant_id AND o.id = i.sales_order_id
	WHERE i.material_id = ANY($1) AND i.in_delivery_quantity > 0 AND a.plant_id = $2 AND o.plant_id IS NULL`

// demand is what promisedSQL and heldSQL find, read in one statement and
// counted out of a stock of nothing, in the base unit at the material's
// factor now.
func demand(ctx context.Context, tx pgx.Tx, o Order, ms map[uuid.UUID]materials.Material) (stockLeft, error) {
	ids := make([]uuid.UUID, 0, len(ms))
	s := stockLeft{left: map[uuid.UUID]*big.Rat{}, unknown: map[uuid.UUID]bool{}}
	for id := range ms {
		ids, s.left[id] = append(ids, id), new(big.Rat)
	}
	type open struct {
		Material uuid.UUID
		Unit     string
		Quantity string
	}
	rows, _ := tx.Query(ctx, `SELECT material_id, uom_code, trim_scale(sum(q))::text FROM (`+promisedSQL+` UNION ALL `+heldSQL+`) promised
		GROUP BY material_id, uom_code HAVING sum(q) > 0 ORDER BY material_id, uom_code`, ids, *o.PlantID, o.ID)
	opens, err := pgx.CollectRows(rows, pgx.RowToStructByPos[open])
	if err != nil {
		return stockLeft{}, fmt.Errorf("sales: what is promised at plant %s: %w", *o.PlantID, err)
	}
	for _, d := range opens {
		s.take(d.Material, inBase(ms[d.Material], money.ParseRat(d.Quantity), d.Unit))
	}
	return s, nil
}

// addStock adds what the plant holds of the materials to what is
// available of them.
func (e *Endpoints) addStock(ctx context.Context, plant uuid.UUID, ids []uuid.UUID, s stockLeft) error {
	held, err := e.Stock.PlantStock(ctx, plant, ids)
	if err != nil {
		return fmt.Errorf("sales: stock at plant %s: %w", plant, err)
	}
	for id, q := range held {
		s.left[id].Add(s.left[id], money.ParseRat(q))
	}
	return nil
}

// inBase is q of unit in the material's base unit at its factor now; nil
// when the unit is no longer one of the material's.
func inBase(m materials.Material, q *big.Rat, unit string) *big.Rat {
	f, err := m.Factor(unit)
	if err != nil {
		return nil
	}
	return q.Mul(q, f)
}

// promisedOpen is what the item promised with its confirmed quantity and
// has not delivered, in the material's base unit (nil: it cannot be
// valued); nothing for one delivered as far as it was confirmed.
func promisedOpen(m materials.Material, confirmed *big.Rat, it OrderItem) *big.Rat {
	open := new(big.Rat).Sub(confirmed, money.ParseRat(it.Delivered))
	if open.Sign() <= 0 {
		return new(big.Rat)
	}
	return inBase(m, open, it.Unit)
}

// allocate counts what the kept items promised out of what is available,
// then confirms the checked ones in item order (confirm).
func (s stockLeft) allocate(p plan) []promise {
	for _, it := range p.kept {
		if it.Confirmed != nil {
			s.take(*it.MaterialID, promisedOpen(p.materials[*it.MaterialID], money.ParseRat(*it.Confirmed), it))
		}
	}
	out := make([]promise, len(p.checked))
	for i, it := range p.checked {
		out[i] = s.confirm(it, p.materials[*it.MaterialID])
	}
	return out
}

// confirm confirms the item as far as the material is available, in the
// item's unit and rounded down to its decimals, and at most its quantity.
// An item checked again keeps at least what it had confirmed, up to its
// quantity: a change confirms an increase as far as there is stock and
// never takes back a promise; in a unit that is no longer the material's
// nothing more is promised. What it promises is counted out of what is
// available.
func (s stockLeft) confirm(it OrderItem, m materials.Material) promise {
	quantity := money.ParseRat(it.Quantity)
	kept := new(big.Rat)
	if it.Confirmed != nil {
		kept = money.MinRat(money.ParseRat(*it.Confirmed), quantity)
	}
	p := promise{item: it, material: *it.MaterialID, before: it.Confirmed}
	reach := kept
	if f, err := m.Factor(it.Unit); err == nil {
		reach = new(big.Rat).Add(money.ParseRat(it.Delivered), money.FloorQuantity(new(big.Rat).Quo(s.free(*it.MaterialID), f)))
	} else {
		p.unvalued = true
	}
	confirmed := money.MaxRat(kept, money.MinRat(quantity, reach))
	p.confirmed = money.PlainQuantity(confirmed)
	s.take(*it.MaterialID, promisedOpen(m, confirmed, it))
	return p
}

// writePromises writes the checked items' confirmed quantities, of which
// availability has at least one.
func writePromises(ctx context.Context, tx pgx.Tx, o Order, ps []promise) error {
	nos, quantities := make([]int32, len(ps)), make([]string, len(ps))
	for i, p := range ps {
		nos[i], quantities[i] = p.item.ItemNo, p.confirmed
	}
	if _, err := tx.Exec(ctx, `UPDATE sales.sales_order_item t SET confirmed_quantity = x.q::numeric
		FROM unnest($2::int[], $3::text[]) AS x(n, q) WHERE t.sales_order_id = $1 AND t.item_no = x.n`, o.ID, nos, quantities); err != nil {
		return fmt.Errorf("sales: confirmed quantities of sales order %s: %w", o.DocumentNumber, err)
	}
	return nil
}
