package collections

import (
	"fmt"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// TestReach: an item reaches the last level whose days overdue it has
// reached, one above the level it was last dunned at at most, and never
// falls below that; below the first level's days an item never dunned
// reaches none (ADR-0065 §4).
func TestReach(t *testing.T) {
	levels := []config.DunningLevel{{Level: 1, DaysOverdue: 5}, {Level: 2, DaysOverdue: 20}, {Level: 3, DaysOverdue: 40}}
	for _, c := range []struct {
		name                 string
		days, previous, want int
	}{
		{"not overdue", 0, 0, 0},
		{"a day short of the first level", 4, 0, 0},
		{"at the first level's days", 5, 0, 1},
		{"far overdue, never dunned: one level", 400, 0, 1},
		{"dunned once, at the second level's days", 20, 1, 2},
		{"dunned once, a day short of the second level", 19, 1, 1},
		{"dunned once, past every level: one level more", 400, 1, 2},
		{"dunned twice, past every level", 41, 2, 3},
		{"at the highest level, which stays", 400, 3, 3},
		{"dunned at a level its days no longer reach", 6, 2, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := reach(levels, c.days, c.previous); got != c.want {
				t.Errorf("reach(%d days, previous %d): got %d, want %d", c.days, c.previous, got, c.want)
			}
		})
	}
	t.Run("no levels", func(t *testing.T) {
		if got := reach(nil, 400, 0); got != 0 {
			t.Errorf("reach without levels: got %d, want 0", got)
		}
	})
	t.Run("one level", func(t *testing.T) {
		one := levels[:1]
		if got := fmt.Sprint(reach(one, 400, 0), reach(one, 400, 1)); got != "1 1" {
			t.Errorf("never dunned and dunned once: got %s, want 1 1", got)
		}
	})
}

// TestRuleOfDroppedLevel (review round 1): an item kept at level 3 after
// the company dropped to two levels is charged as level 2, the highest,
// not refused or failed.
func TestRuleOfDroppedLevel(t *testing.T) {
	proc := procedure{levels: []config.DunningLevel{{Level: 1, DaysOverdue: 5}, {Level: 2, DaysOverdue: 20, Charge: "15.00", Interest: true}},
		rate: 9000, exp: 2}
	run := Run{}
	notices, err := charge(&run, []Notice{{Items: []Item{{Level: 3, AmountLC: 16000, DaysOverdue: 61}}}}, proc)
	if err != nil || len(notices) != 1 {
		t.Fatalf("got %v, %v, want one notice", notices, err)
	}
	if n := notices[0]; n.Level != 3 || n.ChargeAmount != 1500 || n.InterestAmount != 241 {
		t.Errorf("got level %d, charge %d, interest %d, want 3, 1500, 241", n.Level, n.ChargeAmount, n.InterestAmount)
	}
}

// TestInterestOn: interest is amount × rate × days ÷ 365, the rate in
// thousandths of a percent a year, rounded half away from zero in the
// amount's minor units; out of range is an error (T76d).
func TestInterestOn(t *testing.T) {
	for _, c := range []struct {
		name   string
		amount money.Minor
		rate   int64
		days   int32
		want   money.Minor
	}{
		{"160.00 at 9% for 61 days", 16000, 9000, 61, 241},
		{"half a cent rounds up", 18250, 1000, 1, 1},
		{"short of half a cent rounds down", 18249, 1000, 1, 0},
		{"a year at 10%", 100000, 10000, 365, 10000},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := interestOn(c.amount, c.rate, c.days)
			if err != nil || got != c.want {
				t.Errorf("got %d (%v), want %d", got, err, c.want)
			}
		})
	}
	if _, err := interestOn(money.Minor(1)<<62, 99999, 30000); err == nil {
		t.Error("interest out of range: got no error, want one")
	}
}
