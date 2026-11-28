package mrp

import (
	"fmt"
	"math/big"
	"slices"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Period lot sizes (ADR-0084 As built, period lot sizes; SAP MARC-DISLS
// TB, WB and MB, from memory): a run projects what the plant has, its
// stock and receipts, down by each demand on the day it is due, and
// wherever the projection falls below the reorder point it needs the
// difference; the needs of one period, a day, an ISO week (Monday to
// Sunday) or a calendar month of the company's days, are one lot, needed
// by the period's first requirement date: the first day the projection
// falls below the reorder point in it. The lot is bounded and rounded as
// lot for lot is (bounded), and what it brings counts towards the next
// periods. Demand overdue is needed on the run's day, the day a plant
// already below its reorder point needs its first lot. Receipts carry no
// date of their own here (the reads' open quantities), so they count
// from the run's day, as the reorder point procedure counts them.

// lot is a quantity a run proposes, needed by a day.
type lot struct {
	quantity *big.Rat
	due      time.Time
}

// dated is a demand of a material in its base unit due on a day.
type dated struct {
	day      time.Time
	quantity *big.Rat
}

// onDay is the lots of the quantities, each needed by the day.
func onDay(quantities []*big.Rat, day time.Time) []lot {
	out := make([]lot, len(quantities))
	for i, q := range quantities {
		out[i] = lot{quantity: q, due: day}
	}
	return out
}

// periodStart is the first day of the procedure's period that day falls
// in: the day itself, the Monday of its ISO week, or the first of its
// month. Days are dates at midnight UTC, as every date is held
// (clock.DayIn).
func periodStart(procedure string, day time.Time) time.Time {
	switch procedure {
	case Weekly:
		return day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7)) // Monday is 0 days into its week, Sunday 6
	case Monthly:
		return time.Date(day.Year(), day.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	return day
}

// projection is a material's quantity projected through its demand by
// day, against its reorder point, and the lots proposed so far.
type projection struct {
	reorderPoint, projected *big.Rat
	lots                    []lot
}

// periodLots sizes a material's demand by period from the run's day: the
// lots a run proposes, in the order of their days, none while the
// projection stays at or above the reorder point. More than MaxLots lots
// over the periods is 409 mrp.too_many_lots, naming the material.
func (l LotSizing) periodLots(reorderPoint, supply *big.Rat, demand []dated, day time.Time, material string) ([]lot, error) {
	// The run's day leads, with nothing due: a plant below its reorder
	// point before any demand needs its first lot that day.
	days := append([]dated{{day: day, quantity: new(big.Rat)}}, demand...)
	for i := range days {
		if days[i].day.Before(day) {
			days[i].day = day
		}
	}
	slices.SortStableFunc(days, func(a, b dated) int { return a.day.Compare(b.day) })
	p := projection{reorderPoint: reorderPoint, projected: new(big.Rat).Set(supply)}
	for i := 0; i < len(days); {
		n, err := l.period(&p, days[i:], material)
		if err != nil {
			return nil, err
		}
		i += n
	}
	return p.lots, nil
}

// period projects the demand of the period the first of days falls in
// and proposes its lots, needed by its first requirement date, when the
// projection falls below the reorder point in it; it returns how many
// of days were the period's.
func (l LotSizing) period(p *projection, days []dated, material string) (int, error) {
	start := periodStart(l.Procedure, days[0].day)
	var first *time.Time
	n := 0
	for ; n < len(days) && periodStart(l.Procedure, days[n].day).Equal(start); n++ {
		p.projected.Sub(p.projected, days[n].quantity)
		if first == nil && p.projected.Cmp(p.reorderPoint) < 0 {
			first = &days[n].day
		}
	}
	if first == nil {
		return n, nil
	}
	quantities, err := l.bounded(money.CeilQuantity(new(big.Rat).Sub(p.reorderPoint, p.projected)), material)
	if err != nil {
		return 0, err
	}
	for _, q := range quantities {
		p.projected.Add(p.projected, q)
	}
	if p.lots = append(p.lots, onDay(quantities, *first)...); len(p.lots) > MaxLots {
		return 0, problem.Conflict("mrp.too_many_lots", fmt.Sprintf("Material %s needs more than %d lots over its %s periods, more than a run "+
			"proposes; size it by a longer period or a larger maximum lot size (updateMaterialPlan).", material, MaxLots, l.Procedure))
	}
	return n, nil
}
