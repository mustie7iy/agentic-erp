package assets_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/assets"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// month is a period of 2026 by its number (January 1), or of a later year
// past 12.
func month(n int) assets.Month {
	return assets.MonthOf(time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)) + assets.Month(n-1)
}

func start(n int) *assets.Month {
	m := month(n)
	return &m
}

func straight(life int32, residual money.Minor, from int) assets.Plan {
	return assets.Plan{Method: "straight_line", Life: life, Residual: residual, Start: start(from)}
}

func declining(life int32, factor string, residual money.Minor, from int) assets.Plan {
	return assets.Plan{Method: "declining_balance", Life: life, Factor: factor, Residual: residual, Start: start(from)}
}

// acquired is an acquisition of cost valued in the period.
func acquired(n int, cost money.Minor) assets.Move { return assets.Move{Month: month(n), Cost: cost} }

// depreciations are the steps' d_k from the first step, and the net book
// value after the last.
func depreciations(steps []assets.Step) string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = fmt.Sprint(int64(s.Depreciation))
	}
	if len(steps) == 0 {
		return "none"
	}
	return strings.Join(out, " ") + fmt.Sprintf(" | %d", steps[len(steps)-1].BookValue)
}

// TestScheduleWalk: the planned schedule of ADR-0057 §7 per method, in
// minor units, through a period, with planned(P) − booked as a run of P
// posts it and, for a retirement or transfer valued in V, as its catch-up
// planned(V−1) − booked posts it.
func TestScheduleWalk(t *testing.T) {
	cases := []struct {
		name    string
		plan    assets.Plan
		moves   []assets.Move
		through int
		booked  money.Minor
		want    string // d_k from the first step | net book value at the end
		planned money.Minor
	}{
		{name: "straight-line, one acquisition", plan: straight(12, 0, 1), moves: []assets.Move{acquired(1, 1200)}, through: 12,
			want: "100 100 100 100 100 100 100 100 100 100 100 100 | 0", planned: 1200},
		{name: "straight-line to a residual value", plan: straight(12, 120, 1), moves: []assets.Move{acquired(1, 1200)}, through: 12,
			want: "90 90 90 90 90 90 90 90 90 90 90 90 | 120", planned: 1080},
		{name: "straight-line rounded half away from zero over the remaining life", plan: straight(3, 0, 1), moves: []assets.Move{acquired(1, 1000)},
			through: 3, want: "333 334 333 | 0", planned: 1000},
		{name: "declining balance switching to straight-line", plan: declining(12, "2", 0, 1), moves: []assets.Move{acquired(1, 1200)}, through: 12,
			want: "200 167 139 116 96 80 67 67 67 67 67 67 | 0", planned: 1200},
		{name: "declining balance to a residual value", plan: declining(4, "2", 100, 1), moves: []assets.Move{acquired(1, 1000)}, through: 4,
			want: "500 250 125 25 | 100", planned: 900},
		{name: "declining balance at a decimal factor", plan: declining(10, "1.5", 0, 1), moves: []assets.Move{acquired(1, 10000)}, through: 2,
			want: "1500 1275 | 7225", planned: 2775},
		{name: "a run of March: one period's depreciation", plan: straight(12, 0, 1), moves: []assets.Move{acquired(1, 1200)}, through: 3, booked: 200,
			want: "100 100 100 | 900", planned: 300},
		{name: "a repeat run of March posts nothing", plan: straight(12, 0, 1), moves: []assets.Move{acquired(1, 1200)}, through: 3, booked: 300,
			want: "100 100 100 | 900", planned: 300},
		{name: "a late acquisition valued in February, caught up by the March run", plan: straight(12, 0, 1),
			moves: []assets.Move{acquired(1, 1200), acquired(2, 600)}, through: 3, booked: 200, want: "100 155 155 | 1390", planned: 410},
		{name: "an acquisition valued before a retirement posted earlier", plan: straight(12, 0, 1),
			moves: []assets.Move{acquired(1, 1200), {Month: month(4), Cost: -600, Accumulated: 150}, acquired(2, 300)}, through: 4,
			want: "100 127 127 77 | 619", planned: 431},
		{name: "two retirements in one period: the sale's catch-up through February posts March's run back", plan: straight(12, 0, 1),
			moves: []assets.Move{acquired(1, 1200)}, through: 2, booked: 300, want: "100 100 | 1000", planned: 200},
		{name: "two retirements in one period: the scrapping's catch-up after the sale's is zero", plan: straight(12, 0, 1),
			moves: []assets.Move{acquired(1, 1200), {Month: month(3), Cost: -600, Accumulated: 100}}, through: 2, booked: 200,
			want: "100 100 | 1000", planned: 200},
		{name: "two retirements in one period: nothing left to depreciate in March", plan: straight(12, 0, 1),
			moves:   []assets.Move{acquired(1, 1200), {Month: month(3), Cost: -600, Accumulated: 100}, {Month: month(3), Cost: -600, Accumulated: 100}},
			through: 4, booked: 200, want: "100 100 0 0 | 0", planned: 200},
		{name: "a partial retirement's residual value taken", plan: straight(12, 120, 1),
			moves: []assets.Move{acquired(1, 1200), {Month: month(4), Cost: -600, Accumulated: 135, Residual: -60}}, through: 5,
			want: "90 90 90 45 45 | 375", planned: 360},
		{name: "a transfer in's cost, accumulated depreciation and residual value", plan: straight(12, 0, 5),
			moves: []assets.Move{{Month: month(5), Cost: 600, Accumulated: -100, Residual: 60}}, through: 6, want: "37 37 | 426", planned: 74},
		{name: "end of life: cost arriving after it depreciated in its period", plan: straight(3, 0, 1),
			moves: []assets.Move{acquired(1, 300), acquired(5, 90)}, through: 6, want: "100 100 100 0 90 0 | 0", planned: 390},
		{name: "end of life: a declining balance asset at its residual value", plan: declining(2, "2", 10, 1),
			moves: []assets.Move{acquired(1, 100)}, through: 4, want: "90 0 0 0 | 10", planned: 90},
		{name: "a credit memo leaving the book value below the residual value: no depreciation, never negative", plan: straight(10, 100, 1),
			moves: []assets.Move{acquired(1, 1000), acquired(2, -850)}, through: 3, want: "90 0 0 | 60", planned: 90},
		{name: "a cost share rounded up and its residual value down leave the book value a unit below it", plan: straight(2, 4, 1),
			moves: []assets.Move{acquired(1, 10), {Month: month(2), Cost: -7, Accumulated: 2, Residual: -1}}, through: 3,
			want: "3 0 0 | 2", planned: 3},
		{name: "a depreciation start after the acquisition", plan: straight(12, 0, 3), moves: []assets.Move{acquired(1, 1200)}, through: 4,
			want: "0 0 100 100 | 1000", planned: 200},
		{name: "an acquisition reversed: its reversal cancels it", plan: straight(12, 0, 1),
			moves: []assets.Move{acquired(1, 1200), acquired(1, -1200)}, through: 2, booked: 100, want: "0 0 | 0", planned: 0},
		{name: "no depreciation start: a planned asset whose acquisition was reversed", plan: assets.Plan{Method: "straight_line", Life: 12},
			moves: []assets.Move{acquired(1, 1200), acquired(1, -1200)}, through: 2, booked: 100, want: "0 0 | 0", planned: 0},
		{name: "a start after the period: no steps", plan: straight(12, 0, 6), through: 2, want: "none", planned: 0},
		{name: "a move after the period is ignored", plan: straight(12, 0, 1), moves: []assets.Move{acquired(1, 1200), acquired(3, 600)}, through: 2,
			want: "100 100 | 1000", planned: 200},
		{name: "across a year end", plan: straight(24, 0, 12), moves: []assets.Move{acquired(12, 2400)}, through: 14, want: "100 100 100 | 2100",
			planned: 300},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			steps, err := assets.Walk(c.plan, c.moves, month(c.through))
			if err != nil {
				t.Fatalf("walk: got %v, want none", err)
			}
			if got := depreciations(steps); got != c.want {
				t.Errorf("schedule: got %s, want %s", got, c.want)
			}
			if got := assets.Planned(steps, month(c.through)); got != c.planned {
				t.Errorf("planned: got %d, want %d", got, c.planned)
			}
		})
	}
}

// TestCatchUps: the difference planned − booked a run or catch-up posts
// (ADR-0057 §7) in the leaf's two-retirement case: cost 1200 over twelve
// periods, runs of January to March posted (300); half sold valued March,
// whose catch-up through February is −100; then the rest scrapped valued
// March, whose catch-up is 0, its loss the 500 of book value left.
func TestCatchUps(t *testing.T) {
	plan, moves := straight(12, 0, 1), []assets.Move{acquired(1, 1200)}
	booked := money.Minor(300)
	steps, _ := assets.Walk(plan, moves, month(2))
	sale := assets.Planned(steps, month(2)) - booked
	if sale != -100 {
		t.Errorf("the sale's catch-up: got %d, want -100", sale)
	}
	booked += sale
	cost, accumulated := money.Minor(1200), -booked
	soldCost, soldAccumulated := cost/2, -accumulated/2
	moves = append(moves, assets.Move{Month: month(3), Cost: -soldCost, Accumulated: soldAccumulated})
	steps, _ = assets.Walk(plan, moves, month(2))
	scrapping := assets.Planned(steps, month(2)) - booked
	if scrapping != 0 {
		t.Errorf("the scrapping's catch-up: got %d, want 0", scrapping)
	}
	restCost, restAccumulated := cost-soldCost, -(accumulated + soldAccumulated)
	if loss := restCost - restAccumulated; loss != 500 {
		t.Errorf("the scrapping's loss: got %d, want 500", loss)
	}
	moves = append(moves, assets.Move{Month: month(3), Cost: -restCost, Accumulated: restAccumulated})
	steps, _ = assets.Walk(plan, moves, month(3))
	if run := assets.Planned(steps, month(3)) - booked; run != 0 {
		t.Errorf("a repeat run of March after both: got %d, want 0", run)
	}
}

// TestScheduleWalkRefused: terms the walk cannot compute fail it: an
// unknown method, a declining factor that is no decimal, and a declining
// amount out of range.
func TestScheduleWalkRefused(t *testing.T) {
	for name, c := range map[string]struct {
		plan assets.Plan
		cost money.Minor
	}{
		"an unknown method":     {assets.Plan{Method: "sum_of_years", Life: 12, Start: start(1)}, 1200},
		"a factor of no number": {declining(12, "two", 0, 1), 1200},
		"out of range":          {declining(2, "10", 0, 1), money.Minor(1) << 62},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := assets.Walk(c.plan, []assets.Move{acquired(1, c.cost)}, month(1)); err == nil {
				t.Error("walk: got no error, want one")
			}
		})
	}
	_, err := assets.Walk(declining(2, "10", 0, 1), []assets.Move{acquired(1, money.Minor(1)<<62)}, month(1))
	if !errors.Is(err, money.ErrOverflow) {
		t.Errorf("out of range: got %v, want money.ErrOverflow", err)
	}
}
