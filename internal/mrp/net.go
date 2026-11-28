package mrp

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Net requirements (ADR-0084 §3): per planned material, in its base
// unit, what the plant has available is its stock plus its open receipts
// less its demand: its confirmed sales orders' and the intercompany
// orders' and stock transport orders' it supplies and has not issued;
// below the reorder point, the shortage is what brings it back there,
// rounded up to six decimals, and is proposed as a requisition, sized
// by the plan's lot sizing procedure (lotsize.go), by period from the
// demand's days for a period procedure (period.go).

// counts are a run's quantities by material in the base unit, and its
// demand by the day it is due.
type counts struct {
	demand, stock, receipts map[uuid.UUID]*big.Rat
	due                     map[uuid.UUID][]dated
}

// count reads the plant's demand, stock and open receipts of the planned
// materials, the taken-over proposals' receipts added (settle).
func (e *Endpoints) count(ctx context.Context, w *work) (counts, error) {
	c := counts{demand: map[uuid.UUID]*big.Rat{}, stock: map[uuid.UUID]*big.Rat{}, receipts: w.receipts, due: map[uuid.UUID][]dated{}}
	demand, err := e.demand(ctx, w)
	if err != nil {
		return counts{}, err
	}
	if err := w.tally(demand, c.demand, c.due); err != nil {
		return counts{}, err
	}
	stock, err := e.Supply.Stock(ctx, w.run.PlantID, w.ids)
	if err != nil {
		return counts{}, fmt.Errorf("mrp: stock at plant %s: %w", w.plant.Code, err)
	}
	for id, q := range stock {
		c.stock[id] = money.ParseRat(q)
	}
	receipts, err := e.Supply.Receipts(ctx, w.run.PlantID, w.ids)
	if err != nil {
		return counts{}, fmt.Errorf("mrp: open receipts at plant %s: %w", w.plant.Code, err)
	}
	transports, err := e.Supply.TransportsOnOrder(ctx, w.run.PlantID, w.ids)
	if err != nil {
		return counts{}, fmt.Errorf("mrp: stock transport orders to plant %s: %w", w.plant.Code, err)
	}
	open := make([]Due, 0, len(receipts)+len(transports))
	for _, o := range append(receipts, transports...) {
		open = append(open, Due{Open: o}) // receipts carry no day (period.go)
	}
	return c, w.tally(open, c.receipts, nil)
}

// demand is what the plant must supply of the planned materials by the
// day it is due: its confirmed sales orders' (the day the customer asked
// for, else the order's date), and what the intercompany orders (S2i;
// ADR-0084 As built, batch 91, by their items' delivery dates, else the
// order's date) and the stock transport orders within the company (S2h;
// As built, stock transport orders, by the run's day: the read carries no
// date) it supplies order and it has not issued. Each such order is a
// receipt of the plant it goes to (a purchase order's, or a stock
// transport order's until received), so one order is counted once on
// each side; once issued the stock has left this plant, so the order
// demands nothing more here. Lot sizing by the reorder point alone adds
// the days up; a period procedure sizes by them.
func (e *Endpoints) demand(ctx context.Context, w *work) ([]Due, error) {
	demand, err := e.Supply.DemandDue(ctx, w.run.PlantID, w.ids)
	if err != nil {
		return nil, fmt.Errorf("mrp: demand at plant %s: %w", w.plant.Code, err)
	}
	intercompany, err := e.Supply.IntercompanyDue(ctx, w.run.PlantID, w.ids)
	if err != nil {
		return nil, fmt.Errorf("mrp: intercompany orders from plant %s: %w", w.plant.Code, err)
	}
	transports, err := e.Supply.TransportsDue(ctx, w.run.PlantID, w.ids)
	if err != nil {
		return nil, fmt.Errorf("mrp: stock transport orders from plant %s: %w", w.plant.Code, err)
	}
	demand = append(demand, intercompany...)
	for _, o := range transports {
		demand = append(demand, Due{Open: o, Day: w.day})
	}
	return demand, nil
}

// shortage is what brings the available quantity (stock plus receipts
// less demand) back to the reorder point, rounded up to six decimals;
// zero when it is not below it.
func shortage(reorderPoint, available *big.Rat) *big.Rat {
	if available.Cmp(reorderPoint) >= 0 {
		return new(big.Rat)
	}
	return money.CeilQuantity(new(big.Rat).Sub(reorderPoint, available))
}

// net counts the planned materials and proposes a requisition for each
// shortage, of the lots its plan sizes, in material order, writing the
// run's lines.
func (e *Endpoints) net(ctx context.Context, tx pgx.Tx, w *work) error {
	c, err := e.count(ctx, w)
	if err != nil {
		return err
	}
	prices, err := e.Supply.Prices(ctx, w.run.PlantID, w.ids)
	if err != nil {
		return fmt.Errorf("mrp: prices at plant %s: %w", w.plant.Code, err)
	}
	for _, pl := range w.plans {
		l, lots, err := sized(pl, w, c)
		if err != nil {
			return err
		}
		if len(lots) > 0 {
			p, err := e.propose(ctx, tx, w, pl, l, lots, prices[pl.MaterialID])
			if err != nil {
				return err
			}
			n := int32(len(lots)) // at most MaxLots
			l.RequisitionID, l.RequisitionNumber, l.Lots = &p.ID, &p.Number, &n
			w.proposed = append(w.proposed, proposal{material: pl.MaterialID, Proposed: p})
		}
		w.lines = append(w.lines, l)
	}
	return nil
}

// sized is a planned material's line and the lots it proposes: its
// shortage below the reorder point sized by the plan's procedure, by
// period from the demand's days for a period procedure, every other
// lot needed by the run's day. A period-sized line keeps its lots with
// their days (Schedule).
func sized(pl Plan, w *work, c counts) (Line, []lot, error) {
	m := w.materials[pl.MaterialID]
	rop, demand, stock, receipts := money.ParseRat(pl.ReorderPoint), orZero(c.demand[m.ID]), orZero(c.stock[m.ID]), orZero(c.receipts[m.ID])
	supply := new(big.Rat).Add(stock, receipts)
	available := new(big.Rat).Sub(supply, demand)
	short := shortage(rop, available)
	var lots []lot
	if pl.Lots.periodic() {
		ls, err := pl.Lots.periodLots(rop, supply, c.due[m.ID], w.day, m.Number)
		if err != nil {
			return Line{}, nil, err
		}
		lots = ls
	} else {
		qs, err := pl.Lots.lots(short, available, m.Number)
		if err != nil {
			return Line{}, nil, err
		}
		lots = onDay(qs, w.day)
	}
	l := Line{MaterialID: m.ID, Unit: m.BaseUnit, ReorderPoint: money.PlainQuantity(rop), Demand: money.PlainQuantity(demand),
		Stock: money.PlainQuantity(stock), Receipts: money.PlainQuantity(receipts), Shortage: money.PlainQuantity(short), LotSize: pl.Lots.Procedure,
		Proposed: money.PlainQuantity(sum(lots))}
	if pl.Lots.periodic() {
		for _, x := range lots {
			l.Schedule = append(l.Schedule, LotDue{Quantity: money.PlainQuantity(x.quantity), NeededBy: x.due})
		}
	}
	return l, lots, nil
}

// sum is the total of the lots, zero for none.
func sum(lots []lot) *big.Rat {
	total := new(big.Rat)
	for _, x := range lots {
		total.Add(total, x.quantity)
	}
	return total
}

// propose raises the requisition of a line's shortage, an item per lot:
// the material in its base unit, in the company's currency at its
// valuation price at the plant (zero when it is not valued there),
// naming the plan's account, needed by its first lot's day (the run's
// day but for a period procedure's lots, which are in the order of their
// days).
func (e *Endpoints) propose(ctx context.Context, tx pgx.Tx, w *work, pl Plan, l Line, lots []lot, price Price) (Proposed, error) {
	exp, err := currency.Exponent(ctx, tx, w.le.LocalCurrency, nil)
	if err != nil {
		return Proposed{}, fmt.Errorf("mrp: currency %s: %w", w.le.LocalCurrency, err)
	}
	quantities := make([]string, len(lots))
	for i, x := range lots {
		quantities[i] = money.PlainQuantity(x.quantity)
	}
	p, err := e.Supply.Propose(ctx, Proposal{LegalEntityID: w.le.ID, MaterialID: pl.MaterialID, AccountID: pl.AccountID, Currency: w.le.LocalCurrency,
		Lots: quantities, Unit: l.Unit, Price: price.Price.Plain(exp), PriceUnit: max(price.PriceUnit, 1), NeededBy: lots[0].due})
	if err != nil {
		return Proposed{}, fmt.Errorf("mrp: propose %s %s of material %s at plant %s: %w", l.Proposed, l.Unit, w.materials[pl.MaterialID].Number,
			w.plant.Code, err)
	}
	return p, nil
}

// writeRun writes a run's lines and proposals.
func writeRun(ctx context.Context, tx pgx.Tx, p reqctx.Principal, w work) error {
	n := len(w.lines)
	mats, units, rops, demands, stocks, receipts, shorts := make([]uuid.UUID, n), make([]string, n), make([]string, n), make([]string, n),
		make([]string, n), make([]string, n), make([]string, n)
	reqs, numbers, procs, proposed, lots := make([]*uuid.UUID, n), make([]*string, n), make([]string, n), make([]string, n), make([]*int32, n)
	for i, l := range w.lines {
		mats[i], units[i], rops[i], demands[i], stocks[i], receipts[i], shorts[i] = l.MaterialID, l.Unit, l.ReorderPoint, l.Demand, l.Stock, l.Receipts, l.Shortage
		reqs[i], numbers[i], procs[i], proposed[i], lots[i] = l.RequisitionID, l.RequisitionNumber, l.LotSize, l.Proposed, l.Lots
	}
	if _, err := tx.Exec(ctx, `INSERT INTO mrp.run_line (tenant_id, run_id, material_id, uom_code, reorder_point, demand, stock, receipts, shortage,
			requisition_id, requisition_number, lot_size, proposed, lots)
		SELECT $1, $2, m, u, r::numeric, d::numeric, s::numeric, x::numeric, sh::numeric, q, qn, ls, pr::numeric, lt
		FROM unnest($3::uuid[], $4::text[], $5::text[], $6::text[], $7::text[], $8::text[], $9::text[], $10::uuid[], $11::text[], $12::text[],
			$13::text[], $14::int[]) AS t(m, u, r, d, s, x, sh, q, qn, ls, pr, lt)`, p.TenantID, w.run.ID, mats, units, rops, demands, stocks, receipts,
		shorts, reqs, numbers, procs, proposed, lots); err != nil {
		return fmt.Errorf("mrp: lines of planning run %s: %w", w.run.ID, err)
	}
	if err := writeSchedules(ctx, tx, p, w); err != nil {
		return err
	}
	for _, x := range w.proposed {
		if _, err := tx.Exec(ctx, `INSERT INTO mrp.proposal (tenant_id, requisition_id, requisition_number, requisition_version, run_id, plant_id,
				material_id) VALUES ($1, $2, $3, $4, $5, $6, $7)`, p.TenantID, x.ID, x.Number, x.Version, w.run.ID, w.run.PlantID, x.material); err != nil {
			return fmt.Errorf("mrp: proposal %s of planning run %s: %w", x.Number, w.run.ID, err)
		}
	}
	_, err := tx.Exec(ctx, `UPDATE mrp.run SET status = 'completed', run_date = $2, materials_planned = $3, proposals_created = $4,
		proposals_replaced = $5, version = version + 1, updated_by = $6 WHERE id = $1`, w.run.ID, w.day, n, len(w.proposed), w.replaced, p.ID)
	return errs.Wrapf(err, "mrp: complete planning run %s", w.run.ID)
}

// writeSchedules writes the period-sized lines' lots, numbered from 1
// in the order of the requisition's items.
func writeSchedules(ctx context.Context, tx pgx.Tx, p reqctx.Principal, w work) error {
	var mats []uuid.UUID
	var nos []int32
	var quantities []string
	var days []time.Time
	for _, l := range w.lines {
		for i, x := range l.Schedule {
			no := int32(i + 1) // at most MaxLots
			mats, nos, quantities, days = append(mats, l.MaterialID), append(nos, no), append(quantities, x.Quantity), append(days, x.NeededBy)
		}
	}
	if len(mats) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO mrp.run_line_lot (tenant_id, run_id, material_id, lot_no, quantity, needed_by)
		SELECT $1, $2, m, n, q::numeric, d FROM unnest($3::uuid[], $4::int[], $5::text[], $6::date[]) AS t(m, n, q, d)`,
		p.TenantID, w.run.ID, mats, nos, quantities, days)
	return errs.Wrapf(err, "mrp: lots of planning run %s", w.run.ID)
}

// finish audits the completed run and succeeds its Operation.
func (e *Endpoints) finish(ctx context.Context, w work) error {
	proposed := make([]map[string]any, len(w.proposed))
	for i, x := range w.proposed {
		proposed[i] = map[string]any{"requisition_id": x.ID, "requisition_number": x.Number, "material_id": x.material}
	}
	if err := e.Runs.Succeed(ctx, w.run.OperationID, runPath(w.run.ID), map[string]any{"run_id": w.run.ID, "status": RunCompleted}); err != nil {
		return fmt.Errorf("mrp: succeed the operation of planning run %s: %w", w.run.ID, err)
	}
	return audit.Record(ctx, "mrp.run.complete", "mrp.run", w.run.ID, map[string]any{"plant_id": w.run.PlantID, "plant_code": w.plant.Code,
		"run_date": w.day.Format(time.DateOnly), "materials_planned": len(w.lines), "proposed": proposed, "proposals_replaced": w.replaced})
}

// FailRun sets a queued run failed with the problem that stopped it,
// audits it and fails its Operation with the problem, unless it was
// cancelled, its final status. A run no longer queued is left alone.
func (e *Endpoints) FailRun(ctx context.Context, id uuid.UUID, why *problem.Problem) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	r, err := lockRun(ctx, tx, id)
	if err != nil || r.Status != RunQueued {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE mrp.run SET status = 'failed', error = $2, version = version + 1, updated_by = $3 WHERE id = $1`,
		id, failureOf(why), p.ID); err != nil {
		return fmt.Errorf("mrp: fail planning run %s: %w", id, err)
	}
	cancelled, err := e.Runs.Cancelled(ctx, r.OperationID, true)
	if err != nil {
		return fmt.Errorf("mrp: the operation of planning run %s: %w", id, err)
	}
	if !cancelled {
		if err := e.Runs.Fail(ctx, r.OperationID, why); err != nil {
			return fmt.Errorf("mrp: fail the operation of planning run %s: %w", id, err)
		}
	}
	return audit.Record(ctx, "mrp.run.fail", "mrp.run", id, map[string]any{"plant_id": r.PlantID, "code": why.Code, "detail": why.Detail})
}

// UnfinishedRuns lists the runs queued in the transaction's tenant, by
// run with its Operation: the work of the in-process runner of a cell
// without a workflow engine, which takes up a run whose Operation was
// cancelled too, so that PlanRun fails it.
func (e *Endpoints) UnfinishedRuns(ctx context.Context, tx pgx.Tx) (map[uuid.UUID]uuid.UUID, error) {
	rows, _ := tx.Query(ctx, `SELECT id, operation_id FROM mrp.run WHERE status = 'queued'`)
	out := map[uuid.UUID]uuid.UUID{}
	var run, op uuid.UUID
	_, err := pgx.ForEachRow(rows, []any{&run, &op}, func() error { out[run] = op; return nil })
	return out, errs.Wrapf(err, "mrp: unfinished planning runs")
}
