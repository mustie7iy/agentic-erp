package collections

import (
	"fmt"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
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
