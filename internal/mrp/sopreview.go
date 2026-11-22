package mrp

import (
	"cmp"
	"context"
	"fmt"
	"math/big"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// An S&OP review (ADR-0084 As built S2n, plan-supply-chain.md §S2n;
// SAP IBP's S&OP review in monthly buckets, from memory) counts, for each
// material the company plans at a plant (its planning data, whatever its
// MRP type) that is active, kept in stock and extended to the plant, its
// demand and supply there in its base unit, and adds them up by material
// group and unit, month by month of the horizon. Quantities stay exact
// (big.Rat) until a line writes them, to six decimals.
//
// The forecast of a month at a plant is S2m's rule (ADR-0082 As built
// S2m) in months: the moving average of the plant's goods issues of the
// HistoryDays to the review's day, never below nothing, spread over the
// month's days in the horizon, or what the plant's confirmed sales orders
// due in the month ordered and have not delivered, whichever is greater
// (the orders consume the forecast); an order due before the horizon is
// due in its first month, one due after it is left out. The supply the
// plant has now (stock, purchase orders' open quantities, stock
// transports, the proposals planning runs made that are still to be
// ordered) is the first month's: receipts carry no date here, as a
// planning run counts them (ADR-0084 §6).

// The review's horizon and history.
const (
	// HistoryDays are the 13 weeks of seven days the moving average is
	// of, ending on the review's day (S2m's HistoryWeeks).
	HistoryDays = 91
	// DefaultMonths and MaxMonths bound the horizon's calendar months.
	DefaultMonths = 3
	MaxMonths     = 12
)

// bucket is a month of the horizon: its first day, and the first and
// last of its days in the horizon.
type bucket struct{ month, first, last time.Time }

// days are the bucket's days.
func (b bucket) days() int64 { return int64(b.last.Sub(b.first).Hours())/24 + 1 }

// horizon is the months' buckets from the day after the day: the first
// from that day, each to its month's last day.
func horizon(day time.Time, months int) []bucket {
	first := day.AddDate(0, 0, 1)
	out := make([]bucket, months)
	for i := range out {
		month := time.Date(first.Year(), first.Month()+time.Month(i), 1, 0, 0, 0, 0, first.Location())
		out[i] = bucket{month: month, first: month, last: month.AddDate(0, 1, -1)}
	}
	out[0].first = first
	return out
}

// pair is a material planned at a plant.
type pair struct{ plant, material uuid.UUID }

// group is a line's material group and unit.
type group struct{ name, unit string }

// sums are a group's quantities: per month its forecast and orders, and
// the supply of the first month.
type sums struct {
	materials                              map[uuid.UUID]bool
	forecast, orders                       []*big.Rat
	stock, purchases, transports, proposed *big.Rat
}

// review is a review being counted: the company's day and horizon, the
// materials it plans by plant, and the groups' sums.
type review struct {
	le        tenancy.Entity
	day       time.Time
	months    []bucket
	materials map[uuid.UUID]materials.Material
	plants    []uuid.UUID               // by id
	planned   map[uuid.UUID][]uuid.UUID // each plant's materials, by id
	pairs     map[pair]bool
	codes     map[uuid.UUID]string // the plants' codes
	groups    map[group]*sums
}

// sumsOf is the material's group's sums, the material counted in it.
func (r *review) sumsOf(m materials.Material) *sums {
	g := group{m.Group, m.BaseUnit}
	s := r.groups[g]
	if s == nil {
		n := len(r.months)
		s = &sums{materials: map[uuid.UUID]bool{}, forecast: zeros(n), orders: zeros(n), stock: new(big.Rat), purchases: new(big.Rat),
			transports: new(big.Rat), proposed: new(big.Rat)}
		r.groups[g] = s
	}
	s.materials[m.ID] = true
	return s
}

// zeros are n exact zeros.
func zeros(n int) []*big.Rat {
	out := make([]*big.Rat, n)
	for i := range out {
		out[i] = new(big.Rat)
	}
	return out
}

// monthOf is the bucket an order due on the day is due in: the first for
// a day before the horizon, -1 for one after it.
func (r *review) monthOf(day time.Time) int {
	for i, b := range r.months {
		if !day.After(b.last) {
			return i
		}
	}
	return -1
}

// prepareReview reads the company's planned pairs of materials still active,
// kept in stock and extended to the plant, by plant and material, and the
// plants' codes; a company planning none has nothing to review (422
// mrp.sop_plan_empty).
func (e *Endpoints) prepareReview(ctx context.Context, tx pgx.Tx, r *review) error {
	rows, _ := tx.Query(ctx, `SELECT plant_id, material_id FROM mrp.material_plan WHERE legal_entity_id = $1 ORDER BY plant_id, material_id`, r.le.ID)
	var got []pair
	var x pair
	if _, err := pgx.ForEachRow(rows, []any{&x.plant, &x.material}, func() error { got = append(got, x); return nil }); err != nil {
		return fmt.Errorf("mrp: material plans of company %s: %w", r.le.Code, err)
	}
	ids := make([]uuid.UUID, len(got))
	for i, p := range got {
		ids[i] = p.material
	}
	var err error
	if r.materials, err = e.Materials.Materials(ctx, ids); err != nil {
		return fmt.Errorf("mrp: materials of company %s: %w", r.le.Code, err)
	}
	for _, p := range got {
		m, ok := r.materials[p.material]
		if _, extended := m.Plant(p.plant); !ok || m.Status != "active" || !m.Stocked() || !extended {
			continue
		}
		if len(r.planned[p.plant]) == 0 {
			r.plants = append(r.plants, p.plant)
		}
		r.planned[p.plant], r.pairs[p] = append(r.planned[p.plant], p.material), true
	}
	if len(r.plants) == 0 {
		return problem.Unprocessable("mrp.sop_plan_empty", fmt.Sprintf("Company %s plans no active material kept in stock at a plant it is extended to, "+
			"so there is nothing to review; plan one first (POST /v1/material-plans).", r.le.Code))
	}
	units, err := e.Plants.OrgUnits(ctx, r.plants)
	if err != nil {
		return fmt.Errorf("mrp: plants of company %s: %w", r.le.Code, err)
	}
	for id, u := range units {
		r.codes[id] = u.Code
	}
	return nil
}

// countPlant counts a plant's planned materials into their groups: each
// month's forecast and orders, and the first month's stock, purchase
// orders and stock transports.
func (e *Endpoints) countPlant(ctx context.Context, r *review, plant uuid.UUID) error {
	ids, code := r.planned[plant], r.codes[plant]
	issued, err := e.Supply.Issues(ctx, plant, ids, r.day.AddDate(0, 0, 1-HistoryDays), r.day)
	if err != nil {
		return fmt.Errorf("mrp: goods issues at plant %s: %w", code, err)
	}
	ordered, err := e.ordersDue(ctx, r, plant)
	if err != nil {
		return err
	}
	stock, err := e.Supply.Stock(ctx, plant, ids)
	if err != nil {
		return fmt.Errorf("mrp: stock at plant %s: %w", code, err)
	}
	receipts, err := e.Supply.Receipts(ctx, plant, ids)
	if err != nil {
		return fmt.Errorf("mrp: open receipts at plant %s: %w", code, err)
	}
	transports, err := e.Supply.Transports(ctx, plant, ids)
	if err != nil {
		return fmt.Errorf("mrp: stock transports at plant %s: %w", code, err)
	}
	for _, id := range ids {
		m := r.materials[id]
		s := r.sumsOf(m)
		rate := money.MaxRat(orZero(money.ParseRat(issued[id])), new(big.Rat))
		rate.Quo(rate, big.NewRat(HistoryDays, 1))
		for i, b := range r.months {
			o := ordered[id][i]
			s.forecast[i].Add(s.forecast[i], money.MaxRat(new(big.Rat).Mul(rate, big.NewRat(b.days(), 1)), o))
			s.orders[i].Add(s.orders[i], o)
		}
		s.stock.Add(s.stock, orZero(money.ParseRat(stock[id])))
	}
	if err := r.add(receipts, code, func(s *sums) *big.Rat { return s.purchases }); err != nil {
		return err
	}
	return r.add(transports, code, func(s *sums) *big.Rat { return s.transports })
}

// ordersDue are what the plant's confirmed orders due in each month of
// the horizon demand of its planned materials, in their base units.
func (e *Endpoints) ordersDue(ctx context.Context, r *review, plant uuid.UUID) (map[uuid.UUID][]*big.Rat, error) {
	due, err := e.Supply.DemandDue(ctx, plant, r.planned[plant])
	if err != nil {
		return nil, fmt.Errorf("mrp: demand at plant %s: %w", r.codes[plant], err)
	}
	out := map[uuid.UUID][]*big.Rat{}
	for _, id := range r.planned[plant] {
		out[id] = zeros(len(r.months))
	}
	for _, d := range due { // sales answers for the materials asked alone, each planned at the plant
		i := r.monthOf(d.Day)
		if i < 0 {
			continue
		}
		q, err := inBase(r.materials, d.Open, r.codes[plant], "the review")
		if err != nil {
			return nil, err
		}
		out[d.MaterialID][i].Add(out[d.MaterialID][i], q)
	}
	return out, nil
}

// add adds open quantities at the plant to the sums field gives of each
// one's group, in its base unit.
func (r *review) add(open []Open, plant string, field func(*sums) *big.Rat) error {
	for _, o := range open {
		q, err := inBase(r.materials, o, plant, "the review")
		if err != nil {
			return err
		}
		if q != nil {
			f := field(r.sumsOf(r.materials[o.MaterialID]))
			f.Add(f, q)
		}
	}
	return nil
}

// countProposals counts the requisitions planning runs proposed at the
// company's plants, open or taken over, that are still to be ordered
// (counted): their items of materials planned at the proposal's plant,
// into the first month's proposals.
func (e *Endpoints) countProposals(ctx context.Context, tx pgx.Tx, r *review) error {
	rows, _ := tx.Query(ctx, `SELECT requisition_id, plant_id FROM mrp.proposal WHERE plant_id = ANY($1) AND status IN ('open', 'taken_over')
		ORDER BY requisition_id`, r.plants)
	at := map[uuid.UUID]uuid.UUID{}
	var ids []uuid.UUID
	var req, plant uuid.UUID
	if _, err := pgx.ForEachRow(rows, []any{&req, &plant}, func() error { at[req], ids = plant, append(ids, req); return nil }); err != nil {
		return fmt.Errorf("mrp: proposals at the plants of company %s: %w", r.le.Code, err)
	}
	if len(ids) == 0 {
		return nil
	}
	states, err := e.Supply.Requisitions(ctx, ids)
	if err != nil {
		return fmt.Errorf("mrp: requisitions proposed in company %s: %w", r.le.Code, err)
	}
	for _, id := range ids {
		st := states[id]
		if !counted[st.Status] {
			continue
		}
		var mine []Open
		for _, it := range st.Items {
			if r.pairs[pair{at[id], it.MaterialID}] {
				mine = append(mine, it)
			}
		}
		if err := r.add(mine, r.codes[at[id]], func(s *sums) *big.Rat { return s.proposed }); err != nil {
			return err
		}
	}
	return nil
}

// lines are the review's lines, by group, unit and month, the consensus
// the forecast; the supply stands in the first month.
func (r *review) lines() []SopLine {
	keys := make([]group, 0, len(r.groups))
	for g := range r.groups {
		keys = append(keys, g)
	}
	slices.SortFunc(keys, func(a, b group) int { return cmp.Or(cmp.Compare(a.name, b.name), cmp.Compare(a.unit, b.unit)) })
	var out []SopLine
	for _, g := range keys {
		s := r.groups[g]
		for i, b := range r.months {
			l := SopLine{Group: g.name, Unit: g.unit, Month: b.month, FirstDay: b.first, LastDay: b.last, Materials: int32(len(s.materials)),
				Forecast: money.PlainQuantity(s.forecast[i]), SalesOrders: money.PlainQuantity(s.orders[i]), Stock: "0", PurchaseOrders: "0",
				StockTransports: "0", Proposals: "0"}
			if i == 0 {
				l.Stock, l.PurchaseOrders = money.PlainQuantity(s.stock), money.PlainQuantity(s.purchases)
				l.StockTransports, l.Proposals = money.PlainQuantity(s.transports), money.PlainQuantity(s.proposed)
			}
			l.Consensus = l.Forecast
			out = append(out, l)
		}
	}
	return out
}

// countReview counts the company's review on its day over the months.
func (e *Endpoints) countReview(ctx context.Context, tx pgx.Tx, le tenancy.Entity, day time.Time, months int) ([]SopLine, error) {
	r := &review{le: le, day: day, months: horizon(day, months), planned: map[uuid.UUID][]uuid.UUID{}, pairs: map[pair]bool{},
		codes: map[uuid.UUID]string{}, groups: map[group]*sums{}}
	if err := e.prepareReview(ctx, tx, r); err != nil {
		return nil, err
	}
	for _, plant := range r.plants {
		if err := e.countPlant(ctx, r, plant); err != nil {
			return nil, err
		}
	}
	if err := e.countProposals(ctx, tx, r); err != nil {
		return nil, err
	}
	return r.lines(), nil
}
