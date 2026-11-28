package mrp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// A planning run's work (ADR-0084 §3): PlanRun carries a queued run out
// in one transaction in the tenant's scope as the run's requester, which
// the workflow MrpRun calls as its activity through the workflows.Mrp
// port (or the in-process runner, without a workflow engine), and
// FailRun ends it with the problem that stopped it. Both are idempotent:
// a run no longer queued is left alone, so a step run again after a
// commit whose completion was lost does nothing. A run that fails
// proposes and cancels nothing, its transaction rolled back.

// lockRun reads a run FOR UPDATE, whoever may act in its company: the
// run's own work, as its requester.
func lockRun(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Run, error) {
	r, err := scanRun(tx.QueryRow(ctx, runSelect+` WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, problem.NotFoundOf("planning run")
	}
	return r, errs.Wrapf(err, "mrp: lock planning run %s", id)
}

// work is a run being carried out: the run, its plant and company, the
// company's day, the materials it plans with their plans, and what it
// proposes, replaces and writes.
type work struct {
	run       Run
	plant     tenancy.Unit
	le        tenancy.Entity
	day       time.Time
	plans     []Plan
	materials map[uuid.UUID]materials.Material
	ids       []uuid.UUID
	receipts  map[uuid.UUID]*big.Rat // what proposals a person took over have open, by material, in its base unit
	replaced  int
	lines     []Line
	proposed  []proposal
}

// proposal is a requisition the run proposed for a material.
type proposal struct {
	material uuid.UUID
	Proposed
}

// PlanRun carries out a queued run: under the run's lock it refuses a
// run whose Operation was cancelled (409 mrp.run_cancelled), reads the
// company's day and the plant's plans by reorder point under their share
// locks, settles the plant's earlier proposals (settle), counts each
// planned material's demand, stock and receipts, proposes a requisition
// for each shortage, writes the lines, completes the run and succeeds its
// Operation. A run no longer queued is left alone.
func (e *Endpoints) PlanRun(ctx context.Context, id uuid.UUID) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	r, err := lockRun(ctx, tx, id)
	if err != nil || r.Status != RunQueued {
		return err
	}
	cancelled, err := e.Runs.Cancelled(ctx, r.OperationID, true)
	switch {
	case err != nil:
		return fmt.Errorf("mrp: the operation of planning run %s: %w", id, err)
	case cancelled:
		return problem.Conflict("mrp.run_cancelled", fmt.Sprintf("The operation of planning run %s was cancelled before the run was carried out: "+
			"it proposed nothing; start a new run of the plant.", id))
	}
	w, err := e.prepare(ctx, tx, r)
	if err != nil {
		return err
	}
	if err := e.settle(ctx, tx, &w); err != nil {
		return err
	}
	if err := e.net(ctx, tx, &w); err != nil {
		return err
	}
	if err := writeRun(ctx, tx, p, w); err != nil {
		return err
	}
	return e.finish(ctx, w)
}

// prepare reads what a run plans: its plant and company, the company's
// day now, and the plans by reorder point of materials still active, kept
// in stock and extended to the plant, by material.
func (e *Endpoints) prepare(ctx context.Context, tx pgx.Tx, r Run) (work, error) {
	units, err := e.Plants.OrgUnits(ctx, []uuid.UUID{r.PlantID})
	if err != nil {
		return work{}, fmt.Errorf("mrp: plant of planning run %s: %w", r.ID, err)
	}
	le, _, err := e.Plants.Entity(ctx, r.LegalEntityID)
	if err != nil {
		return work{}, fmt.Errorf("mrp: company of planning run %s: %w", r.ID, err)
	}
	day, err := config.DocumentDay(ctx, e.Settings, le.Code, clock.Now(e.Now))
	if err != nil {
		return work{}, fmt.Errorf("mrp: day of planning run %s: %w", r.ID, err)
	}
	plans, err := planned(ctx, tx, r.PlantID)
	if err != nil {
		return work{}, err
	}
	ids := make([]uuid.UUID, len(plans))
	for i, pl := range plans {
		ids[i] = pl.MaterialID
	}
	ms, err := e.Materials.Materials(ctx, ids)
	if err != nil {
		return work{}, fmt.Errorf("mrp: materials of planning run %s: %w", r.ID, err)
	}
	w := work{run: r, plant: units[r.PlantID], le: le, day: day.Date, materials: ms, receipts: map[uuid.UUID]*big.Rat{}}
	for _, pl := range plans {
		m, ok := ms[pl.MaterialID]
		if _, extended := m.Plant(r.PlantID); ok && m.Status == "active" && m.Stocked() && extended {
			w.plans, w.ids = append(w.plans, pl), append(w.ids, pl.MaterialID)
		}
	}
	return w, nil
}

// earlier is a proposal of an earlier run of the plant still open or
// taken over.
type earlier struct {
	requisition, material uuid.UUID
	version               int32
	status                string
}

// counted are the requisition statuses in which a proposal a person took
// over is still to be ordered, so the plant expects it.
var counted = map[string]bool{"draft": true, "pending_approval": true, "approved": true, "in_sourcing": true}

// settle settles the plant's earlier proposals still open or taken over,
// in requisition order, the order their row locks are taken in: an open
// one still a draft at the version its run created it in is withdrawn
// (cancelled) and marked replaced by this run; one a person changed or
// submitted since is taken over, theirs from then on; one taken over and
// still to be ordered counts its items as the plant's receipts, and one
// ordered, cancelled or rejected is closed, counted no more.
func (e *Endpoints) settle(ctx context.Context, tx pgx.Tx, w *work) error {
	rows, _ := tx.Query(ctx, `SELECT requisition_id, material_id, requisition_version, status FROM mrp.proposal
		WHERE plant_id = $1 AND status IN ('open', 'taken_over') ORDER BY requisition_id FOR UPDATE`, w.run.PlantID)
	ps, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (earlier, error) {
		var x earlier
		return x, r.Scan(&x.requisition, &x.material, &x.version, &x.status)
	})
	if err != nil || len(ps) == 0 {
		return errs.Wrapf(err, "mrp: proposals of plant %s", w.plant.Code)
	}
	ids := make([]uuid.UUID, len(ps))
	for i, x := range ps {
		ids[i] = x.requisition
	}
	states, err := e.Supply.Requisitions(ctx, ids)
	if err != nil {
		return fmt.Errorf("mrp: requisitions proposed at plant %s: %w", w.plant.Code, err)
	}
	for _, x := range ps {
		status, err := e.settled(ctx, w, x, states[x.requisition])
		if err != nil {
			return err
		}
		if err := moveProposal(ctx, tx, w, x, status); err != nil {
			return err
		}
	}
	return nil
}

// settled is what becomes of an earlier proposal (settle), the
// requisition as it stands: replaced, taken over (counting its items)
// or closed.
func (e *Endpoints) settled(ctx context.Context, w *work, x earlier, r Requisition) (string, error) {
	if x.status == proposalOpen && r.Status == "draft" && r.Version == x.version {
		withdrawn, err := e.Supply.Withdraw(ctx, x.requisition, x.version)
		if err != nil {
			return "", fmt.Errorf("mrp: withdraw requisition %s: %w", x.requisition, err)
		}
		if withdrawn {
			w.replaced++
			return proposalReplaced, nil
		}
		// A person changed or cancelled it between the read and the
		// withdrawal's lock: read it again as they left it (read committed),
		// so the run counts what is on order now, not what it read before.
		again, err := e.Supply.Requisitions(ctx, []uuid.UUID{x.requisition})
		if err != nil {
			return "", fmt.Errorf("mrp: requisition %s: %w", x.requisition, err)
		}
		r = again[x.requisition]
	}
	if !counted[r.Status] {
		return proposalClosed, nil
	}
	for _, it := range r.Items {
		q, err := w.inBase(it)
		if err != nil {
			return "", err
		}
		if q != nil {
			w.receipts[it.MaterialID] = new(big.Rat).Add(orZero(w.receipts[it.MaterialID]), q)
		}
	}
	return proposalTaken, nil
}

// moveProposal records what became of an earlier proposal, when it
// changed.
func moveProposal(ctx context.Context, tx pgx.Tx, w *work, x earlier, status string) error {
	if status == x.status {
		return nil
	}
	var by *uuid.UUID
	if status == proposalReplaced {
		by = &w.run.ID
	}
	_, err := tx.Exec(ctx, `UPDATE mrp.proposal SET status = $2, replaced_by_run_id = $3 WHERE requisition_id = $1`, x.requisition, status, by)
	return errs.Wrapf(err, "mrp: proposal %s %s", x.requisition, status)
}

// orZero is r, or zero for none.
func orZero(r *big.Rat) *big.Rat {
	if r == nil {
		return new(big.Rat)
	}
	return r
}

// inBase is an open quantity in its material's base unit; nil for a
// material the run does not plan. A unit that is no longer one of the
// material's cannot be counted, so the run is refused (409
// mrp.unit_not_convertible) rather than plan with less than is there.
func (w *work) inBase(o Open) (*big.Rat, error) {
	return inBase(w.materials, o, w.plant.Code, "the run")
}

// inBase is an open quantity at the plant in its material's base unit;
// nil for a material not among the materials. A unit that is no longer
// one of the material's is refused (409 mrp.unit_not_convertible) naming
// who cannot count it, rather than count less than is there.
func inBase(ms map[uuid.UUID]materials.Material, o Open, plant, counter string) (*big.Rat, error) {
	m, ok := ms[o.MaterialID]
	if !ok {
		return nil, nil
	}
	f, err := m.Factor(o.Unit)
	if err != nil {
		return nil, problem.Conflict("mrp.unit_not_convertible", fmt.Sprintf("Material %s has %s %s open at plant %s, and %s is no longer one of its "+
			"units, so %s cannot count it in %s; give the material the unit again or change the document.", m.Number, o.Quantity, o.Unit,
			plant, o.Unit, counter, m.BaseUnit))
	}
	return new(big.Rat).Mul(money.ParseRat(o.Quantity), f), nil
}

// tally adds the open quantities up per material in its base unit into
// into and, with due, keeps each by its day: one conversion for the
// demand and the receipts, so a unit no longer the material's is refused
// alike for both.
func (w *work) tally(open []Due, into map[uuid.UUID]*big.Rat, due map[uuid.UUID][]dated) error {
	for _, o := range open {
		q, err := w.inBase(o.Open)
		if err != nil {
			return err
		}
		if q != nil { // nil: a material the run does not plan
			into[o.MaterialID] = new(big.Rat).Add(orZero(into[o.MaterialID]), q)
			if due != nil {
				due[o.MaterialID] = append(due[o.MaterialID], dated{day: o.Day, quantity: q})
			}
		}
	}
	return nil
}

// failureOf encodes a run's problem.
func failureOf(why *problem.Problem) []byte {
	b, _ := json.Marshal(RunError{Code: why.Code, Detail: why.Detail}) // two strings always encode
	return b
}
