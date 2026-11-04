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
// a planned asset without a depreciation start).
type Plan struct {
	Method   string
	Life     int32
	Factor   string
	Residual money.Minor
	Start    *Month
}

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
	moves = slices.Clone(moves)
	slices.SortStableFunc(moves, func(a, b Move) int { return int(a.Month) - int(b.Month) })
	first := through + 1
	if p.Start != nil {
		first = min(first, *p.Start)
	}
	if len(moves) > 0 {
		first = min(first, moves[0].Month)
	}
	var steps []Step
	var cost, taken, depreciation money.Minor // taken: accumulated depreciation moves brought (negative) or took (positive)
	residual, i := p.Residual, 0
	for m := first; m <= through; m++ {
		for ; i < len(moves) && moves[i].Month <= m; i++ {
			cost, taken, residual = cost+moves[i].Cost, taken+moves[i].Accumulated, residual+moves[i].Residual
		}
		book := cost + taken - depreciation
		var d money.Minor
		if p.Start != nil && m >= *p.Start {
			var err error
			if d, err = p.period(int32(m-*p.Start)+1, book, residual); err != nil {
				return nil, err
			}
		}
		depreciation += d
		steps = append(steps, Step{Month: m, Depreciation: d, BookValue: book - d})
	}
	return steps, nil
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
