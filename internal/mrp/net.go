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
// orders' it supplies and has not issued; below the reorder point, the
// shortage is what brings it back there, rounded up to six decimals, and
// is proposed as a requisition.

// counts are a run's quantities by material in the base unit.
type counts struct{ demand, stock, receipts map[uuid.UUID]*big.Rat }

// count reads the plant's demand, stock and purchase orders' open
// receipts of the planned materials, the taken-over proposals' receipts
// added (settle). The demand is the confirmed sales orders' and that of
// the intercompany orders the plant supplies and has not issued (S2i;
// ADR-0084 As built, batch 91), whatever day they are due, as a sales
// order's is. Such an order is a purchase order of the receiving plant,
// whose run counts it as a receipt; once issued (643) the stock has left
// the supplying plant, so the order demands nothing more. A stock
// transport order within the company is counted on neither side (§2).
func (e *Endpoints) count(ctx context.Context, w *work) (counts, error) {
	c := counts{demand: map[uuid.UUID]*big.Rat{}, stock: map[uuid.UUID]*big.Rat{}, receipts: w.receipts}
	demand, err := e.Supply.Demand(ctx, w.run.PlantID, w.ids)
	if err != nil {
		return counts{}, fmt.Errorf("mrp: demand at plant %s: %w", w.plant.Code, err)
	}
	intercompany, err := e.Supply.IntercompanyDue(ctx, w.run.PlantID, w.ids)
	if err != nil {
		return counts{}, fmt.Errorf("mrp: intercompany orders from plant %s: %w", w.plant.Code, err)
	}
	for _, d := range intercompany {
		demand = append(demand, d.Open)
	}
	if err := w.sum(demand, c.demand); err != nil {
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
	return c, w.sum(receipts, c.receipts)
}

// shortage is what brings stock plus receipts less demand back to the
// reorder point, rounded up to six decimals; zero when they are not
// below it.
func shortage(reorderPoint, demand, stock, receipts *big.Rat) *big.Rat {
	available := new(big.Rat).Sub(new(big.Rat).Add(stock, receipts), demand)
	if available.Cmp(reorderPoint) >= 0 {
		return new(big.Rat)
	}
	return money.CeilQuantity(new(big.Rat).Sub(reorderPoint, available))
}

// net counts the planned materials and proposes a requisition for each
// shortage, in material order, writing the run's lines.
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
		m := w.materials[pl.MaterialID]
		rop, demand, stock, receipts := money.ParseRat(pl.ReorderPoint), orZero(c.demand[m.ID]), orZero(c.stock[m.ID]), orZero(c.receipts[m.ID])
		short := shortage(rop, demand, stock, receipts)
		l := Line{MaterialID: m.ID, Unit: m.BaseUnit, ReorderPoint: money.PlainQuantity(rop), Demand: money.PlainQuantity(demand),
			Stock: money.PlainQuantity(stock), Receipts: money.PlainQuantity(receipts), Shortage: money.PlainQuantity(short)}
		if short.Sign() > 0 {
			p, err := e.propose(ctx, tx, w, pl, l, prices[m.ID])
			if err != nil {
				return err
			}
			l.RequisitionID, l.RequisitionNumber = &p.ID, &p.Number
			w.proposed = append(w.proposed, proposal{material: m.ID, Proposed: p})
		}
		w.lines = append(w.lines, l)
	}
	return nil
}

// propose raises the requisition of a line's shortage: the material in
// its base unit, in the company's currency at its valuation price at the
// plant (zero when it is not valued there), naming the plan's account,
// needed by the run's day.
func (e *Endpoints) propose(ctx context.Context, tx pgx.Tx, w *work, pl Plan, l Line, price Price) (Proposed, error) {
	exp, err := currency.Exponent(ctx, tx, w.le.LocalCurrency, nil)
	if err != nil {
		return Proposed{}, fmt.Errorf("mrp: currency %s: %w", w.le.LocalCurrency, err)
	}
	p, err := e.Supply.Propose(ctx, Proposal{LegalEntityID: w.le.ID, MaterialID: pl.MaterialID, AccountID: pl.AccountID, Currency: w.le.LocalCurrency,
		Quantity: l.Shortage, Unit: l.Unit, Price: price.Price.Plain(exp), PriceUnit: max(price.PriceUnit, 1), NeededBy: w.day})
	if err != nil {
		return Proposed{}, fmt.Errorf("mrp: propose %s %s of material %s at plant %s: %w", l.Shortage, l.Unit, w.materials[pl.MaterialID].Number,
			w.plant.Code, err)
	}
	return p, nil
}

// writeRun writes a run's lines and proposals.
func writeRun(ctx context.Context, tx pgx.Tx, p reqctx.Principal, w work) error {
	n := len(w.lines)
	mats, units, rops, demands, stocks, receipts, shorts := make([]uuid.UUID, n), make([]string, n), make([]string, n), make([]string, n),
		make([]string, n), make([]string, n), make([]string, n)
	reqs, numbers := make([]*uuid.UUID, n), make([]*string, n)
	for i, l := range w.lines {
		mats[i], units[i], rops[i], demands[i], stocks[i], receipts[i], shorts[i] = l.MaterialID, l.Unit, l.ReorderPoint, l.Demand, l.Stock, l.Receipts, l.Shortage
		reqs[i], numbers[i] = l.RequisitionID, l.RequisitionNumber
	}
	if _, err := tx.Exec(ctx, `INSERT INTO mrp.run_line (tenant_id, run_id, material_id, uom_code, reorder_point, demand, stock, receipts, shortage,
			requisition_id, requisition_number)
		SELECT $1, $2, m, u, r::numeric, d::numeric, s::numeric, x::numeric, sh::numeric, q, qn
		FROM unnest($3::uuid[], $4::text[], $5::text[], $6::text[], $7::text[], $8::text[], $9::text[], $10::uuid[], $11::text[])
			AS t(m, u, r, d, s, x, sh, q, qn)`, p.TenantID, w.run.ID, mats, units, rops, demands, stocks, receipts, shorts, reqs, numbers); err != nil {
		return fmt.Errorf("mrp: lines of planning run %s: %w", w.run.ID, err)
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
