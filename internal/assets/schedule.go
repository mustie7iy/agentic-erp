package assets

import (
	"fmt"
	"slices"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// Month is a regular posting period as a count of months: a fiscal year's
// twelve regular periods are calendar months from its start month (the
// ledger's calendars, internal/ledger/setup.go), so the k-th period from a
// depreciation start is k-1 months after it, whatever the fiscal year.
type Month int

// MonthOf is the regular period a date falls in.
func MonthOf(d time.Time) Month { return Month(d.Year()*12 + int(d.Month()) - 1) }

// Plan is an asset's depreciation terms in one ledger as the schedule
// walks them (ADR-0057 §7): the method, the useful life in periods, the
// declining factor (a plain decimal; declining balance only), the terms'
// residual value and the period depreciation starts in (nil: none yet,
// a planned asset without a depreciation start), and its changes in
// estimate in month order.
type Plan struct {
	Method   string
	Life     int32
	Factor   string
	Residual money.Minor
	Start    *Month
	Changes  []Change
}

// Change is a change in estimate (ADR-0057 §7, T57j; IAS 8.36-38): from
// the start of its period on, its method, useful life (still counted from
// the depreciation start), factor and residual value replace the plan's,
// so the periods before keep what was planned for them, and each period
// from it on depreciates the net book value then over the life left. Of
// changes from one period, the last one given wins.
type Change struct {
	Month    Month
	Method   string
	Life     int32
	Factor   string
	Residual money.Minor
}

// at is the plan with the changes from the period or before applied: the
// terms in force in it.
func (p Plan) at(m Month) Plan {
	for _, c := range p.Changes {
		if c.Month > m {
			break
		}
		p.Method, p.Life, p.Factor, p.Residual = c.Method, c.Life, c.Factor, c.Residual
	}
	return p
}

// ResidualAt is the terms' residual value in force in the period, the
// plan's or its latest change's by then (Plan.at): what a retirement or
// transfer valued in it takes its share of (ADR-0057 §8).
func (p Plan) ResidualAt(m Month) money.Minor { return p.at(m).Residual }

// Move is a movement the schedule applies at the start of the period of
// its value date (ADR-0057 §7): an acquisition's cost, the cost,
// accumulated depreciation (negative, a credit) and residual value a
// transfer in brought, or those a retirement or transfer out took
// (negated); each the signed change it recorded. Depreciation movements
// are not moves: the walk computes depreciation, it does not read it.
type Move struct {
	Month                       Month
	Cost, Accumulated, Residual money.Minor
}

// Step is one period of the walk: its depreciation d_k (zero before the
// depreciation start) and the net book value at its end, after d_k.
type Step struct {
	Month        Month
	Depreciation money.Minor
	BookValue    money.Minor
}

// Walk is the planned schedule (ADR-0057 §7) from the earliest of the
// depreciation start and the moves' periods through the period through:
// the moves of a period are applied at its start, those after through
// are ignored, and each period from the start (k = 1) takes d_k of the
// net book value B_k and residual value R_k then, by the method, rounded
// half away from zero to minor units and held to max(0, min(d_k, B_k -
// R_k)); from k = n on, d_k = max(0, B_k - R_k). Moves are applied in the
// order given within a period; their effects add, so the order changes
// nothing. A plan whose start is after through, without moves by then,
// has no steps.
func Walk(p Plan, moves []Move, through Month) ([]Step, error) {
	var steps []Step
	err := walk(p, moves, through, func(s Step) { steps = append(steps, s) })
	return steps, err
}

// PlannedThrough is Planned(Walk(p, moves, through), through) without the
// steps: a run's batch and its preview walk every selected asset, and
// keeping each one's steps allocated some 270 MB a preview over 100,000
// assets (T57f3).
func PlannedThrough(p Plan, moves []Move, through Month) (money.Minor, error) {
	var sum money.Minor
	err := walk(p, moves, through, func(s Step) { sum += s.Depreciation })
	return sum, err
}

// walk visits Walk's steps in order.
func walk(p Plan, moves []Move, through Month, visit func(Step)) error {
	byMonth := func(a, b Move) int { return int(a.Month) - int(b.Month) }
	if !slices.IsSortedFunc(moves, byMonth) { // copied only to sort: most assets have one move
		moves = slices.Clone(moves)
		slices.SortStableFunc(moves, byMonth)
	}
	first := through + 1
	if p.Start != nil {
		first = min(first, *p.Start)
	}
	if len(moves) > 0 {
		first = min(first, moves[0].Month)
	}
	// taken: accumulated depreciation moves brought (negative) or took
	// (positive); moved: the residual value they brought or took.
	var cost, taken, moved, depreciation money.Minor
	i := 0
	for m := first; m <= through; m++ {
		for ; i < len(moves) && moves[i].Month <= m; i++ {
			cost, taken, moved = cost+moves[i].Cost, taken+moves[i].Accumulated, moved+moves[i].Residual
		}
		book := cost + taken - depreciation
		var d money.Minor
		if p.Start != nil && m >= *p.Start {
			in := p.at(m)
			var err error
			if d, err = in.period(int32(m-*p.Start)+1, book, in.Residual+moved); err != nil {
				return err
			}
		}
		depreciation += d
		visit(Step{Month: m, Depreciation: d, BookValue: book - d})
	}
	return nil
}

// Planned is the planned depreciation through a period, Σ d_k for k up to
// it (ADR-0057 §7, planned(P)).
func Planned(steps []Step, through Month) money.Minor {
	var sum money.Minor
	for _, s := range steps {
		if s.Month <= through {
			sum += s.Depreciation
		}
	}
	return sum
}

// period is d_k of the k-th period from the start, with B_k the net book
// value and R_k the residual value then.
func (p Plan) period(k int32, book, residual money.Minor) (money.Minor, error) {
	left := book - residual
	if k >= p.Life {
		return max(0, left), nil
	}
	// Divide fails only on a divisor below one or the most negative
	// amount divided by one; the periods left here are two or more.
	straight, _ := money.Divide(left, int64(p.Life-k+1))
	d := straight
	switch p.Method {
	case config.StraightLine:
	case config.DecliningBalance:
		declining, err := money.Scale(book, p.Factor, int64(p.Life))
		if err != nil {
			return 0, fmt.Errorf("assets: declining balance depreciation of %d at %s over %d periods: %w", book, p.Factor, p.Life, err)
		}
		d = max(declining, straight)
	default:
		return 0, fmt.Errorf("assets: depreciation method %q is not one the schedule knows", p.Method)
	}
	return max(0, min(d, left)), nil
}
